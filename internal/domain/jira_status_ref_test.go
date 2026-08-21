package domain_test

import (
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The stored form of a status rule's members. Two shapes are readable and one
// is writable: rules write {id,name} objects, and rows written before statuses
// were identified — bare names — stay readable forever, because they are live
// data rather than a migration that has not run.

func TestJiraStatusRefs_RoundTrip(t *testing.T) {
	refs := []domain.JiraStatusRef{{ID: "10000", Name: "To Do"}, {ID: "10001", Name: "In Progress"}}
	raw, err := domain.MarshalJiraStatusRefs(refs)
	if err != nil {
		t.Fatalf("MarshalJiraStatusRefs: %v", err)
	}
	got, err := domain.UnmarshalJiraStatusRefs(raw)
	if err != nil {
		t.Fatalf("UnmarshalJiraStatusRefs: %v", err)
	}
	if !reflect.DeepEqual(got, refs) {
		t.Errorf("round trip = %+v, want %+v", got, refs)
	}
}

func TestJiraStatusRefs_NilMarshalsAsEmptyArray(t *testing.T) {
	// The CHECK constraints recognize one empty form, and a nil slice rendering
	// as `null` would not be it.
	raw, err := domain.MarshalJiraStatusRefs(nil)
	if err != nil {
		t.Fatalf("MarshalJiraStatusRefs: %v", err)
	}
	if raw != "[]" {
		t.Errorf("nil members marshalled as %q, want []", raw)
	}
}

func TestJiraStatusRefs_LegacyNameArray(t *testing.T) {
	got, err := domain.UnmarshalJiraStatusRefs(`["To Do", "Backlog"]`)
	if err != nil {
		t.Fatalf("UnmarshalJiraStatusRefs on the legacy shape: %v", err)
	}
	want := []domain.JiraStatusRef{{Name: "To Do"}, {Name: "Backlog"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("legacy members = %+v, want names with no ids", got)
	}
}

func TestJiraStatusRefs_EmptyColumn(t *testing.T) {
	for _, raw := range []string{"", "   ", "[]"} {
		got, err := domain.UnmarshalJiraStatusRefs(raw)
		if err != nil {
			t.Fatalf("UnmarshalJiraStatusRefs(%q): %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("UnmarshalJiraStatusRefs(%q) = %+v, want empty", raw, got)
		}
	}
}

func TestJiraStatusRef_CanonicalRoundTrip(t *testing.T) {
	ref := domain.JiraStatusRef{ID: "10002", Name: "Done"}
	raw, err := domain.MarshalJiraStatusRef(ref)
	if err != nil {
		t.Fatalf("MarshalJiraStatusRef: %v", err)
	}
	got, err := domain.UnmarshalJiraStatusRef(raw)
	if err != nil {
		t.Fatalf("UnmarshalJiraStatusRef: %v", err)
	}
	if got != ref {
		t.Errorf("round trip = %+v, want %+v", got, ref)
	}
}

func TestJiraStatusRef_UnsetCanonicalMarshalsEmpty(t *testing.T) {
	// "" is what the stores turn into SQL NULL, which is the form the
	// complete-or-empty CHECK constraints expect on an unmapped rule.
	raw, err := domain.MarshalJiraStatusRef(domain.JiraStatusRef{})
	if err != nil {
		t.Fatalf("MarshalJiraStatusRef: %v", err)
	}
	if raw != "" {
		t.Errorf("unset canonical marshalled as %q, want the empty string", raw)
	}
	got, err := domain.UnmarshalJiraStatusRef("")
	if err != nil {
		t.Fatalf("UnmarshalJiraStatusRef(\"\"): %v", err)
	}
	if !got.IsZero() {
		t.Errorf("empty column = %+v, want the zero ref", got)
	}
}

func TestJiraStatusRef_LegacyBareName(t *testing.T) {
	// The canonical column held the display name itself, unquoted, before
	// statuses were identified.
	got, err := domain.UnmarshalJiraStatusRef("In Progress")
	if err != nil {
		t.Fatalf("UnmarshalJiraStatusRef on the legacy shape: %v", err)
	}
	if got != (domain.JiraStatusRef{Name: "In Progress"}) {
		t.Errorf("legacy canonical = %+v, want a name with no id", got)
	}
}

func TestJiraStatusRef_SameStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b domain.JiraStatusRef
		want bool
	}{
		{"ids decide", domain.JiraStatusRef{ID: "1", Name: "Old"}, domain.JiraStatusRef{ID: "1", Name: "New"}, true},
		{"different ids", domain.JiraStatusRef{ID: "1"}, domain.JiraStatusRef{ID: "2"}, false},
		{"names when one has no id", domain.JiraStatusRef{Name: "Done"}, domain.JiraStatusRef{ID: "3", Name: "Done"}, true},
		{"zero matches nothing", domain.JiraStatusRef{}, domain.JiraStatusRef{Name: "Done"}, false},
	} {
		if got := tc.a.SameStatus(tc.b); got != tc.want {
			t.Errorf("%s: SameStatus = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestJiraProjectStatusRules_Armed: a legacy name-only row is armed on exactly
// the same terms as an identified one — arming is about the rules being
// complete, not about how their statuses are referenced.
func TestJiraProjectStatusRules_Armed(t *testing.T) {
	armed := domain.JiraProjectStatusRules{
		PickupMembers:       []domain.JiraStatusRef{{Name: "To Do"}},
		InProgressMembers:   []domain.JiraStatusRef{{Name: "In Progress"}},
		InProgressCanonical: domain.JiraStatusRef{Name: "In Progress"},
		DoneMembers:         []domain.JiraStatusRef{{Name: "Done"}},
		DoneCanonical:       domain.JiraStatusRef{Name: "Done"},
	}
	if !armed.Armed() {
		t.Error("a complete legacy row should read as armed")
	}
	unmapped := armed
	unmapped.DoneCanonical = domain.JiraStatusRef{}
	unmapped.DoneMembers = nil
	if unmapped.Armed() {
		t.Error("a row with an unmapped done rule should not read as armed")
	}
}
