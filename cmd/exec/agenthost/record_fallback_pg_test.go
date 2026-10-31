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
// ledger row survives a deliverable write RLS refuses. Deliverables are keyed
// org-wide but updated team-scoped, so a manual run in team B acting on a
// comment team A's run recorded cannot update A's row under its user's claims.
// The refused upsert must not take the ledger row with it, A's deliverable must be
// left as it was, and the same holds for a branch push, whose ledger row is
// composed inside the deliverable upsert.
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
		key := domain.GitHubCommentKey(domain.GitHubIssueComment, 7)
		owned, err := stores.Deliverables.UpsertSystem(ctx, orgID, domain.Deliverable{
			ConversationID: convA, TeamID: teamA, Provider: domain.ProviderGitHub,
			Kind: domain.DeliverableKindComment, Scope: scope, Target: "octo/api#3", ExternalID: "7",
			NodeID: "IC_kw7", State: domain.DeliverableStateCommentPosted, DedupKey: key,
		})
		if err != nil {
			t.Fatalf("seed team A's comment: %v", err)
		}

		RecordExternalWrite(ctx, stores, infoB, &domain.Deliverable{
			Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindComment,
			ExternalID: "7", State: domain.DeliverableStateCommentDeleted, DedupKey: key,
		}, &domain.ExternalAction{
			Provider: domain.ProviderGitHub, Action: domain.ActionCommentDeleted,
			Target: "octo/api", ExternalID: "7", Credential: domain.CredentialGitHubApp,
		})

		if rows := ledgerFor(convB); len(rows) != 1 || rows[0].Action != domain.ActionCommentDeleted || rows[0].TeamID != teamB {
			t.Errorf("team B's ledger rows = %+v, want its one comment_deleted row", rows)
		}
		got, err := stores.Deliverables.GetByNodeIDSystem(ctx, orgID, scope, "IC_kw7")
		if err != nil || got == nil {
			t.Fatalf("read team A's comment: %v, %v", got, err)
		}
		if got.ID != owned.ID || got.State != domain.DeliverableStateCommentPosted || got.TeamID != teamA {
			t.Errorf("team A's comment = %+v, want it left as team A recorded it", got)
		}
	})

	t.Run("branch push", func(t *testing.T) {
		branch, ok := domain.NewBranchDeliverable("octo/api", "refs/heads/shared", "abc123", true)
		if !ok {
			t.Fatal("NewBranchDeliverable refused the fixture branch")
		}
		seed := branch
		seed.ConversationID, seed.TeamID, seed.Scope = convA, teamA, scope
		if _, err := stores.Deliverables.UpsertSystem(ctx, orgID, seed); err != nil {
			t.Fatalf("seed team A's branch: %v", err)
		}

		pushed, ok := domain.NewBranchDeliverable("octo/api", "refs/heads/shared", "def456", false)
		if !ok {
			t.Fatal("NewBranchDeliverable refused the pushed branch")
		}
		if _, err := newDirectRuntime(stores, infoB).UpsertDeliverable(ctx, pushed); err == nil {
			t.Fatal("UpsertDeliverable = nil error, want team B refused team A's branch row")
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
