package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestExternalActionStore_Postgres_RoundTrip drives Record + ListByOrgSystem
// against the AdminDB-wired store (RLS bypassed — behavior, not auth). Pins every
// field round-trips, an empty id is server-generated, and an empty dedup_key is
// filled with a uuid. TFAC-483.
func TestExternalActionStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgArtifactConversation(t, h, orgID, teamID, userID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	in := domain.ExternalAction{
		OrgID:          orgID,
		TeamID:         teamID,
		Provider:       domain.ArtifactProviderGitHub,
		Action:         domain.ActionPRMarkedReady,
		Target:         "octo/repo#123",
		ExternalID:     "123",
		URL:            "https://github.com/octo/repo/pull/123",
		FromState:      domain.ArtifactStatePRDraft,
		ToState:        domain.ArtifactStatePROpen,
		ConversationID: conversationID,
		ActorUserID:    userID,
		Credential:     domain.CredentialGitHubApp,
		DetailJSON:     `{"k":"v"}`,
		// DedupKey empty → server fills a uuid.
	}
	if err := stores.ExternalActions.Record(ctx, orgID, in); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, _, err := stores.ExternalActions.ListByOrgSystem(ctx, orgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	a := got[0]
	if a.ID == "" || a.OccurredAt.IsZero() || a.DedupKey == "" {
		t.Errorf("expected generated id/occurred/dedup, got id=%q occurred=%v dedup=%q", a.ID, a.OccurredAt, a.DedupKey)
	}
	if a.Provider != "github" || a.Action != domain.ActionPRMarkedReady || a.Target != "octo/repo#123" ||
		a.ExternalID != "123" || a.URL != in.URL || a.FromState != "draft" || a.ToState != "open" ||
		a.ConversationID != conversationID || a.ActorUserID != userID || a.Credential != "github_app" || a.DetailJSON != `{"k":"v"}` {
		t.Errorf("round-trip mismatch: %+v", a)
	}
}

// TestExternalActionStore_Postgres_BranchTwinDedup pins the append-only twin
// collapse: two RecordSystem calls with the same explicit dedup_key (the git
// hook+proxy branch twin) leave one row, first-write-wins — never an error or a
// mutation. TFAC-483.
func TestExternalActionStore_Postgres_BranchTwinDedup(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.BranchPushDedupKey("run-1", "refs/heads/feat", "abc123")
	twin := func(url string) domain.ExternalAction {
		return domain.ExternalAction{
			OrgID: orgID, Provider: domain.ArtifactProviderGitHub, Action: domain.ActionBranchPushed,
			Target: "octo/repo", URL: url, Credential: domain.CredentialGitHubApp, DedupKey: key,
		}
	}
	if err := stores.ExternalActions.RecordSystem(ctx, orgID, twin("hook")); err != nil {
		t.Fatalf("hook RecordSystem: %v", err)
	}
	if err := stores.ExternalActions.RecordSystem(ctx, orgID, twin("proxy")); err != nil {
		t.Fatalf("proxy RecordSystem (should DO NOTHING): %v", err)
	}
	var count int
	var url string
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*), MAX(url) FROM external_actions WHERE dedup_key = $1`, key).Scan(&count, &url); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 || url != "hook" {
		t.Errorf("twin not collapsed first-write-wins: count=%d url=%q", count, url)
	}
}

// TestExternalActionStore_Postgres_RLS pins the production org-scoped RLS layer
// (external_actions_all): RecordSystem (admin) lands without claims; an org
// member's Record (app pool, under claims) lands and is readable via ListByTeam;
// a write whose org_id ≠ the claims org is rejected by the WITH CHECK; and a
// different-org member reads zero (cross-org isolation). The policy is org-scoped
// (not team-scoped like artifacts), so a same-org member of another team can
// still read — the team-admin/org-admin gate lives at the HTTP layer. TFAC-483.
func TestExternalActionStore_Postgres_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	ctx := context.Background()

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	// RecordSystem (admin pool) lands without any claims context.
	if err := stores.ExternalActions.RecordSystem(ctx, orgA, domain.ExternalAction{
		OrgID: orgA, TeamID: teamA, Provider: domain.ArtifactProviderJira,
		Action: domain.ActionIssueTransitioned, Target: "SKY-1",
		Credential: domain.CredentialJiraOrg, DedupKey: "sys-1",
	}); err != nil {
		t.Fatalf("RecordSystem (admin): %v", err)
	}

	// Alice (orgA member) Records under her claims via the app pool — the
	// external_actions_all WITH CHECK (org_id = current_org_id) passes.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).ExternalActions.Record(ctx, orgA, domain.ExternalAction{
			OrgID: orgA, TeamID: teamA, Provider: domain.ArtifactProviderGitHub,
			Action: domain.ActionPRMarkedReady, Target: "octo/repo#1", ActorUserID: alice,
			Credential: domain.CredentialGitHubApp, DedupKey: "app-1",
		})
	}); err != nil {
		t.Fatalf("alice Record under RLS: %v", err)
	}

	// Alice reads her org's actions via the team read (app pool, RLS-active).
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		rows, _, err := pgstore.NewForTx(tx, pgtest.SecretKey).ExternalActions.ListByTeam(ctx, orgA, teamA, domain.ExternalActionListOpts{})
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			t.Errorf("alice ListByTeam saw %d rows, want 2 (the system + app rows on teamA)", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatalf("alice read path: %v", err)
	}

	// A write whose org_id is NOT the claims org is rejected by the WITH CHECK.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).ExternalActions.Record(ctx, orgB, domain.ExternalAction{
			OrgID: orgB, Provider: domain.ArtifactProviderGitHub, Action: domain.ActionPRCreated,
			Target: "octo/repo#9", Credential: domain.CredentialGitHubApp, DedupKey: "cross-org",
		})
	})
	pgtest.AssertRLSViolation(t, err)

	// Bob (a different org) reads zero of orgA's actions — cross-org isolation.
	if err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
		rows, _, err := pgstore.NewForTx(tx, pgtest.SecretKey).ExternalActions.ListByTeam(ctx, orgA, teamA, domain.ExternalActionListOpts{})
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("bob (orgB) saw %d of orgA's actions — RLS leaked across orgs", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatalf("bob read path: %v", err)
	}

	// ListByOrgSystem (admin pool) sees orgA's rows org-wide.
	all, _, err := stores.ExternalActions.ListByOrgSystem(ctx, orgA, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("ListByOrgSystem saw %d orgA rows, want 2", len(all))
	}
}

// TestExternalActionStore_Postgres_ListByConversation pins the conversation-scoped read on the app
// pool: one conversation's rows, and — because the read runs under the same
// org-scoped policy as ListByTeam — nothing at all for a member of another org
// who guesses a conversation id. The handler's own conversation-visibility
// check is what narrows
// this within an org; RLS is the backstop across orgs.
func TestExternalActionStore_Postgres_ListByConversation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	convA := seedPgArtifactConversation(t, h, orgA, teamA, alice)
	ctx := context.Background()

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	// One row on the conversation, one with no conversation at all — the shape
	// a purged conversation leaves behind (FK ON DELETE SET NULL), which must
	// not surface under any conversation.
	for _, act := range []domain.ExternalAction{
		{
			OrgID: orgA, TeamID: teamA, Provider: domain.ArtifactProviderGitHub,
			Action: domain.ActionGHChannelWrite, Target: "octo/repo", ConversationID: convA,
			Credential: domain.CredentialGitHubApp, DedupKey: "run-1",
		},
		{
			OrgID: orgA, TeamID: teamA, Provider: domain.ArtifactProviderGitHub,
			Action: domain.ActionBranchPushed, Target: "octo/repo",
			Credential: domain.CredentialGitHubApp, DedupKey: "detached-1",
		},
	} {
		if err := stores.ExternalActions.RecordSystem(ctx, orgA, act); err != nil {
			t.Fatalf("RecordSystem %s: %v", act.Action, err)
		}
	}

	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		rows, _, err := pgstore.NewForTx(tx, pgtest.SecretKey).ExternalActions.ListByConversation(ctx, orgA, convA, domain.ExternalActionListOpts{})
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Action != domain.ActionGHChannelWrite {
			t.Errorf("alice ListByConversation saw %+v, want just the conversation's own row", rows)
		}
		return nil
	}); err != nil {
		t.Fatalf("alice ListByConversation: %v", err)
	}

	if err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
		rows, _, err := pgstore.NewForTx(tx, pgtest.SecretKey).ExternalActions.ListByConversation(ctx, orgA, convA, domain.ExternalActionListOpts{})
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("bob (orgB) saw %d rows for an orgA run — RLS leaked across orgs", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatalf("bob ListByConversation: %v", err)
	}
}
