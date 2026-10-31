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
// org-wide but updated team-scoped, so a manual run in team B writing the
// deliverable of a pull request team A's run recorded cannot update A's row
// under its user's claims. The refused upsert must not take the ledger row
// with it, and A's deliverable must be left as it was.
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

	key := domain.PullRequestDedupKey("PR_kw3")
	owned, err := stores.Deliverables.UpsertSystem(ctx, orgID, domain.Deliverable{
		ConversationID: convA, TeamID: teamA, Provider: domain.ProviderGitHub,
		Kind: domain.DeliverableKindPullRequest, Scope: scope, Target: "octo/api#3", ExternalID: "3",
		NodeID: "PR_kw3", State: domain.DeliverableStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("seed team A's pull request: %v", err)
	}

	RecordExternalWrite(ctx, stores, infoB, &domain.Deliverable{
		Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest,
		Target: "octo/api#3", ExternalID: "3", State: domain.DeliverableStatePROpen, DedupKey: key,
	}, &domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionPRMarkedReady,
		Target: "octo/api#3", ExternalID: "3", Credential: domain.CredentialGitHubApp,
	})

	if rows := ledgerFor(convB); len(rows) != 1 || rows[0].Action != domain.ActionPRMarkedReady || rows[0].TeamID != teamB {
		t.Errorf("team B's ledger rows = %+v, want its one pr_marked_ready row", rows)
	}
	got, err := stores.Deliverables.ListByConversationSystem(ctx, orgID, convA)
	if err != nil || len(got) != 1 {
		t.Fatalf("read team A's pull request: %+v, %v", got, err)
	}
	if got[0].ID != owned.ID || got[0].State != domain.DeliverableStatePRDraft || got[0].TeamID != teamA {
		t.Errorf("team A's pull request = %+v, want it left as team A recorded it", got[0])
	}
}
