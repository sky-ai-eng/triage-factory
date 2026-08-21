package tracker

import (
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The discovery JQL is built from status IDS. A Jira workflow references the
// status entity, so an id keeps a query matching after someone renames the
// status; the name a rule captured at arm time goes stale.

func TestJiraStatusTerms_IDsGoInBare(t *testing.T) {
	// Bare is how JQL is told to read a term as a status id. Quoting it would
	// make it a name — the one thing the id is there to stop mattering.
	got := jiraStatusTerms([]domain.JiraStatusRef{
		{ID: "10000", Name: "To Do"},
		{ID: "10001", Name: "In Progress"},
	})
	if want := "10000, 10001"; got != want {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestJiraStatusTerms_LegacyNameFallback(t *testing.T) {
	// A rule armed before statuses were identified has only names to offer, so
	// the clause falls back to the quoted name it always used.
	got := jiraStatusTerms([]domain.JiraStatusRef{{Name: "To Do"}, {Name: "Backlog"}})
	if want := `"To Do", "Backlog"`; got != want {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestJiraStatusTerms_MixedSet(t *testing.T) {
	// Half-filled sets happen while a team is mid-arming; each ref takes the
	// path it can support.
	got := jiraStatusTerms([]domain.JiraStatusRef{{ID: "10000", Name: "To Do"}, {Name: "Backlog"}})
	if want := `10000, "Backlog"`; got != want {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestJiraStatusTerms_NonNumericIDTakesTheNamePath(t *testing.T) {
	// An id that isn't a number would be read as a NAME if written bare, which
	// would match nothing at all — quietly. The name is the honest fallback.
	got := jiraStatusTerms([]domain.JiraStatusRef{{ID: "not-a-number", Name: "To Do"}})
	if want := `"To Do"`; got != want {
		t.Errorf("terms = %q, want %q", got, want)
	}
}

func TestJiraStatusTerms_EmptySet(t *testing.T) {
	// "" is what the callers test for to drop the clause entirely, rather than
	// emitting `status IN ()`.
	for _, refs := range [][]domain.JiraStatusRef{nil, {}, {{}}} {
		if got := jiraStatusTerms(refs); got != "" {
			t.Errorf("terms(%+v) = %q, want the empty clause", refs, got)
		}
	}
}

// TestJiraRules_AllDoneMembers: the union subtasks are classified against is
// deduplicated on status identity, so one status configured by two projects
// contributes once — and it carries the id, because an inlined subtask reports
// the same status object its parent does.
func TestJiraRules_AllDoneMembers(t *testing.T) {
	rules := JiraRules{
		{Key: "SKY", DoneMembers: []domain.JiraStatusRef{{ID: "1", Name: "Done"}}},
		{Key: "OPS", DoneMembers: []domain.JiraStatusRef{{ID: "2", Name: "Resolved"}, {ID: "1", Name: "Done"}}},
	}
	got := rules.AllDoneMembers()
	want := []domain.JiraStatusRef{{ID: "1", Name: "Done"}, {ID: "2", Name: "Resolved"}}
	if !slices.Equal(got, want) {
		t.Errorf("AllDoneMembers = %v, want %v deduped in first-seen order", got, want)
	}
}
