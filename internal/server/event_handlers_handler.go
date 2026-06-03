package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// /api/event-handlers — unified successor to /api/task-rules + /api/triggers
// (SKY-259). The two frontend pages (rules tab + triggers tab) keep their
// split UX but hit this one endpoint family with a kind filter.
//
// Wire shape:
//
//   GET    /api/event-handlers[?kind=rule|trigger]   — list
//   POST   /api/event-handlers                        — create (kind in body)
//   PATCH  /api/event-handlers/{id}                   — partial update
//   PUT    /api/event-handlers/{id}                   — replacement update
//                                                       (alias for PATCH for
//                                                       trigger-style "send
//                                                       the full mutable set"
//                                                       calls — same handler)
//   DELETE /api/event-handlers/{id}                   — hard delete; system
//                                                       rows soft-disable
//   POST   /api/event-handlers/{id}/toggle            — flip enabled bit
//   POST   /api/event-handlers/{id}/promote           — rule → trigger
//   PUT    /api/event-handlers/reorder                — rules-only sort_order

// GET /api/event-handlers
func (s *Server) handleEventHandlersList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != domain.EventHandlerKindRule && kind != domain.EventHandlerKindTrigger {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "kind must be 'rule', 'trigger', or omitted (returns both)",
		})
		return
	}
	// ?team_id= narrows to one team's handlers (+ org-visible) on the
	// multi-team prompts page; absent/solo returns everything visible.
	teamID := singleTeamParam(r)
	var handlers []domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handlers, e = tx.EventHandlers.List(r.Context(), orgID, kind, teamID)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if handlers == nil {
		handlers = []domain.EventHandler{}
	}
	writeJSON(w, http.StatusOK, handlers)
}

// POST /api/event-handlers
//
// kind in body; per-kind fields are validated accordingly.
//
//   - kind='rule':    name + event_type are required; default_priority
//     defaults to 0.5 and sort_order to 0 when omitted.
//     enabled defaults to true.
//   - kind='trigger': blueprint_id + event_type are required;
//     breaker_threshold defaults to 4,
//     min_autonomy_suitability to 0.0, enabled to false
//     when omitted. The defaults are load-bearing for
//     drag-to-create paths in the prompts UI that supply
//     only the minimum identifying fields — match the
//     pre-SKY-259 /api/triggers behavior.
type createEventHandlerRequest struct {
	Kind               string `json:"kind"`
	EventType          string `json:"event_type"`
	ScopePredicateJSON string `json:"scope_predicate_json"`
	Enabled            *bool  `json:"enabled"`

	// Rule-only.
	Name            *string  `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`

	// Trigger-only.
	BlueprintID            string   `json:"blueprint_id"`
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`

	// TeamID is the acting team the write picker supplied — the team
	// this rule/trigger is created under. Required in the UI when the
	// caller belongs to ≥2 teams; empty (sole-team fallback) otherwise.
	TeamID string `json:"team_id"`
}

func (s *Server) handleEventHandlerCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var req createEventHandlerRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Kind != domain.EventHandlerKindRule && req.Kind != domain.EventHandlerKindTrigger {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "kind must be 'rule' or 'trigger'",
		})
		return
	}
	if req.EventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event_type is required"})
		return
	}
	if _, ok := events.Get(req.EventType); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown event_type: " + req.EventType})
		return
	}
	canonical, err := events.ValidatePredicateJSON(req.EventType, req.ScopePredicateJSON)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	h := domain.EventHandler{
		ID:        uuid.New().String(),
		Kind:      req.Kind,
		EventType: req.EventType,
		Source:    domain.EventHandlerSourceUser,
	}
	if canonical != "" {
		h.ScopePredicateJSON = &canonical
	}

	switch req.Kind {
	case domain.EventHandlerKindRule:
		if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required for kind=rule"})
			return
		}
		h.Name = strings.TrimSpace(*req.Name)
		priority := 0.5
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_priority must be between 0 and 1"})
				return
			}
			priority = *req.DefaultPriority
		}
		h.DefaultPriority = &priority
		sortOrder := 0
		if req.SortOrder != nil {
			sortOrder = *req.SortOrder
		}
		h.SortOrder = &sortOrder
		h.Enabled = true
		if req.Enabled != nil {
			h.Enabled = *req.Enabled
		}

	case domain.EventHandlerKindTrigger:
		if req.BlueprintID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "blueprint_id is required for kind=trigger"})
			return
		}
		// Verify the blueprint exists (clearer 404 than the downstream FK
		// integrity error) AND that the acting team owns it. A trigger may
		// only fire a blueprint its own team owns: the DB enforces this via
		// the (blueprint_id, team_id) composite FK, but we resolve the acting
		// team here and pre-check for a clean 400 instead of a generic
		// constraint error. Both lookups share one tx so the blueprint's team
		// and the resolved acting team are read under the same claims.
		var blueprint *domain.Blueprint
		var crossTeamBlueprint bool
		var blueprintHasTrigger bool
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			blueprint, e = tx.Blueprints.Get(r.Context(), orgID, req.BlueprintID)
			if e != nil || blueprint == nil {
				return e
			}
			teamID, e := resolveActingTeam(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
			if e != nil {
				return e
			}
			if blueprint.TeamID != "" && blueprint.TeamID != teamID {
				crossTeamBlueprint = true
			}
			// A blueprint is fired by exactly one event: pre-check for a clean
			// 409 instead of letting the partial-unique index surface a raw 500.
			existing, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.BlueprintID)
			if e != nil {
				return e
			}
			if len(existing) > 0 {
				blueprintHasTrigger = true
			}
			return nil
		}); err != nil {
			if writeIfActingTeamError(w, err) {
				return
			}
			internalError(w, "event_handlers", err)
			return
		}
		if blueprint == nil {
			notFound(w, "blueprint")
			return
		}
		if crossTeamBlueprint {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "blueprint_id references a blueprint owned by another team"})
			return
		}
		if blueprintHasTrigger {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
			return
		}
		h.BlueprintID = req.BlueprintID
		h.TriggerType = domain.TriggerTypeEvent
		threshold := 4
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold must be positive"})
				return
			}
			threshold = *req.BreakerThreshold
		}
		h.BreakerThreshold = &threshold
		minAutonomy := 0.0
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_autonomy_suitability must be between 0 and 1"})
				return
			}
			minAutonomy = *req.MinAutonomySuitability
		}
		h.MinAutonomySuitability = &minAutonomy
		// Triggers default disabled (project convention) — explicit
		// opt-in via Enabled=true survives the default.
		h.Enabled = false
		if req.Enabled != nil {
			h.Enabled = *req.Enabled
		}
	}

	var fresh *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := resolveActingTeam(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		if e := tx.EventHandlers.Create(r.Context(), orgID, teamID, h); e != nil {
			return e
		}
		var ge error
		fresh, ge = tx.EventHandlers.Get(r.Context(), orgID, h.ID)
		return ge
	}); err != nil {
		if writeIfActingTeamError(w, err) {
			return
		}
		// Driver-specific error strings ("UNIQUE constraint failed: ..." on
		// SQLite, "duplicate key value violates unique constraint ..." on
		// Postgres) leak schema names + index identifiers to clients.
		// Sniff the substrings to classify but return a generic message;
		// log the raw error for operators.
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
			log.Printf("[event_handlers] create conflict (kind=%s, event_type=%s): %v", h.Kind, h.EventType, err)
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "An event handler with this configuration already exists.",
			})
			return
		}
		log.Printf("[event_handlers] create failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Failed to create event handler.",
		})
		return
	}
	if fresh != nil {
		writeJSON(w, http.StatusCreated, fresh)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

// PATCH /api/event-handlers/{id} (also bound to PUT for trigger-style replace)
//
// Partial update. Any field left nil/absent is unchanged. kind and
// event_type are immutable (kind transitions go through /promote;
// event_type changes would invalidate the predicate schema). For
// triggers, blueprint_id is also immutable here.
type patchEventHandlerRequest struct {
	ScopePredicateJSON json.RawMessage `json:"scope_predicate_json"`
	Enabled            *bool           `json:"enabled"`

	// Rule fields.
	Name            *string  `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`

	// Trigger fields.
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
}

func (s *Server) handleEventHandlerUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req patchEventHandlerRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	var existing *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if existing == nil {
		notFound(w, "event handler")
		return
	}

	updated := *existing

	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}

	// Predicate update — three distinguishable cases:
	//   - absent (len==0):         leave unchanged
	//   - explicit null ("null"):  clear to match-all
	//   - JSON string / object:    validate + canonicalise
	if len(req.ScopePredicateJSON) > 0 {
		raw := string(req.ScopePredicateJSON)
		if raw == "null" {
			updated.ScopePredicateJSON = nil
		} else {
			var asString string
			if err := json.Unmarshal(req.ScopePredicateJSON, &asString); err == nil {
				raw = asString
			}
			canonical, err := events.ValidatePredicateJSON(existing.EventType, raw)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			if canonical == "" {
				updated.ScopePredicateJSON = nil
			} else {
				updated.ScopePredicateJSON = &canonical
			}
		}
	}

	switch existing.Kind {
	case domain.EventHandlerKindRule:
		if req.Name != nil {
			trimmed := strings.TrimSpace(*req.Name)
			if trimmed == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
				return
			}
			updated.Name = trimmed
		}
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "default_priority must be between 0 and 1"})
				return
			}
			v := *req.DefaultPriority
			updated.DefaultPriority = &v
		}
		if req.SortOrder != nil {
			v := *req.SortOrder
			updated.SortOrder = &v
		}

	case domain.EventHandlerKindTrigger:
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold must be positive"})
				return
			}
			v := *req.BreakerThreshold
			updated.BreakerThreshold = &v
		}
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_autonomy_suitability must be between 0 and 1"})
				return
			}
			v := *req.MinAutonomySuitability
			updated.MinAutonomySuitability = &v
		}
	}

	var fresh *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.EventHandlers.Update(r.Context(), orgID, updated); e != nil {
			return e
		}
		var ge error
		fresh, ge = tx.EventHandlers.Get(r.Context(), orgID, id)
		return ge
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if fresh != nil {
		writeJSON(w, http.StatusOK, fresh)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// DELETE /api/event-handlers/{id}
//
// User rows hard-delete; system rows soft-disable in place (Seed runs
// on every boot with INSERT-OR-IGNORE, so a hard delete would
// resurrect the row).
func (s *Server) handleEventHandlerDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var existing *domain.EventHandler
	var disabled bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if existing == nil {
			return nil
		}
		if existing.Source == domain.EventHandlerSourceSystem {
			if e := tx.EventHandlers.SetEnabled(r.Context(), orgID, id, false); e != nil {
				return e
			}
			disabled = true
			return nil
		}
		return tx.EventHandlers.Delete(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if existing == nil {
		notFound(w, "event handler")
		return
	}
	if disabled {
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "disabled",
			"reason": "system handlers cannot be deleted (they would be resurrected on next boot); disabled instead",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/event-handlers/{id}/toggle
func (s *Server) handleEventHandlerToggle(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	var existing *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if existing == nil {
			return nil
		}
		return tx.EventHandlers.SetEnabled(r.Context(), orgID, id, req.Enabled)
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if existing == nil {
		notFound(w, "event handler")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": req.Enabled})
}

// POST /api/event-handlers/{id}/promote
//
// Rule → trigger transition. Body carries the trigger-side fields the
// promoted row needs (blueprint_id, breaker_threshold,
// min_autonomy_suitability, optionally a new predicate). The store enforces
// atomicity via a single UPDATE that flips kind and populates the trigger
// fields together.
type promoteEventHandlerRequest struct {
	BlueprintID            string   `json:"blueprint_id"`
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
	ScopePredicateJSON     *string  `json:"scope_predicate_json"`
}

func (s *Server) handleEventHandlerPromote(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	var req promoteEventHandlerRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.BlueprintID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "blueprint_id is required"})
		return
	}
	if req.BreakerThreshold == nil || req.MinAutonomySuitability == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "breaker_threshold and min_autonomy_suitability are required"})
		return
	}

	var existing *domain.EventHandler
	var blueprint *domain.Blueprint
	var blueprintHasTrigger bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if existing == nil {
			return nil
		}
		if existing.Kind != domain.EventHandlerKindRule {
			return nil
		}
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, req.BlueprintID)
		if e != nil {
			return e
		}
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.BlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			blueprintHasTrigger = true
		}
		return nil
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if existing == nil {
		notFound(w, "event handler")
		return
	}
	if existing.Kind != domain.EventHandlerKindRule {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "only rules can be promoted"})
		return
	}
	if blueprint == nil {
		notFound(w, "blueprint")
		return
	}
	// Same-team guard: a promoted trigger may only fire a blueprint the rule's
	// own team owns. The DB enforces this via the (blueprint_id, team_id)
	// composite FK on the Promote UPDATE; pre-check for a clean 400.
	if existing.TeamID != "" && blueprint.TeamID != "" && blueprint.TeamID != existing.TeamID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "blueprint_id references a blueprint owned by another team"})
		return
	}
	// A blueprint is fired by exactly one event: refuse promoting a second
	// trigger onto an already-triggered blueprint with a clean 409.
	if blueprintHasTrigger {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
		return
	}

	predicate := existing.ScopePredicateJSON
	if req.ScopePredicateJSON != nil {
		canonical, verr := events.ValidatePredicateJSON(existing.EventType, *req.ScopePredicateJSON)
		if verr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": verr.Error()})
			return
		}
		if canonical == "" {
			predicate = nil
		} else {
			predicate = &canonical
		}
	}

	target := domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            req.BlueprintID,
		BreakerThreshold:       req.BreakerThreshold,
		MinAutonomySuitability: req.MinAutonomySuitability,
		ScopePredicateJSON:     predicate,
	}
	var fresh *domain.EventHandler
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.EventHandlers.Promote(r.Context(), orgID, id, target); e != nil {
			return e
		}
		var ge error
		fresh, ge = tx.EventHandlers.Get(r.Context(), orgID, id)
		return ge
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

// PUT /api/event-handlers/reorder
//
// Rules-only — trigger IDs in the list are silently skipped by the
// store (sort_order is rule-only by CHECK constraint).
func (s *Server) handleEventHandlerReorder(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var ids []string
	if !decodeJSON(w, r, &ids, "expected array of handler IDs") {
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty ID list"})
		return
	}
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.EventHandlers.Reorder(r.Context(), orgID, ids)
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}
