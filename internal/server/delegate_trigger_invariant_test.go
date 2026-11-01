package server

import (
	"reflect"
	"strings"
	"testing"
)

// TestDelegateRequestStructs_NoTriggerTypeFieldFromAPI pins trigger_type as
// server-side provenance: a request struct carrying a trigger_type tag would let
// a caller send "event" and bypass resolvePrompt's app-pool branch, reading
// prompts outside its RLS visibility.
func TestDelegateRequestStructs_NoTriggerTypeFieldFromAPI(t *testing.T) {
	// Every request body type a delegate-related handler unmarshals into.
	delegateRequestTypes := []any{
		taskCreateRequest{},
		taskDelegateRequest{},
	}

	forbidden := map[string]bool{
		"trigger_type": true,
		"triggertype":  true,
	}

	for _, raw := range delegateRequestTypes {
		typ := reflect.TypeOf(raw)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if !field.IsExported() {
					// encoding/json ignores unexported fields entirely;
					// no caller-input vector here.
					continue
				}
				// encoding/json's name resolution: the tag name (before
				// the first comma) wins when set; otherwise the field's
				// Go name is matched case-insensitively. Both branches
				// must be checked or an untagged `TriggerType string`
				// or a `json:",omitempty"` (tag options without a name)
				// would silently accept caller-supplied "triggerType".
				tag := field.Tag.Get("json")
				name := strings.SplitN(tag, ",", 2)[0]
				if name == "-" {
					// Explicit "skip this field" — encoding/json won't
					// unmarshal into it regardless of payload.
					continue
				}
				if name == "" {
					name = field.Name
				}
				if forbidden[strings.ToLower(name)] {
					t.Errorf("%s.%s would accept caller-supplied triggerType (effective JSON name: %q) — delegate trigger_type must be server-side provenance, never caller input (see internal/delegate/delegate.go DelegateOpts.TriggerType)", typ.Name(), field.Name, name)
				}
			}
		})
	}
}
