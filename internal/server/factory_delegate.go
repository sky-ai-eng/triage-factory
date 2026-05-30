package server

import (
	"log"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// factoryDelegateRequest is the body for POST /api/factory/delegate.
// All four fields are required; dedup_key may be empty for non-
// discriminator event types (the common case).
type factoryDelegateRequest struct {
	EntityID  string `json:"entity_id"`
	EventType string `json:"event_type"`
	DedupKey  string `json:"dedup_key"`
	PromptID  string `json:"prompt_id"`
}

// factoryDelegateResponse mirrors the /api/tasks/{id}/swipe delegate
// response: on partial success (claim stamped, run didn't fire),
// DelegateError carries the spawner error and RunID stays empty. The
// FE renders the "delegate failed — retry" affordance on the bot-
// claimed card regardless of whether the failure was a 400-class
// (ErrPromptNotFound) or 500-class (DB / spawner internal) error.
// ClaimStamped is always true on a 200 response — the user's gesture
// committed at the claim axis even when the run didn't materialize.
type factoryDelegateResponse struct {
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	DelegateError string `json:"delegate_error,omitempty"`
	ClaimStamped  bool   `json:"claim_stamped"`
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
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.EntityID == "" || req.EventType == "" || req.PromptID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity_id, event_type, and prompt_id are required"})
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
		writeJSON(w, http.StatusConflict, map[string]string{"error": "entity is closed; cannot delegate"})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no matching event for entity at this station"})
		return
	}

	// Spawner availability gate runs after every request + state
	// validation (400/404/409) so callers learn about bad input
	// before they hit the infrastructure gap. Sits before the
	// FindOrCreateTask + RecordSwipe writes so a missing spawner
	// can't leave a half-applied delegate (task + swipe row but no
	// run). Tests rely on this ordering to exercise the 404/409/400
	// paths without installing a spawner.
	if s.spawner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "spawner not configured"})
		return
	}

	// Default priority — mirrors internal/routing/router.go:210-215,
	// including the predicate-match filter. Iterating *all* enabled
	// rules for the event type would inflate priority whenever a
	// high-priority rule's scope_predicate doesn't actually match
	// this event's metadata (e.g., a 0.9-priority rule scoped to a
	// specific repo would lift priority for every event of that type
	// even on unrelated entities). Empty predJSON always matches per
	// the events package contract.
	defaultPriority := 0.5
	schema, schemaOK := events.Get(req.EventType)
	var handlers []domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handlers, e = tx.EventHandlers.GetEnabledForEvent(r.Context(), orgID, req.EventType)
		return e
	}); err != nil {
		internalError(w, "factory", err)
		return
	}
	for _, h := range handlers {
		if h.Kind != domain.EventHandlerKindRule || h.DefaultPriority == nil {
			// Trigger rows have no DefaultPriority; skip.
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
		matched, err := schema.Match(predJSON, primaryEvent.MetadataJSON)
		if err != nil {
			log.Printf("[factory] event_handler %s predicate error: %v", h.ID, err)
			continue
		}
		if matched && *h.DefaultPriority > defaultPriority {
			defaultPriority = *h.DefaultPriority
		}
	}

	var task *domain.Task
	var created bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := resolveActingTeam(r.Context(), tx.Teams, orgID)
		if e != nil {
			return e
		}
		task, created, e = tx.Tasks.FindOrCreate(r.Context(), orgID, teamID, req.EntityID, req.EventType, req.DedupKey, primaryEvent.ID, defaultPriority)
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
				log.Printf("[factory] failed to record spawned task_event for %s: %v", task.ID, recErr)
			}
		}
		return nil
	}); err != nil {
		internalError(w, "factory", err)
		return
	}

	// SKY-261 B+ alignment with swipe-delegate: the user's gesture is
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
	// SKY-261 acceptance: re-check team_agents.enabled at gesture
	// time. Factory drag-to-bot is the same semantic as swipe-up
	// "delegate to bot" — both refuse with 409 when the bot is off
	// for this team.
	a, enabled, err := s.agentEnabledForOrg(r.Context(), orgID, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delegate failed: " + err.Error()})
		return
	}
	if !enabled {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "bot is disabled for this team; enable it in team settings to delegate",
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim stamp failed: " + err.Error()})
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
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "task is claimed by another user; refusing to steal",
		})
		return
	}

	// Record the swipe_events audit row BEFORE the spawn attempt.
	// The audit captures the user's gesture (drag-to-bot), which is
	// real regardless of whether the run materializes. The earlier
	// "after-spawn-success only" placement meant partial-success
	// gestures (claim stamped, spawn failed) had no audit trail —
	// inconsistent with swipe-delegate (which audits at the top of
	// the swipe handler) and with the SKY-261 B+ semantic that
	// claim is commitment regardless of run outcome. RecordSwipe
	// failure stays non-fatal because the claim col + WS broadcast
	// already captured the state-level effect; the audit is best-
	// effort.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Swipes.RecordSwipe(r.Context(), orgID, task.ID, "delegate", 0)
		return e
	}); err != nil {
		log.Printf("[factory] failed to record delegate swipe for task %s: %v", task.ID, err)
	}

	// Now attempt the spawn. Delegate's failure modes (prompt not
	// found, DB error creating the run row) DON'T unstamp the claim
	// — the user's commitment is real, the run just didn't fire.
	// The response carries delegate_error so the FE can render the
	// "delegate failed — retry" affordance on the now-bot-claimed card.
	// task.ClaimedByAgentID is set so spawner.Delegate's actor stamping
	// reads it correctly.
	task.ClaimedByAgentID = a.ID
	runID, err := s.spawner.Delegate(*task, delegate.DelegateOpts{
		OrgID:            orgID,
		ExplicitPromptID: req.PromptID,
		TriggerType:      "manual",
		CreatorUserID:    userID,
	})
	if err != nil {
		// Claim is already stamped (and broadcast), swipe audit
		// already recorded. The run didn't fire — mirror the
		// swipe-delegate convention: 200 OK with delegate_error
		// populated and run_id empty. The FE reads delegate_error
		// to render the "delegate didn't fire, retry" affordance
		// on the now-bot-claimed card; refetch still fires so the
		// Board picks up the new claim col + the task surfaces in
		// the Agent lane immediately. Whether the underlying error
		// was a 400-class (ErrPromptNotFound, ErrPromptUnspecified)
		// or 500-class (DB, spawner internal) is irrelevant to the
		// caller — the response shape is the same.
		writeJSON(w, http.StatusOK, factoryDelegateResponse{
			TaskID:        task.ID,
			RunID:         "",
			DelegateError: err.Error(),
			ClaimStamped:  true,
		})
		return
	}

	writeJSON(w, http.StatusOK, factoryDelegateResponse{
		TaskID:       task.ID,
		RunID:        runID,
		ClaimStamped: true,
	})
}
