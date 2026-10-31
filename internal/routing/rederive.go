package routing

import (
	"context"
	"fmt"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// The post-scoring re-evaluation. The ranked trigger walk (walkTriggers) holds
// at the first team whose deferred trigger (MinAutonomySuitability > 0) has no
// score yet, on the promise that the walk runs again once the task has one.
// That obligation is a task_rederive_queue row, admitted by the score write in
// the transaction that writes the scores (workkinds.TaskReDerive); the worker
// in rederive_worker.go claims it, evaluates the task through evaluateReDerive
// below, and admits whatever the evaluation decided to fire onto
// pending_firings inside the completion, so the decision and its record
// commit together or not at all.
//
// The evaluation itself is a pure function of what it reads: it fires
// nothing and writes nothing. It returns a plan, and the completion is where
// the plan becomes rows, and where a conflict the plan decided is posted.

// reDeriveFiring is one trigger the evaluation decided to fire: the handler,
// the team it fires for, and the agent it claims the task for.
type reDeriveFiring struct {
	Trigger    domain.EventHandler
	FiringTeam string
	AgentID    string
}

// reDerivePlan is the evaluation's verdict. Task is the task as read, nil
// when it is gone; Firings is empty for the many decided outcomes that fire
// nothing. Conflict is the automation conflict the walk decided between one
// identity's teams, nil when it decided none; the completion posts it once
// the firings it rests on have landed.
type reDerivePlan struct {
	Task     *domain.Task
	Firings  []reDeriveFiring
	Conflict *automationConflict
}

// evaluateReDerive runs the task's ranked trigger walk against the score it
// carries now. Every decided outcome — including "nothing to fire" and "task
// gone" — returns a plan and nil. err is non-nil only where a read failed
// before a verdict could be reached: the task load, the primary event read,
// handler matching, owner resolution, an unreadable per-team switch, or a
// preflight read; the caller retries those rather than treating them as a
// decision.
//
// The walk is the event-time path's own, resumed where it held: the matched
// handlers and the routing are recomputed from the task's primary event by
// matchHandlers and resolveTeamRouting, through the event's current view, and
// walkTriggers decides them from the hold (fromHold). Nothing about the
// event-time walk is persisted, so the two can differ only by what changed in
// between — about one scoring cycle, since a task is scored once and a bump
// does not rescore it. With the score in hand no trigger is undecided: a
// deferred trigger whose threshold the score meets fires, one below it skips,
// and the teams below the hold are decided in rank order, their immediate
// triggers included. The switches stay live reads, as they are at event time:
// a trigger disabled or deleted, auto_delegate_enabled, team_agents, the
// breaker.
//
// orgID is the scoring context — every task the scorer writes belongs to the
// org whose runner wrote it. A trigger the walk fires is planned only after
// the preflight the event-time path runs (autoDelegatePreflight), so a
// re-derived firing passes exactly the gates an event-time one does.
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

	// No score landed — nothing to gate against. Decided, not deferred: the
	// event-time walk already ran without a score, and this one would decide
	// nothing differently.
	if task.AutonomySuitability == nil {
		return plan, nil
	}

	evt, err := r.events.GetSystem(ctx, orgID, task.PrimaryEventID)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("load event %s: %w", task.PrimaryEventID, err)
	}
	if evt == nil {
		return plan, nil
	}

	scopeCache := map[string]bool{}
	matchedRules, matchedTriggers, _, err := r.matchHandlers(ctx, orgID, *evt, scopeCache)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("match handlers for event %s: %w", evt.ID, err)
	}
	// With no deferred trigger matched, the event-time walk held nothing and
	// decided every trigger; the score leaves nothing to decide.
	if !slices.ContainsFunc(matchedTriggers, deferredTrigger) {
		return plan, nil
	}
	routing, ok, err := r.resolveTeamRouting(ctx, orgID, *evt, task.EntityID, matchedRules, matchedTriggers, scopeCache)
	if err != nil {
		return reDerivePlan{}, fmt.Errorf("resolve owner for event %s: %w", evt.ID, err)
	}
	if !ok {
		return plan, nil
	}

	walk, err := r.walkTriggers(ctx, orgID, task, routing, true, func(trigger domain.EventHandler, acting string) (bool, error) {
		// The checks the event-time path runs before it decides a firing:
		// the cross-team exclusive-claim skip, the org agent, the per-team
		// bot switch, the breaker.
		agentID, proceed, err := r.autoDelegatePreflight(ctx, orgID, task, trigger, task.EntityID, acting)
		if err != nil || !proceed {
			return false, err
		}
		routerLog.Info("re-derive: planning a firing", "task_id", taskID, "suitability", *task.AutonomySuitability, "trigger_id", trigger.ID, "team", acting)
		plan.Firings = append(plan.Firings, reDeriveFiring{Trigger: trigger, FiringTeam: acting, AgentID: agentID})
		// The firing's admission stamps the bot's claim for this team, and
		// the event-time path reads that claim off the in-memory task when
		// the next matched trigger comes round: a second team's trigger
		// against the same task is the cross-team duplication the one-task
		// model refuses. Mirror the claim the way claimCommitted will after
		// the commit, so the remaining candidates see what they would have
		// seen at event time.
		if agentID != "" {
			task.ClaimedByAgentID = agentID
			if acting != "" {
				task.TeamID = teamIDPtr(acting)
			}
		}
		return true, nil
	})
	if err != nil {
		return reDerivePlan{}, err
	}
	plan.Conflict = walk.conflict(routing)
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
