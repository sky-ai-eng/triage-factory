package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRecordExternalWrite_StampsTheOrgScopePerProvider pins the exec funnel's
// scope rule: every row it records — whatever provider, and whichever door
// (the shared funnel, a branch push through UpsertArtifact) — is recorded in
// the org's current scope for its provider, and a scope the caller put on the
// row is overwritten rather than trusted, the same as the run identity.
func TestRecordExternalWrite_StampsTheOrgScopePerProvider(t *testing.T) {
	stores, conn := newTestDB(t)
	seedConversation(t, stores, conn, "conv-scope", runmode.LocalDefaultUserID, "manual")
	if _, err := conn.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'jira', 'https://acme.atlassian.net/')
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = excluded.base_url
	`, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed jira site: %v", err)
	}
	ctx := context.Background()
	settings, err := stores.Orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	want := func(provider string) string {
		t.Helper()
		s := domain.ExternalObjectScope(provider, settings)
		if s == "" {
			t.Fatalf("the fixture gives %s no scope, so this test would prove nothing for it", provider)
		}
		return s
	}

	info := ConversationInfo{
		OrgID: runmode.LocalDefaultOrgID, UserID: runmode.LocalDefaultUserID,
		TeamID: runmode.LocalDefaultTeamID, ConversationID: "conv-scope",
	}
	RecordExternalWrite(ctx, stores, info, &domain.Artifact{
		Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
		Scope:  "https://elsewhere.example.com",
		Target: "octo/api#1", ExternalID: "c-1", State: domain.ArtifactStateCommentPosted,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, "c-1", ""),
	}, &domain.ExternalAction{
		Provider: domain.ArtifactProviderGitHub, Scope: "https://elsewhere.example.com",
		Action: domain.ActionCommentPosted, Target: "octo/api#1", Credential: domain.CredentialGitHubApp,
	})
	RecordExternalWrite(ctx, stores, info, nil, &domain.ExternalAction{
		Provider: domain.ArtifactProviderJira, Action: domain.ActionIssueCommentPosted,
		Target: "ENG-1", Credential: domain.CredentialJiraOrg,
	})
	RecordExternalWrite(ctx, stores, info, nil, egressDeniedAction("conv-scope", "pypi.org:443", "off-allowlist"))
	branch, ok := domain.NewBranchArtifact("octo/api", "refs/heads/fix", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact refused a valid ref")
	}
	if _, err := NewLocal(stores, info).UpsertArtifact(ctx, branch); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	byProvider := map[string]string{}
	for _, a := range listConversationArtifacts(t, stores, "conv-scope") {
		byProvider["artifact:"+a.Provider] = a.Scope
	}
	actions, _, err := stores.ExternalActions.ListByConversation(ctx, runmode.LocalDefaultOrgID, "conv-scope", domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	for _, a := range actions {
		byProvider["action:"+a.Provider+":"+a.Action] = a.Scope
	}
	for key, provider := range map[string]string{
		"artifact:github": domain.ArtifactProviderGitHub,
		"artifact:git":    domain.ArtifactProviderGit,
		"action:github:" + domain.ActionCommentPosted:    domain.ArtifactProviderGitHub,
		"action:github:" + domain.ActionBranchPushed:     domain.ArtifactProviderGitHub,
		"action:jira:" + domain.ActionIssueCommentPosted: domain.ArtifactProviderJira,
		"action:network:" + domain.ActionEgressDenied:    domain.ArtifactProviderNetwork,
	} {
		got, recorded := byProvider[key]
		if !recorded {
			t.Errorf("%s was not recorded", key)
			continue
		}
		if w := want(provider); got != w {
			t.Errorf("%s scope = %q, want the org's %q", key, got, w)
		}
	}
}
