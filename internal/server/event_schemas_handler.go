package server

import (
	"net/http"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

var opEventSchemaList = httpx.NewJSONOp[httpx.NoBody, map[string]schemaPayload](httpx.OpSpec{
	Pattern:     "GET /api/event-schemas",
	ID:          "list_event_schemas",
	Summary:     "List event predicate schemas",
	Description: "Returns the predicate fields of every event type the organization is licensed for, as an object keyed by event type. The whole set is returned at once, with no pagination.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// GET /api/event-schemas
//
// Returns a map keyed by event_type, where each value is the predicate field
// schema for that event type. Used by the frontend's predicate editor to
// render typed inputs (checkbox for bools, dropdown for enums, text for
// strings, etc.). A gated-off event type's schema is omitted (TFAC-524) —
// the predicate editor never offers a field set for a source the org can't
// use.
func handleEventSchemasList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	all := events.All()

	out := make(map[string]schemaPayload, len(all))
	for _, sc := range all {
		if !entitlements.EventTypeAllowed(orgID, sc.EventType) {
			continue
		}
		out[sc.EventType] = toPayload(sc)
	}
	opEventSchemaList.Write(w, out)
}

var opEventSchemaGet = httpx.NewJSONOp[httpx.NoBody, schemaPayload](httpx.OpSpec{
	Pattern:     "GET /api/event-schemas/{event_type}",
	ID:          "get_event_schema",
	Summary:     "Get an event predicate schema",
	Description: "Returns the predicate fields of one event type, the fields an event handler's scope_predicate may name. An event type the organization is not licensed for is answered as unknown.",
	PathParams:  map[string]string{"event_type": "The event type's id, from GET /api/event-types."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// GET /api/event-schemas/{event_type}
//
// Returns the predicate field schema for a single event type, or 404 if the
// event_type isn't registered — or (TFAC-524) is gated off for the org,
// deliberately indistinguishable from unknown.
func handleEventSchemaGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	eventType := r.PathValue("event_type")
	sc, found := events.Get(eventType)
	if !found || !entitlements.EventTypeAllowed(orgID, eventType) {
		httpx.WriteErrors(w, http.StatusNotFound, httpx.ErrorItem{
			Reason: httpx.ReasonNotFound, Message: "unknown event type: " + eventType,
		})
		return
	}
	opEventSchemaGet.Write(w, toPayload(sc))
}

// schemaPayload is the wire shape for a single event schema. Intentionally
// narrower than events.EventSchema — the reflect.Type fields and the
// matcher closure have no meaningful JSON form and shouldn't leak.
type schemaPayload struct {
	EventType string               `json:"event_type" doc:"The event type's id."`
	Fields    []events.FieldSchema `json:"fields" doc:"The fields a predicate for this event type may name, ordered by name."`
}

func toPayload(sc events.EventSchema) schemaPayload {
	// Copy + stable-sort fields by name so API responses are deterministic
	// (reflect field order happens to be declaration order, but relying on
	// that in a public API feels fragile).
	fields := append([]events.FieldSchema(nil), sc.Fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return schemaPayload{
		EventType: sc.EventType,
		Fields:    fields,
	}
}
