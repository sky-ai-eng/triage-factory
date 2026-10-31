package tracker

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// recordTFComment records a comment TF posted the way the agent's comment
// verbs do: one ledger row whose object key is the comment's key. No comment
// deliverable is written, so the tracker's marking of the comment as TF's
// rests on the ledger alone.
func recordTFComment(t *testing.T, stores db.Stores, provider, scope, target, key string) {
	t.Helper()
	action, credential := domain.ActionCommentPosted, domain.CredentialGitHubApp
	switch provider {
	case domain.ProviderJira:
		action, credential = domain.ActionIssueCommentPosted, domain.CredentialJiraOrg
	case domain.ProviderLinear:
		action, credential = domain.ActionIssueCommentPosted, domain.CredentialLinearOrg
	}
	a := domain.ExternalAction{
		TeamID:      runmode.LocalDefaultTeamID,
		Provider:    provider,
		Scope:       scope,
		Action:      action,
		Target:      target,
		Credential:  credential,
		PerformedBy: domain.PerformedByAgent,
	}
	a.SetObject(domain.ObjectKindComment, key)
	if err := stores.ExternalActions.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("record TF's comment %s: %v", key, err)
	}
}
