package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunExternalActionPostedCommentConformance covers the read touch's lookup of
// the entity a comment TF posted is on, PostedCommentEntitySystem. It answers
// with the entity of the newest comment_posted row on the scope asked about
// whose node_id is the one asked for and that names an entity. It answers ""
// for a node id only another act's row carries (an edit, a pr_created row),
// for one posted on another scope, for a posted comment's row that names no
// entity, and for a node id nothing recorded. An empty node id is an empty
// answer, and an empty scope is refused. It runs on the node-id suite's
// fixture.
func RunExternalActionPostedCommentConformance(t *testing.T, mk ExternalActionNodeIDFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		hostA = TestGitHubHost
		hostB = TestOtherGitHubHost
	)
	f := mk(t)
	entity := func(scope, key, nodeID string) string {
		t.Helper()
		e, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, domain.ProviderGitHub, scope, key, nodeID, domain.EntityKindPullRequest, "", "")
		if err != nil {
			t.Fatalf("seed entity %s: %v", key, err)
		}
		return e.ID
	}
	var (
		first  = entity(hostA, "octo/api#7", "PR_kw7")
		second = entity(hostA, "octo/api#8", "PR_kw8")
		onB    = entity(hostB, "octo/api#7", "PR_kwB7")
	)
	record := func(scope, action, nodeID, entityID string, age time.Duration) {
		t.Helper()
		a := domain.ExternalAction{
			ID:             uuid.NewString(),
			TeamID:         f.TeamID,
			Provider:       domain.ProviderGitHub,
			Scope:          scope,
			Action:         action,
			Target:         "octo/api#7",
			ConversationID: f.ConversationID,
			Credential:     domain.CredentialGitHubApp,
			PerformedBy:    domain.PerformedByAgent,
			NodeID:         nodeID,
			EntityID:       entityID,
		}
		switch action {
		case domain.ActionCommentPosted, domain.ActionCommentEdited:
			a.SetObject(domain.ObjectKindComment, domain.GitHubCommentKey(domain.GitHubIssueComment, int64(len(nodeID))+1000))
		case domain.ActionPRCreated:
			a.SetObject(domain.ObjectKindPullRequest, "")
		}
		if err := f.Stores.ExternalActions.RecordSystem(ctx, f.OrgID, a); err != nil {
			t.Fatalf("record %s %s: %v", action, nodeID, err)
		}
		if age > 0 {
			f.BackdateAction(t, a.ID, age)
		}
	}
	record(hostA, domain.ActionCommentPosted, "IC_posted", first, 0)
	record(hostA, domain.ActionCommentPosted, "IC_twice", first, time.Hour)
	record(hostA, domain.ActionCommentPosted, "IC_twice", second, 0)
	record(hostA, domain.ActionCommentEdited, "IC_edited", first, 0)
	record(hostA, domain.ActionPRCreated, "PR_kw7", first, 0)
	record(hostA, domain.ActionCommentPosted, "IC_unlinked", "", 0)
	record(hostB, domain.ActionCommentPosted, "IC_onB", onB, 0)

	for _, c := range []struct {
		name, scope, nodeID, want string
	}{
		{"posted", hostA, "IC_posted", first},
		{"newest_row_wins", hostA, "IC_twice", second},
		{"an_edit_is_no_post", hostA, "IC_edited", ""},
		{"a_pr_created_row_is_no_comment", hostA, "PR_kw7", ""},
		{"a_row_with_no_entity", hostA, "IC_unlinked", ""},
		{"another_scope", hostA, "IC_onB", ""},
		{"its_own_scope", hostB, "IC_onB", onB},
		{"absent", hostA, "IC_absent", ""},
		{"no_node_id", hostA, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := f.Stores.ExternalActions.PostedCommentEntitySystem(ctx, f.OrgID, c.scope, c.nodeID)
			if err != nil {
				t.Fatalf("PostedCommentEntitySystem: %v", err)
			}
			if got != c.want {
				t.Errorf("entity = %q, want %q", got, c.want)
			}
		})
	}
	if _, err := f.Stores.ExternalActions.PostedCommentEntitySystem(ctx, f.OrgID, "", "IC_posted"); !errors.Is(err, db.ErrExternalObjectScopeRequired) {
		t.Errorf("PostedCommentEntitySystem with no scope: err = %v, want ErrExternalObjectScopeRequired", err)
	}
}
