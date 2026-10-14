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

// TestCommentedPredicate pins commenter_in matching any of the new comments'
// authors, case-insensitively, beside the PR-level filters.
func TestCommentedPredicate(t *testing.T) {
	draft := true
	meta := GitHubPRCommentedMetadata{
		Author: "aidan", Commenter: "alice", CommenterLogins: []string{"bob", "alice"},
		Repo: "octo/repo", IsDraft: true, Labels: []string{"needs-reply"},
	}
	cases := []struct {
		name string
		pred GitHubPRCommentedPredicate
		want bool
	}{
		{"empty predicate", GitHubPRCommentedPredicate{}, true},
		{"commenter_in names an earlier commenter", GitHubPRCommentedPredicate{CommenterIn: []string{"Bob"}}, true},
		{"commenter_in names nobody who commented", GitHubPRCommentedPredicate{CommenterIn: []string{"carol"}}, false},
		{"author_in on the PR author", GitHubPRCommentedPredicate{AuthorIn: []string{"aidan"}}, true},
		{"author_in on a commenter", GitHubPRCommentedPredicate{AuthorIn: []string{"alice"}}, false},
		{"repo, draft and label", GitHubPRCommentedPredicate{Repo: strPtr("octo/repo"), IsDraft: &draft, HasLabel: strPtr("needs-reply")}, true},
		{"another repo", GitHubPRCommentedPredicate{Repo: strPtr("octo/other")}, false},
		{"a label the PR lacks", GitHubPRCommentedPredicate{HasLabel: strPtr("urgent")}, false},
	}
	for _, tc := range cases {
		if got := tc.pred.Matches(meta); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
