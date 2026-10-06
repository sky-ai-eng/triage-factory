package domain_test

import (
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestLinearStateRefs_RoundTrip(t *testing.T) {
	refs := []domain.LinearStateRef{
		{ID: "s1", Name: "Todo", Type: "unstarted"},
		{ID: "s2", Name: "In Progress", Type: "started"},
	}
	raw, err := domain.MarshalLinearStateRefs(refs)
	if err != nil {
		t.Fatalf("MarshalLinearStateRefs: %v", err)
	}
	got, err := domain.UnmarshalLinearStateRefs(raw)
	if err != nil {
		t.Fatalf("UnmarshalLinearStateRefs: %v", err)
	}
	if !reflect.DeepEqual(got, refs) {
		t.Errorf("round trip = %+v, want %+v", got, refs)
	}
}

func TestLinearStateRefs_EmptyForms(t *testing.T) {
	raw, err := domain.MarshalLinearStateRefs(nil)
	if err != nil || raw != "[]" {
		t.Errorf("MarshalLinearStateRefs(nil) = (%q, %v), want []", raw, err)
	}
	for _, raw := range []string{"", "  ", "[]", "null"} {
		got, err := domain.UnmarshalLinearStateRefs(raw)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("UnmarshalLinearStateRefs(%q) = (%#v, %v), want an empty non-nil slice", raw, got, err)
		}
	}
	if _, err := domain.UnmarshalLinearStateRefs(`["Todo"]`); err == nil {
		t.Error("UnmarshalLinearStateRefs accepted bare names; Linear refs are always objects")
	}
}

func TestLinearStateRef_SameState(t *testing.T) {
	cases := []struct {
		name string
		a, b domain.LinearStateRef
		want bool
	}{
		{"same id, renamed", domain.LinearStateRef{ID: "s1", Name: "Todo"}, domain.LinearStateRef{ID: "s1", Name: "To do"}, true},
		{"different ids, same name", domain.LinearStateRef{ID: "s1", Name: "Todo"}, domain.LinearStateRef{ID: "s2", Name: "Todo"}, false},
		{"one without id, same name", domain.LinearStateRef{Name: "Todo"}, domain.LinearStateRef{ID: "s2", Name: "Todo"}, true},
		{"both empty", domain.LinearStateRef{}, domain.LinearStateRef{}, false},
	}
	for _, tc := range cases {
		if got := tc.a.SameState(tc.b); got != tc.want {
			t.Errorf("%s: SameState = %v, want %v", tc.name, got, tc.want)
		}
	}
	refs := []domain.LinearStateRef{{ID: "s1", Name: "Todo"}}
	if !domain.ContainsState(refs, domain.LinearStateRef{ID: "s1"}) || domain.ContainsState(refs, domain.LinearStateRef{ID: "s9", Name: "Todo"}) {
		t.Error("ContainsState disagrees with SameState")
	}
	if !(domain.LinearStateRef{}).IsZero() || (domain.LinearStateRef{Name: "Todo"}).IsZero() {
		t.Error("IsZero is wrong")
	}
}

func TestLinearStateDedupKey(t *testing.T) {
	if got := domain.LinearStateDedupKey(domain.LinearStateRef{ID: "s1", Name: "Todo"}); got != "id:s1" {
		t.Errorf("key = %q, want id:s1", got)
	}
	if got := domain.LinearStateDedupKey(domain.LinearStateRef{Name: "s1"}); got != "name:s1" {
		t.Errorf("key = %q, want name:s1, which cannot collide with id:s1", got)
	}
}
