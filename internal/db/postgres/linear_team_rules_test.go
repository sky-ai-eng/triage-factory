package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestLinearTeamRulesStore_Postgres runs the shared conformance suite with
// AdminDB in both pool slots, so it pins the store's behavior independent of
// RLS; the policies are pinned by the claims-carrying test below.
func TestLinearTeamRulesStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.RunLinearTeamRulesConformance(t, func(t *testing.T) dbtest.LinearTeamRulesFixture {
		t.Helper()
		h.Reset(t)
		orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "linear-rules-conf")
		otherTeamID := pgtest.SeedTeam(t, h, orgID, "other")
		return dbtest.LinearTeamRulesFixture{
			Store:       pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).LinearTeamRules,
			OrgID:       orgID,
			TeamID:      teamID,
			OtherTeamID: otherTeamID,
		}
	})
}

// TestLinearTeamRulesStore_Postgres_RLS runs the store under real claims on
// the app role: a team's rows are readable by its members only, and writable
// by its admins only. The conformance run above cannot see any of this — it
// rides the RLS-bypassing admin connection.
func TestLinearTeamRulesStore_Postgres_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, adminID, teamID := pgtest.SeedOrgWithUser(t, h, "linear-rls")
	otherTeamID := pgtest.SeedTeam(t, h, orgID, "other")
	// outsider is an org member who belongs to the other team only; member
	// belongs to the team without being its admin.
	outsider := pgtest.SeedUser(t, h, "linear-rls-outsider")
	pgtest.AddOrgMember(t, h, outsider, orgID, otherTeamID, "member", "member")
	member := pgtest.SeedUser(t, h, "linear-rls-member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")

	armed := domain.LinearTeamRules{
		LinearTeamID:        "lt-a",
		LinearTeamKey:       "AAA",
		PickupMembers:       []domain.LinearStateRef{{ID: "s-todo", Name: "Todo", Type: "unstarted"}},
		InProgressMembers:   []domain.LinearStateRef{{ID: "s-doing", Name: "Doing", Type: "started"}},
		InProgressCanonical: domain.LinearStateRef{ID: "s-doing", Name: "Doing", Type: "started"},
		DoneMembers:         []domain.LinearStateRef{{ID: "s-done", Name: "Done", Type: "completed"}},
		DoneCanonical:       domain.LinearStateRef{ID: "s-done", Name: "Done", Type: "completed"},
	}

	// The team admin writes, and reads back what it wrote.
	if err := h.WithUser(t, adminID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).LinearTeamRules
		stored, err := store.ReplaceForTeam(ctx, teamID, []domain.LinearTeamRules{armed})
		if err != nil {
			t.Fatalf("admin ReplaceForTeam: %v", err)
		}
		if len(stored) != 1 || stored[0].LinearTeamID != "lt-a" {
			t.Errorf("admin ReplaceForTeam returned %#v, want the one row it wrote", stored)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser(admin): %v", err)
	}

	// A member of a different team sees nothing of this team's rows.
	if err := h.WithUser(t, outsider, orgID, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).LinearTeamRules.ListForTeam(ctx, teamID)
		if err != nil {
			t.Fatalf("outsider ListForTeam: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("a non-member read another team's rules: %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser(outsider): %v", err)
	}

	// A plain member reads them, and cannot rewrite them.
	if err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).LinearTeamRules
		got, err := store.ListForTeam(ctx, teamID)
		if err != nil {
			t.Fatalf("member ListForTeam: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("a team member could not read its team's rules: %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser(member read): %v", err)
	}
	err := h.WithUser(t, member, orgID, func(tx *sql.Tx) error {
		changed := armed
		changed.LinearTeamID = "lt-b"
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).LinearTeamRules.ReplaceForTeam(ctx, teamID, []domain.LinearTeamRules{changed})
		return err
	})
	if err == nil {
		t.Error("a non-admin member rewrote the team's Linear rules")
	}

	stored, err := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey).LinearTeamRules.ListForTeamSystem(ctx, teamID)
	if err != nil {
		t.Fatalf("ListForTeamSystem: %v", err)
	}
	if len(stored) != 1 || stored[0].LinearTeamID != "lt-a" {
		t.Errorf("stored rows after the refused write = %#v, want only lt-a", stored)
	}
}
