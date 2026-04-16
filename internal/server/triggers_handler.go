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
		PromptID           string `json:"prompt_id"`
		EventType          string `json:"event_type"`
		ScopePredicateJSON string `json:"scope_predicate_json"`
		BreakerThreshold   int    `json:"breaker_threshold"`
		CooldownSeconds    int    `json:"cooldown_seconds"`
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
	if req.CooldownSeconds <= 0 {
		req.CooldownSeconds = 60
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
		ID:               uuid.New().String(),
		PromptID:         req.PromptID,
		TriggerType:      domain.TriggerTypeEvent,
		EventType:        req.EventType,
		BreakerThreshold: req.BreakerThreshold,
		CooldownSeconds:  req.CooldownSeconds,
		Enabled:          true,
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
