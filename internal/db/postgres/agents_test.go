package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestAgentStore_Postgres runs the shared AgentStore conformance suite
// against the Postgres impl. AdminDB is used for both pools so Create
// (admin-pool-only path) and reads (app-pool path) both work without
// JWT claims plumbed per subtest — same shape TaskRule / Trigger tests
// use.
func TestAgentStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunAgentStoreConformance(t, func(t *testing.T) (db.AgentStore, string, string) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForAgents(t, h)
		// Reuse the org owner's user id as the SetGitHubPATUser target.
		// agents.github_pat_user_id FKs users(id); the seed already
		// created this user, so the FK is satisfied without seeding
		// another row.
		ownerID := mustOwnerUserForOrg(t, h, orgID)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		return stores.Agents, orgID, ownerID
	})
}

// TestAgentStore_Postgres_CreateIsolatesAcrossOrgs pins the per-org
// isolation invariant: two different orgs both bootstrap their own
// agent row, neither sees the other. The UNIQUE(org_id) constraint
// combined with the per-org BootstrapAgentID derivation ensures the
// ids are distinct.
func TestAgentStore_Postgres_CreateIsolatesAcrossOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	if orgA == orgB {
		t.Fatal("seedPgOrgForAgents returned the same orgID twice")
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	idA, err := stores.Agents.Create(ctx, orgA, domain.Agent{DisplayName: "Bot A"})
	if err != nil {
		t.Fatalf("Create org A: %v", err)
	}
	idB, err := stores.Agents.Create(ctx, orgB, domain.Agent{DisplayName: "Bot B"})
	if err != nil {
		t.Fatalf("Create org B (regression case — would collide on global PK if BootstrapAgentID didn't mix in orgID): %v", err)
	}
	if idA == idB {
		t.Errorf("idA == idB == %q; per-org agents.id derivation broken", idA)
	}

	a, _ := stores.Agents.GetForOrg(ctx, orgA)
	b, _ := stores.Agents.GetForOrg(ctx, orgB)
	if a == nil || b == nil {
		t.Fatalf("expected both rows present; got a=%v b=%v", a, b)
	}
	if a.DisplayName != "Bot A" || b.DisplayName != "Bot B" {
		t.Errorf("display names not isolated: a=%q b=%q", a.DisplayName, b.DisplayName)
	}
}

// TestAgentStore_Postgres_CreateRejectsInsideTx pins the contract
// that Create must not be invoked from a WithTx closure. Escaping to
// the admin pool from inside a caller's tx would silently bypass
// their transaction scope. Mirrors TaskRule / Trigger Seed contract.
func TestAgentStore_Postgres_CreateRejectsInsideTx(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID := seedPgOrgForAgents(t, h)
	ownerID := mustOwnerUserForOrg(t, h, orgID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	err := stores.Tx.WithTx(context.Background(), orgID, ownerID, func(tx db.TxStores) error {
		_, err := tx.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "InTx"})
		return err
	})
	if err == nil {
		t.Fatal("Create inside WithTx returned nil; want explicit refusal")
	}
	if !strings.Contains(err.Error(), "must not be called inside WithTx") {
		t.Errorf("error %q does not mention the inside-WithTx contract", err.Error())
	}
}

// TestAgentStore_Postgres_NonAdminCannotUpdate pins the admin-only
// gate on agents writes. A plain org member must not be able to
// rename the bot, change its default model, or rotate credentials.
func TestAgentStore_Postgres_NonAdminCannotUpdate(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID := seedPgOrgForAgents(t, h)
	bobID := seedPgMember(t, h, orgID, "bob", "member")

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := adminStores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Org Bot"}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	var agentID string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM agents WHERE org_id = $1`, orgID,
	).Scan(&agentID); err != nil {
		t.Fatalf("read agent id: %v", err)
	}

	// bob (member, not admin) attempts to rename — policy filters the
	// row out of the UPDATE's USING set, so we expect 0 rows affected
	// rather than an error.
	err := h.WithUser(t, bobID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE agents SET display_name = $1 WHERE id = $2`,
			"Hijacked", agentID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("non-admin UPDATE matched %d rows; want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("non-admin UPDATE assertion: %v", err)
	}

	// Sanity: row name still "Org Bot".
	var name string
	if err := h.AdminDB.QueryRow(
		`SELECT display_name FROM agents WHERE id = $1`, agentID,
	).Scan(&name); err != nil {
		t.Fatalf("re-read post-bob: %v", err)
	}
	if name != "Org Bot" {
		t.Errorf("display_name=%q after non-admin UPDATE; policy didn't gate", name)
	}
}

// TestAgentStore_Postgres_AdminCanUpdate is the other half of the
// policy: an org admin (carol) can rename the bot.
func TestAgentStore_Postgres_AdminCanUpdate(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID := seedPgOrgForAgents(t, h)
	carolID := seedPgMember(t, h, orgID, "carol", "admin")

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := adminStores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Org Bot"}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	var agentID string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM agents WHERE org_id = $1`, orgID,
	).Scan(&agentID); err != nil {
		t.Fatalf("read agent id: %v", err)
	}

	err := h.WithUser(t, carolID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE agents SET display_name = $1 WHERE id = $2`,
			"Renamed By Admin", agentID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("admin UPDATE matched %d rows; want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("admin UPDATE: %v", err)
	}
}

// TestAgentStore_Postgres_BlocksCrossOrgPATUser pins the
// github_pat_user_id integrity fix from migration 202605120005.
// Without the same-org constraint, an org admin in A could write
// agents.github_pat_user_id = <bob-from-org-B>. Downstream credential
// lookup goes through org_secrets RLS gated on tf.current_org_id() so
// the cross-org PAT itself isn't reachable, but the row's integrity
// is wrong. Defense in depth: RLS refuses the write directly.
func TestAgentStore_Postgres_BlocksCrossOrgPATUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)

	// daveID is admin of orgA only.
	daveID := seedPgMember(t, h, orgA, "dave", "admin")
	// bobBID is a member of orgB only — does NOT belong to orgA.
	bobBID := mustOwnerUserForOrg(t, h, orgB)

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := adminStores.Agents.Create(context.Background(), orgA, domain.Agent{DisplayName: "Bot A"}); err != nil {
		t.Fatalf("seed agent A: %v", err)
	}
	var agentAID string
	if err := h.AdminDB.QueryRow(`SELECT id FROM agents WHERE org_id = $1`, orgA).Scan(&agentAID); err != nil {
		t.Fatalf("read agent A id: %v", err)
	}

	// Dave (org-A admin) attempts to set agents.github_pat_user_id
	// to a user that only belongs to org B. Even though dave passes
	// the tf.user_is_org_admin(orgA) check, the new WITH CHECK
	// predicate refuses because bobB has no org_memberships row in
	// orgA. Postgres surfaces a row-level-security policy violation
	// (42501) rather than a 0-rows-affected outcome on this kind of
	// WITH CHECK failure.
	err := h.WithUser(t, daveID, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE agents SET github_pat_user_id = $1 WHERE id = $2`,
			bobBID, agentAID,
		)
		return err
	})
	if err == nil {
		t.Fatal("cross-org github_pat_user_id UPDATE succeeded; integrity gate missing")
	}

	// Sanity: the row's github_pat_user_id is still NULL.
	var patUser sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT github_pat_user_id FROM agents WHERE id = $1`, agentAID,
	).Scan(&patUser); err != nil {
		t.Fatalf("re-read agent A: %v", err)
	}
	if patUser.Valid {
		t.Errorf("github_pat_user_id corrupted by refused UPDATE: %q", patUser.String)
	}

	// Defense check: dave CAN set it to a user that IS in org A.
	aliceAID := seedPgMember(t, h, orgA, "alice", "member")
	err = h.WithUser(t, daveID, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE agents SET github_pat_user_id = $1 WHERE id = $2`,
			aliceAID, agentAID,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("same-org PAT UPDATE matched %d rows; want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("same-org PAT UPDATE: %v", err)
	}
}

// seedPgOrgForAgents stages (user, org, org_membership) so agents.org_id
// FK satisfies. Returns the org id.
func seedPgOrgForAgents(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	email := fmt.Sprintf("agent-conf-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Agent Conformance User",
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Agent Conformance Org "+orgID[:8], "agent-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID
}
