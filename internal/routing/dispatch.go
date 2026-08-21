package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// HandleEvent routes a single event: entity lifecycle, dedup task
// creation/bump, inline close checks, and auto-delegation. In production it is
// invoked by the durable event-queue drain worker with an already-persisted
// event (evt.ID set) — it is no longer an eventbus subscriber. It is also safe
// to call directly with an unpersisted event (step 1 records it first); that
// path is used by tests.
//
// A non-nil error means this event's ROUTING OBLIGATION is unmet and the
// caller must not consider the event handled. The obligation stages are entity
// resolution, the close phase (the tasks this event resolves plus the entity
// flip a terminating event performs), handler matching, owner resolution, the
// task upsert, and trigger firing (plus the delegation tail behind it): each one
// is load-bearing for whether the right task exists, on the right team, whether
// the ones this event resolved are gone, and whether the right run started —
// and none of them is recoverable by any other path in time. The tracker's
// snapshot-diff is forward-only, so a dropped event is simply never re-derived;
// a lost close is worse still, since a terminating transition emits exactly once
// and nothing else reconciles an entity whose close write failed (only the
// periodic terminal reconciler does, and at sweep latency). The drain worker
// answers an error by requeueing the row for another attempt (see
// parkOrRequeue), so an unmet obligation is retried rather than silently
// consumed.
//
// The tails that hang off those stages stay log-and-continue and never reach
// the return: task visibility writes, Bump, the run-stop cascade's KILL half
// on a closed task, the agent-claim stamp, and the scorer nudge. Each is
// either repaired by the next event on the entity, unreachable by a replay
// anyway, or cosmetic, and promoting them would replay a whole event —
// re-running its stages and re-recording its audit rows — to fix something
// that heals on its own or that the replay cannot touch.
//
// The run-stop cascade is split along exactly that line (see
// closeTaskWithAudit): the durable stop INTENT commits with the task close, so
// the obligation covers it, while the kill that acts on it stays best-effort
// out here. A kill that never lands no longer strands anything — the claim
// gate refuses a cancel-requested blueprint and the reaper finalizes it — so
// there is nothing left for a replay to owe. The task_events audit row moved
// into the close tx for the same reason it always sat next to the close, and
// it is INSERT-or-nothing, so a replay still cannot double it.
//
// Replay is safe by construction, so an error asks for one freely: the task
// upsert is a FindOrCreate under the tasks dedup partial index, and trigger
// firing is fenced on (triggering_event_id, trigger_id) plus the one-active-
// auto-run index, with ErrTaskBusy deferring onto pending_firings. The close
// phase re-attempts only what is still open — its task list read returns active
// rows only, and both the task close and the entity close are guarded
// transitions that no-op on an already-closed row. The one accepted cost is a
// duplicate 'bumped' task_events row per replay.
//
// ctx reaches every store call the routing of this one event makes. The drain
// worker hands it a ctx that carries values but cannot be cancelled
// (processQueuedEvent's context.WithoutCancel), because a claimed row is one
// unit: half a routed event is worse than a whole one that outlives its
// shutdown signal. Callers that route an event outside the worker (tests, a
// direct call) decide their own cancellation posture.
//
// The body is a pipeline of named stages; each short-circuits the rest by
// returning ok=false (or an empty result), setting disp.Disposition on its
// way out so the deferred publish below still reports the outcome.
func (r *Router) HandleEvent(ctx context.Context, evt domain.Event) error {
	// Defensive: every upstream emitter (poller, per-org loop) tags
	// events with evt.OrgID. A missing OrgID indicates an emitter bug — failing
	// loud here prevents tenant-mixed writes that would silently land on the
	// local sentinel. The check lives here at the single entry point;
	// downstream helpers take orgID as a typed parameter and trust it.
	//
	// Unroutable is not the same as handled: this returns an error so the row
	// parks with the reason on it rather than being consumed, which is the
	// difference between an emitter bug someone can find later and one that
	// leaves no trace. Production can't reach it through the queue — the row's
	// org id is stamped onto the event at load — so the parked row is the
	// evidence, not a cost.
	if evt.OrgID == "" {
		routerLog.Error("refusing to route event with no org id; emitter bug", "event_type", evt.EventType)
		return fmt.Errorf("event %s has no org id", evt.EventType)
	}
	orgID := evt.OrgID

	// Ensure the event is persisted. In production the ingestor wrote
	// the events row at enqueue (the durable outbox) and the drain worker
	// loaded it via EventStore.GetSystem, so evt.ID is already set. As a safety
	// net for a caller that hands us an unpersisted event (no id — e.g. a
	// direct test call), record it here; later routing relies on evt.ID
	// referring to a real row, so stop if that insert fails.
	if evt.ID == "" {
		id, err := r.events.RecordSystem(ctx, orgID, evt)
		if err != nil {
			routerLog.Error("failed to record event", "event_type", evt.EventType, "error", err)
			return fmt.Errorf("record event: %w", err)
		}
		evt.ID = id
	}

	// Disposition sentinel (TFAC-593) — published exactly once per handled
	// event, after its outcome's DB writes have landed (task upsert,
	// task_events), so a consumer reading stores on receipt sees the
	// settled state. disp is a value captured by the closure below, so
	// every field write between here and return is visible when the
	// deferred call actually runs — each stage sets disp.Disposition on
	// its way out instead of a publish call at every return site.
	//
	// publishDisposition itself drops OwnershipUnrouted event types
	// (system:* sentinels, including this one): HandleEvent doesn't see
	// them in production (RouterBound gates the durable queue they'd have
	// to arrive through to reach here), but a direct call must not let the
	// router sentinel its own sentinel.
	disp := events.SystemRoutingDispositionMetadata{EventID: evt.ID, EventType: evt.EventType}
	defer func() {
		// Same value, two audiences: the sentinel tells an async event
		// source what happened to its event, the span attribute tells
		// whoever is reading the trace why it ended where it did.
		recordDisposition(ctx, disp.Disposition)
		r.publishDisposition(orgID, disp)
	}()

	// Entitlement gate (TFAC-524) — a gated-off event source (never entitled,
	// or entitled and lapsed; deliberately identical) is frozen: the event
	// stays recorded above (the append-only log is an honest record of what
	// happened) but everything downstream — close phase, task creation,
	// trigger firing — is skipped. This is belt-and-suspenders: the primary
	// enforcement is the gated feature's own ingest self-gating and not
	// producing events at all; this covers lapse races, drained-outbox
	// stragglers, and any future emitter that forgets to gate.
	if !entitlements.EventTypeAllowed(orgID, evt.EventType) {
		disp.Disposition = events.DispositionFrozen
		return nil
	}

	// An org admin turned this source off without unbinding its credential.
	// The drop is HERE, after the event is recorded and before anything
	// ephemeral is derived from it, because this is the single funnel every
	// source's events cross: poll-driven, push-driven, and any future emitter
	// alike. The poll-cycle skip beside it guarantees something different — no
	// API calls, not no tasks — so an event reaching this point from a producer
	// that never went through a poll cycle is still stopped here.
	//
	// The append-only log stays complete, so an admin can still see what
	// happened while the source was paused; only the ephemeral layer is
	// suppressed, which is exactly the split the data model already draws.
	// Existing tasks are untouched — a pause is forward-only, like every other
	// tracking change.
	//
	// An error fails the event rather than letting it through: the caller
	// requeues an errored event, so a transient read costs a retry, while
	// answering "not paused" on a failed read would mint the tasks the pause
	// exists to prevent.
	if r.sourceGate != nil {
		paused, err := r.sourceGate.disabled(ctx, orgID, eventsource.KindOf(evt.EventType))
		if err != nil {
			disp.Disposition = events.DispositionError
			return fmt.Errorf("resolve event source policy: %w", err)
		}
		if paused {
			disp.Disposition = events.DispositionSourceDisabled
			return nil
		}
	}

	// Entity-lifecycle gate — system events and already-closed entities create
	// nothing. A terminating event is gated here while its entity is STILL
	// active (it is what closes the entity, in the close phase below), so it
	// passes; only later stragglers on the now-closed entity drop.
	entityID, ok, err := r.routableEntity(ctx, orgID, evt)
	if err != nil {
		disp.Disposition = events.DispositionError
		return fmt.Errorf("resolve entity: %w", err)
	}
	if !ok {
		disp.Disposition = events.DispositionTasklessUnroutable
		return nil
	}
	disp.EntityID = entityID

	// Close phase — runs unconditionally, before routing, and independent of
	// whether any handler matches. It closes the tasks this event resolves
	// (typed siblings like ci_check_passed→ci_check_failed, and/or the
	// entity-wide set for a terminating event) and reports whether the entity
	// should flip closed. Closing and routing are orthogonal: the same event
	// goes on to create/fire its own task below (the close runs first, so a
	// terminating event clears prior work before minting its riding task).
	closedAny, terminate, closeErr := r.runCloses(ctx, orgID, evt, entityID)
	if closedAny {
		r.broadcastTasksUpdated(orgID)
	}
	if closeErr != nil {
		// Bail BEFORE flipping the entity, not just because the pass failed.
		// routableEntity gates the replay on state='active', so an entity
		// closed here would make the replayed event drop at that gate — the
		// close phase would never run again and whatever tasks are still open
		// would stay open with nothing left to close them. Leaving the entity
		// active is what keeps the retry able to finish the job.
		disp.Disposition = events.DispositionError
		return fmt.Errorf("close phase: %w", closeErr)
	}
	if terminate {
		if _, err := r.entities.CloseSystem(ctx, orgID, entityID); err != nil {
			lifecycleLog.Error("entity close failed", "entity_id", entityID, "error", err)
			disp.Disposition = events.DispositionError
			return fmt.Errorf("close entity: %w", err)
		}
	}

	// One team↔scope cache for the whole event: the per-handler scope gate
	// (matchHandlers) and the owner ladder's identity-tier gate (resolveTeamRouting
	// → trackingTeams) share it, so a team's tracking status is looked up once per
	// event even when it appears in both the matched handlers and the author's team
	// set.
	scopeCache := map[string]bool{}

	// Match event_handlers (rules + triggers) for this event.
	matchedRules, matchedTriggers, err := r.matchHandlers(ctx, orgID, evt, scopeCache)
	if err != nil {
		disp.Disposition = events.DispositionError
		return fmt.Errorf("match handlers: %w", err)
	}
	if len(matchedRules) == 0 && len(matchedTriggers) == 0 {
		// Nothing matched — event is recorded but no task created.
		disp.Disposition = events.DispositionTasklessNoHandler
		return nil
	}

	// Resolve the task's owner team, visibility set, firing order, and
	// creation-seed priority. An error here is "could not find out who owns
	// this" — distinct from the resolved no-owner outcome below, which is a
	// real answer and consumes the event.
	routing, ok, err := r.resolveTeamRouting(ctx, orgID, evt, entityID, matchedRules, matchedTriggers, scopeCache)
	if err != nil {
		disp.Disposition = events.DispositionError
		return fmt.Errorf("resolve owner: %w", err)
	}
	if !ok {
		disp.Disposition = events.DispositionTasklessNoOwner
		return nil
	}
	disp.OwnerTeamID = routing.ownerTeam

	// Find or create the single task for this (entity, event_type,
	// dedup_key); record visibility + lifecycle and enqueue scoring.
	task, created, err := r.upsertTaskForEvent(ctx, orgID, evt, entityID, routing)
	if err != nil {
		disp.Disposition = events.DispositionError
		return fmt.Errorf("upsert task: %w", err)
	}
	if task == nil {
		// became_atomic dedup suppression: an active task already covers
		// the entity, so no new one was minted. Legitimate "no task,"
		// not a failure — buckets with the other taskless outcomes.
		disp.Disposition = events.DispositionTasklessUnroutable
		return nil
	}
	disp.TaskID = task.ID
	if created {
		disp.Disposition = events.DispositionTaskCreated
	} else {
		disp.Disposition = events.DispositionTaskBumped
	}

	// Auto-delegate matching triggers in priority order. The disposition stays
	// task_created/task_bumped even when a trigger failed: the task really was
	// minted, and that is what the sentinel's audience asked about. The
	// returned error is the other audience — the worker, which retries the
	// event so the trigger gets its second chance.
	fired, injectedConversationID, err := r.fireMatchedTriggers(ctx, orgID, evt, entityID, task, routing)
	disp.TriggersFired = fired
	disp.InjectedConversationID = injectedConversationID
	if err != nil {
		return fmt.Errorf("fire triggers: %w", err)
	}
	return nil
}

// publishDisposition is HandleEvent's single deferred publish point for the
// per-event routing disposition sentinel (TFAC-593). Nil-guards the
// publisher and drops OwnershipUnrouted event types (system:* sentinels) so
// the router never sentinels its own sentinel. Best-effort past that: a
// marshal failure is logged and dropped, never fails routing — the outcome
// it describes already landed in the DB before this runs.
func (r *Router) publishDisposition(orgID string, disp events.SystemRoutingDispositionMetadata) {
	if r.publisher == nil {
		return
	}
	if ownershipModelForEvent(disp.EventType) == events.OwnershipUnrouted {
		return
	}
	raw, err := json.Marshal(disp)
	if err != nil {
		routerLog.Warn("marshal routing disposition metadata failed", "event_type", disp.EventType, "error", err)
		return
	}
	r.publisher.Publish(domain.Event{
		OrgID:        orgID,
		EventType:    domain.EventSystemRoutingDisposition,
		MetadataJSON: string(raw),
		OccurredAt:   time.Now().UTC(),
	})
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
//
// err is non-nil ONLY for a genuine store failure — distinct from the
// legitimate ok=false cases (no entity context, dangling reference, closed
// entity) — so the caller can tell "nothing to route to" from "couldn't find
// out" rather than reporting the latter as the former.
func (r *Router) routableEntity(ctx context.Context, orgID string, evt domain.Event) (entityID string, ok bool, err error) {
	if evt.EntityID == nil {
		return "", false, nil
	}
	entityID = *evt.EntityID

	entity, err := r.entities.GetSystem(ctx, orgID, entityID)
	if err != nil {
		routerLog.Error("failed to load entity", "entity_id", entityID, "error", err)
		return "", false, err
	}
	if entity == nil {
		// Dangling reference — surprising, but not a query failure; treated
		// like "nothing to route to."
		routerLog.Error("entity not found", "entity_id", entityID)
		return "", false, nil
	}
	if entity.State != "active" {
		return "", false, nil
	}
	return entityID, true, nil
}

// matchHandlers queries the enabled event_handlers for the event type and
// returns those whose scope predicate matches AND whose team tracks the
// event's entity (the team↔scope gate), split by kind. One query,
// kind-discriminated locally — preserves the rules-before-triggers order via
// the store's kind-ASC ORDER BY.
//
// err is non-nil ONLY when the event_handlers query itself failed — the
// caller must not read that as "queried fine, zero handlers matched" (which
// would misreport an internal error as a legitimate "nothing configured"
// outcome).
func (r *Router) matchHandlers(ctx context.Context, orgID string, evt domain.Event, scopeCache map[string]bool) (matchedRules, matchedTriggers []domain.EventHandler, err error) {
	ctx, span := tracer.Start(ctx, "route.match")
	defer span.End()

	handlers, err := r.handlers.GetEnabledForEventSystem(ctx, orgID, evt.EventType)
	if err != nil {
		span.SetStatus(codes.Error, "query event_handlers")
		routerLog.ErrorContext(ctx, "failed to query event_handlers", "event_type", evt.EventType, "error", err)
		return nil, nil, err
	}

	// scopeCache memoizes the team↔scope (e.g. team↔repo) gate per team for this
	// one event, so a team with several matching handlers does a single tracking
	// lookup. It's owned by HandleEvent and shared with the owner ladder's
	// identity-tier gate (resolveTeamRouting → trackingTeams), so a team gated
	// here isn't re-queried when it also appears in the author's team set.
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
		// Team↔scope gate (repos / Jira projects): a team's
		// handler only fires for events whose entity the team tracks. Dropped
		// here so the team never enters the visibility set, its triggers never
		// fire, and task_teams excludes it for free. System/org-union
		// handlers (NULL team_id) skip the gate.
		if !r.handlerScopeMatchesEvent(ctx, evt, h, scopeCache) {
			continue
		}
		switch h.Kind {
		case domain.EventHandlerKindRule:
			matchedRules = append(matchedRules, h)
		case domain.EventHandlerKindTrigger:
			matchedTriggers = append(matchedTriggers, h)
		}
	}
	span.SetAttributes(telemetry.Count(len(matchedRules) + len(matchedTriggers)))
	return matchedRules, matchedTriggers, nil
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
// (ok=false, err=nil) is a RESOLVED answer and consumes the event; err != nil
// means the resolution could not be completed and the caller must replay. The
// two used to be one value, and that made every store failure beneath here
// indistinguishable from a genuinely external author — no task at all, or worse,
// a task on a watching team that then anchors every future event on the entity
// to itself. The identity and prior-owner reads propagate for that reason; the
// team↔repo / team↔project tracking gates deliberately still fail open (see
// teamTracksEventRepo), since they only narrow a set someone else computed.
//
// It dispatches on the event's ownership model (ownershipModelForEvent), the
// one explicit classification of every event type. Each model resolves its
// own (visibility, owner, firing order, priority); teamTriggers and the
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
//   - OwnershipUnrouted (system:* sentinels) → never reached here at all; a
//     system event carries no EntityID, so routableEntity drops it before
//     HandleEvent gets this far. Falls into the same default branch as Pool
//     purely as a defensive fallback, never exercised in practice.
//
// The matched handlers still gate whether a task is created and supply the
// priority; the model only decides the team set + owner.
func (r *Router) resolveTeamRouting(ctx context.Context, orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler, scopeCache map[string]bool) (eventRouting, bool, error) {
	ctx, span := tracer.Start(ctx, "route.own")
	defer span.End()

	// teamTriggers is model-independent: a team fires its matched triggers iff it
	// lands in the model's orderedTeams. Grouped once, folded into whichever
	// model resolved. Org-visibility handlers (team_id NULL) route to
	// LocalDefaultTeamID via handlerTeamID; the Postgres store resolves that
	// sentinel to the org's team.
	teamTriggers := map[string][]domain.EventHandler{}
	for _, h := range matchedTriggers {
		teamTriggers[handlerTeamID(h)] = append(teamTriggers[handlerTeamID(h)], h)
	}

	var (
		routing eventRouting
		ok      bool
		err     error
	)
	switch ownershipModelForEvent(evt.EventType) {
	case events.OwnershipOwned:
		routing, ok, err = r.resolveOwnedRouting(ctx, orgID, evt, entityID, matchedRules, matchedTriggers, scopeCache)
	case events.OwnershipRequestedParty:
		routing, ok, err = r.resolveRequestedPartyRouting(ctx, orgID, evt, entityID, matchedRules, matchedTriggers)
	default: // events.OwnershipPool, events.OwnershipUnrouted
		routing, ok = resolvePoolRouting(matchedRules, matchedTriggers), true
	}
	if err != nil {
		span.SetStatus(codes.Error, "resolve owner")
		return eventRouting{}, false, err
	}
	if !ok {
		span.SetAttributes(telemetry.Outcome("no_owner"))
		return eventRouting{}, false, nil
	}

	routing.teamTriggers = teamTriggers
	if routing.ownerTeam != "" {
		span.SetAttributes(telemetry.TeamID(routing.ownerTeam))
	}
	span.SetAttributes(telemetry.Count(len(routing.visibleTeams)))
	return routing, true, nil
}

// resolvePoolRouting is the handler-team grouping (OwnershipPool): every team with a
// matched handler is a participant, the highest-priority team is the eagerly-
// stamped owner, and triggers fire in priority-desc / id-asc order. Used for
// unassigned team-pool events (jira:issue:available) and as the
// requested-party fallback. It always resolves — resolveTeamRouting is only
// reached with ≥1 matched handler, so the participant set is non-empty, and it
// reads no store so it cannot fail. A team with only triggers contributes the
// 0.5 trigger-fallback priority via teamRulePriorityScores. teamTriggers is
// filled in by the caller.
func resolvePoolRouting(matchedRules, matchedTriggers []domain.EventHandler) eventRouting {
	seen := map[string]struct{}{}
	for _, h := range matchedRules {
		seen[handlerTeamID(h)] = struct{}{}
	}
	for _, h := range matchedTriggers {
		seen[handlerTeamID(h)] = struct{}{}
	}
	allTeams := sortedKeys(seen)
	scores := teamRulePriorityScores(seen, matchedRules)
	orderedTeams := orderTeamsByScores(allTeams, scores)
	ownerTeam := orderedTeams[0]
	return eventRouting{
		ownerTeam:    ownerTeam,
		visibleTeams: allTeams,
		orderedTeams: orderedTeams,
		taskPriority: scores[ownerTeam],
	}
}

// resolveOwnedRouting routes an owner-ladder event to the entity's owning team.
// A registered event source resolves its own owner first — if hooks exist
// for evt's source, hooks.ResolveOwner supplies (owner, ownerSet, err) in place
// of the built-in provider branch. Built-in providers differ by provider
// (author login for github, assignee account for jira); the shared tail —
// visibility = ownerSet ∪ watchers, firing = members-then-watchers with the
// no-steal invariant — lives in ownerLadderRouting, and registered sources
// inherit all of it unchanged (do NOT reimplement it here). ok=false →
// external identity, no watching handler → no task.
//
// A non-nil error means the owner could not be resolved and the event must be
// replayed rather than routed on a guess. The registered-source arm carries the
// identical contract (see SourceHooks.ResolveOwner) — and needs it more, not
// less: the hook stands in for the WHOLE ladder there, so a swallowed failure
// has no lower tier left to be caught by. resolveSourceOwner is what makes that
// contract a mechanism rather than a rule: a hook that returns empty without
// claiming Unowned is refused, not believed.
func (r *Router) resolveOwnedRouting(ctx context.Context, orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler, scopeCache map[string]bool) (eventRouting, bool, error) {
	var owner string
	var ownerSet []string
	var err error
	if hooks, ok := sourceHooksFor(evt.EventType); ok {
		owner, ownerSet, err = resolveSourceOwner(ctx, hooks, eventSourcePrefix(evt.EventType), orgID, evt, entityID)
	} else if isAuthorCentricGitHubEvent(evt.EventType) {
		owner, ownerSet, err = r.authorCentricOwner(ctx, orgID, evt, entityID, scopeCache)
	} else {
		owner, ownerSet, err = r.assigneeCentricJiraOwner(ctx, orgID, evt, entityID, scopeCache)
	}
	if err != nil {
		return eventRouting{}, false, err
	}
	vis, ownerTeam, ordered, priority, ok := ownerLadderRouting(owner, ownerSet, matchedRules, matchedTriggers)
	return eventRouting{
		ownerTeam:    ownerTeam,
		visibleTeams: vis,
		orderedTeams: ordered,
		taskPriority: priority,
	}, ok, nil
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
//
// An unresolvable identity read errors instead of falling into either arm: the
// scoped-and-empty drop is the answer for a reviewer who really maps to no
// team, and letting a failed lookup borrow it would consume the event with no
// task at all.
func (r *Router) resolveRequestedPartyRouting(ctx context.Context, orgID string, evt domain.Event, entityID string, matchedRules, matchedTriggers []domain.EventHandler) (eventRouting, bool, error) {
	reqTeams, scoped, err := r.reviewRequestVisibilityTeams(ctx, orgID, evt)
	if err != nil {
		return eventRouting{}, false, err
	}
	if !scoped {
		return resolvePoolRouting(matchedRules, matchedTriggers), true, nil
	}
	if len(reqTeams) == 0 {
		routerLog.Warn("review_requested: requested identity maps to no tf team, recording event but no task", "entity_id", entityID)
		return eventRouting{}, false, nil
	}
	return eventRouting{
		visibleTeams: reqTeams,
		orderedTeams: reqTeams,
		taskPriority: maxRuleDefaultPriority(matchedRules),
	}, true, nil
}

// upsertTaskForEvent finds or creates the single task for this (entity,
// event_type, dedup_key), records its visibility set + spawned/bumped
// lifecycle, enqueues scoring, and broadcasts.
//
// Three outcomes, kept distinguishable so the caller never confuses a real
// failure with a deliberate no-op:
//   - (task, created, nil)  — success; created reports which.
//   - (nil, false, nil)     — the became_atomic dedup suppression: an active
//     task already covers the entity, so no new one is minted. Legitimate,
//     not an error.
//   - (nil, false, err)     — a store failure; no task exists to report.
func (r *Router) upsertTaskForEvent(ctx context.Context, orgID string, evt domain.Event, entityID string, routing eventRouting) (task *domain.Task, created bool, err error) {
	ctx, span := tracer.Start(ctx, "route.task", trace.WithAttributes(telemetry.EntityID(entityID)))
	defer span.End()

	// Task createdAt = OccurredAt when the source reported a time, falling back
	// to now. Keeps the backfill path's "stamp the task with the PR's
	// CreatedAt for week-old review requests" semantic — queue ordering reflects
	// when the world said the event happened, not when we noticed.
	//
	// Both arms convert, so one variable cannot carry two zones. The
	// OccurredAt arm converts here rather than trusting its producer: the
	// event may have been built by hand anywhere upstream, and a local-zone
	// instant reaching created_at misorders the task against every row beside
	// it.
	createdAt := time.Now().UTC()
	if !evt.OccurredAt.IsZero() {
		createdAt = evt.OccurredAt.UTC()
	}

	if evt.EventType == domain.EventJiraIssueBecameAtomic {
		// became_atomic is the belated-discovery path for parents whose
		// subtasks just closed. Suppress the new card if any active task
		// already exists on the entity — otherwise an atomic ticket that
		// gained and then lost subtasks ends up with two cards. The
		// standard dedup index can't catch this because the existing
		// task's event_type (e.g. jira:issue:assigned) differs from the
		// new one (jira:issue:became_atomic) — this is a cross-event-type
		// invariant, not a same-key race. FindOrCreateAtUnlessEntityActiveSystem
		// (TFAC-579) backs the active-check + create with a real per-entity
		// lock instead of a separate check-then-act, so a concurrent
		// normal event's task creation on this entity can't land in the
		// gap between the check and the insert. The event is still
		// recorded; only task creation is skipped.
		var suppressed bool
		task, created, suppressed, err = r.tasks.FindOrCreateAtUnlessEntityActiveSystem(ctx, orgID, routing.ownerTeam, entityID, evt.EventType, evt.DedupKey, evt.ID, routing.taskPriority, createdAt)
		if err != nil {
			span.SetStatus(codes.Error, "find or create task")
			routerLog.ErrorContext(ctx, "became_atomic: failed to check/create task on entity", "entity_id", entityID, "error", err)
			return nil, false, err
		}
		if suppressed {
			// Anticipated: the dedup rule doing its job, not a failure.
			span.SetAttributes(telemetry.Outcome("suppressed_entity_active"))
			routerLog.WarnContext(ctx, "became_atomic: entity already has an active task, skipping duplicate creation", "entity_id", entityID)
			return nil, false, nil
		}
	} else {
		task, created, err = r.tasks.FindOrCreateAtSystem(ctx, orgID, routing.ownerTeam, entityID, evt.EventType, evt.DedupKey, evt.ID, routing.taskPriority, createdAt)
		if err != nil {
			span.SetStatus(codes.Error, "find or create task")
			routerLog.ErrorContext(ctx, "failed to find/create task", "event_type", evt.EventType, "entity_id", entityID, "error", err)
			return nil, false, err
		}
	}

	// Record the visibility set transactionally with the task. Additive: a
	// re-arrival matching new teams widens visibility. Failure here is logged
	// but not fatal — the owning team_id still grants the owner visibility; a
	// follow-up event re-attempts the wider set.
	if err := r.tasks.SetVisibilityTeamsSystem(ctx, orgID, task.ID, routing.visibleTeams); err != nil {
		routerLog.Error("failed to set visibility teams for task", "task_id", task.ID, "error", err)
	}

	if created {
		if err := r.tasks.RecordEventSystem(ctx, orgID, task.ID, evt.ID, "spawned"); err != nil {
			routerLog.Error("failed to record spawned task_event", "task_id", task.ID, "error", err)
		}
		routerLog.Info("created task", "task_id", task.ID, "event_type", evt.EventType, "entity_id", entityID, "owner_team", routing.ownerTeam, "visible_teams", len(routing.visibleTeams))
	} else {
		if _, err := r.tasks.BumpSystem(ctx, orgID, task.ID, evt.ID); err != nil {
			routerLog.Error("failed to bump task", "task_id", task.ID, "error", err)
		}
		if err := r.tasks.RecordEventSystem(ctx, orgID, task.ID, evt.ID, "bumped"); err != nil {
			routerLog.Error("failed to record bumped task_event", "task_id", task.ID, "error", err)
		}
	}

	// Enqueue AI scoring (always — produces UI metadata regardless) and
	// broadcast the task update to the frontend.
	r.scorer.Trigger(orgID)
	r.broadcastTasksUpdated(orgID)
	span.SetAttributes(telemetry.TaskID(task.ID))
	return task, created, nil
}

// fireMatchedTriggers auto-delegates the matched triggers against the task, one
// team at a time in priority order. The exclusive claim CAS resolves contention
// (first claimer — human or bot — wins, the rest no-op), so only the
// highest-priority matched team's trigger actually runs; later teams find the
// task claimed and skip inside tryAutoDelegate. The per-team kill switch is
// checked per firing team, and triggers with min_autonomy_suitability > 0 defer
// to the post-scoring re-derive. Returns the count of triggers that actually
// committed the bot to the task (tryAutoDelegate returned true — an immediate
// fire or a successful enqueue onto pending_firings) — deferred triggers and
// every no-op skip inside tryAutoDelegate (already claimed by another team,
// breaker tripped, agent unavailable, disabled for the team, etc.) don't count.
//
// A dependency failure under one trigger does not cancel the others: the loop
// collects and keeps going, so the triggers that can still commit do. The
// joined error asks the caller for a replay, and the replay is fenced per
// (event, trigger) — the siblings that already fired no-op the second time
// through, leaving only the failed one to actually retry.
func (r *Router) fireMatchedTriggers(ctx context.Context, orgID string, evt domain.Event, entityID string, task *domain.Task, routing eventRouting) (int, string, error) {
	ctx, span := tracer.Start(ctx, "route.triggers", trace.WithAttributes(telemetry.TaskID(task.ID)))
	defer span.End()

	fired := 0
	injectedConversationID := ""
	var errs []error
	for _, teamID := range routing.orderedTeams {
		triggers := routing.teamTriggers[teamID]
		if len(triggers) == 0 {
			continue
		}
		// Normalize the org-visible sentinel to the resolved owner team so the
		// kill-switch / team_agents / claim all read a real team.
		acting := effectiveActingTeam(teamID, teamIDValue(task))
		enabled, err := r.autoDelegateEnabledForTeam(ctx, acting)
		if err != nil {
			// Unreadable is not disabled. The gate still refuses to fire on a
			// team whose switch it couldn't read, but the event goes back on
			// the queue rather than being consumed as if the user had turned
			// automation off.
			errs = append(errs, fmt.Errorf("auto-delegate gate for team %s: %w", acting, err))
			continue
		}
		if !enabled {
			// Diagnosable, not silent: a matched trigger that never fires because
			// auto-delegate is off for the team is the "a task was created but no
			// run started" surprise. One line at Info names the reason and the
			// knob (team_settings.auto_delegate_enabled).
			routerLog.Info("auto-trigger skipped: auto-delegate disabled for team",
				"team", acting, "task_id", task.ID, "event_type", evt.EventType, "matched_triggers", len(triggers))
			continue
		}
		for _, trigger := range triggers {
			if trigger.MinAutonomySuitability != nil && *trigger.MinAutonomySuitability > 0 {
				continue // deferred to post-scoring handler
			}
			committed, err := r.tryAutoDelegateTrackingInjection(ctx, orgID, task, trigger, entityID, evt.ID, acting, &injectedConversationID)
			if err != nil {
				errs = append(errs, fmt.Errorf("trigger %s: %w", trigger.ID, err))
				continue
			}
			if committed {
				fired++
			}
		}
	}
	span.SetAttributes(telemetry.Count(fired))
	return fired, injectedConversationID, errors.Join(errs...)
}
