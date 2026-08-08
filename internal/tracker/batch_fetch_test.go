package tracker

import (
	"fmt"
	"testing"
)

// TestMissingJiraKeys pins the gap the batch-fetch warning reports: keys that
// came back in no page, in request order. A key absent from the response is
// skipped by the diff loop, so without this the entity just stops moving —
// which is also exactly how a truncated page would present.
func TestMissingJiraKeys(t *testing.T) {
	cases := []struct {
		desc     string
		keys     []string
		returned []string
		want     []string
	}{
		{"every key answered", []string{"SKY-1", "SKY-2"}, []string{"SKY-1", "SKY-2"}, nil},
		{"one key absent", []string{"SKY-1", "SKY-2", "SKY-3"}, []string{"SKY-1", "SKY-3"}, []string{"SKY-2"}},
		{"nothing came back", []string{"SKY-1", "SKY-2"}, nil, []string{"SKY-1", "SKY-2"}},
		// A moved issue answers under its new key: the tracked one is absent,
		// and the substitute must not paper over it.
		{"moved issue", []string{"SKY-1"}, []string{"OPS-9"}, []string{"SKY-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			results := map[string]jiraIssueState{}
			for _, k := range tc.returned {
				results[k] = jiraIssueState{}
			}
			if got := missingJiraKeys(tc.keys, results); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("missingJiraKeys(%v) = %v, want %v", tc.keys, got, tc.want)
			}
		})
	}
}
