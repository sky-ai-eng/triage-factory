package db

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// The enable-set columns carry three distinguishable states, and the whole
// design rests on telling them apart: NULL is "no preference expressed" and
// resolves to a default, a named set is frozen at what it names, and a set
// naming nothing enables nothing. Anything else is a corrupt row.
func TestModelSetColumn_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []string
		stored any
	}{
		{"absent", nil, nil},
		{"a named set", []string{"a", "b"}, `["a","b"]`},
		// Non-nil and empty. Nothing writes it, but if a row holds it the read
		// must hand back the same thing rather than collapsing it onto nil —
		// the resolvers answer differently for the two.
		{"a set naming nothing", []string{}, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ModelSetColumnValue(tc.models)
			if !reflect.DeepEqual(got, tc.stored) {
				t.Fatalf("ModelSetColumnValue(%#v) = %#v, want %#v", tc.models, got, tc.stored)
			}
			var col sql.NullString
			if s, ok := got.(string); ok {
				col = sql.NullString{String: s, Valid: true}
			}
			back, err := UnmarshalModelSetColumn(col, "org_settings.enabled_models")
			if err != nil {
				t.Fatalf("UnmarshalModelSetColumn: %v", err)
			}
			if !reflect.DeepEqual(back, tc.models) {
				t.Errorf("round-trip = %#v, want %#v", back, tc.models)
			}
			// DeepEqual passes nil against nil and []string{} against
			// []string{}, but the two must not be interchangeable here — state
			// it directly, because it is the property the resolvers key on.
			if (back == nil) != (tc.models == nil) {
				t.Errorf("round-trip lost the nil/non-nil distinction: got %#v, want %#v", back, tc.models)
			}
		})
	}
}

// The empty string is not a second spelling of NULL. Nothing writes it, so a
// row holding it is corrupt — and reading corrupt as absent would resolve to
// the whole catalog, enabling models nobody chose. It fails, naming the column.
func TestUnmarshalModelSetColumn_EmptyStringIsCorrupt(t *testing.T) {
	got, err := UnmarshalModelSetColumn(
		sql.NullString{String: "", Valid: true}, "team_settings.enabled_models")
	if err == nil {
		t.Fatalf("an empty string decoded to %#v, want a decode failure", got)
	}
	if got != nil {
		t.Errorf("a failed decode handed back %#v, want nothing", got)
	}
	if !strings.Contains(err.Error(), "team_settings.enabled_models") {
		t.Errorf("error %q does not name the column to go fix", err)
	}

	// Malformed JSON fails the same way, for the same reason.
	if _, err := UnmarshalModelSetColumn(
		sql.NullString{String: "not json", Valid: true}, "org_settings.enabled_models"); err == nil {
		t.Error("malformed JSON decoded without error")
	}
}
