package events

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// predicateFaults validates pred and returns the faults it was refused for,
// failing the test if the refusal is not a *PredicateError.
func predicateFaults(t *testing.T, eventType, pred string) []PredicateFault {
	t.Helper()
	got, err := ValidatePredicateJSON(eventType, pred)
	if err == nil {
		t.Fatalf("%s validated as %q, want it refused", pred, got)
	}
	var pe *PredicateError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: error %v is not a *PredicateError", pred, err)
	}
	return pe.Faults
}

// Every fault in a predicate is reported, each naming its key and its kind,
// and none in the decoder's words.
func TestValidatePredicateJSON_ReportsEveryFault(t *testing.T) {
	faults := predicateFaults(t, domain.EventGitHubPRMentioned,
		`{"repo":"acme/api","author_id_in":"583231","mention_source":"issue","commenter_id_in":["5","","5"],"is_draft":"yes"}`)
	want := []struct {
		kind FaultKind
		key  string
	}{
		{FaultInvalidType, "author_id_in"},
		{FaultInvalidValue, "commenter_id_in"},
		{FaultInvalidValue, "commenter_id_in"},
		{FaultInvalidType, "is_draft"},
		{FaultInvalidValue, "mention_source"},
		{FaultUnknownField, "repo"},
	}
	if len(faults) != len(want) {
		t.Fatalf("faults = %+v, want %d", faults, len(want))
	}
	for i, w := range want {
		f := faults[i]
		if f.Kind != w.kind || f.Key != w.key {
			t.Errorf("fault %d = %+v, want kind %d on %s", i, f, w.kind, w.key)
		}
		if strings.Contains(f.Message, "json:") || !strings.Contains(f.Message, w.key) {
			t.Errorf("fault %d message %q should name %s in TF's own words", i, f.Message, w.key)
		}
	}
}

// A list entry that equals an earlier one ignoring case repeats it, on an
// identity list that matches ignoring case and on an id list alike; entries
// that differ are kept in the order given.
func TestValidatePredicateJSON_RefusesRepeatedEntries(t *testing.T) {
	for _, tc := range []struct{ eventType, pred string }{
		{domain.EventJiraIssueAssigned, `{"assignee_in":["557058:abc","557058:ABC"]}`},
		{domain.EventGitHubPRCommented, `{"commenter_id_in":["5","6","5"]}`},
		{domain.EventLinearIssueAssigned, `{"linear_team_id_in":["a1b2c3d4-0000-4000-8000-000000000001","A1B2C3D4-0000-4000-8000-000000000001"]}`},
		{domain.EventJiraIssueAssigned, `{"assignee_in":["ſam","sam"]}`},
	} {
		faults := predicateFaults(t, tc.eventType, tc.pred)
		if len(faults) != 1 || faults[0].Kind != FaultInvalidValue || !strings.Contains(faults[0].Message, "repeats") {
			t.Errorf("%s: faults = %+v, want one repeated entry", tc.pred, faults)
		}
	}

	got, err := ValidatePredicateJSON(domain.EventJiraIssueAssigned, `{"assignee_in":["b","a"]}`)
	if err != nil || got != `{"assignee_in":["b","a"]}` {
		t.Errorf("distinct entries = (%q, %v), want them kept in order", got, err)
	}
}

// Keys match the schema exactly, so a key spelled in another case is not a
// second spelling of the field.
func TestValidatePredicateJSON_KeysAreCaseSensitive(t *testing.T) {
	faults := predicateFaults(t, domain.EventGitHubPROpened, `{"Author_ID_In":["583231"]}`)
	if len(faults) != 1 || faults[0].Kind != FaultUnknownField || faults[0].Key != "Author_ID_In" {
		t.Errorf("faults = %+v, want Author_ID_In unknown", faults)
	}
}

func TestValidatePredicateJSON_RefusesANonObject(t *testing.T) {
	for _, pred := range []string{`["583231"]`, `"author_id_in"`, `{"author_id_in":`} {
		faults := predicateFaults(t, domain.EventGitHubPROpened, pred)
		if len(faults) != 1 || faults[0].Kind != FaultInvalidType || faults[0].Key != "" {
			t.Errorf("%s: faults = %+v, want one about the whole predicate", pred, faults)
		}
	}
}

// The list check is generic: every string_list field of every registered
// schema refuses an empty entry and a repeated one.
func TestValidatePredicateJSON_EveryListRefusesEmptyAndRepeatedEntries(t *testing.T) {
	checked := 0
	for eventType, s := range All() {
		for _, f := range s.Fields {
			if f.Type != "string_list" {
				continue
			}
			checked++
			for _, list := range []string{`[""]`, `["x","X"]`} {
				faults := predicateFaults(t, eventType, fmt.Sprintf(`{%q:%s}`, f.Name, list))
				if len(faults) != 1 || faults[0].Kind != FaultInvalidValue || faults[0].Key != f.Name {
					t.Errorf("%s %s=%s: faults = %+v, want one value fault", eventType, f.Name, list, faults)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no string_list predicate field registered")
	}
}

func TestFoldKeyAgreesWithEqualFold(t *testing.T) {
	words := []string{"a", "A", "k", "K", "K", "s", "S", "ſ", "ß", "ẞ", "σ", "ς", "Σ", "i", "I", "ı", "İ", "", "ab", "aB"}
	for _, a := range words {
		for _, b := range words {
			if got, want := foldKey(a) == foldKey(b), strings.EqualFold(a, b); got != want {
				t.Errorf("foldKey(%q) == foldKey(%q) is %v, EqualFold says %v", a, b, got, want)
			}
		}
	}
}

// pointerPred is a predicate whose matcher has a pointer receiver, so it is
// registered by its pointer type.
type pointerPred struct {
	Kind  *string  `json:"kind,omitempty" enum:"a,b"`
	IDsIn []string `json:"ids_in,omitempty"`
}

func (p *pointerPred) Matches(struct{}) bool { return true }

// A predicate type registered by pointer validates as its struct does.
func TestValidatePredicateJSON_PointerPredicateType(t *testing.T) {
	const eventType = "test:pointer_predicate"
	Register(NewSchema[struct{}, *pointerPred](eventType, OwnershipPool))
	t.Cleanup(func() { Reset(eventType) })

	got, err := ValidatePredicateJSON(eventType, `{"ids_in":["1"],"kind":"a"}`)
	if err != nil || got != `{"kind":"a","ids_in":["1"]}` {
		t.Errorf("valid predicate = (%q, %v), want it canonical", got, err)
	}
	faults := predicateFaults(t, eventType, `{"ids_in":["1","1"],"kind":"c","other":true}`)
	if len(faults) != 3 {
		t.Errorf("faults = %+v, want the repeat, the enum miss and the unknown key", faults)
	}
}

// An edit is refused for an empty or repeated entry only in a list it
// changes: a list re-sent exactly as the row stores it is kept as it is.
func TestValidatePredicateEdit_KeepsAStoredList(t *testing.T) {
	prior := `{"author_id_in":["123","123"],"commenter_id_in":["",""]}`
	got, err := ValidatePredicateEdit(domain.EventGitHubPRCommented, `{"commenter_id_in":["",""],"author_id_in":["123","123"]}`, &prior)
	if err != nil || got != prior {
		t.Errorf("re-sent stored lists = (%q, %v), want them kept", got, err)
	}

	_, err = ValidatePredicateEdit(domain.EventGitHubPRCommented, `{"author_id_in":["123","123","5"],"commenter_id_in":["",""]}`, &prior)
	var pe *PredicateError
	if !errors.As(err, &pe) || len(pe.Faults) != 1 || pe.Faults[0].Key != "author_id_in" {
		t.Errorf("changed list: err = %v, want one repeat on author_id_in", err)
	}
}

// A predicate refused only for its values hands back the rest of it, without
// what it refuses, for the caller's other checks to read; a schema fault
// leaves nothing to read.
func TestValidatePredicateJSON_Remainder(t *testing.T) {
	for _, tc := range []struct {
		pred string
		want string // "" for no remainder
	}{
		{`{"commenter_id_in":["5","","5","6"],"mention_source":"issue","is_draft":true}`, `{"commenter_id_in":["5","6"],"is_draft":true}`},
		{`{"commenter_id_in":["",""]}`, ""},
		{`{"commenter_id_in":["5","5"],"repo":"acme/api"}`, ""},
		{`{"commenter_id_in":["5","5"],"is_draft":"yes"}`, ""},
	} {
		_, err := ValidatePredicateJSON(domain.EventGitHubPRMentioned, tc.pred)
		var pe *PredicateError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: err = %v, want a *PredicateError", tc.pred, err)
		}
		got := ""
		if pe.Remainder != nil {
			got = *pe.Remainder
		}
		if got != tc.want {
			t.Errorf("%s: remainder = %q, want %q", tc.pred, got, tc.want)
		}
	}
}
