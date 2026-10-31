package tracker

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestMissingJiraIssues pins the gap the batch-fetch warning reports: the keys
// of the entities that came back in no page, in request order. An entity with
// no answer is skipped by the diff loop and left to the confirmation pass, so
// without this the entity just stops moving — which is also exactly how a
// truncated page would present.
func TestMissingJiraIssues(t *testing.T) {
	entities := []domain.Entity{
		{ID: "e1", SourceID: "SKY-1"},
		{ID: "e2", SourceID: "SKY-2"},
		{ID: "e3", SourceID: "SKY-3"},
	}
	cases := []struct {
		desc     string
		answered []string // entity ids with a result
		want     []string
	}{
		{"every entity answered", []string{"e1", "e2", "e3"}, nil},
		{"one entity absent", []string{"e1", "e3"}, []string{"SKY-2"}},
		{"nothing came back", nil, []string{"SKY-1", "SKY-2", "SKY-3"}},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			results := map[string]jiraIssueState{}
			for _, id := range tc.answered {
				results[id] = jiraIssueState{}
			}
			if got := missingJiraIssues(entities, results); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("missingJiraIssues = %v, want %v", got, tc.want)
			}
		})
	}
}
