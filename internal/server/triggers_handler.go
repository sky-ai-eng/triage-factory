package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

func (s *Server) handleTriggersList(w http.ResponseWriter, r *http.Request) {
	triggers, err := db.ListPromptTriggers(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if triggers == nil {
		triggers = []domain.PromptTrigger{}
	}
	writeJSON(w, http.StatusOK, triggers)
}

func (s *Server) handleTriggerCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PromptID               string   `json:"prompt_id"`
		EventType              string   `json:"event_type"`
		ScopePredicateJSON     string   `json:"scope_predicate_json"`
		BreakerThreshold       int      `json:"breaker_threshold"`
		MinAutonomySuitability *float64 `json:"min_autonomy_suitability"` // pointer: absent → default 0.0
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.PromptID == "" || req.EventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt_id and event_type are required"})
		return
	}

	// Reject unregistered event types early — saves a downstream FK violation
	// and gives a clearer error to the client.
	if _, ok := events.Get(req.EventType); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown event_type: " + req.EventType})
		return
	}

	// Canonicalise the predicate. Empty / {} / null normalises to "" (match-all).
	canonicalPredicate, err := events.ValidatePredicateJSON(req.EventType, req.ScopePredicateJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Apply defaults
	if req.BreakerThreshold <= 0 {
		req.BreakerThreshold = 4
	}

	// Validate + default min_autonomy_suitability
	var minAutonomy float64
	if req.MinAutonomySuitability != nil {
		minAutonomy = *req.MinAutonomySuitability
		if minAutonomy < 0 || minAutonomy > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_autonomy_suitability must be between 0 and 1"})
			return
		}
	}

	// Validate prompt exists
	prompt, err := db.GetPrompt(s.db, req.PromptID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	trigger := domain.PromptTrigger{
		ID:                     uuid.New().String(),
		PromptID:               req.PromptID,
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              req.EventType,
		BreakerThreshold:       req.BreakerThreshold,
		MinAutonomySuitability: minAutonomy,
		Enabled:                false,
	}
	// Empty canonical string → match-all → NULL in DB.
	if canonicalPredicate != "" {
		trigger.ScopePredicateJSON = &canonicalPredicate
	}

	if err := db.SavePromptTrigger(s.db, trigger); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a trigger already exists for this prompt and event type"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Re-read to get server-set timestamps
	created, _ := db.GetPromptTrigger(s.db, trigger.ID)
	if created != nil {
		writeJSON(w, http.StatusCreated, created)
	} else {
		writeJSON(w, http.StatusCreated, trigger)
	}
}

// PUT /api/triggers/{id}
//
// Updates the mutable-config subset of an existing trigger: scope predicate,
// breaker threshold, autonomy gate. Deliberately does NOT accept prompt_id
// or event_type — changing those means the trigger is semantically a
// different thing (predicate schema is keyed on event_type, and the DB has
// a unique constraint on (prompt_id, event_type)), so delete+recreate is
// the right shape. `enabled` is owned by POST /api/triggers/{id}/toggle
// and is not accepted here either.
//
// All body fields are required (PUT replace semantics): clients must send
// the current values for fields they don't intend to change. Use a PATCH
// variant later if that becomes annoying.
func (s *Server) handleTriggerUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req struct {
		ScopePredicateJSON     string  `json:"scope_predicate_json"`
		BreakerThreshold       int     `json:"breaker_threshold"`
		MinAutonomySuitability float64 `json:"min_autonomy_suitability"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	existing, err := db.GetPromptTrigger(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "trigger not found"})
		return
	}

	if req.BreakerThreshold <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold must be positive"})
		return
	}
	if req.MinAutonomySuitability < 0 || req.MinAutonomySuitability > 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_autonomy_suitability must be between 0 and 1"})
		return
	}

	// Canonicalise the predicate against the existing event_type — the client
	// can't change event_type on an update, so we use the trigger's current
	// value (not anything from the request).
	canonicalPredicate, err := events.ValidatePredicateJSON(existing.EventType, req.ScopePredicateJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Mutate only the config-subset fields; keep identity + toggle state.
	updated := *existing
	updated.BreakerThreshold = req.BreakerThreshold
	updated.MinAutonomySuitability = req.MinAutonomySuitability
	if canonicalPredicate == "" {
		updated.ScopePredicateJSON = nil
	} else {
		updated.ScopePredicateJSON = &canonicalPredicate
	}

	if err := db.SavePromptTrigger(s.db, updated); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Re-read so the response reflects the updated_at timestamp the DB set.
	fresh, _ := db.GetPromptTrigger(s.db, id)
	if fresh != nil {
		writeJSON(w, http.StatusOK, fresh)
	} else {
		writeJSON(w, http.StatusOK, updated)
	}
}

func (s *Server) handleTriggerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := db.DeletePromptTrigger(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleTriggerToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Verify the trigger exists before updating
	existing, err := db.GetPromptTrigger(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "trigger not found"})
		return
	}

	if err := db.SetTriggerEnabled(s.db, id, req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": req.Enabled})
}
