package server

import (
	"net/http"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// GET /api/event-schemas
//
// Returns a map keyed by event_type, where each value is the predicate field
// schema for that event type. Used by the frontend's predicate editor to
// render typed inputs (checkbox for bools, dropdown for enums, text for
// strings, etc.).
func (s *Server) handleEventSchemasList(w http.ResponseWriter, r *http.Request) {
	all := events.All()

	out := make(map[string]schemaPayload, len(all))
	for _, sc := range all {
		out[sc.EventType] = toPayload(sc)
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/event-schemas/{event_type}
//
// Returns the predicate field schema for a single event type, or 404 if
// the event_type isn't registered.
func (s *Server) handleEventSchemaGet(w http.ResponseWriter, r *http.Request) {
	eventType := r.PathValue("event_type")
	sc, ok := events.Get(eventType)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown event type: " + eventType})
		return
	}
	writeJSON(w, http.StatusOK, toPayload(sc))
}

// schemaPayload is the wire shape for a single event schema. Intentionally
// narrower than events.EventSchema — the reflect.Type fields and the
// matcher closure have no meaningful JSON form and shouldn't leak.
type schemaPayload struct {
	EventType string               `json:"event_type"`
	Fields    []events.FieldSchema `json:"fields"`
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
