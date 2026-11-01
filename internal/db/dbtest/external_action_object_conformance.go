package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunExternalActionObjectConformance covers the ledger's object identity: a
// row's object_kind and object_key round-trip through both write pools, and
// the dialect's SQL twin of domain.ObjectID renders the same string as
// domain.ObjectID does for the row read back, for one row of every kind, and
// NULL for a row that names no object or whose kind's identifying columns
// are empty. It runs on the node-id suite's fixture, whose ObjectIDOf
// evaluates the dialect's expression for one row.
func RunExternalActionObjectConformance(t *testing.T, mk ExternalActionNodeIDFactory) {
	t.Helper()
	ctx := context.Background()
	const (
		jiraSite        = "https://acme.atlassian.net"
		linearWorkspace = "ws-object-conformance"
	)

	f := mk(t)
	pr, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, "github", TestGitHubHost, "octo/api#7", "PR_obj7", domain.EntityKindPullRequest, "A PR", "")
	if err != nil {
		t.Fatalf("seed pull request entity: %v", err)
	}
	issue, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, "github", TestGitHubHost, "octo/api#8", "I_obj8", domain.EntityKindIssue, "An issue", "")
	if err != nil {
		t.Fatalf("seed issue entity: %v", err)
	}
	repo, err := f.Stores.Repos.UpsertSystem(ctx, f.OrgID, domain.Repository{Host: TestGitHubHost, Owner: "octo", Repo: "api"})
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}

	row := func(provider, scope, action, kind, key string) domain.ExternalAction {
		return domain.ExternalAction{
			ID:             uuid.NewString(),
			TeamID:         f.TeamID,
			Provider:       provider,
			Scope:          scope,
			Action:         action,
			Target:         "octo/api",
			ConversationID: f.ConversationID,
			Credential:     domain.CredentialGitHubApp,
			PerformedBy:    domain.PerformedByAgent,
			ObjectKind:     kind,
			ObjectKey:      key,
		}
	}
	withEntity := func(a domain.ExternalAction, id string) domain.ExternalAction {
		a.EntityID = id
		return a
	}
	withRepository := func(a domain.ExternalAction) domain.ExternalAction {
		a.RepositoryID = repo.ID
		return a
	}
	gh := domain.ProviderGitHub
	cases := []struct {
		name   string
		action domain.ExternalAction
		object bool
	}{
		{"pull_request", withEntity(row(gh, TestGitHubHost, domain.ActionPRCreated, domain.ObjectKindPullRequest, ""), pr.ID), true},
		{"github_issue", withEntity(row(gh, TestGitHubHost, domain.ActionIssueClosed, domain.ObjectKindIssue, ""), issue.ID), true},
		{"jira_issue", row(domain.ProviderJira, jiraSite, domain.ActionIssueTransitioned, domain.ObjectKindIssue,
			domain.ObjectKey(domain.ProviderJira, domain.ObjectKindIssue, domain.JiraIssueResource(jiraSite, "10042"), "")), true},
		{"linear_issue", row(domain.ProviderLinear, linearWorkspace, domain.ActionIssueUpdated, domain.ObjectKindIssue,
			domain.ObjectKey(domain.ProviderLinear, domain.ObjectKindIssue, "uuid-1", "")), true},
		{"repository", withRepository(row(gh, TestGitHubHost, domain.ActionRepoEdited, domain.ObjectKindRepository, "")), true},
		{"branch", withRepository(row(gh, TestGitHubHost, domain.ActionBranchPushed, domain.ObjectKindBranch, "refs/heads/fix")), true},
		{"label", withRepository(row(gh, TestGitHubHost, domain.ActionLabelDefinitionEdited, domain.ObjectKindLabel, "needs triage")), true},
		{"release", withRepository(row(gh, TestGitHubHost, domain.ActionReleaseEdited, domain.ObjectKindRelease, "901")), true},
		{"workflow", withRepository(row(gh, TestGitHubHost, domain.ActionWorkflowDispatched, domain.ObjectKindWorkflow, "ci.yml")), true},
		{"workflow_run", withRepository(row(gh, TestGitHubHost, domain.ActionWorkflowRunCancelled, domain.ObjectKindWorkflowRun, "77")), true},
		{"review", row(gh, TestGitHubHost, domain.ActionReviewSubmitted, domain.ObjectKindReview, domain.GitHubReviewObjectKey("4242")), true},
		{"comment", row(gh, TestGitHubHost, domain.ActionCommentPosted, domain.ObjectKindComment,
			domain.GitHubCommentKey(domain.GitHubReviewComment, 555)), true},
		{"message", row(domain.ProviderSlack, domain.SlackScope, domain.ActionSlackMessagePosted, domain.ObjectKindMessage,
			domain.ObjectKey(domain.ProviderSlack, domain.ObjectKindMessage, domain.SlackMessageResource("chan-row", "1700000000.000100"), "")), true},
		{"no_object", row(gh, TestGitHubHost, domain.ActionGitDenied, "", ""), false},
		{"pull_request_with_no_entity", row(gh, TestGitHubHost, domain.ActionPRMerged, domain.ObjectKindPullRequest, ""), false},
		{"branch_with_no_repository", row(gh, TestGitHubHost, domain.ActionBranchPushed, domain.ObjectKindBranch, "refs/heads/x"), false},
	}
	// Every kind in the vocabulary has a row, so a kind the CHECK or the SQL
	// twin was not taught fails here.
	for _, kind := range domain.ObjectKinds() {
		covered := false
		for _, c := range cases {
			covered = covered || (c.object && c.action.ObjectKind == kind)
		}
		if !covered {
			t.Errorf("object kind %q has no case", kind)
		}
	}
	for i, c := range cases {
		record := f.Stores.ExternalActions.RecordSystem
		if i%2 == 0 {
			record = f.Stores.ExternalActions.Record
		}
		if err := record(ctx, f.OrgID, c.action); err != nil {
			t.Fatalf("record %s: %v", c.name, err)
		}
	}

	stored, _, err := f.Stores.ExternalActions.ListByOrgSystem(ctx, f.OrgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	byID := map[string]domain.ExternalAction{}
	for _, a := range stored {
		byID[a.ID] = a
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := byID[c.action.ID]
			if !ok {
				t.Fatalf("row %s was not read back", c.action.ID)
			}
			if got.ObjectKind != c.action.ObjectKind || got.ObjectKey != c.action.ObjectKey {
				t.Errorf("object read back as (%q, %q), want (%q, %q)", got.ObjectKind, got.ObjectKey, c.action.ObjectKind, c.action.ObjectKey)
			}
			want, wantOK := domain.ObjectID(got)
			if wantOK != c.object {
				t.Fatalf("domain.ObjectID = (%q, %v), want an identity: %v", want, wantOK, c.object)
			}
			sqlID, sqlOK := f.ObjectIDOf(t, got.ID)
			if sqlOK != wantOK || sqlID != want {
				t.Errorf("SQL object id = (%q, non-NULL %v), domain.ObjectID = (%q, %v)", sqlID, sqlOK, want, wantOK)
			}
		})
	}
}
