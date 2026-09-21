package routing

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// The post-scoring re-evaluation. Triggers with MinAutonomySuitability > 0
// are skipped during HandleEvent on the promise that a task's deferred
// triggers are evaluated once it has a score. That obligation is a
// task_rederive_queue row, admitted by the score write in the transaction
// that writes the scores (workkinds.TaskReDerive); the worker in
// rederive_worker.go claims it, evaluates the task through evaluateReDerive
// below, and admits whatever the evaluation decided to fire onto
// pending_firings inside the completion, so the decision and its record
// commit together or not at all.
//
// The evaluation itself is a pure function of what it reads: it fires
// nothing and writes nothing. It returns a plan, and the completion is where
// the plan becomes rows.

// reDeriveFiring is one trigger the evaluation decided to fire: the handler,
// the team it fires for, and the agent it claims the task for.
type reDeriveFiring struct {
	Trigger    domain.EventHandler
	FiringTeam string
	AgentID    string
}

// reDerivePlan is the evaluation's verdict. Task is the task as read, nil
// when it is gone; Firings is empty for the many decided outcomes that fire
// nothing.
type reDerivePlan struct {
	Task    *domain.Task
	Firings []reDeriveFiring
}

// evaluateReDerive evaluates one task's deferred triggers against the score
// it carries now. Every decided outcome — including "nothing to fire" and
// "task gone" — returns a plan and nil. err is non-nil only where a read
// failed before a verdict could be reached: the task load, the handlers
// read, the metadata read, the visibility read, an unreadable per-team
// switch, or a preflight read; the caller retries those rather than treating
// them as a decision.
//
// orgID is the scoring context — every task the scorer writes belongs to the
// org whose runner wrote it. Per-team auto_delegate_enabled is checked per
// firing team after the task's teams are resolved, and the preflight the
// event-time path runs (autoDelegatePreflight) runs here per candidate
// trigger, so a re-derived firing passes exactly the gates an event-time
// one does.
func (r *Router) evaluateReDerive(ctx context.Context, orgID, taskID string) (reDerivePlan, error) {
	task, err := r.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("load task: %w", err)
	}
	plan := reDerivePlan{Task: task}
	// A task that no longer exists is decided, not deferred — there is
	// nothing left to evaluate.
	if task == nil {
		return plan, nil
	}

	// Entitlement gate — a task on a now-gated-off event type fires nothing
	// during the post-scoring pass, mirroring HandleEvent's freeze.
	if !entitlements.EventTypeAllowed(orgID, task.EventType) {
		return plan, nil
	}

	// Only queued tasks are re-derived. The lifecycle axis is {queued,
	// in_progress, snoozed, done, dismissed}, so this gate also covers
	// snoozed (a snoozed task is on a "wait" until its wake-on-bump event
	// lands; a deferred-threshold re-derive should not bypass that signal).
	if task.Status != "queued" {
		return plan, nil
	}

	// Re-derive must not promote a task that's already claimed. The
	// responsibility axis lives on the claim columns, not status — so a
	// queued task may still be "already taken" by either the bot
	// (auto-delegate already fired and enqueued a firing, or drag-to-bot
	// stamped) or a user ("I'll take this myself" claim). Either claim
	// column set means the commitment is real and the lifecycle event that
	// ends it will arrive via its own path.
	if task.ClaimedByAgentID != "" || task.ClaimedByUserID != "" {
		return plan, nil
	}

	// No score landed — nothing to gate against. Decided, not deferred: a
	// task with no score has nothing for a later pass to do differently.
	if task.AutonomySuitability == nil {
		return plan, nil
	}

	// Fetch handlers for this event type — same call HandleEvent uses,
	// kind-discriminated here.
	handlers, err := r.handlers.GetEnabledForEventSystem(ctx, orgID, task.EventType)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("query event_handlers for %s: %w", task.EventType, err)
	}

	// Fetch the primary event's metadata for predicate matching.
	metadata, err := r.events.GetMetadataSystem(ctx, orgID, task.PrimaryEventID)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("fetch metadata of event %s: %w", task.PrimaryEventID, err)
	}

	// The task's recorded visibility set — the teams whose handlers matched
	// the original event. Re-derive re-queries every enabled trigger for the
	// event type, so without this gate a trigger whose team was never part
	// of the situation (enabled after the fact, or otherwise absent from the
	// match) could match the stored metadata, fire, and consolidate
	// ownership onto a task that team can't see. The owner team_id always
	// grants visibility, so it qualifies too.
	visibleTeams, err := r.tasks.VisibilityTeamsSystem(ctx, orgID, taskID)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("fetch visibility teams of task %s: %w", taskID, err)
	}
	visibleSet := map[string]struct{}{}
	if owner := teamIDValue(task); owner != "" {
		visibleSet[owner] = struct{}{}
	}
	for _, vt := range visibleTeams {
		visibleSet[vt] = struct{}{}
	}

	// Author-centric tasks fire only the OWNER's automation (the same rule
	// HandleEvent applies): a deferred trigger must belong to the owning
	// team, and a NULL owner fires nothing. Other event types keep the
	// visibility-set gate so a matched team's deferred trigger fires against
	// the shared task.
	authorCentric := isAuthorCentricGitHubEvent(task.EventType)

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
		// also not a decision: the read failed before a verdict. A switch
		// that reads false is a decision.
		enabled, err := r.autoDelegateEnabledForTeam(ctx, firingTeam)
		if err != nil {
			return reDerivePlan{}, fmt.Errorf("read auto_delegate switch of team %s: %w", firingTeam, err)
		}
		if !enabled {
			continue
		}
		minAutonomy := derefFloatDefault(trigger.MinAutonomySuitability, 0)
		// Only deferred triggers — immediate ones already fired in HandleEvent.
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

		// The checks the event-time path runs before it decides a firing:
		// the cross-team exclusive-claim skip, the org agent, the per-team
		// bot switch, the breaker.
		agentID, proceed, err := r.autoDelegatePreflight(ctx, orgID, task, trigger, task.EntityID, firingTeam)
		if err != nil {
			return reDerivePlan{}, fmt.Errorf("preflight trigger %s: %w", trigger.ID, err)
		}
		if !proceed {
			continue
		}

		routerLog.Info("re-derive: task suitability meets trigger threshold, planning a firing", "task_id", taskID, "suitability", *task.AutonomySuitability, "trigger_id", trigger.ID, "threshold", minAutonomy)
		plan.Firings = append(plan.Firings, reDeriveFiring{Trigger: trigger, FiringTeam: firingTeam, AgentID: agentID})
		// The firing's admission stamps the bot's claim for this team, and
		// the event-time path reads that claim off the in-memory task when
		// the next matched trigger comes round: a second team's trigger
		// against the same task is the cross-team duplication the one-task
		// model refuses. Mirror the claim the way claimCommitted will after
		// the commit, so the remaining candidates see what they would have
		// seen at event time.
		if agentID != "" {
			task.ClaimedByAgentID = agentID
			if firingTeam != "" {
				task.TeamID = teamIDPtr(firingTeam)
			}
		}
	}
	return plan, nil
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
