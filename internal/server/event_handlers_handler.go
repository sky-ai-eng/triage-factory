package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// eventHandlersHandler serves the event-handler (rule + trigger) config
// endpoints. It holds the transactional store runner the handlers use;
// routes() registers its methods through the api()/apiMutating() wrappers.
type eventHandlersHandler struct {
	tx db.TxRunner
	az *authz.Checker
}

// gateHandlerWrite rejects a viewer before a write against an existing event
// handler (TFAC-447), pre-loading the row to resolve its team. Returns false
// (caller should return — a response was written) for a viewer (403) or a load
// failure (500). A missing handler passes through (returns true) so the
// handler's own nil-check renders the 404 — never reached for an absent row.
// The same shape as gateBlueprintWrite.
func (eh *eventHandlersHandler) gateHandlerWrite(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var existing *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return false
	}
	if existing == nil {
		return true // let the handler's own nil-check render the 404
	}
	return eh.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID)
}

// /api/event-handlers — unified successor to /api/task-rules + /api/triggers.
// The two frontend pages (rules tab + triggers tab) keep their
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
//   DELETE /api/event-handlers/{id}                   — delete (soft for shipped rows)
//   POST   /api/event-handlers/{id}/toggle            — flip enabled bit
//   POST   /api/event-handlers/{id}/promote           — rule → trigger
//   PUT    /api/event-handlers/reorder                — rules-only sort_order

// eventHandlerListRequest is the body of POST /api/event-handlers/list. Kind
// and TeamID are the old ?kind= / ?team_id= params, unchanged in meaning and
// now validated strictly — a corrupt team filter used to be dropped, which
// widened the read to every team.
type eventHandlerListRequest struct {
	// Kind is "rule", "trigger", or omitted (both).
	Kind   string `json:"kind"`
	TeamID string `json:"team_id"`

	httpx.PageRequest
}

type eventHandlerListFilterKey struct {
	Kind   string `json:"kind"`
	TeamID string `json:"team_id"`
}

// POST /api/event-handlers/list
func (eh *eventHandlersHandler) handleEventHandlersList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req eventHandlerListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.Kind != "" && req.Kind != domain.EventHandlerKindRule && req.Kind != domain.EventHandlerKindTrigger {
		v.Invalid("kind", "kind must be 'rule', 'trigger', or omitted (returns both)")
	}
	teamIDFilterField(&v, req.TeamID)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(eventHandlerListFilterKey{
		Kind: req.Kind, TeamID: req.TeamID,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The entitlement gate (TFAC-524) hides a handler bound to an event type
	// this org isn't licensed for. It rides into the query as a filter, not a
	// post-read trim, so the page and total_count agree about what the caller
	// can see.
	filter := db.EventHandlerListFilter{Kind: req.Kind, TeamID: req.TeamID, GatedEventTypes: gatedEventTypes(orgID)}

	var (
		handlers []domain.EventHandler
		total    int
	)
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handlers, total, e = tx.EventHandlers.List(r.Context(), orgID, filter, db.ListOpts{Limit: page.Limit, Offset: page.Offset})
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	httpx.WriteList(w, page, handlers, total)
}

// handleEventHandlerGet is the canonical single read, in the list's row shape.
// Before it, five mutation routes each preloaded the row internally and no
// route would hand it back — a client could edit a handler it had no way to
// read.
//
// Like the blueprint single read, it is deliberately NOT entitlement-gated the
// way the list is. The gate is a visibility rule for a BROWSE surface: it keeps
// a handler bound to an unlicensed event type out of a list nobody asked for it
// by name. A caller who already holds the id has it from a run, a trigger, or a
// link, and the five mutation routes on that same id are ungated — so gating
// only the read would make a handler editable but unreadable, which is the
// exact asymmetry this route was added to close.
//
// GET /api/event-handlers/{id}
func (eh *eventHandlersHandler) handleEventHandlerGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "event handler")
	if !ok {
		return
	}

	var handler *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handler, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if handler == nil {
		notFound(w, "event handler")
		return
	}
	writeJSON(w, http.StatusOK, handler)
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
//     the original /api/triggers behavior.
type createEventHandlerRequest struct {
	Kind               string `json:"kind"`
	EventType          string `json:"event_type"`
	ScopePredicateJSON string `json:"scope_predicate_json"`
	Enabled            *bool  `json:"enabled"`

	// AppliesToUnowned opts the rule into reaching entities the team doesn't
	// own (TFAC-517) — the explicit "watch" flag, default off. Visibility
	// only; never confers firing rights (the TFAC-514 invariant).
	AppliesToUnowned *bool `json:"applies_to_unowned"`

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

func (eh *eventHandlersHandler) handleEventHandlerCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var req createEventHandlerRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Kind != domain.EventHandlerKindRule && req.Kind != domain.EventHandlerKindTrigger {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "kind must be 'rule' or 'trigger'", Field: "kind"})
		return
	}
	if req.EventType == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "event_type is required", Field: "event_type"})
		return
	}
	if _, ok := events.Get(req.EventType); !ok {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "unknown event_type: " + req.EventType, Field: "event_type"})
		return
	}
	// TFAC-524: a gated-off event source can't be handed a new handler. Only
	// create needs this — event_type is immutable on update, and toggle/
	// promote/retarget can't change it either (the router freeze in Part 6
	// makes an enabled-but-gated handler inert regardless).
	if !entitlements.EventTypeAllowed(orgID, req.EventType) {
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{Reason: httpx.ReasonForbidden, Message: "event source not enabled for this organization", Field: "event_type"})
		return
	}
	canonical, err := events.ValidatePredicateJSON(req.EventType, req.ScopePredicateJSON)
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: "scope_predicate_json"})
		return
	}

	// Reject viewers before authoring a rule/trigger (TFAC-447): resolve the
	// acting team read-only and gate, so a viewer gets a clean 403 rather than
	// the event_handlers_insert RLS WITH CHECK surfacing as a 500.
	if !gateActingTeamWrite(w, r, eh.tx, eh.az, orgID, userID, req.TeamID, "event_handlers") {
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
	// applies_to_unowned is a routing-scope flag on both kinds; defaults off.
	if req.AppliesToUnowned != nil {
		h.AppliesToUnowned = *req.AppliesToUnowned
	}

	switch req.Kind {
	case domain.EventHandlerKindRule:
		if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "name is required for kind=rule", Field: "name"})
			return
		}
		h.Name = strings.TrimSpace(*req.Name)
		priority := 0.5
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "default_priority must be between 0 and 1", Field: "default_priority"})
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
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "blueprint_id is required for kind=trigger", Field: "blueprint_id"})
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
		if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			blueprint, e = tx.Blueprints.Get(r.Context(), orgID, req.BlueprintID)
			if e != nil || blueprint == nil {
				return e
			}
			teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
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
			if teamscope.WriteIfSelectionError(w, err) {
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
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonCrossTeamRef, Message: "blueprint_id references a blueprint owned by another team", Field: "blueprint_id"})
			return
		}
		if blueprintHasTrigger {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
			return
		}
		h.BlueprintID = req.BlueprintID
		h.TriggerType = domain.TriggerTypeEvent
		threshold := 4
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "breaker_threshold must be positive", Field: "breaker_threshold"})
				return
			}
			threshold = *req.BreakerThreshold
		}
		h.BreakerThreshold = &threshold
		minAutonomy := 0.0
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "min_autonomy_suitability must be between 0 and 1", Field: "min_autonomy_suitability"})
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

	var fresh domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		fresh, e = tx.EventHandlers.Create(r.Context(), orgID, teamID, h)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		// A unique-constraint failure leaks schema/index names; translate to a
		// generic 409 and log the raw error for operators.
		if isUniqueViolation(err) {
			eventHandlersLog.Warn("create conflict", "kind", h.Kind, "event_type", h.EventType, "error", err)
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonDuplicateName, Message: "an event handler with this configuration already exists"})
			return
		}
		eventHandlersLog.Error("create failed", "error", err)
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusCreated, fresh)
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

	// AppliesToUnowned toggles the watch-scope flag (TFAC-517); absent leaves
	// it unchanged.
	AppliesToUnowned *bool `json:"applies_to_unowned"`

	// Rule fields.
	Name            *string  `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`

	// Trigger fields.
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
}

func (eh *eventHandlersHandler) handleEventHandlerUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "event handler")
	if !ok {
		return
	}

	var req patchEventHandlerRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	var existing *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	// Viewers can't edit a rule/trigger (TFAC-447). Gate on the loaded row's
	// team — no extra round-trip.
	if !eh.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID) {
		return
	}

	updated := *existing

	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	if req.AppliesToUnowned != nil {
		updated.AppliesToUnowned = *req.AppliesToUnowned
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
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: err.Error(), Field: "scope_predicate_json"})
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
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "name cannot be empty", Field: "name"})
				return
			}
			updated.Name = trimmed
		}
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "default_priority must be between 0 and 1", Field: "default_priority"})
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
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "breaker_threshold must be positive", Field: "breaker_threshold"})
				return
			}
			v := *req.BreakerThreshold
			updated.BreakerThreshold = &v
		}
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "min_autonomy_suitability must be between 0 and 1", Field: "min_autonomy_suitability"})
				return
			}
			v := *req.MinAutonomySuitability
			updated.MinAutonomySuitability = &v
		}
	}

	var fresh domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		fresh, e = tx.EventHandlers.Update(r.Context(), orgID, updated)
		return e
	}); err != nil {
		// The read above resolved the handler, so a miss here is the row going
		// away between the two transactions. It used to answer 200 with the
		// merged struct — a response describing a write that did not land.
		if errors.Is(err, db.ErrNoSuchEventHandler) {
			notFound(w, "event handler")
			return
		}
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

// DELETE /api/event-handlers/{id}
//
// A shipped copy (system_slug set) soft-deletes: EventHandlers.Delete stamps
// deleted_at, leaving the (org_id, team_id, system_slug) slot occupied so the
// boot-time shipped-content sync never resurrects it. A user-created handler
// (no system_slug) hard-deletes. Every read path filters deleted_at IS NULL,
// so the two are indistinguishable to callers other than the sync itself.
func (eh *eventHandlersHandler) handleEventHandlerDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "event handler")
	if !ok {
		return
	}

	// Viewers can't delete a rule/trigger (TFAC-447). A missing handler passes
	// through to the 404 below.
	if !eh.gateHandlerWrite(w, r, orgID, userID, id) {
		return
	}

	var existing *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if existing == nil {
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/event-handlers/{id}/toggle
func (eh *eventHandlersHandler) handleEventHandlerToggle(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "event handler")
	if !ok {
		return
	}
	// Enabled is a pointer so an absent field stays distinguishable from an
	// explicit false: a non-pointer bool zero-values an empty body into a
	// disable, silently turning the handler off with a 200.
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		badRequest(w, "enabled is required")
		return
	}
	enabled := *req.Enabled
	// Viewers can't enable/disable a rule/trigger (TFAC-447).
	if !eh.gateHandlerWrite(w, r, orgID, userID, id) {
		return
	}
	var toggled domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		toggled, e = tx.EventHandlers.SetEnabled(r.Context(), orgID, id, enabled)
		return e
	}); err != nil {
		if errors.Is(err, db.ErrNoSuchEventHandler) {
			notFound(w, "event handler")
			return
		}
		internalError(w, "event_handlers", err)
		return
	}
	// The handler itself, not {id, enabled}: the write knows the row, and a
	// client that has to re-fetch to see the rest of it is the reason the
	// status stub was a bug rather than a shortcut.
	writeJSON(w, http.StatusOK, toggled)
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

func (eh *eventHandlersHandler) handleEventHandlerPromote(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "event handler")
	if !ok {
		return
	}

	var req promoteEventHandlerRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.BlueprintID == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "blueprint_id is required", Field: "blueprint_id"})
		return
	}
	var v httpx.Validation
	if req.BreakerThreshold == nil {
		v.Missing("breaker_threshold")
	} else if *req.BreakerThreshold <= 0 {
		// The range checks create and PATCH enforce. Promote skipped them, so
		// it could persist breaker_threshold:-1 / min_autonomy_suitability:7 —
		// values the same fields reject through every other door.
		v.OutOfRange("breaker_threshold", "breaker_threshold must be positive")
	}
	if req.MinAutonomySuitability == nil {
		v.Missing("min_autonomy_suitability")
	} else if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
		v.OutOfRange("min_autonomy_suitability", "min_autonomy_suitability must be between 0 and 1")
	}
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	// Viewers can't promote a rule to a trigger (TFAC-447).
	if !eh.gateHandlerWrite(w, r, orgID, userID, id) {
		return
	}

	var existing *domain.EventHandler
	var blueprint *domain.Blueprint
	var blueprintHasTrigger bool
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "only rules can be promoted"})
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
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonCrossTeamRef, Message: "blueprint_id references a blueprint owned by another team", Field: "blueprint_id"})
		return
	}
	// A blueprint is fired by exactly one event: refuse promoting a second
	// trigger onto an already-triggered blueprint with a clean 409.
	if blueprintHasTrigger {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
		return
	}

	predicate := existing.ScopePredicateJSON
	if req.ScopePredicateJSON != nil {
		canonical, verr := events.ValidatePredicateJSON(existing.EventType, *req.ScopePredicateJSON)
		if verr != nil {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: verr.Error(), Field: "scope_predicate_json"})
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
	var fresh domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		fresh, e = tx.EventHandlers.Promote(r.Context(), orgID, id, target)
		return e
	}); err != nil {
		// The blueprintHasTrigger pre-check and this Promote run in separate
		// transactions, so a concurrent promote onto the same blueprint can pass
		// the check window and hit the partial-unique index here — surface a
		// clean 409, not a raw 500.
		if isUniqueViolation(err) {
			eventHandlersLog.Warn("promote conflict, blueprint already triggered", "error", err)
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
			return
		}
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

// POST /api/event-handlers/{id}/retarget
//
// Re-points a trigger at a different blueprint — the canvas gesture of dragging
// a trigger arrow's head off one blueprint's entry and onto another's. A single
// UPDATE of blueprint_id (RetargetBlueprint) preserves the handler row and its
// id, so run history and the event-trigger fence survive the move. blueprint_id
// in the body is the new target; it must exist, be same-team, and be
// trigger-less (a blueprint is fired by exactly one event).
type retargetEventHandlerRequest struct {
	BlueprintID string `json:"blueprint_id"`
}

func (eh *eventHandlersHandler) handleEventHandlerRetarget(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := uuidPathOr404(w, r, "id", "event handler")
	if !ok {
		return
	}

	var req retargetEventHandlerRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.BlueprintID == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "blueprint_id is required", Field: "blueprint_id"})
		return
	}

	// Viewers can't retarget a trigger (TFAC-447).
	if !eh.gateHandlerWrite(w, r, orgID, userID, id) {
		return
	}

	var (
		existing   *domain.EventHandler
		blueprint  *domain.Blueprint
		fresh      *domain.EventHandler
		notTrigger bool
		crossTeam  bool
		hasTrigger bool
		noChange   bool
	)
	// Validation + the UPDATE share one tx, so the "blueprint already triggered"
	// check and the write see the same snapshot — no promote-style race window.
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		if e != nil || existing == nil {
			return e
		}
		if existing.Kind != domain.EventHandlerKindTrigger {
			notTrigger = true
			return nil
		}
		if existing.BlueprintID == req.BlueprintID {
			noChange = true
			return nil
		}
		blueprint, e = tx.Blueprints.Get(r.Context(), orgID, req.BlueprintID)
		if e != nil || blueprint == nil {
			return e
		}
		if existing.TeamID != "" && blueprint.TeamID != "" && blueprint.TeamID != existing.TeamID {
			crossTeam = true
			return nil
		}
		triggers, e := tx.EventHandlers.ListForBlueprint(r.Context(), orgID, req.BlueprintID)
		if e != nil {
			return e
		}
		if len(triggers) > 0 {
			hasTrigger = true
			return nil
		}
		retargeted, e := tx.EventHandlers.RetargetBlueprint(r.Context(), orgID, id, req.BlueprintID)
		if e != nil {
			return e
		}
		fresh = &retargeted
		return nil
	}); err != nil {
		if isUniqueViolation(err) {
			eventHandlersLog.Warn("retarget conflict, blueprint already triggered", "error", err)
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
			return
		}
		internalError(w, "event_handlers", err)
		return
	}
	if existing == nil {
		notFound(w, "event handler")
		return
	}
	if notTrigger {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "only triggers can be retargeted"})
		return
	}
	if noChange {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	if blueprint == nil {
		notFound(w, "blueprint")
		return
	}
	if crossTeam {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonCrossTeamRef, Message: "blueprint_id references a blueprint owned by another team", Field: "blueprint_id"})
		return
	}
	if hasTrigger {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this blueprint already has a trigger — a blueprint is fired by exactly one event"})
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

// PUT /api/event-handlers/reorder
//
// Rules-only — trigger IDs in the list are silently skipped by the
// store (sort_order is rule-only by CHECK constraint).
func (eh *eventHandlersHandler) handleEventHandlerReorder(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var ids []string
	// A bare JSON array body — strict decoding still applies (single value,
	// no trailing junk, capped size); DisallowUnknownFields has nothing to
	// reject on a slice.
	if !httpx.DecodeJSONStrict(w, r, &ids) {
		return
	}
	if len(ids) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "ids must contain at least one id", Field: "ids"})
		return
	}
	// Reorder rewrites sort_order across a team's handler list — a viewer can't
	// (TFAC-447). The list is one team's handlers, so gating on the first id's
	// team is representative; RLS is the backstop for a hand-crafted mixed list.
	if !eh.gateHandlerWrite(w, r, orgID, userID, ids[0]) {
		return
	}
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.EventHandlers.Reorder(r.Context(), orgID, ids)
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}
