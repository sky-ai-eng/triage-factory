package events

import (
	"strings"
	"testing"
)

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
		Repo: "octo/repo", RepositoryID: "repo-1", IsDraft: true, Labels: []string{"needs-reply"},
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
		{"repo, draft and label", GitHubPRCommentedPredicate{RepoIn: []string{"repo-1"}, IsDraft: &draft, HasLabel: strPtr("needs-reply")}, true},
		{"repo_in names the repository among others", GitHubPRCommentedPredicate{RepoIn: []string{"repo-2", "repo-1"}}, true},
		{"another repository", GitHubPRCommentedPredicate{RepoIn: []string{"repo-2"}}, false},
		{"repo_in by name matches nothing", GitHubPRCommentedPredicate{RepoIn: []string{"octo/repo"}}, false},
		{"a label the PR lacks", GitHubPRCommentedPredicate{HasLabel: strPtr("urgent")}, false},
	}
	for _, tc := range cases {
		if got := tc.pred.Matches(meta); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMentionedPredicate pins the mentioned identity filters folding case, the
// source filter, and a body mention, which has no commenter, matching no
// commenter_in list.
func TestMentionedPredicate(t *testing.T) {
	comment := GitHubPRMentionedMetadata{
		Author: "aidan", Commenter: "alice", CommentID: 7, MentionedLogin: "bob",
		MentionSource: MentionSourceComment, Repo: "octo/repo",
	}
	team := GitHubPRMentionedMetadata{Author: "aidan", MentionedTeam: "acme/backend", MentionSource: MentionSourceBody, Repo: "octo/repo"}
	cases := []struct {
		name string
		pred GitHubPRMentionedPredicate
		meta GitHubPRMentionedMetadata
		want bool
	}{
		{"empty predicate", GitHubPRMentionedPredicate{}, comment, true},
		{"mentioned_login folds case", GitHubPRMentionedPredicate{MentionedLogin: strPtr("Bob")}, comment, true},
		{"mentioned_login on someone else", GitHubPRMentionedPredicate{MentionedLogin: strPtr("carol")}, comment, false},
		{"mentioned_login on a team mention", GitHubPRMentionedPredicate{MentionedLogin: strPtr("bob")}, team, false},
		{"mentioned_team folds case", GitHubPRMentionedPredicate{MentionedTeam: strPtr("Acme/Backend")}, team, true},
		{"mention_source", GitHubPRMentionedPredicate{MentionSource: strPtr(MentionSourceComment)}, comment, true},
		{"another mention_source", GitHubPRMentionedPredicate{MentionSource: strPtr(MentionSourceReview)}, comment, false},
		{"commenter_in on the commenter", GitHubPRMentionedPredicate{CommenterIn: []string{"Alice"}}, comment, true},
		{"commenter_in on a body mention", GitHubPRMentionedPredicate{CommenterIn: []string{"aidan"}}, team, false},
	}
	for _, tc := range cases {
		if got := tc.pred.Matches(tc.meta); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestValidatePredicateJSON_Enum pins that a field with a closed set of values
// refuses one outside it, naming the field, and keeps one inside it.
func TestValidatePredicateJSON_Enum(t *testing.T) {
	got, err := ValidatePredicateJSON("github:pr:mentioned", `{"mention_source":"review_comment"}`)
	if err != nil || got != `{"mention_source":"review_comment"}` {
		t.Errorf("valid source = (%q, %v), want it kept", got, err)
	}
	for _, bad := range []string{`{"mention_source":"Comment"}`, `{"mention_source":""}`, `{"mention_source":"issue"}`} {
		if _, err := ValidatePredicateJSON("github:pr:mentioned", bad); err == nil {
			t.Errorf("%s validated, want it refused", bad)
		} else if !strings.Contains(err.Error(), "mention_source") {
			t.Errorf("%s: error %q does not name the field", bad, err)
		}
	}
}
