package dbtest

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunExternalActionCommentKeysConformance covers the tracker's read of which
// comments TF posted, PresentPostedCommentKeysSystem. It answers for the
// object key of a comment_posted or issue_comment_posted row on the scope it
// is asked about, whoever performed it, across GitHub, Jira and Linear. It
// does not answer for a key whose only rows are another action on the same
// comment (an edit, a reaction), for a key posted on another scope, or for a
// key nothing recorded. An empty key list is an empty answer, and an empty
// scope is refused. It runs on the node-id suite's fixture.
func RunExternalActionCommentKeysConformance(t *testing.T, mk ExternalActionNodeIDFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		hostA           = TestGitHubHost
		hostB           = TestOtherGitHubHost
		jiraSite        = "https://acme.atlassian.net"
		linearWorkspace = "ws-comment-keys"
	)
	f := mk(t)
	comment := func(provider, scope, action, performedBy, key string) domain.ExternalAction {
		a := domain.ExternalAction{
			ID:             uuid.NewString(),
			TeamID:         f.TeamID,
			Provider:       provider,
			Scope:          scope,
			Action:         action,
			Target:         "octo/api#7",
			ConversationID: f.ConversationID,
			Credential:     domain.CredentialGitHubApp,
			PerformedBy:    performedBy,
		}
		a.SetObject(domain.ObjectKindComment, key)
		return a
	}
	gh := domain.ProviderGitHub
	var (
		conversation  = domain.GitHubCommentKey(domain.GitHubIssueComment, 1001)
		threadReply   = domain.GitHubCommentKey(domain.GitHubReviewComment, 1002)
		edited        = domain.GitHubCommentKey(domain.GitHubIssueComment, 1003)
		reactedTo     = domain.GitHubCommentKey(domain.GitHubIssueComment, 1004)
		otherHost     = domain.GitHubCommentKey(domain.GitHubIssueComment, 1005)
		absent        = domain.GitHubCommentKey(domain.GitHubIssueComment, 1006)
		jiraComment   = domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindComment, domain.JiraIssueResource(jiraSite, "10042"), "20001")
		linearComment = domain.ObjectKey(domain.ProviderLinear, domain.DeliverableKindComment, "issue-uuid-1", "comment-uuid-1")
	)
	for _, a := range []domain.ExternalAction{
		comment(gh, hostA, domain.ActionCommentPosted, domain.PerformedByAgent, conversation),
		comment(gh, hostA, domain.ActionCommentPosted, domain.PerformedByPerson, threadReply),
		comment(gh, hostA, domain.ActionCommentEdited, domain.PerformedByAgent, edited),
		comment(gh, hostA, domain.ActionReactionAdded, domain.PerformedByAgent, reactedTo),
		comment(gh, hostB, domain.ActionCommentPosted, domain.PerformedByAgent, otherHost),
		comment(domain.ProviderJira, jiraSite, domain.ActionIssueCommentPosted, domain.PerformedByAgent, jiraComment),
		comment(domain.ProviderLinear, linearWorkspace, domain.ActionIssueCommentPosted, domain.PerformedBySystem, linearComment),
	} {
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, a); err != nil {
			t.Fatalf("record %s %s: %v", a.Action, a.ObjectKey, err)
		}
	}

	for _, c := range []struct {
		name  string
		scope string
		keys  []string
		want  map[string]bool
	}{
		{"github", hostA, []string{conversation, threadReply, edited, reactedTo, otherHost, absent},
			map[string]bool{conversation: true, threadReply: true}},
		{"other_host", hostB, []string{conversation, otherHost}, map[string]bool{otherHost: true}},
		{"jira", jiraSite, []string{jiraComment, conversation}, map[string]bool{jiraComment: true}},
		{"linear", linearWorkspace, []string{linearComment}, map[string]bool{linearComment: true}},
		{"no_keys", hostA, nil, map[string]bool{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := f.Stores.ExternalActions.PresentPostedCommentKeysSystem(ctx, f.OrgID, c.scope, c.keys)
			if err != nil {
				t.Fatalf("PresentPostedCommentKeysSystem: %v", err)
			}
			if got == nil || !maps.Equal(got, c.want) {
				t.Errorf("present = %v, want %v", got, c.want)
			}
		})
	}
	if _, err := f.Stores.ExternalActions.PresentPostedCommentKeysSystem(ctx, f.OrgID, "", []string{conversation}); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
		t.Errorf("PresentPostedCommentKeysSystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
	}
}
