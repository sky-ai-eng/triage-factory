package agenthost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRecordExternalWrite_Postgres_CrossTeamKeepsTheLedgerRow pins that a
// ledger row survives an artifact write RLS refuses. Artifacts are keyed
// org-wide but updated team-scoped, so a manual run in team B acting on a
// comment team A's run recorded cannot update A's row under its user's claims.
// The refused upsert must not take the ledger row with it, A's artifact must be
// left as it was, and the same holds for a branch push, whose ledger row is
// composed inside the artifact upsert.
func TestRecordExternalWrite_Postgres_CrossTeamKeepsTheLedgerRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	// The production pool split: system writes on the admin pool, and claims
	// writes on the app pool, where RLS applies.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgID, userB, teamB := pgtest.SeedOrgWithUser(t, h, "cross-team")
	teamA := pgtest.SeedTeam(t, h, orgID, "cross-team-a")
	scope := pgtest.OrgGitHubHost(t, h, orgID)
	conversation := func(teamID string) string {
		id := uuid.New().String()
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
			VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'running', 'team')
		`, id, orgID, teamID, userB)
		return id
	}
	convA, convB := conversation(teamA), conversation(teamB)
	infoB := ConversationInfo{OrgID: orgID, UserID: userB, TeamID: teamB, ConversationID: convB}

	ledgerFor := func(conversationID string) []domain.ExternalAction {
		t.Helper()
		rows, _, err := stores.ExternalActions.ListByOrgSystem(ctx, orgID, domain.ExternalActionListOpts{})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		var out []domain.ExternalAction
		for _, r := range rows {
			if r.ConversationID == conversationID {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("comment", func(t *testing.T) {
		key := domain.GitHubCommentDedupKey(domain.GitHubIssueComment, 7)
		owned, err := stores.Artifacts.UpsertSystem(ctx, orgID, domain.Artifact{
			ConversationID: convA, TeamID: teamA, Provider: domain.ArtifactProviderGitHub,
			Kind: domain.ArtifactKindComment, Scope: scope, Target: "octo/api#3", ExternalID: "7",
			NodeID: "IC_kw7", State: domain.ArtifactStateCommentPosted, DedupKey: key,
		})
		if err != nil {
			t.Fatalf("seed team A's comment: %v", err)
		}

		RecordExternalWrite(ctx, stores, infoB, &domain.Artifact{
			Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindComment,
			ExternalID: "7", State: domain.ArtifactStateCommentDeleted, DedupKey: key,
		}, &domain.ExternalAction{
			Provider: domain.ArtifactProviderGitHub, Action: domain.ActionCommentDeleted,
			Target: "octo/api", ExternalID: "7", Credential: domain.CredentialGitHubApp,
		})

		if rows := ledgerFor(convB); len(rows) != 1 || rows[0].Action != domain.ActionCommentDeleted || rows[0].TeamID != teamB {
			t.Errorf("team B's ledger rows = %+v, want its one comment_deleted row", rows)
		}
		got, err := stores.Artifacts.GetByNodeIDSystem(ctx, orgID, scope, "IC_kw7")
		if err != nil || got == nil {
			t.Fatalf("read team A's comment: %v, %v", got, err)
		}
		if got.ID != owned.ID || got.State != domain.ArtifactStateCommentPosted || got.TeamID != teamA {
			t.Errorf("team A's comment = %+v, want it left as team A recorded it", got)
		}
	})

	t.Run("branch push", func(t *testing.T) {
		branch, ok := domain.NewBranchArtifact("octo/api", "refs/heads/shared", "abc123", true)
		if !ok {
			t.Fatal("NewBranchArtifact refused the fixture branch")
		}
		seed := branch
		seed.ConversationID, seed.TeamID, seed.Scope = convA, teamA, scope
		if _, err := stores.Artifacts.UpsertSystem(ctx, orgID, seed); err != nil {
			t.Fatalf("seed team A's branch: %v", err)
		}

		pushed, ok := domain.NewBranchArtifact("octo/api", "refs/heads/shared", "def456", false)
		if !ok {
			t.Fatal("NewBranchArtifact refused the pushed branch")
		}
		if _, err := newDirectRuntime(stores, infoB).UpsertArtifact(ctx, pushed); err == nil {
			t.Fatal("UpsertArtifact = nil error, want team B refused team A's branch row")
		}
		var pushes []domain.ExternalAction
		for _, r := range ledgerFor(convB) {
			if r.Action == domain.ActionBranchPushed {
				pushes = append(pushes, r)
			}
		}
		if len(pushes) != 1 || pushes[0].ExternalID != "refs/heads/shared" {
			t.Errorf("team B's branch_pushed rows = %+v, want its one push", pushes)
		}
	})
}
