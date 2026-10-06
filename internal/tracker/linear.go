package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// linearBatchSize is how many tracked issues one refresh request asks
	// for, the most linear.Client.GetIssues takes.
	linearBatchSize = 50

	// linearSearchMaxPages bounds one discovery query's pagination, so a
	// cursor that never reports its last page cannot keep a cycle asking.
	// At 50 issues a page it is far past any queue a team works from.
	linearSearchMaxPages = 100

	// linearConfirmBudget caps the issues a cycle asks Linear about one at a
	// time: tracked issues the batch read did not return, and entities with
	// no snapshot to batch by. Each is a request on top of the batch reads,
	// and a whole team's issues can go missing at once when a credential
	// loses access. The rest wait for the next cycle; entities are listed
	// oldest-polled first, so each cycle's budget reaches the ones the last
	// one did not.
	linearConfirmBudget = 20
)

// LinearTeamRule is the tracker-local view of one Linear team's tracking
// rules, merged across every TF team that arms it. The state sets are refs
// with ids, and discovery filters by id, so a state renamed in Linear keeps
// matching.
type LinearTeamRule struct {
	ID, Key                  string
	Pickup, InProgress, Done []domain.LinearStateRef
}

// LinearRules is the org's merged Linear rule set, one entry per Linear team.
type LinearRules []LinearTeamRule

// ForTeam returns the rule for a Linear team id, or nil when the team is not
// armed. An entity whose team is no longer armed has no done set, so nothing
// reads as terminal for it — the Jira rule for a removed project.
func (r LinearRules) ForTeam(id string) *LinearTeamRule {
	for i := range r {
		if r[i].ID == id {
			return &r[i]
		}
	}
	return nil
}

func (r LinearRules) doneForTeam(id string) []domain.LinearStateRef {
	if rule := r.ForTeam(id); rule != nil {
		return rule.Done
	}
	return nil
}

// allDone is every armed team's done states, deduplicated. Sub-issues are
// classified against it rather than their parent's team alone, because a
// sub-issue can belong to another team.
func (r LinearRules) allDone() []domain.LinearStateRef {
	seen := map[string]bool{}
	out := make([]domain.LinearStateRef, 0)
	for _, rule := range r {
		for _, ref := range rule.Done {
			if key := domain.LinearStateDedupKey(ref); !seen[key] {
				seen[key] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

// LinearClient is the part of *linear.Client a Linear cycle uses.
type LinearClient interface {
	Viewer(ctx context.Context) (linear.User, error)
	SearchIssues(ctx context.Context, f linear.IssueFilter, after string) (linear.IssuePage, error)
	GetIssues(ctx context.Context, ids []string) ([]linear.Issue, error)
	GetIssue(ctx context.Context, idOrIdentifier string) (*linear.Issue, error)
}

// linearIssueState is an issue's diff-scope snapshot plus its description,
// which is mirrored onto entities.description rather than stored in the
// snapshot, as on Jira.
type linearIssueState struct {
	Snap                            domain.LinearSnapshot
	Description                     string
	DiscoveredAssignedToCurrentUser bool
}

// RefreshLinear runs one tracking cycle for an org's Linear issues: discovery
// per armed team, a batched refresh of every active Linear entity, the diff,
// and the poll-complete sentinel. It returns the number of events enqueued.
//
// A rate limit ends the cycle at once, wherever it lands: the error matching
// linear.ErrRateLimited is returned, no further request is sent, and the
// sentinel is not emitted. The poller schedules the org's next cycle at the
// reset the error carries. Entity writes made before it stand. A rate-limited
// request says nothing about the issues it asked for, so it never counts as
// one missing from a batch and never leads to unreachable.
//
// All entity reads and writes are scoped to the Tracker's orgID. Persistence
// calls take context.Background() for the reason RefreshGitHub gives; the
// snapshot-with-events commits take the cycle's ctx.
func (t *Tracker) RefreshLinear(ctx context.Context, client LinearClient, teams LinearRules) (int, error) {
	orgID := t.orgID
	startedAt := time.Now()
	terminal := func(snap domain.LinearSnapshot) bool {
		return domain.ContainsState(teams.doneForTeam(snap.TeamID), snap.StateRef())
	}
	emitted := 0

	// Phase 1: discovery. Entities found before a failure are seeded either
	// way; the error decides what happens after.
	discovered, discoveryErr := t.discoverLinear(ctx, client, teams)
	for _, state := range discovered {
		snap := state.Snap
		entity, created, err := t.entities.FindOrCreateSystem(context.Background(), orgID, "linear", snap.Identifier, "issue", snap.Title, snap.URL)
		if err != nil {
			trackerLog.Error("create entity failed", "source_id", snap.Identifier, "error", err)
			continue
		}
		if created {
			snapJSON, _ := json.Marshal(snap)
			switch {
			case terminal(snap):
				// Done before TF saw it: snapshot and closed state in one
				// statement, and nothing emitted.
				if ok, err := t.entities.CloseWithSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
					trackerLog.Error("seed terminal linear snapshot failed", "source_id", snap.Identifier, "error", err)
				} else if !ok {
					trackerLog.Warn("seed terminal linear snapshot CAS lost race, skipping", "source_id", snap.Identifier)
				}
			case state.DiscoveredAssignedToCurrentUser:
				// Assigned to someone else, the issue matched neither query,
				// so arriving through the assigned-to-viewer query is itself
				// the assignment. The event commits with the first snapshot;
				// seeding first would retire it unseen.
				evts := DiffLinearSnapshots(domain.LinearSnapshot{}, snap, entity.ID, teams.doneForTeam(snap.TeamID))
				if ok, enqueued, err := t.emitWithSnapshotCAS(ctx, orgID, entity.ID, string(snapJSON), entity.PollSeq, evts); err != nil {
					trackerLog.Error("seed assigned linear snapshot+event failed", "source_id", snap.Identifier, "error", err)
				} else if !ok {
					trackerLog.Warn("seed assigned linear snapshot CAS lost race, skipping", "source_id", snap.Identifier)
				} else {
					emitted += enqueued
				}
			default:
				if ok, err := t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
					trackerLog.Error("seed snapshot failed", "source_id", snap.Identifier, "error", err)
				} else if !ok {
					trackerLog.Warn("seed snapshot CAS lost race, skipping", "source_id", snap.Identifier)
				}
			}
			if state.Description != "" {
				if _, err := t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, state.Description); err != nil {
					trackerLog.Error("seed description failed", "source_id", snap.Identifier, "error", err)
				}
			}
			continue
		}
		t.mirrorLinearText(orgID, *entity, state)
		// A closed issue reappearing open reactivates with the discovery
		// snapshot in the same statement; Phase 2 re-diffs from it.
		if !terminal(snap) && entity.State == "closed" {
			snapJSON, _ := json.Marshal(snap)
			if reactivated, err := t.entities.ReactivateWithSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
				trackerLog.Error("reactivate entity failed", "source_id", snap.Identifier, "error", err)
			} else if !reactivated {
				trackerLog.Warn("reactivate entity CAS lost race, skipping", "source_id", snap.Identifier)
			} else {
				trackerLog.Info("reactivated entity (reopened)", "source_id", snap.Identifier)
			}
		}
	}
	if discoveryErr != nil {
		// Rate limited, or every call failed on the connection: either way the
		// cycle did not hear from Linear, so it must not report a completed poll.
		return emitted, discoveryErr
	}

	// Phase 2: refresh every active entity, a batch at a time, diffing each
	// batch as it lands so a rate limit partway through keeps the batches
	// before it.
	entities, err := t.entities.ListActiveSystem(context.Background(), orgID, "linear")
	if err != nil {
		return emitted, fmt.Errorf("list active linear entities: %w", err)
	}
	if len(entities) == 0 {
		t.EmitPollComplete(ctx, "linear", startedAt, 0, emitted)
		return emitted, nil
	}

	allDone := teams.allDone()
	prevs := make(map[string]*domain.LinearSnapshot, len(entities))
	var batched, individual []domain.Entity
	for _, e := range entities {
		if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
			// No snapshot: a stub created outside the poller, or one whose
			// snapshot a source pause cleared. There is no UUID to batch by,
			// so it is fetched by identifier and quietly seeded.
			individual = append(individual, e)
			continue
		}
		var prev domain.LinearSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &prev); err != nil || prev.ID == "" {
			trackerLog.Warn("corrupt linear snapshot, reseeding", "source_id", e.SourceID, "error", err)
			individual = append(individual, e)
			continue
		}
		prevs[e.ID] = &prev
		batched = append(batched, e)
	}

	refreshed, missing, err := t.refreshLinearBatches(ctx, client, orgID, batched, prevs, teams, allDone)
	emitted += refreshed
	if err != nil {
		return emitted, err
	}

	confirmed, err := t.confirmLinearIndividually(ctx, client, orgID, append(individual, missing...), prevs, teams, allDone)
	emitted += confirmed
	if err != nil {
		return emitted, err
	}

	trackerLog.InfoContext(ctx, "linear refresh", "discovered", len(discovered), "entities", len(entities), "events", emitted)
	t.EmitPollComplete(ctx, "linear", startedAt, len(entities), emitted)
	return emitted, nil
}

// refreshLinearBatches reads the batched entities from Linear by UUID and
// applies each answer. It returns the events enqueued and the entities Linear
// did not return, which the caller asks about one at a time. A failed batch
// ends the refresh: a rate limit is returned as itself, so the caller sees
// linear.ErrRateLimited.
func (t *Tracker) refreshLinearBatches(ctx context.Context, client LinearClient, orgID string, batched []domain.Entity, prevs map[string]*domain.LinearSnapshot, teams LinearRules, allDone []domain.LinearStateRef) (emitted int, missing []domain.Entity, err error) {
	if len(batched) == 0 {
		return 0, nil, nil
	}
	ctx, span := tracer.Start(ctx, "tracker.linear.batch_fetch",
		trace.WithAttributes(telemetry.Count(len(batched))))
	defer span.End()

	for i := 0; i < len(batched); i += linearBatchSize {
		batch := batched[i:min(i+linearBatchSize, len(batched))]
		ids := make([]string, len(batch))
		for j, e := range batch {
			ids[j] = prevs[e.ID].ID
		}
		issues, err := client.GetIssues(ctx, ids)
		if err != nil {
			if errors.Is(err, linear.ErrRateLimited) {
				span.SetAttributes(telemetry.Outcome("rate_limited"))
				return emitted, nil, err
			}
			span.SetStatus(codes.Error, "batch fetch")
			return emitted, nil, fmt.Errorf("batch fetch linear issues %d-%d: %w", i, i+len(batch), err)
		}
		byID := make(map[string]linear.Issue, len(issues))
		for _, issue := range issues {
			byID[issue.ID] = issue
		}
		for _, e := range batch {
			issue, ok := byID[prevs[e.ID].ID]
			if !ok {
				missing = append(missing, e)
				continue
			}
			emitted += t.applyLinearIssue(ctx, orgID, e, prevs[e.ID], issue, teams, allDone)
		}
	}
	if len(missing) > 0 {
		span.SetAttributes(telemetry.Outcome("partial"))
	}
	return emitted, missing, nil
}

// confirmLinearIndividually asks Linear about each entity the batch read could
// not answer for: snapshot-less entities by identifier, and tracked issues
// absent from their batch by UUID. Absence from a batch is never proof the
// issue is gone, so only Linear answering not-found for the issue itself emits
// unreachable; an issue that does come back is applied like any refreshed one.
// Any other failure is no evidence either way, and the entity is asked about
// again next cycle.
func (t *Tracker) confirmLinearIndividually(ctx context.Context, client LinearClient, orgID string, candidates []domain.Entity, prevs map[string]*domain.LinearSnapshot, teams LinearRules, allDone []domain.LinearStateRef) (int, error) {
	if len(candidates) == 0 {
		return 0, nil
	}
	ctx, span := tracer.Start(ctx, "tracker.linear.confirm_missing",
		trace.WithAttributes(telemetry.Count(len(candidates))))
	defer span.End()

	emitted, retired := 0, 0
	for i, e := range candidates {
		if i >= linearConfirmBudget {
			span.SetAttributes(telemetry.Outcome("partial"))
			trackerLog.InfoContext(ctx, "linear confirmation budget spent; remaining issues asked about next cycle",
				"budget", linearConfirmBudget, "deferred", len(candidates)-i)
			break
		}
		ref := e.SourceID
		if prev := prevs[e.ID]; prev != nil {
			ref = prev.ID
		}
		issue, err := client.GetIssue(ctx, ref)
		switch {
		case err == nil:
			emitted += t.applyLinearIssue(ctx, orgID, e, prevs[e.ID], *issue, teams, allDone)
		case errors.Is(err, linear.ErrRateLimited):
			span.SetAttributes(telemetry.Outcome("rate_limited"))
			return emitted, err
		case errors.Is(err, linear.ErrNotFound):
			t.emitLinearUnreachable(ctx, orgID, e, nil, "not_found")
			emitted++
			retired++
		default:
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "linear issue confirmation failed; entity left tracked",
				"source_id", e.SourceID, "entity_id", e.ID, "error", err)
		}
	}
	if retired > 0 {
		span.SetAttributes(telemetry.Disposition("entities_retired"), telemetry.Attempt(retired))
	}
	return emitted, nil
}

// applyLinearIssue applies one fresh read of an entity's issue and returns the
// events it enqueued. prev is nil for an entity with no usable snapshot, which
// is seeded without a diff.
func (t *Tracker) applyLinearIssue(ctx context.Context, orgID string, e domain.Entity, prev *domain.LinearSnapshot, issue linear.Issue, teams LinearRules, allDone []domain.LinearStateRef) int {
	if reason := linearIssueGone(e, issue, teams); reason != "" {
		t.emitLinearUnreachable(ctx, orgID, e, &issue, reason)
		return 1
	}
	state := linearIssueToState(issue, allDone)
	snap := state.Snap
	done := teams.doneForTeam(snap.TeamID)
	terminal := func(s domain.LinearSnapshot) bool { return domain.ContainsState(done, s.StateRef()) }
	snapJSON, _ := json.Marshal(snap)

	if prev == nil {
		// Quiet seed. A diff from nothing would emit a first-discovery event
		// for state that predates tracking, and after a source pause one per
		// known issue at once.
		var ok bool
		var err error
		if terminal(snap) {
			ok, err = t.entities.CloseWithSnapshotCASSystem(context.Background(), orgID, e.ID, string(snapJSON), e.PollSeq)
		} else {
			ok, err = t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, e.ID, string(snapJSON), e.PollSeq)
		}
		if err != nil {
			trackerLog.Error("seed linear stub snapshot failed", "source_id", e.SourceID, "error", err)
		} else if !ok {
			trackerLog.Warn("seed linear stub snapshot CAS lost race, skipping", "source_id", e.SourceID)
		}
		t.mirrorLinearText(orgID, e, state)
		return 0
	}

	evts := DiffLinearSnapshots(*prev, snap, e.ID, done)
	if owed, ok := t.closeOwed(ctx, orgID, e.ID, terminal(*prev), terminal(snap)); ok {
		evts = append(evts, owed)
	}
	prevJSON, _ := json.Marshal(*prev)
	ok, enqueued, err := t.commitRefresh(ctx, orgID, e.ID, string(prevJSON), string(snapJSON), e.PollSeq, evts)
	if err != nil {
		trackerLog.Error("linear snapshot+events commit failed; suppressing this cycle's transitions (re-diffed next cycle)", "source_id", e.SourceID, "error", err)
		return 0
	}
	if !ok {
		trackerLog.Warn("linear snapshot CAS lost race (stale poll_seq); suppressing this cycle's transitions", "source_id", e.SourceID)
		return 0
	}
	t.mirrorLinearText(orgID, e, state)
	return enqueued
}

// mirrorLinearText brings the entity's title and description up to the
// issue's. Best effort, outside the snapshot transaction, as on Jira.
func (t *Tracker) mirrorLinearText(orgID string, e domain.Entity, state linearIssueState) {
	if e.Title != state.Snap.Title {
		_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, e.ID, state.Snap.Title)
	}
	if e.Description != state.Description {
		_, _ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, e.ID, state.Description)
	}
}

// linearIssueGone reports why an entity's issue can no longer be tracked, or
// "" when it can:
//
//   - trashed: Linear still answers for an issue in the trash, but it is
//     deleted from every view a person works from.
//   - moved: the issue answers under another identifier, which is what a move
//     to another Linear team does. The entity is keyed by the identifier, so
//     the issue becomes a new entity when discovery finds it, and this one
//     retires.
//   - archived: archived in a state outside its team's done set — taken out
//     of the workflow without finishing it. Archived in a done state is the
//     ordinary terminal path.
func linearIssueGone(e domain.Entity, issue linear.Issue, teams LinearRules) string {
	switch {
	case issue.Trashed:
		return "trashed"
	case issue.Identifier != "" && issue.Identifier != e.SourceID:
		return "moved"
	case issue.ArchivedAt != "" && !domain.ContainsState(teams.doneForTeam(issue.Team.ID), linearStateRef(issue.State)):
		return "archived"
	}
	return ""
}

// emitLinearUnreachable publishes the terminal event for an entity whose issue
// Linear will no longer give TF. The metadata is the stored snapshot's
// last-known state; a field the snapshot lacks is filled from the issue when
// there is one (a trashed, moved or archived issue still answered), so the
// event carries the issue's team whenever anything knows it. Published rather
// than committed with a snapshot, as Jira's is: there is no new snapshot, and
// closing the entity is the router's job.
func (t *Tracker) emitLinearUnreachable(ctx context.Context, orgID string, e domain.Entity, issue *linear.Issue, reason string) {
	var snap domain.LinearSnapshot
	if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			trackerLog.WarnContext(ctx, "corrupt linear snapshot on an unreachable issue; emitting with last-known fields blank",
				"source_id", e.SourceID, "entity_id", e.ID, "error", err)
			snap = domain.LinearSnapshot{}
		}
	}
	if issue != nil {
		fresh := linearIssueToState(*issue, nil).Snap
		if snap.ID == "" {
			snap.ID = fresh.ID
		}
		if snap.TeamID == "" {
			snap.TeamID, snap.TeamKey = fresh.TeamID, fresh.TeamKey
		}
		if snap.Title == "" {
			snap.Title = fresh.Title
		}
		if snap.State.IsZero() {
			snap.State = fresh.State
		}
	}
	if snap.TeamKey == "" {
		snap.TeamKey = extractProject(e.SourceID)
	}
	snap.Identifier = e.SourceID
	entityID := e.ID
	trackerLog.InfoContext(ctx, "linear will not give TF this issue any more; retiring entity",
		"source_id", e.SourceID, "entity_id", e.ID, "reason", reason)
	t.publish(ctx, domain.Event{
		OrgID:     orgID,
		EventType: domain.EventLinearIssueUnreachable,
		EntityID:  &entityID,
		MetadataJSON: mustJSON(events.LinearIssueUnreachableMetadata{
			LinearIssueIdentity: linearIdentity(snap),
			LastStatus:          snap.State.Name,
		}),
		// No source time: Linear does not say when an issue stopped being
		// readable, so occurred_at stays NULL. created_at is set for the bus,
		// which hands this struct to subscribers as-is.
		CreatedAt: time.Now(),
	})
}

// discoverLinear runs each armed team's two discovery queries and returns the
// issues they found, deduplicated by UUID:
//
//   - pickup: unassigned issues in the team's pickup states. An empty pickup
//     set sends no query rather than an unfiltered one.
//   - assigned to the credential's own user, outside the team's done states.
//     The user is read once per cycle. Under an app install it is the app
//     user, which nothing is assigned to, so the query returns nothing.
//
// One query failing does not stop the others. A rate limit does: it is
// returned at once with whatever the queries before it found. The pass also
// returns an error when every call failed on the connection, so the cycle
// does not report a poll Linear never answered.
func (t *Tracker) discoverLinear(ctx context.Context, client LinearClient, teams LinearRules) ([]linearIssueState, error) {
	if len(teams) == 0 {
		return nil, nil
	}
	ctx, span := tracer.Start(ctx, "tracker.linear.discover",
		trace.WithAttributes(telemetry.Count(len(teams))))
	defer span.End()

	outcomes := make([]error, 0, 1+2*len(teams))
	viewer, verr := client.Viewer(ctx)
	if verr != nil {
		if errors.Is(verr, linear.ErrRateLimited) {
			span.SetAttributes(telemetry.Outcome("rate_limited"))
			return nil, verr
		}
		trackerLog.Log(ctx, upstream.LogLevel(verr, slog.LevelError), "linear viewer read failed; skipping the assigned-to-viewer queries this cycle", "error", verr)
		outcomes = append(outcomes, verr)
	}

	type query struct {
		teamKey  string
		filter   linear.IssueFilter
		assigned bool
	}
	var queries []query
	for _, team := range teams {
		if team.ID == "" {
			continue
		}
		if pickup := domain.LinearStateIDs(team.Pickup); len(pickup) > 0 {
			queries = append(queries, query{
				teamKey: team.Key,
				filter:  linear.IssueFilter{TeamID: team.ID, StateIDsIn: pickup, Unassigned: true},
			})
		}
		if verr == nil && viewer.ID != "" {
			queries = append(queries, query{
				teamKey:  team.Key,
				filter:   linear.IssueFilter{TeamID: team.ID, AssigneeID: viewer.ID, StateIDsNotIn: domain.LinearStateIDs(team.Done)},
				assigned: true,
			})
		}
	}

	allDone := teams.allDone()
	seen := map[string]bool{}
	var all []linearIssueState
	failed := 0
	for _, q := range queries {
		issues, err := searchLinearIssues(ctx, client, q.filter)
		for _, issue := range issues {
			if seen[issue.ID] {
				continue
			}
			seen[issue.ID] = true
			state := linearIssueToState(issue, allDone)
			state.DiscoveredAssignedToCurrentUser = q.assigned
			all = append(all, state)
		}
		if errors.Is(err, linear.ErrRateLimited) {
			span.SetAttributes(telemetry.Outcome("rate_limited"))
			return all, err
		}
		outcomes = append(outcomes, err)
		if err != nil {
			failed++
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelError), "linear discovery query failed", "team", q.teamKey, "error", err)
		}
	}
	if failed > 0 {
		span.SetAttributes(telemetry.Outcome("partial"), telemetry.Attempt(failed))
	}
	if err := discoveryUnreached("linear", outcomes); err != nil {
		span.SetStatus(codes.Error, "every call failed")
		span.SetAttributes(telemetry.Outcome("failed"))
		return all, err
	}
	return all, nil
}

// searchLinearIssues walks one discovery query to its last page. On an error
// it returns the pages read before it with the error.
func searchLinearIssues(ctx context.Context, client LinearClient, f linear.IssueFilter) ([]linear.Issue, error) {
	var out []linear.Issue
	after := ""
	for range linearSearchMaxPages {
		page, err := client.SearchIssues(ctx, f, after)
		if err != nil {
			return out, err
		}
		out = append(out, page.Items...)
		if !page.HasNextPage {
			return out, nil
		}
		if page.EndCursor == "" || page.EndCursor == after {
			return out, errors.New("linear: search cursor did not advance")
		}
		after = page.EndCursor
	}
	return out, fmt.Errorf("linear: search ran past %d pages", linearSearchMaxPages)
}

func linearStateRef(s linear.WorkflowState) domain.LinearStateRef {
	return domain.LinearStateRef{ID: s.ID, Name: s.Name, Type: s.Type}
}

// linearIssueToState converts a Linear issue to its diff-scope snapshot and its
// stored description. allDone classifies sub-issues: one whose state is not in
// it, or whose state is unknown, counts as open.
func linearIssueToState(issue linear.Issue, allDone []domain.LinearStateRef) linearIssueState {
	labels := issue.Labels
	if labels == nil {
		// [] rather than null, so a stored snapshot and a fresh one marshal
		// the same and an unchanged issue is not a snapshot write.
		labels = []string{}
	}
	snap := domain.LinearSnapshot{
		ID:            issue.ID,
		Identifier:    issue.Identifier,
		Title:         issue.Title,
		BodyHash:      linearBodyHash(issue.Description),
		State:         linearStateRef(issue.State),
		Priority:      issue.Priority,
		PriorityLabel: issue.PriorityLabel,
		Labels:        labels,
		TeamID:        issue.Team.ID,
		TeamKey:       issue.Team.Key,
		URL:           issue.URL,
		CreatedAt:     issue.CreatedAt,
		UpdatedAt:     issue.UpdatedAt,
		Archived:      issue.ArchivedAt != "",
		Trashed:       issue.Trashed,
	}
	if a := issue.Assignee; a != nil {
		snap.Assignee = a.Name
		if snap.Assignee == "" {
			snap.Assignee = a.DisplayName
		}
		snap.AssigneeUserID = a.ID
	}
	if p := issue.Parent; p != nil {
		snap.ParentID, snap.ParentIdentifier = p.ID, p.Identifier
	}
	if c := issue.LastComment; c != nil {
		snap.LastCommentID, snap.LastCommentAt = c.ID, c.CreatedAt
	}
	for _, child := range issue.Children {
		if !domain.ContainsState(allDone, linearStateRef(child.State)) {
			snap.OpenChildCount++
		}
	}
	return linearIssueState{
		Snap:        snap,
		Description: truncateDescription(issue.Description, descriptionStoreMaxRunes),
	}
}

// linearBodyHash fingerprints a description the way GitHub's markdown body is
// fingerprinted: the hash of the body as a JSON string. Linear answers null
// for an empty description, which the client reads as "", so a description
// always has a hash and clearing one is a change.
func linearBodyHash(description string) string {
	raw, _ := json.Marshal(description)
	return domain.JSONBodyHash(raw)
}
