package events

import "testing"

func strPtr(s string) *string { return &s }

// TestReviewRequestedPredicate_RequestedIdentityCaseInsensitive pins that the
// requested_login / requested_team filters fold case — GitHub logins and team
// handles are case-insensitive, so a rule typed "Alice" must match an event
// carrying "alice".
func TestReviewRequestedPredicate_RequestedIdentityCaseInsensitive(t *testing.T) {
	cases := []struct {
		name string
		pred GitHubPRReviewRequestedPredicate
		meta GitHubPRReviewRequestedMetadata
		want bool
	}{
		{
			name: "login fold match",
			pred: GitHubPRReviewRequestedPredicate{RequestedLogin: strPtr("Alice")},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice"},
			want: true,
		},
		{
			name: "login mismatch",
			pred: GitHubPRReviewRequestedPredicate{RequestedLogin: strPtr("bob")},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice"},
			want: false,
		},
		{
			name: "team fold match",
			pred: GitHubPRReviewRequestedPredicate{RequestedTeam: strPtr("Acme/Backend")},
			meta: GitHubPRReviewRequestedMetadata{RequestedTeam: "acme/backend"},
			want: true,
		},
		{
			name: "nil predicate matches anything",
			pred: GitHubPRReviewRequestedPredicate{},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice"},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pred.Matches(tc.meta); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}
