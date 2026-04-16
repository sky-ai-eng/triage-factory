// Package events is the typed schema registry for every event the system
// emits. For each event type it defines:
//
//   - a Metadata struct (everything the poller captures — the durable audit
//     trail),
//   - a Predicate struct (curated subset, every field optional via `*T`,
//     used by `task_rules` and `prompt_triggers` to scope matches),
//   - a Matches method on the predicate,
//   - and a schema registration (FieldSchema slice for frontend
//     introspection, a type-erased matcher for runtime evaluation).
//
// The event ID constants themselves still live in `internal/domain/event.go`
// — this package is additive, not a rename. The poller rewrite (SKY-178) will
// eventually consolidate the taxonomy here.
//
// See docs/data-model-target.md "Predicate schema" for the full rationale.
package events

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// EventSchema is the typed descriptor for one event_type.
type EventSchema struct {
	EventType     string       `json:"event_type"`
	MetadataType  reflect.Type `json:"-"`
	PredicateType reflect.Type `json:"-"`
	// Fields is the predicate field set, precomputed at registration time
	// so `GET /api/event-schemas` is a map lookup.
	Fields []FieldSchema `json:"fields"`
	// Match evaluates a predicate JSON blob against a metadata JSON blob.
	// Empty predJSON means "no filter" — always matches.
	Match Matcher `json:"-"`
}

// FieldSchema describes one predicate field for the frontend editor.
type FieldSchema struct {
	Name        string   `json:"name"`                  // JSON key
	Type        string   `json:"type"`                  // "bool" | "string" | "int" | "string_list"
	EnumValues  []string `json:"enum_values,omitempty"` // optional, from `enum:"a,b,c"` tag
	Description string   `json:"description,omitempty"` // from `doc:"..."` tag
}

// Matcher is the type-erased predicate evaluator.
type Matcher func(predJSON, metaJSON string) (bool, error)

var (
	mu       sync.RWMutex
	registry = map[string]EventSchema{}
)

// Register adds an EventSchema. Call from each source file's init(). Panics
// on duplicate registration — schemas are compile-time config.
func Register(s EventSchema) {
	mu.Lock()
	defer mu.Unlock()
	if s.EventType == "" {
		panic("events.Register: EventType must be set")
	}
	if _, dup := registry[s.EventType]; dup {
		panic(fmt.Sprintf("events.Register: duplicate registration for %q", s.EventType))
	}
	registry[s.EventType] = s
}

// Get returns the schema for an event type, ok=false if not registered.
func Get(eventType string) (EventSchema, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := registry[eventType]
	return s, ok
}

// All returns a copy of the registry map, keyed by event type.
func All() map[string]EventSchema {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]EventSchema, len(registry))
	for k, v := range registry {
		out[k] = v
	}
	return out
}

// fieldsFromPredicate reflects over a predicate struct type and produces a
// FieldSchema slice. Every predicate field is expected to be a pointer (so
// nil means "no filter"); slice-typed fields represent set-match semantics.
//
// Supported tags:
//
//	json:"field_name,omitempty"  — JSON key (required)
//	doc:"human-readable blurb"    — shown in the predicate editor
//	enum:"a,b,c"                  — dropdown values for string fields
func fieldsFromPredicate(t reflect.Type) []FieldSchema {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("fieldsFromPredicate: expected struct, got %s", t.Kind()))
	}

	var out []FieldSchema
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}

		jsonTag := sf.Tag.Get("json")
		name, _, _ := strings.Cut(jsonTag, ",")
		if name == "" || name == "-" {
			continue
		}

		kind := predicateFieldKind(sf.Type)
		if kind == "" {
			panic(fmt.Sprintf("fieldsFromPredicate: unsupported type %s on %s.%s", sf.Type, t.Name(), sf.Name))
		}

		fs := FieldSchema{
			Name:        name,
			Type:        kind,
			Description: sf.Tag.Get("doc"),
		}
		if enumTag := sf.Tag.Get("enum"); enumTag != "" {
			fs.EnumValues = strings.Split(enumTag, ",")
		}
		out = append(out, fs)
	}
	return out
}

// predicateFieldKind returns the FieldSchema.Type string for a reflect.Type,
// or "" if the type is not a recognised predicate field shape.
func predicateFieldKind(t reflect.Type) string {
	// Predicates use *T to distinguish "filter applied" from "filter absent."
	// Slice types (like []string) represent set-match and are used directly.
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "string_list"
		}
	}
	return ""
}

// newSchema is the internal constructor used by source-specific files. It
// reflects over the predicate type to derive Fields and stitches a type-
// erased matcher on top of the caller-supplied typed matcher.
func newSchema[Meta any, Pred interface{ Matches(Meta) bool }](eventType string) EventSchema {
	var m Meta
	var p Pred
	metaT := reflect.TypeOf(m)
	predT := reflect.TypeOf(p)

	matcher := func(predJSON, metaJSON string) (bool, error) {
		var meta Meta
		if metaJSON != "" {
			if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
				return false, fmt.Errorf("decode %s metadata: %w", eventType, err)
			}
		}
		if predJSON == "" {
			// No predicate = match-all (task_rules with NULL scope_predicate_json).
			return true, nil
		}
		var pred Pred
		if err := json.Unmarshal([]byte(predJSON), &pred); err != nil {
			return false, fmt.Errorf("decode %s predicate: %w", eventType, err)
		}
		return pred.Matches(meta), nil
	}

	return EventSchema{
		EventType:     eventType,
		MetadataType:  metaT,
		PredicateType: predT,
		Fields:        fieldsFromPredicate(predT),
		Match:         matcher,
	}
}
