package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// factoryDelegateRequest is the body for POST /api/factory/delegate.
// All four fields are required; dedup_key may be empty for non-
// discriminator event types (the common case).
type factoryDelegateRequest struct {
	EntityID    string `json:"entity_id"`
	EventType   string `json:"event_type"`
	DedupKey    string `json:"dedup_key"`
	BlueprintID string `json:"blueprint_id"`
	// TeamID is the acting team the write picker supplied — the team the
	// synthesized task is created (and bot-claimed) under. Required in the
	// UI when the caller belongs to ≥2 teams; empty (sole-team fallback)
	// otherwise.
	TeamID string `json:"team_id"`
}

// factoryDelegateResponse is the success shape: the task the drop resolved
// to and the run it fired. ClaimStamped is always true on a 200 — the user's
// gesture committed at the claim axis and the run materialized. A failed
// spawn is a real error status (422/500 via writeDelegateSpawnError), not a
// 200 with an error field; the claim survives that failure, so the client
// refetches and finds the task bot-claimed with no run.
type factoryDelegateResponse struct {
	TaskID         string `json:"task_id"`
	ConversationID string `json:"conversation_id"`
	ClaimStamped   bool   `json:"claim_stamped"`
}

// handleFactoryDelegate is the drag-to-delegate endpoint behind the
// station drawer's drop-on-runs gesture. Find-or-create on the task
// keeps the UX uniform: every queued chip is draggable, and dropping
// either reuses the existing task at this station or synthesizes a
// new one anchored on the most recent matching event.
//
// Race-safe via the partial unique index on
// (entity_id, event_type, dedup_key) WHERE status NOT IN ('done',
// 'dismissed') — concurrent drops resolve to the same task.
func (s *Server) handleFactoryDelegate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var req factoryDelegateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.EntityID == "" {
		v.Missing("entity_id")
	}
	if req.EventType == "" {
		v.Missing("event_type")
	}
	if req.BlueprintID == "" {
		v.Missing("blueprint_id")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Reject viewers before any task is synthesized (TFAC-447): delegation is
	// agent work a viewer can't start. Resolve the acting team read-only (no
	// last-acting stamp — we may be about to 403) and gate on it. Without this
	// the tasks_insert RLS WITH CHECK would fail the FindOrCreate as a generic
	// 500 instead of a clean role-named 403. A bad team pick still 400s via the
	// selection-error mapping, same as the main path.
	var actingTeam string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "factory", err)
		return
	}
	if !s.az.RequireTeamWrite(w, r, orgID, userID, actingTeam) {
		return
	}

	// Entity must exist and be active. The factory snapshot's 60s
	// soft-close grace window means a chip can ride the final
	// animation hop after its entity already flipped to closed; if the
	// user drags during that window, we shouldn't synthesize a fresh
	// task on a merged/closed entity (no second close-cascade would
	// clean it up — it'd run to completion against a closed PR).
	// Mirrors the router's "task creation requires active entity"
	// contract at routing/router.go.
	var entity *domain.Entity
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		entity, e = tx.Entities.Get(r.Context(), orgID, req.EntityID)
		return e
	}); err != nil {
		internalError(w, "factory", err)
		return
	}
	if entity == nil {
		notFound(w, "entity")
		return
	}
	if entity.State != "active" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonAlreadyTerminal,
			Message: "entity is closed; cannot delegate",
		})
		return
	}

	// Anchor the (possibly synthesized) task on the most recent event
	// matching all three of (entity_id, event_type, dedup_key). The
	// dedup_key filter is pushed into the SQL — picking the latest
	// event by type alone and rejecting a mismatch would 400 every
	// time a sibling discriminator (e.g. label_added "help wanted")
	// fired more recently than the dragged chip's discriminator
	// (label_added "bug"). If no matching event exists the entity
	// isn't actually at this station; refuse rather than fabricate
	// an anchor.
	var primaryEvent *domain.Event
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		primaryEvent, e = tx.Events.LatestForEntityTypeAndDedupKey(r.Context(), orgID, req.EntityID, req.EventType, req.DedupKey)
		return e
	}); err != nil {
		internalError(w, "factory", err)
		return
	}
	if primaryEvent == nil {
		// The body parsed fine; the (entity, event_type, dedup_key) triple it
		// names doesn't exist — a semantic fault in what was referenced, so
		// 422 rather than 400.
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "no matching event for entity at this station",
		})
		return
	}

	// Spawner availability gate runs after every request + state
	// validation (400/404/409) so callers learn about bad input
	// before they hit the infrastructure gap. Sits before the
	// Tasks.FindOrCreate + RecordSwipe writes so a missing spawner
	// can't leave a half-applied delegate (task + swipe row but no
	// run). Tests rely on this ordering to exercise the 404/409/400
	// paths without installing a spawner.
	if s.spawner == nil {
		httpx.WriteErrors(w, http.StatusServiceUnavailable, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "spawner not configured",
		})
		return
	}

	schema, schemaOK := events.Get(req.EventType)

	var task *domain.Task
	var created bool
	var claimTeamID string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Resolve the acting team FIRST, so the priority scan below can
		// limit itself to that team's rules. Scanning before resolving
		// let a sibling team's high-priority rule inflate a task created
		// for a different team (Codex review on PR #263).
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}

		// Default priority — mirrors internal/routing/router.go's
		// predicate-match filter. Only rules belonging to the acting team
		// (or org-visible system rules with no team) count: a rule owned
		// by another team must not lift the priority of a task created
		// for this team. Iterating *all* enabled rules would also inflate
		// priority whenever a high-priority rule's scope_predicate doesn't
		// match this event's metadata, so the predicate gate stays too.
		// Empty predJSON always matches per the events package contract.
		defaultPriority := 0.5
		handlers, e := tx.EventHandlers.GetEnabledForEvent(r.Context(), orgID, req.EventType)
		if e != nil {
			return e
		}
		for _, h := range handlers {
			if h.Kind != domain.EventHandlerKindRule || h.DefaultPriority == nil {
				// Trigger rows have no DefaultPriority; skip.
				continue
			}
			// Team scope: count a rule only if it belongs to the acting
			// team or is genuinely org-visible (empty TeamID). A sibling
			// team's rule (non-empty TeamID that isn't the acting team)
			// must not lift the priority of a task we're stamping for
			// teamID. Note multi-mode system rules are seeded per-team
			// (visibility='team', real team_id), so this correctly counts
			// the acting team's own shipped rules and excludes siblings'.
			if h.TeamID != "" && h.TeamID != teamID {
				continue
			}
			if !schemaOK {
				// No registered schema → predicate can't be evaluated.
				// Mirrors matchPredicate's quietly-permissive behavior:
				// the rule is skipped, falling back to 0.5.
				continue
			}
			predJSON := ""
			if h.ScopePredicateJSON != nil {
				predJSON = *h.ScopePredicateJSON
			}
			matched, merr := schema.Match(predJSON, primaryEvent.MetadataJSON)
			if merr != nil {
				factoryLog.Warn("event_handler predicate error, skipping", "event_handler", h.ID, "error", merr)
				continue
			}
			if matched && *h.DefaultPriority > defaultPriority {
				defaultPriority = *h.DefaultPriority
			}
		}

		task, created, e = tx.Tasks.FindOrCreate(r.Context(), orgID, teamID, req.EntityID, req.EventType, req.DedupKey, primaryEvent.ID, defaultPriority)
		if e != nil {
			return e
		}
		// The bot-enablement gate (below) must check the team the handoff
		// will consolidate onto, which for a found multi-team task can
		// differ from task.TeamID (the latter may be a team the caller
		// isn't in). Resolve it here in the same tx.
		claimTeamID, e = tx.Tasks.ResolveClaimTeam(r.Context(), orgID, task.ID, userID)
		if e != nil {
			return e
		}
		// Mirror the router's audit linkage: when a brand-new task is
		// synthesized, record a task_events row linking it to the event
		// it was anchored on. Same kind="spawned" the router uses at
		// internal/routing/router.go:223-227, so a future timeline UI
		// reading task_events sees a uniform shape regardless of which
		// path created the task. Non-fatal — audit gap is preferable to
		// failing the delegate after the row is already in tasks.
		//
		// We don't record kind="bumped" on the find branch: the drag is
		// the user's gesture (already captured in swipe_events via
		// RecordSwipe), not a fresh event landing — there's nothing new
		// to link the existing task to.
		if created {
			if recErr := tx.Tasks.RecordEvent(r.Context(), orgID, task.ID, primaryEvent.ID, "spawned"); recErr != nil {
				factoryLog.Warn("failed to record spawned task_event", "task", task.ID, "error", recErr)
			}
		}
		return nil
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "factory", err)
		return
	}

	// Alignment with the task delegate route: the user's gesture is
	// commitment regardless of run outcome. Stamp the agent claim
	// BEFORE attempting the spawn so a failed Delegate leaves the
	// task in the bot's lane (with no run, surfacing as a "delegate
	// failed — retry" card on the Board) rather than disappearing
	// the gesture entirely. The user-facing semantic: "I told the bot
	// to take this. The bot took the assignment but couldn't get the
	// run going on this attempt."
	//
	// claim is the responsibility axis (commitment); runs are the
	// execution axis. They're orthogonal; a failed run doesn't
	// invalidate the assignment.
	// Re-check team_agents.enabled at gesture
	// time. Factory drag-to-bot is the same semantic as the task
	// delegate route — both refuse with 409 when the bot is off for
	// this team. Gate on claimTeamID — the team HandoffAgentClaim will
	// consolidate the task onto — not the org default and not the
	// pre-handoff task.TeamID (which for a found multi-team task can be a
	// team the caller isn't in, whose team_agents row RLS would hide,
	// wrongly reporting the bot disabled).
	a, enabled, err := s.agentEnabledForTeam(r.Context(), orgID, userID, claimTeamID)
	if err != nil {
		factoryLog.Error("delegate aborted", "task", task.ID, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "delegate failed" + localDetail(err),
		})
		return
	}
	if !enabled {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "bot is disabled for this team; enable it in team settings to delegate",
		})
		return
	}
	// HandoffAgentClaim handles all three legitimate factory drop
	// transitions: unclaimed → bot, user-claimed-by-me → bot
	// (a chip the user previously claimed via the Board), and the
	// idempotent same-agent no-op. Refuses on a different-user
	// claim — the factory drop shouldn't steal.
	var handoffResult db.HandoffResult
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handoffResult, e = tx.Tasks.HandoffAgentClaim(r.Context(), orgID, task.ID, a.ID, userID)
		return e
	}); err != nil {
		factoryLog.Error("failed to stamp agent claim", "task", task.ID, "error", err)
		httpx.WriteErrors(w, http.StatusInternalServerError, httpx.ErrorItem{
			Reason: httpx.ReasonInternal, Message: "claim stamp failed" + localDetail(err),
		})
		return
	}
	switch handoffResult {
	case db.HandoffChanged:
		s.ws.Broadcast(websocket.Event{
			Type:  "task_claimed",
			OrgID: orgID,
			Data: map[string]any{
				"task_id":             task.ID,
				"claimed_by_agent_id": a.ID,
				"claimed_by_user_id":  "",
			},
		})
	case db.HandoffNoOp:
		// Bot already owns it (e.g., a sibling factory drop landed
		// first). Skip the broadcast and continue to the spawn so
		// the user's gesture still gets a run if one isn't already
		// underway.
	case db.HandoffRefused:
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: "task is claimed by another user; refusing to steal",
		})
		return
	}

	// Record the swipe_events audit row BEFORE the spawn attempt.
	// The audit captures the user's gesture (drag-to-bot), which is
	// real regardless of whether the run materializes. The earlier
	// "after-spawn-success only" placement meant partial-success
	// gestures (claim stamped, spawn failed) had no audit trail —
	// inconsistent with the task delegate route (which audits as soon
	// as the claim stamp accepts) and with the semantic that
	// claim is commitment regardless of run outcome. RecordSwipe
	// failure stays non-fatal because the claim col + WS broadcast
	// already captured the state-level effect; the audit is best-
	// effort.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Swipes.RecordSwipe(r.Context(), orgID, task.ID, "delegate", 0, nil)
		return e
	}); err != nil {
		factoryLog.Warn("failed to record delegate swipe", "task", task.ID, "error", err)
	}

	// Now attempt the spawn. Delegate's failure modes (blueprint not
	// found, DB error creating the conversation row) DON'T unstamp the
	// claim — the user's commitment is real, the conversation just didn't
	// fire.
	// task.ClaimedByAgentID mirrors the just-stamped claim for the shared
	// task object; the actor is passed explicitly so the conversation's
	// frozen blueprint_run actor matches it.
	task.ClaimedByAgentID = a.ID
	blueprintRunID, err := s.spawner.Delegate(*task, delegate.DelegateOpts{
		OrgID:               orgID,
		ExplicitBlueprintID: req.BlueprintID,
		TriggerType:         "manual",
		CreatorUserID:       userID,
		ActorAgentID:        a.ID,
	})
	if err != nil {
		// Claim is already stamped (and broadcast), swipe audit already
		// recorded — the commitment is real, the run just didn't fire. That
		// partial state is reported as the error it is (422 for a bad
		// blueprint reference, 500 for a spawn/DB fault — the same mapping
		// the task delegate route uses), never a 200. Reason SPAWN_FAILED is
		// what tells the FE the claim survived, so it refetches and renders
		// the "delegate didn't fire, retry" affordance on the bot-claimed
		// card instead of the plain failure toast.
		writeDelegateSpawnError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, factoryDelegateResponse{
		TaskID: task.ID,
		// TODO(TFAC-840): carries the blueprint_run id, not a conversation id —
		// a wire contract, so correcting it is that ticket's call.
		ConversationID: blueprintRunID,
		ClaimStamped:   true,
	})
}
