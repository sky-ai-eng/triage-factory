package events

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode"
)

// FaultKind is what a PredicateFault finds wrong: the predicate's shape
// against its event type's schema, or a value the shape admits and the field
// does not.
type FaultKind int

const (
	// FaultUnknownField is a key the event type's predicate does not have.
	FaultUnknownField FaultKind = iota + 1
	// FaultInvalidType is a value of the wrong JSON type for its key, or a
	// predicate that is not a JSON object.
	FaultInvalidType
	// FaultInvalidValue is a value of the right type that its field refuses:
	// one outside an enum, or an empty or repeated list entry.
	FaultInvalidValue
)

// PredicateFault is one thing wrong with a predicate.
type PredicateFault struct {
	Kind FaultKind
	// Key is the predicate key the fault is about, empty when the fault is
	// about the predicate as a whole.
	Key     string
	Message string
}

// PredicateError is every fault ValidatePredicateJSON found in a predicate.
type PredicateError struct {
	EventType string
	Faults    []PredicateFault
}

func (e *PredicateError) Error() string {
	messages := make([]string, len(e.Faults))
	for i, f := range e.Faults {
		messages[i] = f.Message
	}
	return fmt.Sprintf("invalid predicate for %s: %s", e.EventType, strings.Join(messages, "; "))
}

// ValidatePredicateJSON parses `rawJSON` against the registered predicate
// type for `eventType`. On success it returns the canonical re-marshalling
// of the predicate (stable field order); on failure it returns a
// *PredicateError naming every fault it found. It refuses an unknown key, a
// known one in a different case included, and a list holding an entry twice,
// so the DB persists one canonical form per meaning.
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

	// Each key is decoded on its own rather than the whole object in one
	// strict decode, which stops at the first fault and words it in the
	// decoder's text.
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
		return "", &PredicateError{EventType: eventType, Faults: []PredicateFault{{
			Kind: FaultInvalidType, Message: "the predicate must be a JSON object",
		}}}
	}
	pred := reflect.New(s.PredicateType).Elem()
	fields := predicateFieldIndex(s.PredicateType)
	var faults []PredicateFault
	for _, key := range slices.Sorted(maps.Keys(object)) {
		i, known := fields[key]
		if !known {
			faults = append(faults, PredicateFault{
				Kind: FaultUnknownField, Key: key, Message: fmt.Sprintf("%s has no predicate field %q", eventType, key),
			})
			continue
		}
		f := pred.Field(i)
		if err := json.Unmarshal(object[key], f.Addr().Interface()); err != nil {
			faults = append(faults, PredicateFault{
				Kind: FaultInvalidType, Key: key, Message: key + " must be " + kindNoun(predicateFieldKind(f.Type())),
			})
			continue
		}
		faults = append(faults, valueFaults(key, s.PredicateType.Field(i), f)...)
	}
	if len(faults) > 0 {
		return "", &PredicateError{EventType: eventType, Faults: faults}
	}

	canonical, err := json.Marshal(pred.Interface())
	if err != nil {
		return "", fmt.Errorf("re-encode predicate: %w", err)
	}
	// If the struct has no non-nil fields after decode, the result is `{}`
	// which we also treat as match-all.
	if string(canonical) == "{}" {
		return "", nil
	}
	return string(canonical), nil
}

// predicateFieldIndex maps each JSON key of a predicate struct to its field
// index, under the rules fieldsFromPredicate advertises the keys by.
func predicateFieldIndex(t reflect.Type) map[string]int {
	index := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		if name, _, _ := strings.Cut(sf.Tag.Get("json"), ","); name != "" && name != "-" {
			index[name] = i
		}
	}
	return index
}

// kindNoun names the JSON a FieldSchema.Type takes.
func kindNoun(kind string) string {
	switch kind {
	case "bool":
		return "true or false"
	case "int":
		return "an integer"
	case "string_list":
		return "a list of strings"
	default:
		return "a string"
	}
}

// valueFaults checks a decoded field for values its type admits and the field
// does not: a string outside its `enum` tag's list, the closed set the
// predicate editor offers for it, and an empty or repeated list entry.
func valueFaults(key string, sf reflect.StructField, f reflect.Value) []PredicateFault {
	if enumTag := sf.Tag.Get("enum"); enumTag != "" && f.Kind() == reflect.Pointer && !f.IsNil() && f.Elem().Kind() == reflect.String {
		allowed := strings.Split(enumTag, ",")
		if v := f.Elem().String(); !slices.Contains(allowed, v) {
			return []PredicateFault{{
				Kind: FaultInvalidValue, Key: key, Message: fmt.Sprintf("%s must be one of %s, not %q", key, strings.Join(allowed, ", "), v),
			}}
		}
	}
	if list, ok := f.Interface().([]string); ok {
		return listFaults(key, list)
	}
	return nil
}

// listFaults refuses an empty entry, which matches only events that lack the
// value, and a repeated one: a list is a set, and its stored text is the one
// form of what it means, which the rule editor compares to tell whether a
// predicate changed. Two entries repeat when they are equal ignoring case.
// That is how the identity lists match, and every other list holds ids, each
// a number or a UUID, which letter case does not distinguish, so one
// comparison holds for every list.
func listFaults(key string, list []string) []PredicateFault {
	var faults []PredicateFault
	first := make(map[string]int, len(list))
	for i, entry := range list {
		if entry == "" {
			faults = append(faults, PredicateFault{
				Kind: FaultInvalidValue, Key: key, Message: fmt.Sprintf("%s[%d] is empty", key, i),
			})
			continue
		}
		folded := foldKey(entry)
		if j, seen := first[folded]; seen {
			faults = append(faults, PredicateFault{
				Kind: FaultInvalidValue, Key: key, Message: fmt.Sprintf("%s[%d] %q repeats %s[%d] %q; entries are compared ignoring case", key, i, entry, key, j, list[j]),
			})
			continue
		}
		first[folded] = i
	}
	return faults
}

// foldKey maps s to one representative of the strings strings.EqualFold
// counts equal to it: each rune becomes the least rune of its case-folding
// orbit, which is the equivalence EqualFold tests rune by rune.
func foldKey(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}
