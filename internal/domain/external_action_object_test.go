package domain

import (
	"slices"
	"testing"
)

// TestActionObjectKind_ClassifiesEveryAction holds the action vocabulary to
// the object kinds: every action either names a kind in ObjectKinds or is one
// of the actions that name none, so an action added without a decision about
// its object fails here.
func TestActionObjectKind_ClassifiesEveryAction(t *testing.T) {
	none := []string{ActionGHChannelWrite, ActionGraphQLWrite, ActionPRReverted, ActionRepoCreated}
	for _, action := range ExternalActionTypes() {
		kind := ActionObjectKind(action)
		switch {
		case slices.Contains(RefusalOrFailureActions(), action) || slices.Contains(none, action):
			if kind != "" {
				t.Errorf("%s names object kind %q, want none", action, kind)
			}
		case !slices.Contains(ObjectKinds(), kind):
			t.Errorf("%s names object kind %q, want one of %v", action, kind, ObjectKinds())
		}
	}
}

// TestActionNamesKind_GitHubPullRequestsAndIssuesShareActions pins the one
// widening of the action's own kind: a GitHub action of a pull request or an
// issue may name either, since GitHub serves both through one set of
// endpoints, while another provider's issue action names only an issue.
func TestActionNamesKind_GitHubPullRequestsAndIssuesShareActions(t *testing.T) {
	cases := []struct {
		provider, action, kind string
		want                   bool
	}{
		{ProviderGitHub, ActionIssueUpdated, ObjectKindIssue, true},
		{ProviderGitHub, ActionIssueUpdated, ObjectKindPullRequest, true},
		{ProviderGitHub, ActionLabelAdded, ObjectKindIssue, true},
		{ProviderGitHub, ActionReactionAdded, ObjectKindIssue, true},
		{ProviderGitHub, ActionReactionAdded, ObjectKindComment, true},
		{ProviderGitHub, ActionCommentPosted, ObjectKindPullRequest, false},
		{ProviderGitHub, ActionBranchPushed, ObjectKindPullRequest, false},
		{ProviderGitHub, ActionGitDenied, ObjectKindPullRequest, false},
		{ProviderGitHub, ActionIssueUpdated, "", false},
		{ProviderJira, ActionIssueUpdated, ObjectKindIssue, true},
		{ProviderJira, ActionIssueUpdated, ObjectKindPullRequest, false},
	}
	for _, c := range cases {
		if got := ActionNamesKind(c.provider, c.action, c.kind); got != c.want {
			t.Errorf("ActionNamesKind(%s, %s, %q) = %v, want %v", c.provider, c.action, c.kind, got, c.want)
		}
	}
}

// TestDeliverableAction_NamesTheObject pins the object a person's act on a
// deliverable names: a submitted review by GitHub's review id, a review whose
// id is unknown none, and a pull request the kind alone, its entity being the
// identity.
func TestDeliverableAction_NamesTheObject(t *testing.T) {
	review := NewReviewDeliverable("octo/api", 7, "PR_kwDOnode", "abc", "conv-1")
	review.ExternalID = "4242"
	act := DeliverableAction(&review, PerformedByPerson, "u", ActionReviewSubmitted,
		DeliverableStateReviewPending, DeliverableStateReviewSubmitted, CredentialGitHubApp)
	if act.ObjectKind != ObjectKindReview || act.ObjectKey != "github:review:4242" {
		t.Errorf("submitted review names (%q, %q), want (review, github:review:4242)", act.ObjectKind, act.ObjectKey)
	}

	unknown := NewReviewDeliverable("octo/api", 7, "PR_kwDOnode", "abc", "conv-1")
	act = DeliverableAction(&unknown, PerformedByPerson, "u", ActionReviewSubmitted,
		DeliverableStateReviewPending, DeliverableStateReviewSubmitted, CredentialGitHubApp)
	if act.ObjectKind != "" || act.ObjectKey != "" {
		t.Errorf("review with no id names (%q, %q), want none", act.ObjectKind, act.ObjectKey)
	}

	pr := NewPullRequestDeliverable("octo/api", 3, "PR_3", "fix", "main", "", "t", "b", true)
	pr.EntityID = "entity-3"
	act = DeliverableAction(&pr, PerformedByPerson, "u", ActionPRClosed, "", "", CredentialGitHubApp)
	if act.ObjectKind != ObjectKindPullRequest || act.ObjectKey != "" {
		t.Errorf("closed pull request names (%q, %q), want (pull_request, \"\")", act.ObjectKind, act.ObjectKey)
	}
}
