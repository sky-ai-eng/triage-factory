package server

import (
	"encoding/json"
	"fmt"
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

// /api/event-handlers — the ordered evaluation surface task_rules and
// prompt_triggers were unified into. The unification is a READ property: one
// list, one row shape, `kind` as the explicit discriminator. The write surface
// is split per kind, because the two kinds require different fields, validate
// differently, and default `enabled` in opposite directions.
//
// Wire shape:
//
//	POST   /api/event-handlers/list                   — list (both kinds)
//	GET    /api/event-handlers/{id}                   — single read (both kinds)
//	POST   /api/event-handlers/rules                  — create a rule
//	POST   /api/event-handlers/triggers               — create a trigger
//	PATCH  /api/event-handlers/{id}                   — partial update, decoded
//	                                                    against the row's kind
//	DELETE /api/event-handlers/{id}                   — delete (soft for shipped rows)
//	POST   /api/event-handlers/{id}/promote           — rule → trigger
//	POST   /api/event-handlers/{id}/retarget          — re-point a trigger
//	PUT    /api/event-handlers/reorder                — rules-only sort_order
//
// promote and retarget stay verbs because each writes a field PATCH declares
// immutable (kind, blueprint_id) under a fence PATCH cannot express — which is
// what makes them transitions rather than field writes.

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
// link, and the mutation routes on that same id are ungated — so gating
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

// fieldScopePredicate is the wire name of the predicate on every write. One
// name, one type: a JSON object, or null for match-all. The stored column
// stays the canonical JSON text the matcher reads, which is why the read row
// spells it scope_predicate_json — that field is a string, this one is not.
const fieldScopePredicate = "scope_predicate"

// scopePredicateField reads the scope_predicate body field and canonicalises
// it against the event type's registered predicate schema. It reports whether
// the field was named at all, which is what separates "leave the stored
// predicate alone" from "clear it" on a PATCH; a create ignores the flag,
// since an unnamed predicate and an explicitly cleared one both mean match-all
// on a row that has none yet.
//
// A returned nil predicate is match-all — that covers explicit null, {}, and a
// predicate whose fields all decoded empty, which the validator canonicalises
// to the same thing. A string body is refused outright rather than unwrapped:
// accepting both a JSON object and a JSON string holding one is how a single
// field grows two wire types and callers pick different ones.
//
// Faults land on v; the caller must Flush before reading the result.
func scopePredicateField(v *httpx.Validation, raw json.RawMessage, eventType string) (predicate *string, named bool) {
	if raw == nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return nil, true
	}
	if !strings.HasPrefix(trimmed, "{") {
		v.Invalid(fieldScopePredicate, fieldScopePredicate+" must be a JSON object or null")
		return nil, true
	}
	canonical, err := events.ValidatePredicateJSON(eventType, trimmed)
	if err != nil {
		v.Invalid(fieldScopePredicate, err.Error())
		return nil, true
	}
	if canonical == "" {
		return nil, true
	}
	return &canonical, true
}

// eventHandlerCreateCommon is the half of a create body both kinds share.
// Embedding it keeps the two schemas honest about their differences without
// restating what they agree on — and strict decoding still promotes the
// embedded fields, so the other kind's fields remain UNKNOWN_FIELD.
type eventHandlerCreateCommon struct {
	EventType      string          `json:"event_type"`
	ScopePredicate json.RawMessage `json:"scope_predicate"`
	Enabled        *bool           `json:"enabled"`

	// AppliesToUnowned opts the handler into reaching entities the team
	// doesn't own (TFAC-517) — the explicit "watch" flag, default off.
	// Visibility only; never confers firing rights (the TFAC-514 invariant).
	AppliesToUnowned *bool `json:"applies_to_unowned"`

	// TeamID is the acting team the write picker supplied — the team this
	// rule/trigger is created under. Required in the UI when the caller
	// belongs to ≥2 teams; empty (sole-team fallback) otherwise.
	TeamID string `json:"team_id"`
}

// createRuleRequest is the body of POST /api/event-handlers/rules.
// name and event_type are required; default_priority defaults to 0.5,
// sort_order to 0, and enabled to TRUE — a rule the user just authored is
// meant to start classifying.
type createRuleRequest struct {
	eventHandlerCreateCommon

	Name            string   `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`
}

// createTriggerRequest is the body of POST /api/event-handlers/triggers.
// blueprint_id and event_type are required; breaker_threshold defaults to 4,
// min_autonomy_suitability to 0, and enabled to FALSE — auto-delegation is
// opt-in, so a trigger is authored dark and armed deliberately. The defaults
// are load-bearing for the canvas drag-to-connect gesture, which supplies only
// the two identifying fields.
type createTriggerRequest struct {
	eventHandlerCreateCommon

	BlueprintID            string   `json:"blueprint_id"`
	BreakerThreshold       *int     `json:"breaker_threshold"`
	MinAutonomySuitability *float64 `json:"min_autonomy_suitability"`
}

// eventTypeField validates the event_type both create bodies carry: present
// and registered. It reports whether the type resolved, because the predicate
// can only be validated against a known schema.
func eventTypeField(v *httpx.Validation, eventType string) bool {
	if eventType == "" {
		v.Missing("event_type")
		return false
	}
	if _, found := events.Get(eventType); !found {
		v.Invalid("event_type", "unknown event_type: "+eventType)
		return false
	}
	return true
}

// gateEventTypeEntitlement refuses a create bound to an event source this org
// isn't licensed for (TFAC-524), writing the 403 and reporting false. Only
// create needs it — event_type is immutable on update, and promote/retarget
// can't change it either (the router freeze makes an enabled-but-gated handler
// inert regardless).
//
// It answers on its own rather than accumulating onto the body's validation:
// a licensing boundary is not a field the caller can fix by editing the body,
// and 403 is not a status a field-fault envelope can carry.
func gateEventTypeEntitlement(w http.ResponseWriter, orgID, eventType string) bool {
	if entitlements.EventTypeAllowed(orgID, eventType) {
		return true
	}
	httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
		Reason: httpx.ReasonForbidden, Message: "event source not enabled for this organization", Field: "event_type",
	})
	return false
}

// insertEventHandler resolves the acting team and writes the composed row,
// answering 201 with the stored row. Shared by both create routes: everything
// above it differs per kind, everything from here down does not.
func (eh *eventHandlersHandler) insertEventHandler(w http.ResponseWriter, r *http.Request, orgID, userID, pickedTeam string, h domain.EventHandler) {
	var fresh *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, pickedTeam)
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
	if fresh != nil {
		writeJSON(w, http.StatusCreated, fresh)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

// handleEventHandlerCreateRule authors a kind='rule' handler.
//
// POST /api/event-handlers/rules
func (eh *eventHandlersHandler) handleEventHandlerCreateRule(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req createRuleRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	// Two accumulators, because the two fault classes carry different
	// statuses: a malformed body is a 400, a well-formed value outside its
	// range is a 422. Each class still reports every field it found wrong.
	var bad, unproc httpx.Validation

	name := strings.TrimSpace(req.Name)
	if name == "" {
		bad.Missing("name")
	}
	eventTypeOK := eventTypeField(&bad, req.EventType)
	if eventTypeOK && !gateEventTypeEntitlement(w, orgID, req.EventType) {
		return
	}
	var predicate *string
	if eventTypeOK {
		predicate, _ = scopePredicateField(&bad, req.ScopePredicate, req.EventType)
	}
	priority := 0.5
	if req.DefaultPriority != nil {
		if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
			unproc.OutOfRange("default_priority", "default_priority must be between 0 and 1")
		} else {
			priority = *req.DefaultPriority
		}
	}
	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	if unproc.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	// Reject viewers before authoring a rule (TFAC-447): resolve the acting
	// team read-only and gate, so a viewer gets a clean 403 rather than the
	// event_handlers_insert RLS WITH CHECK surfacing as a 500.
	if !gateActingTeamWrite(w, r, eh.tx, eh.az, orgID, userID, req.TeamID, "event_handlers") {
		return
	}

	sortOrder := 0
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	h := domain.EventHandler{
		ID:                 uuid.New().String(),
		Kind:               domain.EventHandlerKindRule,
		EventType:          req.EventType,
		Source:             domain.EventHandlerSourceUser,
		ScopePredicateJSON: predicate,
		Name:               name,
		DefaultPriority:    &priority,
		SortOrder:          &sortOrder,
		Enabled:            true,
	}
	if req.Enabled != nil {
		h.Enabled = *req.Enabled
	}
	if req.AppliesToUnowned != nil {
		h.AppliesToUnowned = *req.AppliesToUnowned
	}
	eh.insertEventHandler(w, r, orgID, userID, req.TeamID, h)
}

// handleEventHandlerCreateTrigger authors a kind='trigger' handler.
//
// POST /api/event-handlers/triggers
func (eh *eventHandlersHandler) handleEventHandlerCreateTrigger(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req createTriggerRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	var bad, unproc httpx.Validation
	if req.BlueprintID == "" {
		bad.Missing("blueprint_id")
	}
	eventTypeOK := eventTypeField(&bad, req.EventType)
	if eventTypeOK && !gateEventTypeEntitlement(w, orgID, req.EventType) {
		return
	}
	var predicate *string
	if eventTypeOK {
		predicate, _ = scopePredicateField(&bad, req.ScopePredicate, req.EventType)
	}
	threshold, minAutonomy := triggerTuningFields(&unproc, req.BreakerThreshold, req.MinAutonomySuitability)
	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	if unproc.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	if !gateActingTeamWrite(w, r, eh.tx, eh.az, orgID, userID, req.TeamID, "event_handlers") {
		return
	}

	// Verify the blueprint exists (clearer 404 than the downstream FK
	// integrity error) AND that the acting team owns it. A trigger may only
	// fire a blueprint its own team owns: the DB enforces this via the
	// (blueprint_id, team_id) composite FK, but we resolve the acting team
	// here and pre-check for a clean 422 instead of a generic constraint
	// error. Both lookups share one tx so the blueprint's team and the
	// resolved acting team are read under the same claims.
	var (
		blueprint           *domain.Blueprint
		crossTeamBlueprint  bool
		blueprintHasTrigger bool
	)
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
		blueprintHasTrigger = len(existing) > 0
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

	h := domain.EventHandler{
		ID:                     uuid.New().String(),
		Kind:                   domain.EventHandlerKindTrigger,
		EventType:              req.EventType,
		Source:                 domain.EventHandlerSourceUser,
		ScopePredicateJSON:     predicate,
		BlueprintID:            req.BlueprintID,
		TriggerType:            domain.TriggerTypeEvent,
		BreakerThreshold:       &threshold,
		MinAutonomySuitability: &minAutonomy,
		// Triggers default disabled (project convention) — explicit opt-in via
		// Enabled=true survives the default.
		Enabled: false,
	}
	if req.Enabled != nil {
		h.Enabled = *req.Enabled
	}
	if req.AppliesToUnowned != nil {
		h.AppliesToUnowned = *req.AppliesToUnowned
	}
	eh.insertEventHandler(w, r, orgID, userID, req.TeamID, h)
}

// triggerTuningFields resolves the two trigger knobs both paths into
// trigger-hood share — create and promote — so the defaults and the ranges
// cannot disagree between them. Out-of-range values land on v (422) and the
// returned value for that field is the default, which the caller never reaches
// because it Flushes first.
func triggerTuningFields(v *httpx.Validation, breaker *int, minAutonomy *float64) (int, float64) {
	threshold := 4
	if breaker != nil {
		if *breaker <= 0 {
			v.OutOfRange("breaker_threshold", "breaker_threshold must be positive")
		} else {
			threshold = *breaker
		}
	}
	suitability := 0.0
	if minAutonomy != nil {
		if *minAutonomy < 0 || *minAutonomy > 1 {
			v.OutOfRange("min_autonomy_suitability", "min_autonomy_suitability must be between 0 and 1")
		} else {
			suitability = *minAutonomy
		}
	}
	return threshold, suitability
}

// The PATCH bodies. There is one per kind, and the row's kind — not a body
// field — chooses which one the request is decoded against, so a rule PATCH
// carrying breaker_threshold is an UNKNOWN_FIELD rather than a field accepted
// and dropped. kind itself appears in neither struct, which is what keeps it
// immutable here (kind transitions go through /promote). event_type is
// immutable too — a new event type would invalidate the predicate schema the
// row was validated against — and for triggers so is blueprint_id (see
// /retarget).
//
// PATCH /api/event-handlers/{id}
type patchEventHandlerCommon struct {
	ScopePredicate json.RawMessage `json:"scope_predicate"`
	Enabled        *bool           `json:"enabled"`

	// AppliesToUnowned toggles the watch-scope flag (TFAC-517); absent leaves
	// it unchanged.
	AppliesToUnowned *bool `json:"applies_to_unowned"`
}

type patchRuleRequest struct {
	patchEventHandlerCommon

	Name            *string  `json:"name"`
	DefaultPriority *float64 `json:"default_priority"`
	SortOrder       *int     `json:"sort_order"`
}

type patchTriggerRequest struct {
	patchEventHandlerCommon

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

	// The row is loaded before the body is decoded, because the row's kind is
	// the schema the body is decoded against.
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
	// Two accumulators for the two statuses — see handleEventHandlerCreateRule.
	var bad, unproc httpx.Validation

	applyCommon := func(c patchEventHandlerCommon) {
		if c.Enabled != nil {
			updated.Enabled = *c.Enabled
		}
		if c.AppliesToUnowned != nil {
			updated.AppliesToUnowned = *c.AppliesToUnowned
		}
		if predicate, named := scopePredicateField(&bad, c.ScopePredicate, existing.EventType); named {
			updated.ScopePredicateJSON = predicate
		}
	}

	switch existing.Kind {
	case domain.EventHandlerKindRule:
		var req patchRuleRequest
		if !httpx.DecodeJSONStrict(w, r, &req) {
			return
		}
		applyCommon(req.patchEventHandlerCommon)
		if req.Name != nil {
			trimmed := strings.TrimSpace(*req.Name)
			if trimmed == "" {
				bad.Invalid("name", "name cannot be empty")
			} else {
				updated.Name = trimmed
			}
		}
		if req.DefaultPriority != nil {
			if *req.DefaultPriority < 0 || *req.DefaultPriority > 1 {
				unproc.OutOfRange("default_priority", "default_priority must be between 0 and 1")
			} else {
				v := *req.DefaultPriority
				updated.DefaultPriority = &v
			}
		}
		if req.SortOrder != nil {
			v := *req.SortOrder
			updated.SortOrder = &v
		}

	case domain.EventHandlerKindTrigger:
		var req patchTriggerRequest
		if !httpx.DecodeJSONStrict(w, r, &req) {
			return
		}
		applyCommon(req.patchEventHandlerCommon)
		if req.BreakerThreshold != nil {
			if *req.BreakerThreshold <= 0 {
				unproc.OutOfRange("breaker_threshold", "breaker_threshold must be positive")
			} else {
				v := *req.BreakerThreshold
				updated.BreakerThreshold = &v
			}
		}
		if req.MinAutonomySuitability != nil {
			if *req.MinAutonomySuitability < 0 || *req.MinAutonomySuitability > 1 {
				unproc.OutOfRange("min_autonomy_suitability", "min_autonomy_suitability must be between 0 and 1")
			} else {
				v := *req.MinAutonomySuitability
				updated.MinAutonomySuitability = &v
			}
		}

	default:
		internalError(w, "event_handlers", fmt.Errorf("event handler %s has unknown kind %q", existing.ID, existing.Kind))
		return
	}

	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	if unproc.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	var fresh *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

// POST /api/event-handlers/{id}/promote
//
// Rule → trigger transition. Body carries the trigger-side fields the
// promoted row needs (blueprint_id, breaker_threshold,
// min_autonomy_suitability, optionally a new predicate). The store enforces
// atomicity via a single UPDATE that flips kind and populates the trigger
// fields together.
//
// The tuning fields default exactly as they do on trigger create
// (triggerTuningFields): the two doors into trigger-hood mint the same row for
// the same body, so a promoted trigger is indistinguishable from one authored
// directly.
type promoteEventHandlerRequest struct {
	BlueprintID            string          `json:"blueprint_id"`
	BreakerThreshold       *int            `json:"breaker_threshold"`
	MinAutonomySuitability *float64        `json:"min_autonomy_suitability"`
	ScopePredicate         json.RawMessage `json:"scope_predicate"`
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
	var bad, unproc httpx.Validation
	if req.BlueprintID == "" {
		bad.Missing("blueprint_id")
	}
	threshold, minAutonomy := triggerTuningFields(&unproc, req.BreakerThreshold, req.MinAutonomySuitability)
	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	if unproc.Flush(w, http.StatusUnprocessableEntity) {
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
	// composite FK on the Promote UPDATE; pre-check for a clean 422.
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

	// The predicate is validated against the rule's event type, which promote
	// never changes. Absent keeps what the rule already matched on.
	predicate := existing.ScopePredicateJSON
	var predBad httpx.Validation
	if next, named := scopePredicateField(&predBad, req.ScopePredicate, existing.EventType); named {
		predicate = next
	}
	if predBad.Flush(w, http.StatusBadRequest) {
		return
	}

	target := domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            req.BlueprintID,
		BreakerThreshold:       &threshold,
		MinAutonomySuitability: &minAutonomy,
		ScopePredicateJSON:     predicate,
	}
	var fresh *domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.EventHandlers.Promote(r.Context(), orgID, id, target); e != nil {
			return e
		}
		var ge error
		fresh, ge = tx.EventHandlers.Get(r.Context(), orgID, id)
		return ge
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
		if e := tx.EventHandlers.RetargetBlueprint(r.Context(), orgID, id, req.BlueprintID); e != nil {
			return e
		}
		fresh, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		return e
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

// reorderEventHandlersRequest is the body of PUT /api/event-handlers/reorder.
// ids is the caller's whole rule list in its new order.
type reorderEventHandlersRequest struct {
	IDs []string `json:"ids"`
}

// PUT /api/event-handlers/reorder
//
// Rules-only: sort_order is rule-only by CHECK constraint, and the position of
// a rule is meaningful only relative to the other rules evaluated beside it.
// So the body must name EXACTLY the caller's visible rule set — an unknown id,
// a trigger id, a deleted id, a duplicate, or an omitted rule is a rejected
// request, not a partially-applied order. The store would otherwise skip each
// of those with zero rows and this route would answer "reordered" for a write
// that half-happened.
//
// Every referenced team is gated, not just the first id's: the list spans
// whatever teams the caller can see, so a viewer on any of them must not be
// able to reorder through a member's team.
func (eh *eventHandlersHandler) handleEventHandlerReorder(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req reorderEventHandlersRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "ids is required and must name every rule visible to you, in the new order",
			Field:   "ids",
		})
		return
	}

	// The caller's whole visible handler set, under the same entitlement gate
	// the list read applies — so "visible" means the same thing to both routes.
	// Unwindowed because the answer is a set comparison, not a page.
	var visible []domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		visible, _, e = tx.EventHandlers.List(r.Context(), orgID,
			db.EventHandlerListFilter{GatedEventTypes: gatedEventTypes(orgID)}, db.Unwindowed)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	rules := map[string]domain.EventHandler{}
	triggers := map[string]struct{}{}
	for _, h := range visible {
		if h.Kind == domain.EventHandlerKindRule {
			rules[h.ID] = h
			continue
		}
		triggers[h.ID] = struct{}{}
	}

	var v httpx.Validation
	seen := make(map[string]struct{}, len(req.IDs))
	var teams []string
	teamSeen := map[string]struct{}{}
	for i, id := range req.IDs {
		field := fmt.Sprintf("ids[%d]", i)
		if _, dup := seen[id]; dup {
			v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "duplicate id " + id, Field: field})
			continue
		}
		seen[id] = struct{}{}
		if rule, isRule := rules[id]; isRule {
			if _, known := teamSeen[rule.TeamID]; !known {
				teamSeen[rule.TeamID] = struct{}{}
				teams = append(teams, rule.TeamID)
			}
			continue
		}
		if _, isTrigger := triggers[id]; isTrigger {
			v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: id + " is a trigger; only rules carry a sort order", Field: field})
			continue
		}
		v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "no rule " + id + " is visible to you", Field: field})
	}
	// Walk `visible` rather than the rules map so the omitted-rule faults come
	// out in the list's own order — one request must always answer the same
	// way, and Go map iteration would shuffle it per call.
	for _, h := range visible {
		if h.Kind != domain.EventHandlerKindRule {
			continue
		}
		if _, named := seen[h.ID]; !named {
			v.Add(httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: "rule " + h.ID + " is missing; ids must name every rule visible to you",
				Field:   "ids",
			})
		}
	}
	// 422, not 400: the body is well-formed JSON of the right shape — what it
	// names doesn't match the data it claims to reorder.
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	// Reorder rewrites sort_order across the whole list, so a viewer on ANY
	// team the list spans can't run it (TFAC-447).
	for _, teamID := range teams {
		if !eh.az.RequireTeamWrite(w, r, orgID, userID, teamID) {
			return
		}
	}

	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.EventHandlers.Reorder(r.Context(), orgID, req.IDs)
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}
