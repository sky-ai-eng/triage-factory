package routing

import (
	"context"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// HandleEvent routes a single event: entity lifecycle, dedup task
// creation/bump, inline close checks, and auto-delegation. In production it is
// invoked by the durable event-queue drain worker with an already-persisted
// event (evt.ID set) — it is no longer an eventbus subscriber. It is also safe
// to call directly with an unpersisted event (step 1 records it first); that
// path is used by tests.
//
// Processing is best-effort and idempotent w.r.t. re-delivery: the queue is
// at-least-once (a crash mid-process replays the event), and the task dedup
// index makes a replay a no-op rather than a double-create.
//
// The body is a pipeline of named stages; each short-circuits the rest by
// returning ok=false (or an empty result).
func (r *Router) HandleEvent(evt domain.Event) {
	// Defensive: every upstream emitter (poller, per-org loop in SKY-312) tags
	// events with evt.OrgID. A missing OrgID indicates an emitter bug — failing
	// loud here prevents tenant-mixed writes that would silently land on the
	// local sentinel. The check lives here at the single entry point;
	// downstream helpers take orgID as a typed parameter and trust it.
	if evt.OrgID == "" {
		routerLog.Error("dropping event with no org id; emitter bug", "event_type", evt.EventType)
		return
	}
	orgID := evt.OrgID

	// Ensure the event is persisted. In production the ingestor wrote
	// the events row at enqueue (the durable outbox) and the drain worker
	// loaded it via EventStore.GetSystem, so evt.ID is already set. As a safety
	// net for a caller that hands us an unpersisted event (no id — e.g. a
	// direct test call), record it here; later routing relies on evt.ID
	// referring to a real row, so stop if that insert fails.
	if evt.ID == "" {
		id, err := r.events.RecordSystem(context.Background(), orgID, evt)
		if err != nil {
			routerLog.Error("failed to record event", "event_type", evt.EventType, "error", err)
			return
		}
		evt.ID = id
	}

	// Entity-lifecycle gates — terminating events close the entity,
	// system events and closed entities create no task.
	entityID, ok := r.routableEntity(orgID, evt)
	if !ok {
		return
	}

	// Inline close checks run unconditionally — they are lifecycle signals that
	// close stale tasks. They must fire even when no task_rules or triggers
	// match, because close-signal events (ci_check_passed, a submitted review,
	// review_request_removed) are not task-creating events.
	if r.runInlineCloseChecks(orgID, evt, entityID) {
		r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})
	}

	// Match event_handlers (rules + triggers) for this event.
	matchedRules, matchedTriggers := r.matchHandlers(orgID, evt)
	if len(matchedRules) == 0 && len(matchedTriggers) == 0 {
		// Nothing matched — event is recorded but no task created.
		return
	}

	// Resolve the task's owner team, visibility set, firing order, and
	// creation-seed priority.
	routing, ok := r.resolveTeamRouting(orgID, evt, entityID, matchedRules, matchedTriggers)
	if !ok {
		return
	}

	// Find or create the single task for this (entity, event_type,
	// dedup_key); record visibility + lifecycle and enqueue scoring.
	task, ok := r.upsertTaskForEvent(orgID, evt, entityID, routing)
	if !ok {
		return
	}

	// Auto-delegate matching triggers in priority order.
	r.fireMatchedTriggers(orgID, evt, entityID, task, routing)
}

// routableEntity applies the entity-lifecycle gates that precede task
// creation. It returns the active entity's id with ok=true when the event
// should proceed to handler matching; ok=false (after performing any required
// side effects) when it should not:
//   - an entity-terminating event closes the entity + cascade-closes its tasks
//     (and broadcasts), then stops — no task creation on a closing entity;
//   - a system event (no entity context) creates no task;
//   - a late event on an already-closed entity is recorded but spawns nothing.
func (r *Router) routableEntity(orgID string, evt domain.Event) (string, bool) {
	if evt.EntityID != nil && EntityTerminatingEvents[evt.EventType] {
		closed, err := r.closeEntity(orgID, *evt.EntityID)
		if err != nil {
			routerLog.Error("entity lifecycle error", "entity_id", *evt.EntityID, "error", err)
		}
		if closed > 0 {
			r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})
		}
		return "", false
	}

	if evt.EntityID == nil {
		return "", false
	}
	entityID := *evt.EntityID

	entity, err := r.entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil || entity == nil {
		routerLog.Error("failed to load entity", "entity_id", entityID, "error", err)
		return "", false
	}
	if entity.State != "active" {
		return "", false
	}
	return entityID, true
}

// matchHandlers queries the enabled event_handlers for the event type and
// returns those whose scope predicate matches AND whose team tracks the
// event's entity (the team↔scope gate), split by kind. One query,
// kind-discriminated locally — preserves the rules-before-triggers order via
// the store's kind-ASC ORDER BY.
func (r *Router) matchHandlers(orgID string, evt domain.Event) (matchedRules, matchedTriggers []domain.EventHandler) {
	handlers, err := r.handlers.GetEnabledForEventSystem(context.Background(), orgID, evt.EventType)
	if err != nil {
		routerLog.Error("failed to query event_handlers", "event_type", evt.EventType, "error", err)
	}

	// scopeCache memoizes the team↔scope (e.g. team↔repo) gate per team for
	// this one event, so a team with several matching handlers does a single
	// tracking lookup.
	scopeCache := map[string]bool{}
	for _, h := range handlers {
		predJSON := ""
		if h.ScopePredicateJSON != nil {
			predJSON = *h.ScopePredicateJSON
		}
		matched, err := matchPredicate(evt.EventType, predJSON, evt.MetadataJSON)
		if err != nil {
			routerLog.Error("event_handler predicate error", "handler_id", h.ID, "kind", h.Kind, "error", err)
			continue
		}
		if !matched {
			continue
		}
		// Team↔scope gate (SKY-375 repos / SKY-376 Jira projects): a team's
		// handler only fires for events whose entity the team tracks. Dropped
		// here so the team never enters the visibility set, its triggers never
		// fire, and SKY-368's task_teams excludes it for free. System/org-union
		// handlers (NULL team_id) skip the gate.
		if !r.handlerScopeMatchesEvent(evt, h, scopeCache) {
			continue
		}
		switch h.Kind {
		case domain.EventHandlerKindRule:
			matchedRules = append(matchedRules, h)
		case domain.EventHandlerKindTrigger:
			matchedTriggers = append(matchedTriggers, h)
		}
	}
	return matchedRules, matchedTriggers
}

// eventRouting is the resolved team-routing for an event: which team owns the
// task, which teams can see it, the deterministic firing order, the per-team
// triggers to fire, and the creation-seed priority.
type eventRouting struct {
	ownerTeam    string
	visibleTeams []string
	orderedTeams []string
	teamTriggers map[string][]domain.EventHandler
	taskPriority float64
}

// resolveTeamRouting computes the owner team, visibility set, firing order, and
// seed priority for an event from its matched handlers. ok=false means the
// event resolves to no team and should create no task (review_requested with
// no mapped reviewer team; an author-centric event from an external author
// with no team watching).
//
// Four routing axes override the default handler-team grouping:
//   - review_requested → the requested reviewer's team(s), scoped at emit time;
//   - author-centric github events → the entity's owning team via the
//     owning-team ladder (the PR author's CI/conflict/feedback lifecycle);
//   - assignee-centric jira events → the assignee's owning team via the same
//     ladder (the issue assignee's assignment/status/comment lifecycle);
//   - everything else (jira:issue:available, etc.) → the handler-team grouping.
//
// The matched handlers still gate whether a task is created and supply the
// priority; only the team set is overridden.
func (r *Router) resolveTeamRouting(orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler) (eventRouting, bool) {
	// Group matched handlers by team. Org-visibility handlers (team_id NULL)
	// route to LocalDefaultTeamID via handlerTeamID; the Postgres store
	// resolves that sentinel to the org's canonical team.
	teamRules := map[string][]domain.EventHandler{}
	teamTriggers := map[string][]domain.EventHandler{}
	for _, h := range matchedRules {
		teamRules[handlerTeamID(h)] = append(teamRules[handlerTeamID(h)], h)
	}
	for _, h := range matchedTriggers {
		teamTriggers[handlerTeamID(h)] = append(teamTriggers[handlerTeamID(h)], h)
	}

	// The visibility set: every team that had any matching handler. Sorted so
	// the task_teams writes are deterministic.
	visibleTeams := make([]string, 0, len(teamRules)+len(teamTriggers))
	seen := map[string]struct{}{}
	for t := range teamRules {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			visibleTeams = append(visibleTeams, t)
		}
	}
	for t := range teamTriggers {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			visibleTeams = append(visibleTeams, t)
		}
	}
	sort.Strings(visibleTeams)

	// orderedTeams ranks the matched teams by per-team rule priority (desc),
	// ties broken by lowest team id. The owner is the first — the
	// highest-priority matched team — and triggers fire in this order, so the
	// team that wins the exclusive claim (and thus consolidates as owner) is
	// deterministic and matches the creation-time owner pick. A team with only
	// triggers contributes the 0.5 trigger-fallback priority. The score map is
	// built once (handlerTeamID runs once per rule, not once per comparison) and
	// reused for both the order and the owner's seed priority; the same helpers
	// rank the ambiguous owner-ladder path (orderTeamsByRulePriority).
	scores := teamRulePriorityScores(seen, matchedRules)
	orderedTeams := orderTeamsByScores(sortedKeys(seen), scores)
	ownerTeam := orderedTeams[0]
	taskPriority := scores[ownerTeam]

	switch {
	case evt.EventType == domain.EventGitHubPRReviewRequested:
		// Falls back to the handler-team visibility above for legacy events
		// that carry no requested identity or when the mapping stores are
		// unwired (scoped=false).
		if reqTeams, scoped := r.reviewRequestVisibilityTeams(orgID, evt); scoped {
			if len(reqTeams) == 0 {
				routerLog.Warn("review_requested: requested identity maps to no tf team, recording event but no task", "entity_id", entityID)
				return eventRouting{}, false
			}
			// NULL owner at creation, like the author-/assignee-centric ladders:
			// the task is created unowned and visible to all the reviewer's
			// teams. fireMatchedTriggers walks orderedTeams (every reviewer team,
			// sorted by id) and the first team whose pr-review trigger wins the
			// exclusive claim consolidates ownership onto itself (delegation.go
			// SetOwnerTeamSystem before the fire); if no team auto-fires, the
			// first human claim consolidates. No behavior change for the
			// single-team reviewer (the fire resolves to their one team); the
			// multi-team reviewer's task is no longer arbitrarily stamped to the
			// lowest-id team before anything acts.
			visibleTeams = reqTeams
			orderedTeams = reqTeams // already sorted by team id in the helper
			ownerTeam = ""          // NULL at creation; the fire (or first human claim) consolidates the owner
			taskPriority = maxRuleDefaultPriority(matchedRules)
		}

	case isAuthorCentricGitHubEvent(evt.EventType):
		// The PR author's CI/conflict/feedback lifecycle → the entity's owning
		// team via the owning-team ladder.
		owner, ownerSet := r.authorCentricOwner(orgID, evt, entityID)
		vt, ot, ord, pri, ok := ownerLadderRouting(owner, ownerSet, matchedRules)
		if !ok {
			// External/non-TF author (dependabot, renovate, outside
			// contributors) and no team opted in via an explicit rule → record
			// the event + durable entity only, no task. Deliberately silent:
			// this is the expected high-volume path with nothing actionable.
			return eventRouting{}, false
		}
		visibleTeams, ownerTeam, orderedTeams, taskPriority = vt, ot, ord, pri

	case isAssigneeCentricJiraEvent(evt.EventType):
		// The issue assignee's assignment/status/comment lifecycle → the
		// assignee's owning team via the same ladder (jira:issue:available is
		// excluded — it's the unassigned team-pool signal and stays on
		// handler-team routing above).
		owner, ownerSet := r.assigneeCentricJiraOwner(orgID, evt, entityID)
		vt, ot, ord, pri, ok := ownerLadderRouting(owner, ownerSet, matchedRules)
		if !ok {
			// External/unassigned account (not a TF member) and no team opted
			// in via an explicit watch rule → record the event + durable entity
			// only, no task. Deliberately silent: this is the high-volume
			// external-assignee path, the local N=1 over-creation fix.
			return eventRouting{}, false
		}
		visibleTeams, ownerTeam, orderedTeams, taskPriority = vt, ot, ord, pri
	}

	return eventRouting{
		ownerTeam:    ownerTeam,
		visibleTeams: visibleTeams,
		orderedTeams: orderedTeams,
		teamTriggers: teamTriggers,
		taskPriority: taskPriority,
	}, true
}

// upsertTaskForEvent finds or creates the single task for this (entity,
// event_type, dedup_key), records its visibility set + spawned/bumped
// lifecycle, enqueues scoring, and broadcasts. ok=false means no task should
// exist (the became_atomic dedup) or creation failed.
func (r *Router) upsertTaskForEvent(orgID string, evt domain.Event, entityID string, routing eventRouting) (*domain.Task, bool) {
	// became_atomic is the belated-discovery path for parents whose subtasks
	// just closed. Suppress the new card if any active task already exists on
	// the entity — otherwise an atomic ticket that gained and then lost
	// subtasks ends up with two cards. The dedup index can't catch this because
	// the existing task's event_type (jira:issue:assigned) differs from the new
	// one (jira:issue:became_atomic). The event is still recorded; only task
	// creation is skipped.
	if evt.EventType == domain.EventJiraIssueBecameAtomic {
		active, err := r.tasks.FindActiveByEntitySystem(context.Background(), orgID, entityID)
		if err != nil {
			routerLog.Error("became_atomic: failed to check active tasks on entity", "entity_id", entityID, "error", err)
			return nil, false
		}
		if len(active) > 0 {
			routerLog.Warn("became_atomic: entity already has an active task, skipping duplicate creation", "entity_id", entityID)
			return nil, false
		}
	}

	// Task createdAt = OccurredAt when the source reported a time, falling back
	// to time.Now(). Keeps the backfill path's "stamp the task with the PR's
	// CreatedAt for week-old review requests" semantic — queue ordering reflects
	// when the world said the event happened, not when we noticed.
	createdAt := time.Now()
	if !evt.OccurredAt.IsZero() {
		createdAt = evt.OccurredAt
	}
	task, created, err := r.tasks.FindOrCreateAtSystem(context.Background(), orgID, routing.ownerTeam, entityID, evt.EventType, evt.DedupKey, evt.ID, routing.taskPriority, createdAt)
	if err != nil {
		routerLog.Error("failed to find/create task", "event_type", evt.EventType, "entity_id", entityID, "error", err)
		return nil, false
	}

	// Record the visibility set transactionally with the task. Additive: a
	// re-arrival matching new teams widens visibility. Failure here is logged
	// but not fatal — the owning team_id still grants the owner visibility; a
	// follow-up event re-attempts the wider set.
	if err := r.tasks.SetVisibilityTeamsSystem(context.Background(), orgID, task.ID, routing.visibleTeams); err != nil {
		routerLog.Error("failed to set visibility teams for task", "task_id", task.ID, "error", err)
	}

	if created {
		if err := r.tasks.RecordEventSystem(context.Background(), orgID, task.ID, evt.ID, "spawned"); err != nil {
			routerLog.Error("failed to record spawned task_event", "task_id", task.ID, "error", err)
		}
		routerLog.Info("created task", "task_id", task.ID, "event_type", evt.EventType, "entity_id", entityID, "owner_team", routing.ownerTeam, "visible_teams", len(routing.visibleTeams))
	} else {
		if err := r.tasks.BumpSystem(context.Background(), orgID, task.ID, evt.ID); err != nil {
			routerLog.Error("failed to bump task", "task_id", task.ID, "error", err)
		}
		if err := r.tasks.RecordEventSystem(context.Background(), orgID, task.ID, evt.ID, "bumped"); err != nil {
			routerLog.Error("failed to record bumped task_event", "task_id", task.ID, "error", err)
		}
	}

	// Enqueue AI scoring (always — produces UI metadata regardless) and
	// broadcast the task update to the frontend.
	r.scorer.Trigger(orgID)
	r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})
	return task, true
}

// fireMatchedTriggers auto-delegates the matched triggers against the task, one
// team at a time in priority order. The exclusive claim CAS resolves contention
// (first claimer — human or bot — wins, the rest no-op), so only the
// highest-priority matched team's trigger actually runs; later teams find the
// task claimed and skip inside tryAutoDelegate. The per-team kill switch is
// checked per firing team, and triggers with min_autonomy_suitability > 0 defer
// to the post-scoring re-derive.
func (r *Router) fireMatchedTriggers(orgID string, evt domain.Event, entityID string, task *domain.Task, routing eventRouting) {
	for _, teamID := range routing.orderedTeams {
		triggers := routing.teamTriggers[teamID]
		if len(triggers) == 0 {
			continue
		}
		// Normalize the org-visible sentinel to the resolved owner team so the
		// kill-switch / team_agents / claim all read a real team.
		acting := effectiveActingTeam(teamID, teamIDValue(task))
		if !r.autoDelegateEnabledForTeam(context.Background(), acting) {
			continue
		}
		for _, trigger := range triggers {
			if trigger.MinAutonomySuitability != nil && *trigger.MinAutonomySuitability > 0 {
				continue // deferred to post-scoring handler
			}
			r.tryAutoDelegate(orgID, task, trigger, entityID, evt.ID, acting)
		}
	}
}
