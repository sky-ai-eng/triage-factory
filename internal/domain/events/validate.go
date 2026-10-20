package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// ValidatePredicateJSON parses `rawJSON` against the registered predicate
// type for `eventType`. On success it returns the canonical re-marshalling
// of the predicate (stable field order); unknown fields are rejected so the
// DB persists a canonical form; on failure it returns an error describing
// what was wrong.
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
	// TODO(TFAC-1122): a duplicate or empty entry in an _in list is accepted,
	// and every fault returns as one plain error the handler answers 400, so
	// an enum miss is not a 422 and an unknown key carries the decoder's text
	// instead of UNKNOWN_FIELD.
	if err := checkEnums(reflect.ValueOf(predPtr).Elem()); err != nil {
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

// checkEnums refuses a string field set to a value outside its `enum` tag's
// list, the closed set the predicate editor offers for it.
func checkEnums(pred reflect.Value) error {
	t := pred.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		enumTag := sf.Tag.Get("enum")
		if enumTag == "" {
			continue
		}
		f := pred.Field(i)
		if f.Kind() != reflect.Pointer || f.IsNil() || f.Elem().Kind() != reflect.String {
			continue
		}
		allowed := strings.Split(enumTag, ",")
		if v := f.Elem().String(); !slices.Contains(allowed, v) {
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			return fmt.Errorf("%s must be one of %s, not %q", name, strings.Join(allowed, ", "), v)
		}
	}
	return nil
}
