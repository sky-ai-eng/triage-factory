package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// eventHandlersHandler serves the event handler endpoints. It holds the
// transactional store runner the handlers use; registerEventHandlerRoutes
// registers its methods through the api()/apiMutating() wrappers.
type eventHandlersHandler struct {
	tx db.TxRunner
	az *authz.Checker
	// catalogs is what a scope predicate's Jira and Linear ids are checked
	// through (checkPredicateCatalog).
	catalogs predicateCatalogClients
}

// /api/event-handlers — one resource, one row shape, and one write per
// intent:
//
//	POST   /api/event-handlers/list     — list
//	GET    /api/event-handlers/{id}     — single read
//	POST   /api/event-handlers          — create
//	PATCH  /api/event-handlers/{id}     — partial update
//	DELETE /api/event-handlers/{id}     — delete (a deleted row stays behind)
//	PUT    /api/event-handlers/reorder  — sort_order for the whole visible list
//
// A handler's mode says what a matching event does, and every handler holds
// the same fields in every mode, so one body describes every handler and a
// change of mode is a field write like any other.

// gateHandlerWrite rejects a viewer before a write against an existing event
// handler, pre-loading the row to resolve its team. Returns false (caller
// should return — a response was written) for a viewer (403) or a load
// failure (500). A missing handler passes through (returns true) so the
// handler's own nil-check renders the 404. The same shape as the blueprint
// handlers' write gate.
func (eh *eventHandlersHandler) gateHandlerWrite(w http.ResponseWriter, r *http.Request, orgID, userID, id string) bool {
	var existing *domain.EventHandler
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

// eventHandlerWire is a handler on the wire: the stored row, plus, on the read
// shapes, whether its source can still produce events for this org.
//
// source_available is derived per read and never stored — the source's own
// state is what moves, and a column would be a copy of it free to go stale. A
// handler whose source has gone unavailable is deliberately still LISTED and
// still COUNTED: unlike the entitlement gate beside it, which hides a handler
// bound to an unlicensed type, an unconfigured source is the reader's to fix,
// so this one is explained rather than made to disappear. Absent means never
// yours; explained means not yet. A write answers with the row it stored,
// which is a different question, so it carries no source_available.
type eventHandlerWire struct {
	ID                     string    `json:"id"`
	TeamID                 string    `json:"team_id"`
	EventType              string    `json:"event_type"`
	Mode                   string    `json:"mode"`
	Name                   string    `json:"name"`
	ScopePredicateJSON     *string   `json:"scope_predicate_json"`
	DefaultPriority        float64   `json:"default_priority"`
	SortOrder              int       `json:"sort_order"`
	BlueprintID            *string   `json:"blueprint_id"`
	BreakerThreshold       int       `json:"breaker_threshold"`
	MinAutonomySuitability float64   `json:"min_autonomy_suitability"`
	AppliesToUnowned       bool      `json:"applies_to_unowned"`
	Source                 string    `json:"source"`
	CreatorUserID          string    `json:"creator_user_id,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
	SourceAvailable        *bool     `json:"source_available,omitempty"`
}

// storedRow is h as a write answers it.
func storedRow(h domain.EventHandler) eventHandlerWire {
	out := eventHandlerWire{
		ID: h.ID, TeamID: h.TeamID, EventType: h.EventType, Mode: h.Mode, Name: h.Name,
		ScopePredicateJSON: h.ScopePredicateJSON, DefaultPriority: h.DefaultPriority, SortOrder: h.SortOrder,
		BreakerThreshold: h.BreakerThreshold, MinAutonomySuitability: h.MinAutonomySuitability,
		AppliesToUnowned: h.AppliesToUnowned, Source: h.Source, CreatorUserID: h.CreatorUserID,
		CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
	}
	if h.BlueprintID != "" {
		id := h.BlueprintID
		out.BlueprintID = &id
	}
	return out
}

// readRow is h as a read answers it, paired with its source's availability.
func readRow(h domain.EventHandler, sources eventsource.Availability) eventHandlerWire {
	out := storedRow(h)
	available := sources.CanProduce(eventsource.KindOf(h.EventType))
	out.SourceAvailable = &available
	return out
}

// readRows is readRow over a page.
func readRows(handlers []domain.EventHandler, sources eventsource.Availability) []eventHandlerWire {
	rows := make([]eventHandlerWire, 0, len(handlers))
	for _, h := range handlers {
		rows = append(rows, readRow(h, sources))
	}
	return rows
}

// eventHandlerListRequest is the body of POST /api/event-handlers/list. Each
// field is a narrowing, validated strictly: a corrupt filter is a 400 naming
// it, never dropped, since dropping one widens the read.
type eventHandlerListRequest struct {
	// Modes narrows to handlers in one of these modes; absent or empty is
	// every mode.
	Modes []string `json:"modes"`
	// EventType narrows to one event type.
	EventType string `json:"event_type"`
	// HasBlueprint narrows to handlers that reference a blueprint (true) or
	// none (false); absent is both.
	HasBlueprint *bool  `json:"has_blueprint"`
	TeamID       string `json:"team_id"`

	httpx.PageRequest
}

type eventHandlerListFilterKey struct {
	Modes        []string `json:"modes"`
	EventType    string   `json:"event_type"`
	HasBlueprint *bool    `json:"has_blueprint"`
	TeamID       string   `json:"team_id"`
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
	// The canonical mode set — sorted, each named once — is what the filter
	// and the page token's fingerprint both read, so two spellings of one set
	// are one query.
	var modes []string
	for i, m := range req.Modes {
		if !domain.ValidEventHandlerMode(m) {
			v.Invalid(fmt.Sprintf("modes[%d]", i), fmt.Sprintf("unknown mode %q: modes are %s", m, strings.Join(domain.EventHandlerModes, ", ")))
			continue
		}
		if !slices.Contains(modes, m) {
			modes = append(modes, m)
		}
	}
	slices.Sort(modes)
	if req.EventType != "" {
		if _, known := events.Get(req.EventType); !known {
			v.Invalid("event_type", "unknown event_type: "+req.EventType)
		}
	}
	teamIDFilterField(&v, req.TeamID)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(eventHandlerListFilterKey{
		Modes: modes, EventType: req.EventType, HasBlueprint: req.HasBlueprint, TeamID: req.TeamID,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The entitlement gate hides a handler bound to an event type this org
	// isn't licensed for. It rides into the query as a filter, not a
	// post-read trim, so the page and total_count agree about what the caller
	// can see.
	filter := db.EventHandlerListFilter{
		Modes: modes, EventType: req.EventType, HasBlueprint: req.HasBlueprint, TeamID: req.TeamID,
		GatedEventTypes: entitlements.GatedEventTypes(orgID),
	}

	var (
		handlers []domain.EventHandler
		total    int
		sources  eventsource.Availability
	)
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handlers, total, e = tx.EventHandlers.List(r.Context(), orgID, filter, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		if e != nil || page.CountOnly {
			return e // count-only answers no rows, so nothing needs the flag
		}
		sources, e = eventsource.Resolve(r.Context(), tx, orgID)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	httpx.WriteList(w, page, readRows(handlers, sources), total)
}

// handleEventHandlerGet is the canonical single read, in the list's row shape.
//
// Like the blueprint single read, it is deliberately NOT entitlement-gated the
// way the list is. The gate is a visibility rule for a BROWSE surface: it keeps
// a handler bound to an unlicensed event type out of a list nobody asked for it
// by name. A caller who already holds the id has it from a run, a notification,
// or a link, and the mutation routes on that same id are ungated — so gating
// only the read would make a handler editable but unreadable.
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

	var (
		handler *domain.EventHandler
		sources eventsource.Availability
	)
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		handler, e = tx.EventHandlers.Get(r.Context(), orgID, id)
		if e != nil || handler == nil {
			return e
		}
		sources, e = eventsource.Resolve(r.Context(), tx, orgID)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	if handler == nil {
		notFound(w, "event handler")
		return
	}
	writeJSON(w, http.StatusOK, readRow(*handler, sources))
}

// fieldScopePredicate is the wire name of the predicate on every write. One
// name, one type: a JSON object, or null for match-all. The stored column
// stays the canonical JSON text the matcher reads, which is why the read row
// spells it scope_predicate_json — that field is a string, this one is not.
const fieldScopePredicate = "scope_predicate"

// appliesToUnownedField refuses applies_to_unowned: true on an event type
// the flag cannot widen: a pool event already reaches every team whose
// tracked set and handler match, and a requested-party or unrouted event
// reaches whom it names. routing.EventSupportsWatch is the rule, and the
// event-type catalog's supports_watch is the same call, which is what the
// editors hide the control on. false is accepted on every event type, so a
// stored flag can always be cleared.
func appliesToUnownedField(v *httpx.Validation, eventType string, set bool) {
	if set && !routing.EventSupportsWatch(eventType) {
		v.Invalid("applies_to_unowned", fmt.Sprintf(
			"applies_to_unowned has no effect on %s: its events reach every team whose handler matches, owner or not (supports_watch is false in GET /api/event-types)", eventType))
	}
}

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
// Faults land on bad (400) or unproc (422) by class, as predicateFaults files
// them. The caller must Flush bad before reading the result. When the
// predicate's only faults are on unproc, the result is the rest of it
// (events.PredicateError.Remainder), for the catalog and reference checks to
// read, so the write reports every 422 at once; the faults on unproc keep it
// from being stored. prior is the predicate the row stores now, nil on
// create (events.ValidatePredicateEdit).
func scopePredicateField(bad, unproc *httpx.Validation, raw json.RawMessage, eventType string, prior *string) (predicate *string, named bool) {
	if raw == nil {
		return nil, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return nil, true
	}
	if !strings.HasPrefix(trimmed, "{") {
		bad.Invalid(fieldScopePredicate, fieldScopePredicate+" must be a JSON object, or null")
		return nil, true
	}
	canonical, err := events.ValidatePredicateEdit(eventType, trimmed, prior)
	if err != nil {
		predicateFaults(bad, unproc, err)
		var pe *events.PredicateError
		if errors.As(err, &pe) {
			return pe.Remainder, true
		}
		return nil, true
	}
	if canonical == "" {
		return nil, true
	}
	return &canonical, true
}

// predicateFaults files each fault the predicate validator found under the
// predicate key it names (scope_predicate.<key>). A predicate that does not
// fit its event type's schema is a malformed body, 400, with an unknown key
// answered UNKNOWN_FIELD like one at the top level of the body; a value the
// schema admits and the field refuses is 422, like an out-of-range priority.
func predicateFaults(bad, unproc *httpx.Validation, err error) {
	var pe *events.PredicateError
	if !errors.As(err, &pe) {
		bad.Invalid(fieldScopePredicate, err.Error())
		return
	}
	for _, f := range pe.Faults {
		field := predicateField(f.Key)
		switch f.Kind {
		case events.FaultUnknownField:
			bad.Add(httpx.ErrorItem{Reason: httpx.ReasonUnknownField, Message: f.Message, Field: field})
		case events.FaultInvalidValue:
			unproc.Invalid(field, f.Message)
		default:
			bad.Invalid(field, f.Message)
		}
	}
}

// eventTypeField validates the event_type a create body carries: present,
// registered, and one the router can carry a handler for. It reports whether
// the type resolved, because the predicate can only be validated against a
// known schema.
//
// A system sentinel (OwnershipUnrouted — poll completions, routing
// dispositions, the poll's close obligation) is refused: the router never
// matches handlers on one, so a handler authored against it would sit
// active and never fire, with nothing to say why. The refusal is a field
// fault the caller can fix, not a permission fault.
func eventTypeField(v *httpx.Validation, eventType string) bool {
	if eventType == "" {
		v.Missing("event_type")
		return false
	}
	schema, found := events.Get(eventType)
	if !found {
		v.Invalid("event_type", "unknown event_type: "+eventType)
		return false
	}
	if schema.Ownership == events.OwnershipUnrouted {
		v.Invalid("event_type", eventType+" is a system event and cannot have a handler")
		return false
	}
	return true
}

// gateEventTypeEntitlement refuses a create bound to an event source this org
// isn't licensed for, writing the 403 and reporting false. Only create needs
// it — event_type is immutable on update (the router freeze makes an active
// but gated handler inert regardless).
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

// gateEventTypeSourceAvailability records a 422 fault on v when the org cannot
// produce events from the source this handler would bind to — no Jira
// credential, no connected Slack workspace, a source TF has not built — and
// reports whether it can. ok is false only when it has already written a
// response: a failed read is a 500, never a source silently reported off.
//
// Create is the only enforcement point there will ever be, which is why it
// exists at all: event_type is immutable after create, and matchHandlers is an
// exact equality on it, so a handler authored against a source the org cannot
// produce is not merely wrong-for-now — no event will ever arrive to match it,
// nothing logs that, and nothing else surfaces it.
//
// It is a 422 INVALID_FIELD rather than the entitlement gate's 403: the value
// names a source that exists and the caller may well be entitled to, it just
// does not resolve for this org yet. That is a semantically invalid reference,
// and it is the caller's to fix — which is exactly what 403 FORBIDDEN would
// deny.
func (eh *eventHandlersHandler) gateEventTypeSourceAvailability(w http.ResponseWriter, r *http.Request, v *httpx.Validation, orgID, userID, eventType string) (available, ok bool) {
	kind := eventsource.KindOf(eventType)
	var (
		state eventsource.State
		known bool
	)
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		state, known, e = eventsource.StateFor(r.Context(), tx, orgID, kind)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return false, false
	}
	if known && state != eventsource.StateAvailable {
		v.Invalid("event_type", sourceUnavailableMessage(kind, state))
		return false, true
	}
	return true, true
}

// sourceUnavailableMessage names the source and what is missing, because the
// remedy differs per state and only one of them is the caller's to carry out.
func sourceUnavailableMessage(kind string, state eventsource.State) string {
	switch state {
	case eventsource.StateUnconfigured:
		return kind + " is not configured for this organization"
	case eventsource.StateDisabled:
		return kind + " is turned off for this organization by an org admin"
	case eventsource.StateUnlicensed:
		return kind + " is not enabled for this organization"
	case eventsource.StateWIP:
		return kind + " events are not supported yet"
	default:
		return kind + " cannot produce events for this organization"
	}
}

// eventHandlerFields is every field a write may set, as the body named it.
// Create and PATCH decode the same fields the same way: absent leaves a field
// as it is (the create default, or the stored value), null clears
// scope_predicate and blueprint_id and is refused for every other field,
// because each always holds a value.
type eventHandlerFields struct {
	Mode                   json.RawMessage `json:"mode"`
	Name                   json.RawMessage `json:"name"`
	ScopePredicate         json.RawMessage `json:"scope_predicate"`
	DefaultPriority        json.RawMessage `json:"default_priority"`
	SortOrder              json.RawMessage `json:"sort_order"`
	BlueprintID            json.RawMessage `json:"blueprint_id"`
	BreakerThreshold       json.RawMessage `json:"breaker_threshold"`
	MinAutonomySuitability json.RawMessage `json:"min_autonomy_suitability"`
	AppliesToUnowned       json.RawMessage `json:"applies_to_unowned"`
}

// named reports whether the body named any field at all.
func (f eventHandlerFields) named() bool {
	return httpx.PatchNamed(f.Mode, f.Name, f.ScopePredicate, f.DefaultPriority, f.SortOrder,
		f.BlueprintID, f.BreakerThreshold, f.MinAutonomySuitability, f.AppliesToUnowned)
}

// fieldWrites is what applyHandlerFields learned beyond the values it wrote:
// which of the fields that need more than the body to decide were named.
type fieldWrites struct {
	mode, name bool
	// blueprint is true when blueprint_id was named; the new value is then on
	// the handler, "" for a null.
	blueprint bool
}

// refuseNull records a null for a field that always holds a value.
func refuseNull(v *httpx.Validation, state httpx.PatchState, field string) {
	if state == httpx.PatchClear {
		v.Invalid(field, field+" cannot be null")
	}
}

// applyHandlerFields writes the fields f names onto h, recording a malformed
// value on bad (400) and an out-of-range one on unproc (422). The predicate and
// applies_to_unowned are checked against h.EventType, with prior the predicate
// the row stores now (nil on create); eventTypeKnown false skips those two
// checks, for a create whose event_type is itself refused. The caller Flushes bad before reading h, and checks the
// row as a whole afterwards (handlerRowFaults), because whether a mode or a
// blueprint is acceptable depends on the other.
//
// It is the one place the field rules live, so the create route and the PATCH
// refuse the same value the same way.
func applyHandlerFields(bad, unproc *httpx.Validation, f eventHandlerFields, h *domain.EventHandler, prior *string, eventTypeKnown bool) fieldWrites {
	var w fieldWrites
	if mode, state := httpx.PatchString(bad, f.Mode, "mode"); state == httpx.PatchSet {
		w.mode = true
		if domain.ValidEventHandlerMode(mode) {
			h.Mode = mode
		} else {
			bad.Invalid("mode", fmt.Sprintf("unknown mode %q: modes are %s", mode, strings.Join(domain.EventHandlerModes, ", ")))
		}
	} else {
		refuseNull(bad, state, "mode")
	}
	if name, state := httpx.PatchString(bad, f.Name, "name"); state == httpx.PatchSet {
		w.name = true
		if trimmed := strings.TrimSpace(name); trimmed == "" {
			bad.Invalid("name", "name cannot be empty")
		} else {
			h.Name = trimmed
		}
	} else {
		refuseNull(bad, state, "name")
	}
	if eventTypeKnown {
		if predicate, named := scopePredicateField(bad, unproc, f.ScopePredicate, h.EventType, prior); named {
			h.ScopePredicateJSON = predicate
		}
	}
	if priority, state := httpx.PatchFloat(bad, f.DefaultPriority, "default_priority"); state == httpx.PatchSet {
		if priority < 0 || priority > 1 {
			unproc.OutOfRange("default_priority", "default_priority must be between 0 and 1")
		} else {
			h.DefaultPriority = priority
		}
	} else {
		refuseNull(bad, state, "default_priority")
	}
	if sortOrder, state := httpx.PatchInt(bad, f.SortOrder, "sort_order"); state == httpx.PatchSet {
		if sortOrder < math.MinInt32 || sortOrder > math.MaxInt32 {
			unproc.OutOfRange("sort_order", fmt.Sprintf("sort_order must be between %d and %d", math.MinInt32, math.MaxInt32))
		} else {
			h.SortOrder = sortOrder
		}
	} else {
		refuseNull(bad, state, "sort_order")
	}
	switch blueprintID, state := httpx.PatchString(bad, f.BlueprintID, "blueprint_id"); state {
	case httpx.PatchSet:
		w.blueprint = true
		if blueprintID == "" {
			bad.Invalid("blueprint_id", "blueprint_id must name a blueprint, or be null for none")
		} else {
			h.BlueprintID = blueprintID
		}
	case httpx.PatchClear:
		w.blueprint = true
		h.BlueprintID = ""
	}
	if threshold, state := httpx.PatchInt(bad, f.BreakerThreshold, "breaker_threshold"); state == httpx.PatchSet {
		if threshold <= 0 || threshold > math.MaxInt32 {
			unproc.OutOfRange("breaker_threshold", fmt.Sprintf("breaker_threshold must be between 1 and %d", math.MaxInt32))
		} else {
			h.BreakerThreshold = threshold
		}
	} else {
		refuseNull(bad, state, "breaker_threshold")
	}
	if suitability, state := httpx.PatchFloat(bad, f.MinAutonomySuitability, "min_autonomy_suitability"); state == httpx.PatchSet {
		if suitability < 0 || suitability > 1 {
			unproc.OutOfRange("min_autonomy_suitability", "min_autonomy_suitability must be between 0 and 1")
		} else {
			h.MinAutonomySuitability = suitability
		}
	} else {
		refuseNull(bad, state, "min_autonomy_suitability")
	}
	if applies, state := httpx.PatchBool(bad, f.AppliesToUnowned, "applies_to_unowned"); state == httpx.PatchSet {
		if eventTypeKnown {
			appliesToUnownedField(unproc, h.EventType, applies)
		}
		h.AppliesToUnowned = applies
	} else {
		refuseNull(bad, state, "applies_to_unowned")
	}
	return w
}

// handlerRowFaults records, on unproc, what is wrong with h as a whole: the
// faults no single field shows. automate needs a blueprint, which is the mode
// field's fault when the body moved the handler into automate and the
// blueprint field's when the body cleared the blueprint of a handler staying
// there. notify needs an event type whose tracked-set gate core owns.
func handlerRowFaults(unproc *httpx.Validation, h domain.EventHandler, w fieldWrites) {
	if h.Automates() && h.BlueprintID == "" {
		if w.blueprint && !w.mode {
			unproc.Invalid("blueprint_id", "blueprint_id cannot be null on a handler in automate; move it to another mode first")
		} else {
			unproc.Invalid("mode", "automate runs the handler's blueprint, and this handler has none: set blueprint_id")
		}
	}
	if h.Notifies() && !domain.NotifyRuleSource(h.EventType) {
		unproc.Invalid("mode", "notify covers GitHub, Jira and Linear events, and "+h.EventType+" is not one")
	}
}

// crossTeamBlueprintFault refuses a blueprint_id naming a blueprint another
// team owns: a handler may only reference its own team's blueprints, which the
// (blueprint_id, team_id) composite FK enforces, and the routes pre-check for
// a clean 422 instead of a constraint error.
var crossTeamBlueprintFault = httpx.ErrorItem{
	Reason: httpx.ReasonCrossTeamRef, Message: "blueprint_id references a blueprint owned by another team", Field: "blueprint_id",
}

// errBlueprintNotFound is a blueprint_id naming no live blueprint the caller
// can see, which the routes answer as a missing blueprint.
var errBlueprintNotFound = errors.New("blueprint not found")

// handlerBlueprint reads the blueprint a write names for a handler of teamID:
// errBlueprintNotFound when none is live, and crossTeam when another team owns
// it.
func handlerBlueprint(ctx context.Context, tx db.TxStores, orgID, teamID, blueprintID string) (blueprint *domain.Blueprint, crossTeam bool, err error) {
	blueprint, err = tx.Blueprints.Get(ctx, orgID, blueprintID)
	if err != nil {
		return nil, false, err
	}
	if blueprint == nil {
		return nil, false, errBlueprintNotFound
	}
	return blueprint, blueprint.TeamID != "" && teamID != "" && blueprint.TeamID != teamID, nil
}

// createEventHandlerRequest is the body of POST /api/event-handlers. event_type
// is required; every other field has a default: mode watch, priority 0.5,
// sort order 0, no blueprint, breaker threshold 4, autonomy floor 0, and
// applies_to_unowned false. name defaults to the blueprint's name when
// blueprint_id is given, and otherwise to the event type's catalog label.
type createEventHandlerRequest struct {
	// TeamID is the acting team the write picker supplied — the team the
	// handler is created under. Required when the caller belongs to ≥2 teams;
	// empty (sole-team fallback) otherwise.
	TeamID    string `json:"team_id"`
	EventType string `json:"event_type"`
	eventHandlerFields
}

// eventTypeLabel is an event type's label in the catalog, or its id when the
// catalog does not list it.
func eventTypeLabel(eventType string) string {
	for _, et := range domain.AllEventTypes() {
		if et.ID == eventType && et.Label != "" {
			return et.Label
		}
	}
	return eventType
}

// POST /api/event-handlers
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

	// Two accumulators, because the two fault classes carry different
	// statuses: a malformed body is a 400, a well-formed value the row cannot
	// hold is a 422. Each class still reports every field it found wrong.
	// The 400s answer at once; the 422s are carried into the write's
	// transaction and answered with the predicate reference faults, so one
	// answer holds every 422 (checkPredicateReferences).
	var bad, unproc httpx.Validation
	eventTypeOK := eventTypeField(&bad, req.EventType)
	if eventTypeOK && !gateEventTypeEntitlement(w, orgID, req.EventType) {
		return
	}
	h := domain.EventHandler{
		ID:               uuid.New().String(),
		Mode:             domain.EventHandlerModeWatch,
		EventType:        req.EventType,
		Source:           domain.EventHandlerSourceUser,
		DefaultPriority:  0.5,
		BreakerThreshold: 4,
	}
	writes := applyHandlerFields(&bad, &unproc, req.eventHandlerFields, &h, nil, eventTypeOK)
	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	// After the body is known well-formed, because this one reads the org's
	// credentials to answer.
	sourceAvailable, ok := eh.gateEventTypeSourceAvailability(w, r, &unproc, orgID, userID, req.EventType)
	if !ok {
		return
	}

	// Reject viewers before authoring a handler: resolve the acting team
	// read-only and gate, so a viewer gets a clean 403 rather than the
	// event_handlers_insert RLS WITH CHECK surfacing as a 500.
	teamID, ok := eh.az.ActingTeamForWrite(w, r, orgID, userID, req.TeamID, "event_handlers")
	if !ok {
		return
	}

	// The blueprint must exist (a clearer 404 than the downstream FK error)
	// and belong to the acting team: the DB enforces that through the
	// (blueprint_id, team_id) composite FK, and the route pre-checks for a
	// clean 422. Both reads share one tx so the blueprint's team and the
	// acting team are read under the same claims. It only reads: the acting
	// team is recorded as the caller's last-written one by the write itself
	// (insertEventHandler), which a refused request never commits.
	if h.BlueprintID != "" {
		var (
			blueprint *domain.Blueprint
			crossTeam bool
		)
		if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			actingTeam, e := teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, teamID)
			if e != nil {
				return e
			}
			blueprint, crossTeam, e = handlerBlueprint(r.Context(), tx, orgID, actingTeam, h.BlueprintID)
			return e
		}); err != nil {
			if errors.Is(err, errBlueprintNotFound) {
				notFound(w, "blueprint")
				return
			}
			if teamscope.WriteIfSelectionError(w, err) {
				return
			}
			internalError(w, "event_handlers", err)
			return
		}
		if crossTeam {
			unproc.Add(crossTeamBlueprintFault)
		}
		if !writes.name {
			h.Name = blueprint.Name
		}
	}
	if h.Name == "" {
		h.Name = eventTypeLabel(h.EventType)
	}
	handlerRowFaults(&unproc, h, writes)

	// A source the org cannot use is not asked about the predicate's ids: the
	// event_type fault already refuses the write, and a source an admin turned
	// off makes no API calls.
	found := unproc.Items()
	var catalogFaults []httpx.ErrorItem
	if sourceAvailable {
		if catalogFaults, ok = eh.checkPredicateCatalog(w, r, orgID, userID, teamID, h.EventType, h.ScopePredicateJSON, nil, len(found) > 0); !ok {
			return
		}
	}
	eh.insertEventHandler(w, r, orgID, userID, teamID, h, found, catalogFaults)
}

// insertEventHandler resolves the acting team and writes the composed row,
// answering 201 with the row the INSERT returned. pickedTeam is the team the
// caller was gated on, found is the body's 422 faults, and catalogFaults are
// what checkPredicateCatalog found in the predicate for that team; the
// transaction answers them with the reference check's
// (checkPredicateReferences).
//
// The answer is the stored row, not the composed one: the write decides
// team_id, source and both timestamps, so the struct handed in here is not a
// picture of what landed.
func (eh *eventHandlersHandler) insertEventHandler(w http.ResponseWriter, r *http.Request, orgID, userID, pickedTeam string, h domain.EventHandler, found, catalogFaults []httpx.ErrorItem) {
	var stored domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, pickedTeam)
		if e != nil {
			return e
		}
		if e := checkPredicateReferences(r.Context(), tx, orgID, teamID, h.EventType, h.ScopePredicateJSON, nil, found, catalogFaults); e != nil {
			return e
		}
		stored, e = tx.EventHandlers.Create(r.Context(), orgID, teamID, h)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		if writeUnprocessableError(w, err) {
			return
		}
		eventHandlersLog.Error("create failed", "error", err)
		internalError(w, "event_handlers", err)
		return
	}
	writeJSON(w, http.StatusCreated, storedRow(stored))
}

// PATCH /api/event-handlers/{id}
//
// Every field of the create body but team_id and event_type, which are
// immutable: the team owns the handler, and a new event type would invalidate
// the predicate schema the row was validated against. Every check is made
// against the row as it would be after the write.
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

	var existing *domain.EventHandler
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	if !eh.az.RequireTeamWrite(w, r, orgID, userID, existing.TeamID) {
		return
	}

	var req eventHandlerFields
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if !req.named() {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "nothing to update: name at least one field",
		})
		return
	}

	updated := *existing
	var bad, unproc httpx.Validation
	writes := applyHandlerFields(&bad, &unproc, req, &updated, existing.ScopePredicateJSON, true)
	if bad.Flush(w, http.StatusBadRequest) {
		return
	}
	if updated.BlueprintID != "" && updated.BlueprintID != existing.BlueprintID {
		var crossTeam bool
		if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			_, crossTeam, e = handlerBlueprint(r.Context(), tx, orgID, existing.TeamID, updated.BlueprintID)
			return e
		}); err != nil {
			if errors.Is(err, errBlueprintNotFound) {
				notFound(w, "blueprint")
				return
			}
			internalError(w, "event_handlers", err)
			return
		}
		if crossTeam {
			unproc.Add(crossTeamBlueprintFault)
		}
	}
	handlerRowFaults(&unproc, updated, writes)

	found := unproc.Items()
	catalogFaults, ok := eh.checkPredicateCatalog(w, r, orgID, userID, existing.TeamID, existing.EventType, updated.ScopePredicateJSON, existing.ScopePredicateJSON, len(found) > 0)
	if !ok {
		return
	}
	var stored domain.EventHandler
	if err := eh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := checkPredicateReferences(r.Context(), tx, orgID, existing.TeamID, existing.EventType, updated.ScopePredicateJSON, existing.ScopePredicateJSON, found, catalogFaults); e != nil {
			return e
		}
		var e error
		stored, e = tx.EventHandlers.Update(r.Context(), orgID, updated)
		return e
	}); err != nil {
		if writeUnprocessableError(w, err) {
			return
		}
		// The row was loaded a moment ago, so a miss here means it was deleted
		// in between. 404 rather than a 200 over a write that touched nothing.
		if errors.Is(err, db.ErrNoSuchEventHandler) {
			notFound(w, "event handler")
			return
		}
		// The body was applied to a read of the row taken before its
		// blueprint moved, most likely a blueprint delete detaching it.
		if errors.Is(err, db.ErrEventHandlerBlueprintChanged) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonConflict,
				Message: "the event handler's blueprint changed or was deleted while this update was made: read the handler again and retry",
			})
			return
		}
		internalError(w, "event_handlers", err)
		return
	}
	eh.recheckJiraDrift(context.WithoutCancel(r.Context()), orgID, userID, *existing, stored)
	writeJSON(w, http.StatusOK, storedRow(stored))
}

// DELETE /api/event-handlers/{id}
//
// EventHandlers.Delete stamps deleted_at: a shipped copy's
// (org_id, team_id, system_slug) slot stays occupied so the boot-time
// shipped-content sync never resurrects it, and the runs a handler started
// keep referencing it. Every read path filters deleted_at IS NULL, so a
// deleted handler is gone to the API.
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

	// Viewers can't delete a handler. A missing handler passes through to the
	// 404 below.
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
	resolveJiraDriftOnDelete(context.WithoutCancel(r.Context()), orgID, *existing)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// reorderEventHandlersRequest is the body of PUT /api/event-handlers/reorder.
// ids is the caller's whole visible handler list in its new order.
type reorderEventHandlersRequest struct {
	IDs []string `json:"ids"`
}

// PUT /api/event-handlers/reorder
//
// The position of a handler is meaningful only relative to the others listed
// beside it, so the body must name EXACTLY the caller's visible handler set,
// every mode included — an unknown id, a deleted id, a duplicate, or an
// omitted handler is a rejected request, not a partially-applied order. The
// store would otherwise skip each of those with zero rows and this route would
// answer "reordered" for a write that half-happened.
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
			Message: "ids is required and must name every event handler visible to you, in the new order",
			Field:   "ids",
		})
		return
	}

	// The caller's whole visible handler set, under the same entitlement gate
	// the list read applies — so "visible" means the same thing to both routes.
	// Unwindowed because the answer is a set comparison, not a page.
	var visible []domain.EventHandler
	if err := eh.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		visible, _, e = tx.EventHandlers.List(r.Context(), orgID,
			db.EventHandlerListFilter{GatedEventTypes: entitlements.GatedEventTypes(orgID)}, db.Unwindowed)
		return e
	}); err != nil {
		internalError(w, "event_handlers", err)
		return
	}
	byID := make(map[string]domain.EventHandler, len(visible))
	for _, h := range visible {
		byID[h.ID] = h
	}

	var v httpx.Validation
	seen := make(map[string]struct{}, len(req.IDs))
	var teams []string
	for i, id := range req.IDs {
		field := fmt.Sprintf("ids[%d]", i)
		if _, dup := seen[id]; dup {
			v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "duplicate id " + id, Field: field})
			continue
		}
		seen[id] = struct{}{}
		h, known := byID[id]
		if !known {
			v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "no event handler " + id + " is visible to you", Field: field})
			continue
		}
		if !slices.Contains(teams, h.TeamID) {
			teams = append(teams, h.TeamID)
		}
	}
	// Walk `visible` rather than the map so the omitted-handler faults come
	// out in the list's own order — one request must always answer the same
	// way, and Go map iteration would shuffle it per call.
	for _, h := range visible {
		if _, named := seen[h.ID]; !named {
			v.Add(httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: "event handler " + h.ID + " is missing; ids must name every event handler visible to you",
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
	// team the list spans can't run it.
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

func (s *Server) registerEventHandlerRoutes() {
	s.api("GET /api/event-schemas", handleEventSchemasList)
	s.api("GET /api/event-schemas/{event_type}", handleEventSchemaGet)
	eh := &eventHandlersHandler{tx: s.tx, az: s.az, catalogs: s}
	s.apiMutating("POST /api/event-handlers/list", eh.handleEventHandlersList)
	s.api("GET /api/event-handlers/{id}", eh.handleEventHandlerGet)
	s.apiMutating("POST /api/event-handlers", eh.handleEventHandlerCreate)
	s.apiMutating("PUT /api/event-handlers/reorder", eh.handleEventHandlerReorder)
	s.apiMutating("PATCH /api/event-handlers/{id}", eh.handleEventHandlerUpdate)
	s.apiMutating("DELETE /api/event-handlers/{id}", eh.handleEventHandlerDelete)
}
