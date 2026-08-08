package routing

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ReDeriveAfterScoring re-checks deferred triggers for tasks that just
// received AI scores. Triggers with MinAutonomySuitability > 0 are skipped
// during HandleEvent and deferred to this pass. orgID is the scoring
// context — the scorer batches per-org so every task in the slice belongs
// to the same tenant. Per-team auto_delegate_enabled is checked inside
// reDeriveTask after the task's team_id is resolved.
//
// Two callers drive it, both from the scorer: the OnScoringCompleted hook
// right after the scores commit (the fast path), and the cycle-start drain
// of tasks.rederive_owed (the crash backstop, for scores whose callback
// never got to run). Both hand it the same work, so the pass discharges the
// owed mark itself — per pass, after the evaluations it covers.
//
// ctx is the scoring cycle's, minus its cancellation (the caller applies
// context.WithoutCancel): this runs asynchronously and outlives the cycle
// that scheduled it, and firing half a deferred trigger — a run inserted,
// its claim unstamped — is worse than one that lands late.
func (r *Router) ReDeriveAfterScoring(ctx context.Context, orgID string, taskIDs []string) {
	evaluated := make([]string, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		if r.reDeriveTask(ctx, orgID, taskID) {
			evaluated = append(evaluated, taskID)
		}
	}
	r.clearReDeriveOwed(ctx, orgID, evaluated)
}

// clearReDeriveOwed discharges the owed mark for the tasks this pass
// actually evaluated. Batched at the end of the pass rather than per task:
// the ordering rule is only that a task is never cleared before its own
// evaluation, and a crash before the batch lands just leaves the whole set
// owed for the next cycle to redo — which is safe, because re-deriving a
// task twice fires at most once (the (triggering_event_id, trigger_id)
// replay fence and the one-active-auto-run index).
//
// A clear failure is logged and dropped for the same reason: the cost is a
// redundant re-derive next cycle.
func (r *Router) clearReDeriveOwed(ctx context.Context, orgID string, taskIDs []string) {
	if r.scores == nil || len(taskIDs) == 0 {
		return
	}
	if err := r.scores.ClearReDeriveOwed(ctx, orgID, taskIDs); err != nil {
		routerLog.Error("re-derive: failed to clear the owed mark", "org", orgID, "count", len(taskIDs), "error", err)
	}
}

// reDeriveTask evaluates one task's deferred triggers. It reports whether
// the evaluation ran to a conclusion — true for every decided outcome,
// including the many "nothing to fire" ones, and false only when a store
// read bailed out before the task could be decided. Only a true discharges
// the owed mark; a bail keeps it, so the next cycle's drain retries rather
// than silently swallowing the deferral the same way a crash used to.
func (r *Router) reDeriveTask(ctx context.Context, orgID, taskID string) bool {
	task, err := r.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil {
		routerLog.Error("re-derive: failed to load task", "task_id", taskID, "error", err)
		return false
	}
	// A task that no longer exists is decided, not deferred — there is
	// nothing left to evaluate and nothing left to owe.
	if task == nil {
		return true
	}

	// Entitlement gate (TFAC-524) — a task on a now-gated-off event type
	// fires nothing during the post-scoring deferred-trigger pass, mirroring
	// HandleEvent's freeze.
	if !entitlements.EventTypeAllowed(orgID, task.EventType) {
		return true
	}

	// Only re-derive queued tasks. The lifecycle axis
	// collapsed to {queued, snoozed, done, dismissed} so this gate
	// also has to cover snoozed (a snoozed task is on a "wait" until
	// its wake-on-bump event lands; a deferred-threshold re-derive
	// should not bypass that signal). done/dismissed naturally fall
	// out of the != "queued" check.
	if task.Status != "queued" {
		return true
	}

	// The per-team auto_delegate kill switch is checked per firing team
	// inside the trigger loop below — one task is now visible to many
	// teams, so a single owner-team gate here would wrongly suppress (or
	// admit) other teams' deferred triggers.

	// Re-derive must not promote a task that's already claimed. The
	// responsibility axis lives on the claim cols, not
	// status — so a queued task may still be "already taken" by either
	// the bot (auto-delegate already fired and enqueued a firing, or
	// drag-to-bot stamped) or a user ("I'll take this myself" claim).
	// Without this guard, re-derive could:
	//   - stamp an agent claim onto a user-claimed task (XOR violation
	//     blocked at the DB level, but the visible state would be a
	//     spurious 500 log + WS toast)
	//   - fire a second bot run on a task the bot is already
	//     committed to (duplicate firing, breaker noise)
	// Either claim col set = "not the re-derive's business; the
	// commitment is real and the lifecycle event that ends the
	// commitment will arrive via its own path."
	if task.ClaimedByAgentID != "" || task.ClaimedByUserID != "" {
		return true
	}

	// No score landed — nothing to gate against. Decided, not deferred: a
	// task with no score has nothing for a later pass to do differently, and
	// keeping it owed would re-derive it every cycle forever.
	if task.AutonomySuitability == nil {
		return true
	}

	// Fetch handlers for this event type — same call HandleEvent uses,
	// kind-discriminated here.
	handlers, err := r.handlers.GetEnabledForEventSystem(ctx, orgID, task.EventType)
	if err != nil {
		routerLog.Error("re-derive: failed to query event_handlers", "event_type", task.EventType, "error", err)
		return false
	}

	// Fetch the primary event's metadata for predicate matching.
	metadata, err := r.events.GetMetadataSystem(ctx, orgID, task.PrimaryEventID)
	if err != nil {
		routerLog.Error("re-derive: failed to fetch event metadata", "event_id", task.PrimaryEventID, "error", err)
		return false
	}

	// The task's recorded visibility set — the teams whose handlers
	// matched the original event. Re-derive re-queries every enabled
	// trigger for the event type, so without this gate a trigger whose
	// team was never part of the situation (enabled after the fact, or
	// otherwise absent from the match) could match the stored metadata,
	// fire, and consolidate ownership onto a task that team can't see.
	// The owner team_id always grants visibility, so it qualifies too.
	visibleTeams, err := r.tasks.VisibilityTeamsSystem(ctx, orgID, taskID)
	if err != nil {
		routerLog.Error("re-derive: failed to fetch visibility teams for task", "task_id", taskID, "error", err)
		return false
	}
	visibleSet := map[string]struct{}{}
	if owner := teamIDValue(task); owner != "" {
		visibleSet[owner] = struct{}{}
	}
	for _, vt := range visibleTeams {
		visibleSet[vt] = struct{}{}
	}

	// Author-centric tasks fire only the OWNER's automation (the same rule
	// HandleEvent applies): a deferred trigger must belong to the owning team,
	// and a NULL owner fires nothing. Other event types keep the visibility-set
	// gate so a matched team's deferred trigger fires against the shared task.
	authorCentric := isAuthorCentricGitHubEvent(task.EventType)

	// Set false by a per-trigger read that couldn't reach a verdict, so the
	// task keeps its owed mark and the next cycle's drain revisits it. A
	// trigger that was evaluated and declined — or fired and failed — is a
	// verdict; only "couldn't find out" defers.
	decided := true

	for _, trigger := range handlers {
		if trigger.Kind != domain.EventHandlerKindTrigger {
			continue
		}
		// Gate first on who may fire, then on the firing team's own
		// auto_delegate kill switch. The org-visible sentinel normalizes to
		// the resolved owner team so it matches the (resolved) entries.
		firingTeam := effectiveActingTeam(handlerTeamID(trigger), teamIDValue(task))
		if authorCentric {
			if owner := teamIDValue(task); owner == "" || firingTeam != owner {
				continue
			}
		} else if _, visible := visibleSet[firingTeam]; !visible {
			continue
		}
		// An unreadable kill switch is never treated as permission, but it is
		// also not a decision: the task keeps its owed mark so the next
		// cycle's drain asks again. A switch that reads false is a decision
		// and needs no retry.
		enabled, err := r.autoDelegateEnabledForTeam(ctx, firingTeam)
		if err != nil {
			decided = false
			continue
		}
		if !enabled {
			continue
		}
		minAutonomy := derefFloatDefault(trigger.MinAutonomySuitability, 0)
		// Only process deferred triggers — immediate ones already fired in HandleEvent.
		if minAutonomy <= 0 {
			continue
		}

		// Autonomy gate.
		if *task.AutonomySuitability < minAutonomy {
			routerLog.Debug("re-derive: task suitability below trigger threshold, skipping", "task_id", taskID, "suitability", *task.AutonomySuitability, "trigger_id", trigger.ID, "threshold", minAutonomy)
			continue
		}

		// Predicate match — same logic as HandleEvent.
		predJSON := ""
		if trigger.ScopePredicateJSON != nil {
			predJSON = *trigger.ScopePredicateJSON
		}
		matched, err := matchPredicate(task.EventType, predJSON, metadata)
		if err != nil {
			routerLog.Error("re-derive: trigger predicate error", "trigger_id", trigger.ID, "error", err)
			continue
		}
		if !matched {
			continue
		}

		routerLog.Info("re-derive: task suitability meets trigger threshold, firing", "task_id", taskID, "suitability", *task.AutonomySuitability, "trigger_id", trigger.ID, "threshold", minAutonomy)
		// Triggering event for the queued firing is the task's primary
		// event — that's the one whose match scored autonomously above
		// threshold. Real-event provenance keeps the audit trail honest
		// when a re-derived firing ends up enqueued.
		// A firing that errors is still a decision this pass reached: the
		// task is now the firing machinery's business (the replay fence, the
		// pending-firings queue, the breaker), and re-deriving it every
		// subsequent cycle would just retry the same call forever.
		if _, err := r.tryAutoDelegate(ctx, orgID, task, trigger, task.EntityID, task.PrimaryEventID, firingTeam); err != nil {
			routerLog.Error("re-derive: deferred trigger failed to fire", "task_id", taskID, "trigger_id", trigger.ID, "error", err)
		}
	}
	return decided
}

// derefIntDefault unwraps a *int with a default if nil. Used on
// trigger-only fields that are nullable on the column level but
// guaranteed non-nil by the per-kind CHECK constraint for kind='trigger'
// rows; the default is the defensive value for the impossible nil case.
func derefIntDefault(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func derefFloatDefault(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}
