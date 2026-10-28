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

// The validator accepts exactly the keys the predicate editor is offered.
func TestPredicateFieldIndexMatchesAdvertisedFields(t *testing.T) {
	for eventType, s := range All() {
		if s.PredicateType == nil {
			continue
		}
		index := predicateFieldIndex(s.PredicateType)
		if len(index) != len(s.Fields) {
			t.Errorf("%s: %d indexed keys, %d advertised", eventType, len(index), len(s.Fields))
		}
		for _, f := range s.Fields {
			if _, ok := index[f.Name]; !ok {
				t.Errorf("%s: %s not indexed", eventType, f.Name)
			}
		}
	}
}
