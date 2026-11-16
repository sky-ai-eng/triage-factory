package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fillEveryField sets every exported field of v to a non-zero value, so an
// omitempty key is on the wire beside the ones that always are.
func fillEveryField(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		if !v.Type().Field(i).IsExported() {
			continue
		}
		f := v.Field(i)
		if f.Kind() == reflect.Pointer {
			f.Set(reflect.New(f.Type().Elem()))
			f = f.Elem()
		}
		switch {
		case f.Type() == reflect.TypeOf(time.Time{}):
			f.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
		case f.Kind() == reflect.String:
			f.SetString("x")
		case f.Kind() == reflect.Int:
			f.SetInt(1)
		case f.Kind() == reflect.Float64:
			f.SetFloat(1)
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		default:
			t.Fatalf("field %s has kind %s, which fillEveryField does not set", v.Type().Field(i).Name, f.Kind())
		}
	}
}

// A conversation marshalled as itself (a blueprint run's steps, the agent
// host's relay) writes the same snake_case keys as the conversation routes.
func TestConversationJSONKeysAreSnakeCase(t *testing.T) {
	var conv Conversation
	fillEveryField(t, reflect.ValueOf(&conv).Elem())
	b, err := json.Marshal(conv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range wire {
		if key != strings.ToLower(key) {
			t.Errorf("key %q holds an uppercase letter", key)
		}
	}
}

// The conversation routes read a place in line and the ended boundary off a
// key's presence, so a conversation that has neither carries none of the keys
// when the struct is marshalled either.
func TestConversationJSONOmitsUnsetPresenceKeys(t *testing.T) {
	b, err := json.Marshal(Conversation{ID: "c1", Status: StatusRunning})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"queue_position", "ended_at", "ended_reason"} {
		if v, ok := wire[key]; ok {
			t.Errorf("%s = %v on a live conversation, want the key absent", key, v)
		}
	}
}
