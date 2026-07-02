package routing

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
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

	// Entitlement gate (TFAC-524) — a gated-off event source (never entitled,
	// or entitled and lapsed; deliberately identical) is frozen: the event
	// stays recorded above (the append-only log is an honest record of what
	// happened) but everything downstream — close phase, task creation,
	// trigger firing — is skipped. This is belt-and-suspenders: the primary
	// enforcement is the gated feature's own ingest self-gating and not
	// producing events at all; this covers lapse races, drained-outbox
	// stragglers, and any future emitter that forgets to gate.
	if !entitlements.EventTypeAllowed(orgID, evt.EventType) {
		return
	}

	// Entity-lifecycle gate — system events and already-closed entities create
	// nothing. A terminating event is gated here while its entity is STILL
	// active (it is what closes the entity, in the close phase below), so it
	// passes; only later stragglers on the now-closed entity drop.
	entityID, ok := r.routableEntity(orgID, evt)
	if !ok {
		return
	}

	// Close phase — runs unconditionally, before routing, and independent of
	// whether any handler matches. It closes the tasks this event resolves
	// (typed siblings like ci_check_passed→ci_check_failed, and/or the
	// entity-wide set for a terminating event) and reports whether the entity
	// should flip closed. Closing and routing are orthogonal: the same event
	// goes on to create/fire its own task below (the close runs first, so a
	// terminating event clears prior work before minting its riding task).
	closedAny, terminate := r.runCloses(orgID, evt, entityID)
	if terminate {
		if err := r.entities.CloseSystem(context.Background(), orgID, entityID); err != nil {
			lifecycleLog.Error("entity close failed", "entity_id", entityID, "error", err)
		}
	}
	if closedAny {
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

// routableEntity is the entity-lifecycle gate that precedes the close + route
// phases. It returns the active entity's id with ok=true when the event should
// proceed; ok=false when it should not:
//   - a system event (no entity context) creates nothing;
//   - an event on an already-closed entity is recorded but spawns nothing — a
//     straggler that arrived after the PR/issue terminated.
//
// A terminating event is NOT special-cased here: it reaches this gate while its
// entity is still active (the close it performs happens in the close phase that
// follows), so it passes and goes on to close + route. Closing the entity is
// the close phase's job, not the gate's.
func (r *Router) routableEntity(orgID string, evt domain.Event) (string, bool) {
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
// event resolves to no team and should create no task (review_requested with no
// mapped reviewer team; an owner-ladder event from an external identity with no
// team watching).
//
// It dispatches on the event's ownership model (ownershipModelForEvent), the one
// explicit classification of every event type (TFAC-519). Each model resolves
// its own (visibility, owner, firing order, priority); teamTriggers and the
// final assembly are shared. There is no "compute the handler-team default then
// overwrite it" — a model that doesn't apply is simply never invoked. The
// models:
//
//   - OwnershipOwned (author-centric github, assignee-centric jira) → the entity's
//     owning team via the owning-team ladder; non-owner teams reach it only via
//     an applies_to_unowned watch handler.
//   - OwnershipRequestedParty (review_requested) → the requested reviewer's team(s),
//     scoped at emit time, with a handler-team fallback for legacy events.
//   - OwnershipPool (jira:issue:available, everything else) → handler-team grouping.
//
// The matched handlers still gate whether a task is created and supply the
// priority; the model only decides the team set + owner.
func (r *Router) resolveTeamRouting(orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler) (eventRouting, bool) {
	// teamTriggers is model-independent: a team fires its matched triggers iff it
	// lands in the model's orderedTeams. Grouped once, threaded through unchanged.
	// Org-visibility handlers (team_id NULL) route to LocalDefaultTeamID via
	// handlerTeamID; the Postgres store resolves that sentinel to the org's team.
	teamTriggers := map[string][]domain.EventHandler{}
	for _, h := range matchedTriggers {
		teamTriggers[handlerTeamID(h)] = append(teamTriggers[handlerTeamID(h)], h)
	}

	var (
		visibleTeams, orderedTeams []string
		ownerTeam                  string
		taskPriority               float64
		ok                         bool
	)
	switch ownershipModelForEvent(evt.EventType) {
	case events.OwnershipOwned:
		visibleTeams, ownerTeam, orderedTeams, taskPriority, ok = r.resolveOwnedRouting(orgID, evt, entityID, matchedRules, matchedTriggers)
	case events.OwnershipRequestedParty:
		visibleTeams, ownerTeam, orderedTeams, taskPriority, ok = r.resolveRequestedPartyRouting(orgID, evt, entityID, matchedRules, matchedTriggers)
	default: // events.OwnershipPool
		visibleTeams, ownerTeam, orderedTeams, taskPriority, ok = resolvePoolRouting(matchedRules, matchedTriggers)
	}
	if !ok {
		return eventRouting{}, false
	}

	return eventRouting{
		ownerTeam:    ownerTeam,
		visibleTeams: visibleTeams,
		orderedTeams: orderedTeams,
		teamTriggers: teamTriggers,
		taskPriority: taskPriority,
	}, true
}

// resolvePoolRouting is the handler-team grouping (OwnershipPool): every team with a
// matched handler is a participant, the highest-priority team is the eagerly-
// stamped owner, and triggers fire in priority-desc / id-asc order. Used for
// unassigned team-pool events (jira:issue:available) and as the
// requested-party fallback. Always ok=true — resolveTeamRouting is only reached
// with ≥1 matched handler, so the participant set is non-empty. A team with only
// triggers contributes the 0.5 trigger-fallback priority via teamRulePriorityScores.
func resolvePoolRouting(matchedRules, matchedTriggers []domain.EventHandler) (visibleTeams []string, ownerTeam string, orderedTeams []string, taskPriority float64, ok bool) {
	seen := map[string]struct{}{}
	for _, h := range matchedRules {
		seen[handlerTeamID(h)] = struct{}{}
	}
	for _, h := range matchedTriggers {
		seen[handlerTeamID(h)] = struct{}{}
	}
	allTeams := sortedKeys(seen)
	scores := teamRulePriorityScores(seen, matchedRules)
	orderedTeams = orderTeamsByScores(allTeams, scores)
	ownerTeam = orderedTeams[0]
	return allTeams, ownerTeam, orderedTeams, scores[ownerTeam], true
}

// resolveOwnedRouting routes an owner-ladder event to the entity's owning team.
// A registered event source resolves its own owner first — if hooks exist
// for evt's source, hooks.ResolveOwner supplies (owner, ownerSet) in place of
// the built-in provider branch. Built-in providers differ by provider
// (author login for github, assignee account for jira); the shared tail —
// visibility = ownerSet ∪ watchers, firing = members-then-watchers with the
// no-steal invariant — lives in ownerLadderRouting, and registered sources
// inherit all of it unchanged (do NOT reimplement it here). ok=false →
// external identity, no watching handler → no task.
func (r *Router) resolveOwnedRouting(orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler) ([]string, string, []string, float64, bool) {
	var owner string
	var ownerSet []string
	if hooks, ok := sourceHooksFor(evt.EventType); ok {
		owner, ownerSet = hooks.ResolveOwner(context.Background(), orgID, evt, entityID)
	} else if isAuthorCentricGitHubEvent(evt.EventType) {
		owner, ownerSet = r.authorCentricOwner(orgID, evt, entityID)
	} else {
		owner, ownerSet = r.assigneeCentricJiraOwner(orgID, evt, entityID)
	}
	return ownerLadderRouting(owner, ownerSet, matchedRules, matchedTriggers)
}

// resolveRequestedPartyRouting routes review_requested to the requested
// reviewer's team(s), resolved from the event's requested identity. scoped=false
// (legacy event with no requested identity, or unwired mapping stores) falls back
// to the handler-team grouping. scoped=true with no mapped team → no task (a
// TF-known reviewer that maps to no team is a config gap, not a reason to
// over-fan). On the scoped path the task is created NULL-owned and visible to
// every reviewer team (already id-sorted by the helper); fireMatchedTriggers
// walks them and the first whose pr-review trigger wins the exclusive claim
// consolidates ownership onto itself, else the first human claim does.
func (r *Router) resolveRequestedPartyRouting(orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler) ([]string, string, []string, float64, bool) {
	reqTeams, scoped := r.reviewRequestVisibilityTeams(orgID, evt)
	if !scoped {
		return resolvePoolRouting(matchedRules, matchedTriggers)
	}
	if len(reqTeams) == 0 {
		routerLog.Warn("review_requested: requested identity maps to no tf team, recording event but no task", "entity_id", entityID)
		return nil, "", nil, 0, false
	}
	return reqTeams, "", reqTeams, maxRuleDefaultPriority(matchedRules), true
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
