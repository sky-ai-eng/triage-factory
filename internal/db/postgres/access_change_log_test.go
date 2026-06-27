package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestAccessChangeLogStore_Postgres_RoundTrip pins that Record (app pool, inside
// a claims-bearing WithTx — the production path) inserts a row whose every field
// reads back intact, the server-side gen_random_uuid() id default fires, and the
// empty optional columns land as NULL. Skips cleanly without Docker via the
// pgtest harness. TFAC-471.
func TestAccessChangeLogStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "founder")

	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionTeamRoleChanged,
			TargetUserID: userID,
			TeamID:       teamID,
			DetailJSON:   `{"new_role":"admin"}`,
		})
	}); err != nil {
		t.Fatalf("Record via WithTx: %v", err)
	}

	var (
		id, org, action             string
		actor, target, team, detail *string
		createdAt                   time.Time
	)
	// Read back on the admin pool (bypasses RLS — no claims context here).
	if err := h.AdminDB.QueryRow(`
		SELECT id::text, org_id::text, actor_user_id::text, action,
		       target_user_id::text, team_id::text, detail_json, created_at
		FROM access_change_log WHERE org_id = $1
	`, orgID).Scan(&id, &org, &actor, &action, &target, &team, &detail, &createdAt); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if id == "" {
		t.Error("expected gen_random_uuid() id, got empty")
	}
	if action != domain.AccessActionTeamRoleChanged {
		t.Errorf("action = %q, want %q", action, domain.AccessActionTeamRoleChanged)
	}
	if actor == nil || *actor != userID || target == nil || *target != userID {
		t.Errorf("actor/target = %v/%v, want %s/%s", actor, target, userID, userID)
	}
	if team == nil || *team != teamID {
		t.Errorf("team_id = %v, want %s", team, teamID)
	}
	if detail == nil || *detail != `{"new_role":"admin"}` {
		t.Errorf("detail_json = %v, want {\"new_role\":\"admin\"}", detail)
	}
	if createdAt.IsZero() {
		t.Error("created_at is zero, want the now() default")
	}
}

// TestAccessChangeLogStore_Postgres_OptionalColumnsAreNull confirms a credential
// action with no actor/target/team and empty detail serializes those columns to
// SQL NULL via the NULLIF guards.
func TestAccessChangeLogStore_Postgres_OptionalColumnsAreNull(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "founder")

	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// A bare credential_set: no actor/target/team, empty detail.
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			Action: domain.AccessActionCredentialSet,
		})
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var actor, target, team, detail *string
	if err := h.AdminDB.QueryRow(`
		SELECT actor_user_id::text, target_user_id::text, team_id::text, detail_json
		FROM access_change_log WHERE org_id = $1
	`, orgID).Scan(&actor, &target, &team, &detail); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if actor != nil || target != nil || team != nil || detail != nil {
		t.Errorf("optional columns = (%v,%v,%v,%v), want all NULL", actor, target, team, detail)
	}
}

// TestAccessChangeLogStore_Postgres_ListByOrg pins newest-first ordering and the
// Limit bound, read back through the app pool (RLS-active) inside WithTx — the
// shape the future org-admin audit view uses.
func TestAccessChangeLogStore_Postgres_ListByOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "founder")

	// Seed three rows with explicit, increasing created_at on the admin pool so
	// the DESC ordering is deterministic (the column DEFAULT now() would tie).
	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	for i, action := range []string{
		domain.AccessActionOrgMemberGranted,
		domain.AccessActionOrgRoleChanged,
		domain.AccessActionOrgMemberRevoked,
	} {
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO access_change_log (org_id, action, created_at)
			VALUES ($1, $2, $3)
		`, orgID, action, base.Add(time.Duration(i)*time.Minute))
	}

	var all, limited []domain.AccessChange
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		if all, e = tx.AccessChangeLog.ListByOrg(ctx, orgID, domain.AccessChangeListOpts{}); e != nil {
			return e
		}
		limited, e = tx.AccessChangeLog.ListByOrg(ctx, orgID, domain.AccessChangeListOpts{Limit: 2})
		return e
	}); err != nil {
		t.Fatalf("ListByOrg via WithTx: %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	// Newest-first: the revoke (latest) leads.
	if all[0].Action != domain.AccessActionOrgMemberRevoked {
		t.Errorf("newest action = %q, want %q", all[0].Action, domain.AccessActionOrgMemberRevoked)
	}
	if all[2].Action != domain.AccessActionOrgMemberGranted {
		t.Errorf("oldest action = %q, want %q", all[2].Action, domain.AccessActionOrgMemberGranted)
	}
	if len(limited) != 2 {
		t.Errorf("limited len = %d, want 2", len(limited))
	}
}

// TestAccessChangeLogStore_Postgres_OffsetAndCategory pins the TFAC-484 viewer
// reads through the app pool: Offset pages the newest-first window and Category
// narrows to the membership/credential action sets (an unknown category is no
// filter), all under RLS inside WithTx.
func TestAccessChangeLogStore_Postgres_OffsetAndCategory(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "founder")

	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	for i, action := range []string{
		domain.AccessActionOrgMemberGranted, // oldest
		domain.AccessActionTeamRoleChanged,  //
		domain.AccessActionCredentialSet,    //
		domain.AccessActionOrgMemberRevoked, // newest
	} {
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO access_change_log (org_id, action, created_at)
			VALUES ($1, $2, $3)
		`, orgID, action, base.Add(time.Duration(i)*time.Minute))
	}

	var off, cred, mem, none []domain.AccessChange
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		if off, e = tx.AccessChangeLog.ListByOrg(ctx, orgID, domain.AccessChangeListOpts{Limit: 2, Offset: 1}); e != nil {
			return e
		}
		if cred, e = tx.AccessChangeLog.ListByOrg(ctx, orgID, domain.AccessChangeListOpts{Category: domain.AccessCategoryCredential}); e != nil {
			return e
		}
		if mem, e = tx.AccessChangeLog.ListByOrg(ctx, orgID, domain.AccessChangeListOpts{Category: domain.AccessCategoryMembership}); e != nil {
			return e
		}
		none, e = tx.AccessChangeLog.ListByOrg(ctx, orgID, domain.AccessChangeListOpts{Category: "bogus"})
		return e
	}); err != nil {
		t.Fatalf("ListByOrg via WithTx: %v", err)
	}

	// Newest-first is revoke, credential, team-role, grant; Offset 1 skips revoke.
	if len(off) != 2 || off[0].Action != domain.AccessActionCredentialSet || off[1].Action != domain.AccessActionTeamRoleChanged {
		t.Errorf("offset page actions = %+v, want [credential_set team_role_changed]", actionsOf(off))
	}
	if len(cred) != 1 || cred[0].Action != domain.AccessActionCredentialSet {
		t.Errorf("credential category = %+v, want one credential_set", actionsOf(cred))
	}
	if len(mem) != 3 {
		t.Fatalf("membership category = %+v, want 3 rows", actionsOf(mem))
	}
	for _, r := range mem {
		if r.Action == domain.AccessActionCredentialSet {
			t.Errorf("membership filter leaked credential_set: %+v", actionsOf(mem))
		}
	}
	if len(none) != 4 {
		t.Errorf("unknown category = %+v, want all 4", actionsOf(none))
	}
}

func actionsOf(rows []domain.AccessChange) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Action
	}
	return out
}

// TestAccessChangeLogStore_Postgres_RLSIsolation pins that the org-scoped policy
// keeps one org's audit log invisible to another: a row written under org A is
// readable under A's claims and absent under B's, even when B explicitly asks
// for A's org id (the RLS USING clause filters it). Mirrors the system_llm_runs
// org-scoping intent on the read side that the future audit view exercises.
func TestAccessChangeLogStore_Postgres_RLSIsolation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "org-a")
	orgB, userB, _ := pgtest.SeedOrgWithUser(t, h, "org-b")

	// Each org records one row under its own claims.
	for _, tc := range []struct{ org, user string }{{orgA, userA}, {orgB, userB}} {
		if err := stores.Tx.WithTx(ctx, tc.org, tc.user, func(tx db.TxStores) error {
			return tx.AccessChangeLog.Record(ctx, tc.org, domain.AccessChange{
				ActorUserID: tc.user,
				Action:      domain.AccessActionOrgRoleChanged,
			})
		}); err != nil {
			t.Fatalf("seed row for %s: %v", tc.org, err)
		}
	}

	// Under A's claims: A's own log is visible (1 row).
	var aRows []domain.AccessChange
	if err := stores.Tx.WithTx(ctx, orgA, userA, func(tx db.TxStores) error {
		var e error
		aRows, e = tx.AccessChangeLog.ListByOrg(ctx, orgA, domain.AccessChangeListOpts{})
		return e
	}); err != nil {
		t.Fatalf("list A as A: %v", err)
	}
	if len(aRows) != 1 {
		t.Errorf("A sees %d rows of its own log, want 1", len(aRows))
	}

	// Under B's claims, asking for A's org id: the USING clause filters A's row
	// out, so B sees nothing (cross-org isolation). B still sees its own log.
	var bSeesA, bSeesB []domain.AccessChange
	if err := stores.Tx.WithTx(ctx, orgB, userB, func(tx db.TxStores) error {
		var e error
		if bSeesA, e = tx.AccessChangeLog.ListByOrg(ctx, orgA, domain.AccessChangeListOpts{}); e != nil {
			return e
		}
		bSeesB, e = tx.AccessChangeLog.ListByOrg(ctx, orgB, domain.AccessChangeListOpts{})
		return e
	}); err != nil {
		t.Fatalf("list as B: %v", err)
	}
	if len(bSeesA) != 0 {
		t.Errorf("B sees %d rows of A's log, want 0 (RLS isolation)", len(bSeesA))
	}
	if len(bSeesB) != 1 {
		t.Errorf("B sees %d rows of its own log, want 1", len(bSeesB))
	}
}

// TestAccessChangeLogStore_Postgres_WriteWrongOrgRejected pins the WITH CHECK
// side: recording a row for a different org than the caller's claims is rejected
// by the policy (defense in depth — the org id in the row must match the tx's
// current_org_id).
func TestAccessChangeLogStore_Postgres_WriteWrongOrgRejected(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "org-a")
	orgB, _, _ := pgtest.SeedOrgWithUser(t, h, "org-b")

	// As A, try to write a row stamped for org B → WITH CHECK violation (42501).
	err := stores.Tx.WithTx(ctx, orgA, userA, func(tx db.TxStores) error {
		return tx.AccessChangeLog.Record(ctx, orgB, domain.AccessChange{
			Action: domain.AccessActionCredentialSet,
		})
	})
	pgtest.AssertRLSViolation(t, err)

	// Nothing landed for org B.
	var n int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM access_change_log WHERE org_id = $1`, orgB,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("org B row count = %d, want 0 (write was rejected)", n)
	}
}
