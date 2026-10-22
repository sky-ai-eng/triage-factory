package events

import (
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

// TestReviewRequestedPredicate_RequestedIdentityByID pins that the
// requested_user_id_in / requested_team_id_in filters match GitHub's ids for
// the requested user or team, never a handle, and that an event recorded
// without an id matches no such filter.
func TestReviewRequestedPredicate_RequestedIdentityByID(t *testing.T) {
	cases := []struct {
		name string
		pred GitHubPRReviewRequestedPredicate
		meta GitHubPRReviewRequestedMetadata
		want bool
	}{
		{
			name: "user id match",
			pred: GitHubPRReviewRequestedPredicate{RequestedUserIDIn: []string{"9", "5"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice", RequestedUserID: "5"},
			want: true,
		},
		{
			name: "another user id",
			pred: GitHubPRReviewRequestedPredicate{RequestedUserIDIn: []string{"9"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice", RequestedUserID: "5"},
			want: false,
		},
		{
			name: "a login matches nothing",
			pred: GitHubPRReviewRequestedPredicate{RequestedUserIDIn: []string{"alice"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice", RequestedUserID: "5"},
			want: false,
		},
		{
			name: "an event recorded without the id",
			pred: GitHubPRReviewRequestedPredicate{RequestedUserIDIn: []string{"5"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice"},
			want: false,
		},
		{
			name: "team id match",
			pred: GitHubPRReviewRequestedPredicate{RequestedTeamIDIn: []string{"11"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedTeam: "acme/backend", RequestedTeamID: "11"},
			want: true,
		},
		{
			name: "a team handle matches nothing",
			pred: GitHubPRReviewRequestedPredicate{RequestedTeamIDIn: []string{"acme/backend"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedTeam: "acme/backend", RequestedTeamID: "11"},
			want: false,
		},
		{
			name: "a team filter on a user request",
			pred: GitHubPRReviewRequestedPredicate{RequestedTeamIDIn: []string{"5"}},
			meta: GitHubPRReviewRequestedMetadata{RequestedLogin: "alice", RequestedUserID: "5"},
			want: false,
		},
		{
			name: "author_id_in on the author",
			pred: GitHubPRReviewRequestedPredicate{AuthorIDIn: []string{"1"}},
			meta: GitHubPRReviewRequestedMetadata{Author: "aidan", AuthorID: "1", RequestedUserID: "5"},
			want: true,
		},
		{
			name: "empty predicate matches anything",
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

// TestReviewPredicates_ReviewerByID pins reviewer_id_in matching the
// reviewer's GitHub id, not their login, on each review event.
func TestReviewPredicates_ReviewerByID(t *testing.T) {
	approved := GitHubPRReviewApprovedMetadata{Author: "aidan", AuthorID: "1", Reviewer: "alice", ReviewerID: "5"}
	if !(GitHubPRReviewApprovedPredicate{ReviewerIDIn: []string{"5"}}).Matches(approved) {
		t.Error("approved: reviewer_id_in on the reviewer's id did not match")
	}
	if (GitHubPRReviewApprovedPredicate{ReviewerIDIn: []string{"alice"}}).Matches(approved) {
		t.Error("approved: reviewer_id_in holding the reviewer's login matched")
	}
	if (GitHubPRReviewApprovedPredicate{ReviewerIDIn: []string{"1"}}).Matches(approved) {
		t.Error("approved: reviewer_id_in on the author's id matched")
	}
	changes := GitHubPRReviewChangesRequestedMetadata{Author: "aidan", AuthorID: "1", Reviewer: "alice"}
	if (GitHubPRReviewChangesRequestedPredicate{ReviewerIDIn: []string{"5"}}).Matches(changes) {
		t.Error("changes_requested: an event without a reviewer id matched reviewer_id_in")
	}
	if !(GitHubPRReviewChangesRequestedPredicate{AuthorIDIn: []string{"1"}}).Matches(changes) {
		t.Error("changes_requested: author_id_in on the author's id did not match")
	}
}

// TestCommentedPredicate pins commenter_id_in matching the GitHub id of any of
// the new comments' authors, beside the PR-level filters.
func TestCommentedPredicate(t *testing.T) {
	draft := true
	meta := GitHubPRCommentedMetadata{
		Author: "aidan", AuthorID: "1", Commenter: "alice", CommenterLogins: []string{"bob", "alice"},
		CommenterID: "5", CommenterIDs: []string{"6", "5"},
		Repo: "octo/repo", RepositoryID: "repo-1", IsDraft: true, Labels: []string{"needs-reply"},
	}
	cases := []struct {
		name string
		pred GitHubPRCommentedPredicate
		want bool
	}{
		{"empty predicate", GitHubPRCommentedPredicate{}, true},
		{"commenter_id_in names an earlier commenter", GitHubPRCommentedPredicate{CommenterIDIn: []string{"6"}}, true},
		{"commenter_id_in names nobody who commented", GitHubPRCommentedPredicate{CommenterIDIn: []string{"7"}}, false},
		{"commenter_id_in by login matches nothing", GitHubPRCommentedPredicate{CommenterIDIn: []string{"bob"}}, false},
		{"author_id_in on the PR author", GitHubPRCommentedPredicate{AuthorIDIn: []string{"1"}}, true},
		{"author_id_in on a commenter", GitHubPRCommentedPredicate{AuthorIDIn: []string{"5"}}, false},
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

	// A poll whose comments were recorded without ids matches no
	// commenter_id_in list.
	old := GitHubPRCommentedMetadata{Commenter: "alice", CommenterLogins: []string{"alice"}}
	if (GitHubPRCommentedPredicate{CommenterIDIn: []string{"5"}}).Matches(old) {
		t.Error("an event without commenter ids matched commenter_id_in")
	}
}

// TestMentionedPredicate pins the mentioned identity filters folding case, the
// source filter, commenter_id_in matching the commenter's GitHub id, and a body
// mention, which has no commenter, matching no commenter_id_in list.
func TestMentionedPredicate(t *testing.T) {
	comment := GitHubPRMentionedMetadata{
		Author: "aidan", AuthorID: "1", Commenter: "alice", CommenterID: "5", CommentID: 7, MentionedLogin: "bob",
		MentionSource: MentionSourceComment, Repo: "octo/repo",
	}
	team := GitHubPRMentionedMetadata{Author: "aidan", AuthorID: "1", MentionedTeam: "acme/backend", MentionSource: MentionSourceBody, Repo: "octo/repo"}
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
		{"commenter_id_in on the commenter", GitHubPRMentionedPredicate{CommenterIDIn: []string{"5"}}, comment, true},
		{"commenter_id_in by login", GitHubPRMentionedPredicate{CommenterIDIn: []string{"alice"}}, comment, false},
		{"commenter_id_in on a body mention", GitHubPRMentionedPredicate{CommenterIDIn: []string{"1"}}, team, false},
	}
	for _, tc := range cases {
		if got := tc.pred.Matches(tc.meta); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestValidatePredicateJSON_LoginFieldsRefused pins that the identity fields
// that took logins are unknown fields now, on every GitHub event that had them.
func TestValidatePredicateJSON_LoginFieldsRefused(t *testing.T) {
	cases := []struct{ eventType, predicate string }{
		{"github:pr:new_commits", `{"author_in":["aidan"]}`},
		{"github:pr:new_commits", `{"author":"aidan"}`},
		{"github:pr:review_approved", `{"reviewer_in":["alice"]}`},
		{"github:pr:review_approved", `{"reviewer":"alice"}`},
		{"github:pr:review_requested", `{"requested_login":"alice"}`},
		{"github:pr:review_requested", `{"requested_team":"acme/backend"}`},
		{"github:pr:review_request_removed", `{"requested_login":"alice"}`},
		{"github:pr:commented", `{"commenter_in":["alice"]}`},
		{"github:pr:mentioned", `{"commenter_in":["alice"]}`},
		{"github:pr:mentioned", `{"commenter":"alice"}`},
		{"github:pr:body_updated", `{"author_in":["aidan"]}`},
	}
	for _, tc := range cases {
		if _, err := ValidatePredicateJSON(tc.eventType, tc.predicate); err == nil {
			t.Errorf("%s %s validated, want an unknown field", tc.eventType, tc.predicate)
		}
	}
	if _, err := ValidatePredicateJSON("github:pr:mentioned", `{"mentioned_login":"bob","commenter_id_in":["5"]}`); err != nil {
		t.Errorf("mentioned_login beside commenter_id_in refused: %v", err)
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
