package httpx

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func fields(v *Validation) map[string]string {
	out := map[string]string{}
	for _, it := range v.Items() {
		out[it.Field] = it.Reason
	}
	return out
}

func TestSearch_TrimsAndCaps(t *testing.T) {
	var v Validation
	if got := Search(&v, "  ci  "); got != "ci" {
		t.Errorf("Search trimmed = %q, want %q", got, "ci")
	}
	if got := Search(&v, "   "); got != "" {
		t.Errorf("Search whitespace = %q, want empty", got)
	}
	if len(v.Items()) != 0 {
		t.Fatalf("valid needles faulted: %+v", v.Items())
	}

	// The cap is in runes: 200 multi-byte runes pass, 201 fail.
	if Search(&v, strings.Repeat("é", MaxSearchRunes)); len(v.Items()) != 0 {
		t.Fatalf("%d runes refused: %+v", MaxSearchRunes, v.Items())
	}
	Search(&v, strings.Repeat("é", MaxSearchRunes+1))
	if fields(&v)["search"] != ReasonInvalidField {
		t.Fatalf("%d runes: faults = %+v, want INVALID_FIELD on search", MaxSearchRunes+1, v.Items())
	}
}

func TestSearchKey_FoldsCase(t *testing.T) {
	if SearchKey("CI Fail") != SearchKey("ci fail") {
		t.Error("two spellings of one needle fingerprint differently")
	}
}

func TestSearchMatches(t *testing.T) {
	cases := []struct {
		needle string
		fields []string
		want   bool
	}{
		{"", []string{"anything"}, true},
		{"ACME", []string{"x", "acme/widgets"}, true},
		{"50%", []string{"a 50% cut"}, true},
		{"50%", []string{"a 500 cut"}, false},
		{"zzz", []string{"acme"}, false},
	}
	for _, c := range cases {
		if got := SearchMatches(c.needle, c.fields...); got != c.want {
			t.Errorf("SearchMatches(%q, %v) = %v, want %v", c.needle, c.fields, got, c.want)
		}
	}
}

func TestSort(t *testing.T) {
	keys := []SortKey{{Name: "name", Default: db.SortAsc}, {Name: "created_at", Default: db.SortDesc}}

	var v Validation
	if got := Sort(&v, "", "", keys); got != (db.Sort{}) {
		t.Errorf("absent = %+v, want the zero sort", got)
	}
	if got := Sort(&v, "name", "", keys); got != (db.Sort{Key: "name", Dir: db.SortAsc}) {
		t.Errorf("name alone = %+v, want its default asc", got)
	}
	if got := Sort(&v, "created_at", "", keys); got != (db.Sort{Key: "created_at", Dir: db.SortDesc}) {
		t.Errorf("created_at alone = %+v, want its default desc", got)
	}
	if got := Sort(&v, "name", db.SortDesc, keys); !got.Desc() {
		t.Errorf("name desc = %+v, want desc", got)
	}
	if len(v.Items()) != 0 {
		t.Fatalf("valid sorts faulted: %+v", v.Items())
	}

	for _, c := range []struct {
		key, dir, field string
	}{
		{"nope", "", "sort_key"},
		{"name", "sideways", "sort_dir"},
		{"", db.SortAsc, "sort_dir"},
	} {
		var v Validation
		Sort(&v, c.key, c.dir, keys)
		if fields(&v)[c.field] != ReasonInvalidField {
			t.Errorf("Sort(%q, %q): faults = %+v, want INVALID_FIELD on %s", c.key, c.dir, v.Items(), c.field)
		}
	}
}

func TestFacets_NeverNil(t *testing.T) {
	if got := Facets(nil); got == nil {
		t.Error("Facets(nil) = nil, want []")
	}
	got := Facets([]db.Facet{{Value: "urgent", Count: 2}})
	if len(got) != 1 || got[0] != (FacetJSON{Value: "urgent", Count: 2}) {
		t.Errorf("Facets = %+v", got)
	}
}
