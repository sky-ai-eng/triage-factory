package sqlite

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSubstituteLocalJiraIdentity: empty assignee_in becomes single-entry,
// GitHub-namespaced keys are ignored (the GitHub seed-time substitution was
// removed — author-centric owner routing scopes those at event time), and the
// edge cases (empty identity, malformed JSON, non-empty preservation) degrade
// cleanly.
func TestSubstituteLocalJiraIdentity(t *testing.T) {
	const accountID = "557058:abc-aidan"
	cases := []struct {
		name      string
		input     string
		localID   string
		want      map[string]any
		wantInput bool
	}{
		{
			name:    "empty assignee_in → single-entry account ID",
			input:   `{"assignee_in":[]}`,
			localID: accountID,
			want:    map[string]any{"assignee_in": []any{accountID}},
		},
		{
			name:      "non-empty assignee_in preserved verbatim",
			input:     `{"assignee_in":["someone-else"]}`,
			localID:   accountID,
			wantInput: true,
		},
		{
			name:      "github author_in ignored by the Jira helper",
			input:     `{"author_in":[]}`,
			localID:   accountID,
			wantInput: true,
		},
		{
			name:      "empty account ID is no-op (Jira not connected yet)",
			input:     `{"assignee_in":[]}`,
			localID:   "",
			wantInput: true,
		},
		{
			name:      "malformed JSON passes through",
			input:     `not-json`,
			localID:   accountID,
			wantInput: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := substituteLocalJiraIdentity(tc.input, tc.localID)
			if tc.wantInput {
				if got != tc.input {
					t.Errorf("expected passthrough; got %q want %q", got, tc.input)
				}
				return
			}
			var actual map[string]any
			if err := json.Unmarshal([]byte(got), &actual); err != nil {
				t.Fatalf("substituted JSON failed to decode: %v\n%s", err, got)
			}
			if !reflect.DeepEqual(actual, tc.want) {
				t.Errorf("substituted JSON mismatch:\ngot:  %v\nwant: %v", actual, tc.want)
			}
		})
	}
}
