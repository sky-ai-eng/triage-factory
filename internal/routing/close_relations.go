package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// This file is the close phase: the single, declared mechanism by which an
// event resolves existing tasks. It replaces the old hardcoded close subsystem
// (a per-event-type switch + a separate entity-wide cascade). Every close —
// "ci_check_passed closes ci_check_failed", "a submitted review satisfies the
// reviewer's request", "pr_merged closes everything" — is one closeRelation,
// distinguished only by data: which event triggers it, which task types it
// closes, an optional gate, an optional per-task filter, and whether it also
// terminates the entity.
//
// Closing is orthogonal to routing. runCloses runs before handler matching, so
// the SAME event can close prior tasks AND go on to create/fire its own (the
// close can't catch a task that does not exist yet — e.g. the riding task a
// terminating event mints for its lifecycle blueprint).

// closeContext is the per-firing value a relation's prepare step computes once
// and its keep predicate consumes per task (a reviewer dedup key, the new
// assignee's owning teams). Opaque to the driver.
type closeContext any

// closeRelation declares that one or more events close some set of active tasks
// on the same entity. Conditions stay in Go (snapshot/identity-coupled, not a
// config DSL) but are reached uniformly through prepare/keep.
type closeRelation struct {
	// onEvents are the event types that trigger this close.
	onEvents []string
	// closes are the target event types whose active tasks this relation
	// closes. A typed close lists one type; an entity-wide close lists the
	// entity's full task-event family.
	closes []string
	// prepare runs once per firing, after target tasks are found to exist.
	// ok=false skips the whole relation (the snapshot / identity gate); the
	// returned context is threaded to keep. nil prepare = proceed with a nil
	// context.
	prepare func(ctx context.Context, r *Router, orgID string, evt domain.Event, entityID string) (closeContext, bool)
	// keep decides per task whether it actually closes (dedup-key narrowing,
	// member-aware skip). nil keep = close every active task of a target type.
	keep func(evt domain.Event, ctx closeContext, t domain.Task) bool
	// terminatesEntity also flips the entity to closed after the tasks close
	// (stop polling / straggler tasks). This is what makes an event
	// "entity-terminating" — there is no separate hardcoded set.
	terminatesEntity bool
	// closeReason audits why the task closed. Defaults to "auto_closed_by_event".
	closeReason string
}

// closeRelations is the system-declared close model. Behavior here is a
// faithful migration of the prior close_checks.go logic; the shape is now
// uniform.
var closeRelations = []closeRelation{
	// CI went green → close the entity's CI-failure tasks, but only if no
	// check is still failing or still running at the latest SHA.
	{
		onEvents: []string{domain.EventGitHubPRCICheckPassed},
		closes:   []string{domain.EventGitHubPRCICheckFailed},
		prepare:  prepareCIPassed,
	},
	// A reviewer submitted a review (any verdict, including changes_requested)
	// → their review_requested obligation is satisfied. Close only that
	// reviewer's per-reviewer task (keyed by user dedup_key).
	{
		onEvents: []string{
			domain.EventGitHubPRReviewApproved,
			domain.EventGitHubPRReviewCommented,
			domain.EventGitHubPRReviewDismissed,
			domain.EventGitHubPRReviewChangesRequested,
		},
		closes:  []string{domain.EventGitHubPRReviewRequested},
		prepare: prepareReviewerKey,
		keep:    keepDedupKey,
	},
	// A review that is NOT a changes-request may also resolve the author's
	// outstanding changes_requested task — but only once no OTHER reviewer
	// still has changes outstanding.
	{
		onEvents: []string{
			domain.EventGitHubPRReviewApproved,
			domain.EventGitHubPRReviewCommented,
			domain.EventGitHubPRReviewDismissed,
		},
		closes:  []string{domain.EventGitHubPRReviewChangesRequested},
		prepare: prepareReviewResolved,
	},
	// A requested reviewer was dropped from the PR → close that reviewer's
	// review_requested task (a legacy removal with no dedup_key closes all).
	{
		onEvents: []string{domain.EventGitHubPRReviewRequestRemoved},
		closes:   []string{domain.EventGitHubPRReviewRequested},
		keep:     keepReviewRequestRemoved,
	},
	// A Jira issue was (re)assigned → retire the stale assigned/available
	// tasks that no longer reflect the assignment, so the new assignment mints
	// a fresh task owned by the new assignee's team.
	{
		onEvents: []string{domain.EventJiraIssueAssigned},
		closes:   []string{domain.EventJiraIssueAssigned, domain.EventJiraIssueAvailable},
		prepare:  prepareJiraReassign,
		keep:     keepJiraReassign,
	},
	// A PR terminated → close every in-flight task on the entity and flip it
	// closed. The terminating events themselves are excluded from the close
	// set: the lifecycle task they mint to ride their blueprint is created
	// after this phase and must survive.
	{
		onEvents:         []string{domain.EventGitHubPRMerged, domain.EventGitHubPRClosed},
		closes:           githubPRTerminalCloseTypes(),
		terminatesEntity: true,
		closeReason:      "entity_closed",
	},
	// A Jira issue completed → same entity-wide close + terminate.
	{
		onEvents:         []string{domain.EventJiraIssueCompleted},
		closes:           jiraIssueTerminalCloseTypes(),
		terminatesEntity: true,
		closeReason:      "entity_closed",
	},
	// Jira stopped resolving a tracked issue's key → close everything on the
	// entity and terminate it. Same shape as completion, for a different
	// reason: a completed issue can still be reopened and go on emitting,
	// while an unreachable one can produce no further observation at all.
	{
		onEvents:         []string{domain.EventJiraIssueUnreachable},
		closes:           jiraIssueUnreachableCloseTypes(),
		terminatesEntity: true,
		closeReason:      "entity_closed",
	},
}

// EntityTerminatingEvents is the set of event types that flip the entity to
// closed when they route. DERIVED from closeRelations (those marked
// terminatesEntity) — there is no separate hardcoded list. Exported for
// callers/tests asking "does this event terminate the entity".
var EntityTerminatingEvents = func() map[string]bool {
	m := map[string]bool{}
	for _, rel := range closeRelations {
		if rel.terminatesEntity {
			for _, et := range rel.onEvents {
				m[et] = true
			}
		}
	}
	return m
}()

// githubPRTerminalCloseTypes is the set of github:pr:* event types whose active
// tasks a terminating PR event (merged/closed) cleans up: every author-centric
// in-flight type plus the requested-reviewer task, EXCLUDING the terminating
// events themselves (their riding lifecycle task is minted after the close
// phase and must survive). Sourced from authorCentricGitHubEventTypes so a new
// in-flight type is covered for free; TestTerminalCloseSet_CoversTaskTypes
// asserts coverage so drift fails loudly instead of leaking tasks on merge.
func githubPRTerminalCloseTypes() []string {
	out := make([]string, 0, len(authorCentricGitHubEventTypes)+1)
	for _, et := range authorCentricGitHubEventTypes {
		if et == domain.EventGitHubPRMerged || et == domain.EventGitHubPRClosed {
			continue
		}
		out = append(out, et)
	}
	return append(out, domain.EventGitHubPRReviewRequested)
}

// jiraIssueTerminalCloseTypes is the Jira analog: every assignee-centric type
// EXCEPT the terminating completed event, plus the unassigned pool task.
func jiraIssueTerminalCloseTypes() []string {
	return jiraCloseTypesExcept(domain.EventJiraIssueCompleted)
}

// jiraIssueUnreachableCloseTypes is what a confirmed-unreachable issue cleans
// up. It spares only that event's own type, so unlike the completed set it closes
// jira:issue:completed tasks — a "this got finished" task on an issue that no
// longer exists has nothing left to act on.
func jiraIssueUnreachableCloseTypes() []string {
	return jiraCloseTypesExcept(domain.EventJiraIssueUnreachable)
}

// jiraCloseTypesExcept builds an entity-wide Jira close set: every
// assignee-centric type plus the unassigned pool task, minus the terminating
// event's own type. That exclusion is the load-bearing part — a terminating
// event's own task is minted after the close phase and has to survive it.
func jiraCloseTypesExcept(terminator string) []string {
	out := make([]string, 0, len(assigneeCentricJiraEventTypes)+1)
	for _, et := range assigneeCentricJiraEventTypes {
		if et == terminator {
			continue
		}
		out = append(out, et)
	}
	return append(out, domain.EventJiraIssueAvailable)
}

// runCloses applies every close relation matching evt: it closes the active
// tasks the event resolves (typed siblings and/or the entity-wide set) and
// reports whether the entity should flip to closed. This is the single close
// phase — both "ci_check_passed closes ci_check_failed" and "pr_merged closes
// everything" run through here, distinguished only by relation data.
//
// closedAny drives the tasks_updated broadcast; terminate drives the entity
// state flip. A terminating relation sets terminate even when no tasks exist to
// close (a merged PR with no live tasks still closes its entity).
//
// The returned error is the close phase's routing obligation: a task this event
// resolves that is still open is work nothing else re-derives, because the
// tracker's snapshot-diff emits a merged/closed transition exactly once. The
// caller propagates it so the queue worker replays the event instead of
// consuming it. Failures are COLLECTED rather than fatal — one unreadable task
// list or one bad close must not stop the siblings from closing, and the replay
// only re-attempts what is still active (the list read returns active tasks
// only, and a task close is a no-op on a terminal row).
func (r *Router) runCloses(ctx context.Context, orgID string, evt domain.Event, entityID string) (closedAny, terminate bool, err error) {
	ctx, span := tracer.Start(ctx, "route.close", trace.WithAttributes(telemetry.EntityID(entityID)))
	defer span.End()

	closed := 0
	defer func() { span.SetAttributes(telemetry.Count(closed)) }()

	var errs []error
	for ri := range closeRelations {
		rel := &closeRelations[ri]
		if !slices.Contains(rel.onEvents, evt.EventType) {
			continue
		}
		if rel.terminatesEntity {
			terminate = true
		}

		var targets []domain.Task
		for _, tt := range rel.closes {
			ts, err := r.tasks.FindActiveByEntityAndTypeSystem(ctx, orgID, entityID, tt)
			if err != nil {
				routerLog.Error("close: list tasks failed", "on_event", evt.EventType, "target_type", tt, "error", err)
				errs = append(errs, fmt.Errorf("list active %s tasks: %w", tt, err))
				continue
			}
			targets = append(targets, ts...)
		}
		if len(targets) == 0 {
			continue
		}

		var cctx closeContext
		if rel.prepare != nil {
			var ok bool
			if cctx, ok = rel.prepare(ctx, r, orgID, evt, entityID); !ok {
				continue
			}
		}

		reason := rel.closeReason
		if reason == "" {
			reason = "auto_closed_by_event"
		}
		// close_event_type records the events_catalog type that triggered the
		// close. Every close here is event-driven (runCloses only runs from an
		// event), so it is always set — for entity-wide "entity_closed" closes as
		// well as typed "auto_closed_by_event" ones; the reason field, not the
		// presence of this column, distinguishes the two. (Non-event closes —
		// run_completed, user_* — leave it NULL, and those don't pass through here.)
		for i := range targets {
			t := targets[i]
			if rel.keep != nil && !rel.keep(evt, cctx, t) {
				continue
			}
			if err := r.closeTaskWithAudit(ctx, orgID, t.ID, evt.ID, reason, evt.EventType); err != nil {
				routerLog.Error("close: failed to close task", "task_id", t.ID, "on_event", evt.EventType, "error", err)
				errs = append(errs, fmt.Errorf("close task %s: %w", t.ID, err))
				continue
			}
			routerLog.InfoContext(ctx, "closed task", "task_id", t.ID, "on_event", evt.EventType, "reason", reason)
			closedAny = true
			closed++
		}
	}
	return closedAny, terminate, errors.Join(errs...)
}

// --- relation hooks ---------------------------------------------------------

// prSnapshotForEntity loads + parses an entity's PR snapshot. The closes that
// read the snapshot (CI green, review resolution) share it.
func (r *Router) prSnapshotForEntity(ctx context.Context, orgID, entityID string) (*domain.PRSnapshot, bool) {
	entity, err := r.entities.GetSystem(ctx, orgID, entityID)
	if err != nil || entity == nil {
		return nil, false
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return nil, false
	}
	return &snap, true
}

// prepareCIPassed gates ci_check_failed closes on a fully-green snapshot: if
// any check is still failing OR still running at the latest SHA, don't close.
//
// Pending blocks the close because it is indistinguishable from an unresolved
// failure: the snapshot keeps only the latest run per check name, and
// re-running a failed check replaces its failing row with a queued/in_progress
// one that has no conclusion yet. "Not currently failing" is not "resolved" —
// inside that window an unrelated check's pass must not close the task, since
// the re-run may fail again. Once the suite completes, whichever pass event
// lands last trips this gate, so the close arrives with the check that
// actually recovered.
func prepareCIPassed(ctx context.Context, r *Router, orgID string, _ domain.Event, entityID string) (closeContext, bool) {
	snap, ok := r.prSnapshotForEntity(ctx, orgID, entityID)
	if !ok {
		return nil, false
	}
	switch domain.CIStatusFromCheckRuns(snap.CheckRuns) {
	case "failure", "pending":
		return nil, false
	}
	return nil, true
}

// reviewerFromMeta pulls the top-level "reviewer" field every typed review
// event carries.
func reviewerFromMeta(evt domain.Event) string {
	var m struct {
		Reviewer string `json:"reviewer"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
		return ""
	}
	return m.Reviewer
}

// prepareReviewerKey yields the submitting reviewer's per-user dedup key so
// keepDedupKey closes only that reviewer's review_requested task.
func prepareReviewerKey(_ context.Context, _ *Router, _ string, evt domain.Event, _ string) (closeContext, bool) {
	reviewer := reviewerFromMeta(evt)
	if reviewer == "" {
		return nil, false
	}
	return events.ReviewerDedupKeyUser(reviewer), true
}

func keepDedupKey(_ domain.Event, ctx closeContext, t domain.Task) bool {
	key, _ := ctx.(string)
	return t.DedupKey == key
}

// prepareReviewResolved gates review_changes_requested closes: the reviewer
// must be identifiable AND no OTHER reviewer may still have changes outstanding.
func prepareReviewResolved(ctx context.Context, r *Router, orgID string, evt domain.Event, entityID string) (closeContext, bool) {
	reviewer := reviewerFromMeta(evt)
	if reviewer == "" {
		return nil, false
	}
	snap, ok := r.prSnapshotForEntity(ctx, orgID, entityID)
	if !ok {
		return nil, false
	}
	for _, rs := range snap.Reviews {
		if rs.State == "CHANGES_REQUESTED" && rs.Author != reviewer {
			return nil, false
		}
	}
	return nil, true
}

// keepReviewRequestRemoved closes the review_requested task matching the
// removal event's reviewer dedup_key; a legacy removal with no dedup_key closes
// every review_requested task (pre-ticket behavior).
func keepReviewRequestRemoved(evt domain.Event, _ closeContext, t domain.Task) bool {
	return evt.DedupKey == "" || t.DedupKey == evt.DedupKey
}

// prepareJiraReassign resolves the new assignee's owning team(s) for the
// member-aware skip and applies the Server/DC display-name fallback: when the
// issue is still assigned to the local user (no accountId), skip the whole
// close so a re-emit doesn't retire that user's own task.
//
// An unreadable assignee identity skips the relation rather than proceeding
// with an empty team set: empty means "the new assignee is on none of our
// teams," which retires every owned task on the issue. The close phase has no
// error channel of its own yet — promoting it is its own ticket — so declining
// to close is how this one refuses to act on an answer it doesn't have.
func prepareJiraReassign(ctx context.Context, r *Router, orgID string, evt domain.Event, entityID string) (closeContext, bool) {
	var meta events.JiraIssueAssignedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return nil, false
	}
	assigneeTeams, err := r.assigneeTeams(ctx, orgID, evt)
	if err != nil {
		lifecycleLog.Error("jira reassign close: assignee team lookup failed, skipping the close", "entity_id", entityID, "error", err)
		return nil, false
	}
	newOwnerTeams := map[string]struct{}{}
	for _, tid := range assigneeTeams {
		newOwnerTeams[tid] = struct{}{}
	}
	if r.users != nil && meta.AssigneeAccountID == "" && meta.Assignee != "" {
		var jiraHost string
		if r.orgs != nil {
			if orgSet, serr := r.orgs.GetSettingsSystem(ctx, orgID); serr == nil {
				jiraHost = orgSet.JiraBaseURL
			}
		}
		if _, localDisplayName, err := r.users.GetJiraIdentitySystem(ctx, runmode.LocalDefaultUserID, jiraHost); err == nil {
			if localDisplayName != "" && strings.EqualFold(meta.Assignee, localDisplayName) {
				return nil, false // still assigned to the local user — don't retire
			}
		}
	}
	return newOwnerTeams, true
}

// keepJiraReassign: the per-assignee assigned task survives only while still
// owned by the new assignee's team; the available pool task always retires once
// the issue is assigned (its owner is the tracking team, so a match would be
// coincidental).
func keepJiraReassign(_ domain.Event, ctx closeContext, t domain.Task) bool {
	if t.EventType == domain.EventJiraIssueAvailable {
		return true
	}
	newOwnerTeams, _ := ctx.(map[string]struct{})
	if owner := teamIDValue(&t); owner != "" {
		if _, ok := newOwnerTeams[owner]; ok {
			return false // still assigned to its owning team — not a reassignment-away
		}
	}
	return true
}

// --- shared close helpers (used by the driver) ------------------------------

// stopConversationsOnClosedTask asks the spawner to stop the conversations the close just ended
// and cancel the blueprints behind them: the stop note on the transcript, the
// kill signal, and the park. conversationIDs is the then-active set the close
// transaction read and stamped, so this acts on exactly what closed rather
// than on a second, later read.
//
// The blueprint half rides along because this is the task's own disposition:
// nothing will resume these conversations, so leaving their blueprints frozen
// 'running' would hold worktrees for finished work. Its `cancel_requested`
// write lands on a flag the tx already set — an idempotent re-set, which
// RequestRunCancelSystem's `cancel_requested = false` guard turns into a
// no-op that reports changed=false, and every caller of it already treats
// that as "already called off" rather than a failure.
//
// Errors are logged and swallowed, and this is the best-effort HALF of a split
// the close transaction owns the other side of. The intent is durable before
// this runs, so a kill that never lands no longer forgets the conversation:
// the claim gate refuses to drive a cancel-requested blueprint, and the
// reaper's cancel arm finalizes it once its executor is gone. What is lost is
// only promptness — a live agent may finish the turn it is on. "no active
// conversation" from the spawner is expected when a conversation races us to
// natural completion; the conversation ends up terminal either way.
//
// So this stays OUT of the close phase's routing obligation, but for a
// narrower reason than before: a replay could not repair it anyway (a replayed
// close finds no active task and never walks back here), and now there is
// nothing left for it to repair.
func (r *Router) stopConversationsOnClosedTask(orgID, taskID string, conversationIDs []string) {
	if r.spawner == nil {
		return
	}
	for _, id := range conversationIDs {
		if err := r.spawner.StopConversationAndCancelBlueprint(orgID, id, "", delegate.StopCauseTaskClosed); err != nil {
			routerLog.Error("stop conversation on task close failed", "conversation", id, "task_id", taskID, "error", err)
		}
	}
}

// closeTaskWithAudit closes a task, records the closing event in task_events,
// and stops the conversations working on it. Every close goes through here
// (including the entity-wide terminating close), so cancellation + audit are
// uniform.
//
// The first three writes — the task's terminal flip, the audit row, and
// `cancel_requested` on the blueprints behind the task's then-active
// conversations — are one transaction. Task state is the authoritative
// invalidation surface, and the intent to stop what it invalidates has to be as
// durable as the invalidation itself: the kill below is reachable exactly once,
// so a close that committed without the intent could leave a conversation the
// system has forgotten it meant to stop, with no replay able to reach it.
//
// The kill then follows, post-commit and best-effort. Only a close this call
// actually performed cascades: an already-terminal task returns no conversation
// ids, so a replayed close neither stamps nor stops — the conversation under it
// may be one a user resumed after the close, which the resume ladder
// deliberately allows.
func (r *Router) closeTaskWithAudit(ctx context.Context, orgID, taskID, closingEventID, closeReason, closeEventType string) error {
	closed, activeConversationIDs, err := r.tasks.CloseWithConversationCancelIntentSystem(ctx, orgID, taskID, closeReason, closeEventType, closingEventID)
	if err != nil {
		return err
	}
	if !closed {
		return nil
	}
	r.stopConversationsOnClosedTask(orgID, taskID, activeConversationIDs)
	return nil
}
