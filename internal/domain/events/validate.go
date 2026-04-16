package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// ValidatePredicateJSON parses `rawJSON` against the registered predicate
// type for `eventType`. On success it returns the canonical re-marshalling
// of the predicate (stable field order, unknown fields dropped) so the DB
// persists a canonical form; on failure it returns an error describing what
// was wrong.
//
// An empty / whitespace-only rawJSON is treated as "match-all" and returns
// "" with no error — callers should persist NULL in that case.
//
// Used by `task_rules` and `prompt_triggers` CRUD handlers.
func ValidatePredicateJSON(eventType, rawJSON string) (string, error) {
	trimmed := strings.TrimSpace(rawJSON)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return "", nil
	}

	s, ok := Get(eventType)
	if !ok {
		return "", fmt.Errorf("unknown event type %q", eventType)
	}

	predPtr := reflect.New(s.PredicateType).Interface()
	dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(predPtr); err != nil {
		return "", fmt.Errorf("invalid predicate for %s: %w", eventType, err)
	}

	canonical, err := json.Marshal(predPtr)
	if err != nil {
		return "", fmt.Errorf("re-encode predicate: %w", err)
	}
	// reflect.New returned a pointer, so predPtr serialises as the struct —
	// good. If the struct has no non-nil fields after decode, the result is
	// `{}` which we also treat as match-all.
	if string(canonical) == "{}" {
		return "", nil
	}
	return string(canonical), nil
}
