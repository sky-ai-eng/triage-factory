package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAccessChangeLogStore_SQLite_RoundTrip pins that Record inserts a row whose
// every field reads back intact, an empty id is app-generated, and the empty
// optional columns (actor/target/team/detail) land as SQL NULL. The migration
// is exercised implicitly — BootstrapSchemaForTest runs every forward migration
// (including 202606260001) on top of the baseline. TFAC-471.
func TestAccessChangeLogStore_SQLite_RoundTrip(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	// A fully-populated membership action: actor, target, team, detail all set.
	if err := stores.AccessChangeLog.Record(ctx, runmode.LocalDefaultOrgID, domain.AccessChange{
		ActorUserID:  "actor-1",
		Action:       domain.AccessActionTeamRoleChanged,
		TargetUserID: "target-1",
		TeamID:       "team-1",
		DetailJSON:   `{"new_role":"admin"}`,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		id, org, action             string
		actor, target, team, detail sql.NullString
		createdAt                   time.Time
	)
	if err := conn.QueryRow(`
		SELECT id, org_id, actor_user_id, action, target_user_id, team_id, detail_json, created_at
		FROM access_change_log
	`).Scan(&id, &org, &actor, &action, &target, &team, &detail, &createdAt); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if id == "" {
		t.Error("expected an app-generated id, got empty string")
	}
	if org != runmode.LocalDefaultOrgID {
		t.Errorf("org_id = %q, want %q", org, runmode.LocalDefaultOrgID)
	}
	if action != domain.AccessActionTeamRoleChanged {
		t.Errorf("action = %q, want %q", action, domain.AccessActionTeamRoleChanged)
	}
	if actor.String != "actor-1" || target.String != "target-1" || team.String != "team-1" {
		t.Errorf("actor/target/team = %q/%q/%q, want actor-1/target-1/team-1", actor.String, target.String, team.String)
	}
	if detail.String != `{"new_role":"admin"}` {
		t.Errorf("detail_json = %q, want {\"new_role\":\"admin\"}", detail.String)
	}
	if createdAt.IsZero() {
		t.Error("created_at is zero, want the CURRENT_TIMESTAMP default")
	}
}

// TestAccessChangeLogStore_SQLite_RecordSystem confirms RecordSystem forwards to
// Record in local mode (N=1, no admin/app pool split) — the row lands and reads
// back. The method exists for parity with the Postgres admin pool (TFAC-486).
func TestAccessChangeLogStore_SQLite_RecordSystem(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.AccessChangeLog.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.AccessChange{
		ActorUserID:  "u1",
		Action:       domain.AccessActionOrgMemberGranted,
		TargetUserID: "u1",
		DetailJSON:   `{"source":"sso_jit","role":"member"}`,
	}); err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}

	var action, detail string
	var actor, target sql.NullString
	if err := conn.QueryRow(`
		SELECT action, actor_user_id, target_user_id, detail_json FROM access_change_log
	`).Scan(&action, &actor, &target, &detail); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if action != domain.AccessActionOrgMemberGranted {
		t.Errorf("action = %q, want %q", action, domain.AccessActionOrgMemberGranted)
	}
	if actor.String != "u1" || target.String != "u1" {
		t.Errorf("actor/target = %q/%q, want u1/u1", actor.String, target.String)
	}
	if detail != `{"source":"sso_jit","role":"member"}` {
		t.Errorf("detail_json = %q, want the sso_jit payload", detail)
	}
}

// TestAccessChangeLogStore_SQLite_OptionalColumnsAreNull confirms a credential
// action with no actor/target/team and an empty detail lands those columns as
// SQL NULL (not the empty string).
func TestAccessChangeLogStore_SQLite_OptionalColumnsAreNull(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.AccessChangeLog.Record(ctx, runmode.LocalDefaultOrgID, domain.AccessChange{
		Action: domain.AccessActionCredentialSet,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var actor, target, team, detail sql.NullString
	if err := conn.QueryRow(
		`SELECT actor_user_id, target_user_id, team_id, detail_json FROM access_change_log`,
	).Scan(&actor, &target, &team, &detail); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if actor.Valid || target.Valid || team.Valid || detail.Valid {
		t.Errorf("optional columns = (%v,%v,%v,%v), want all NULL", actor, target, team, detail)
	}
}

// TestAccessChangeLogStore_SQLite_HonorsProvidedID pins that a caller-supplied
// id is preserved (parity with Postgres) rather than overwritten.
func TestAccessChangeLogStore_SQLite_HonorsProvidedID(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const id = "11111111-1111-1111-1111-111111111111"
	if err := stores.AccessChangeLog.Record(ctx, runmode.LocalDefaultOrgID, domain.AccessChange{
		ID:     id,
		Action: domain.AccessActionOrgMemberRevoked,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var got string
	if err := conn.QueryRow(`SELECT id FROM access_change_log`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != id {
		t.Errorf("id = %q, want %q (caller-supplied id must be honored)", got, id)
	}
}

// TestAccessChangeLogStore_SQLite_ListByOrg pins newest-first ordering and the
// Limit bound. Rows are inserted with explicit, distinct created_at values
// (Record itself stamps the column DEFAULT, whose seconds granularity would tie)
// so the ORDER BY created_at DESC is asserted deterministically.
func TestAccessChangeLogStore_SQLite_ListByOrg(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	seed := func(id, action string, ts time.Time) {
		if _, err := conn.Exec(`
			INSERT INTO access_change_log (id, org_id, action, created_at)
			VALUES (?, ?, ?, ?)
		`, id, runmode.LocalDefaultOrgID, action, ts); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	seed("a", domain.AccessActionOrgMemberGranted, base)
	seed("b", domain.AccessActionOrgRoleChanged, base.Add(time.Minute))
	seed("c", domain.AccessActionOrgMemberRevoked, base.Add(2*time.Minute))

	all, _, err := stores.AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{})
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	// Newest-first: c (latest), then b, then a.
	if all[0].ID != "c" || all[1].ID != "b" || all[2].ID != "a" {
		t.Errorf("order = %q/%q/%q, want c/b/a (newest first)", all[0].ID, all[1].ID, all[2].ID)
	}
	if all[0].Action != domain.AccessActionOrgMemberRevoked {
		t.Errorf("newest action = %q, want %q", all[0].Action, domain.AccessActionOrgMemberRevoked)
	}

	limited, _, err := stores.AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("ListByOrg limited: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "c" || limited[1].ID != "b" {
		t.Errorf("limited = %v, want [c b]", idsOf(limited))
	}
}

// TestAccessChangeLogStore_SQLite_OffsetAndCategory pins the TFAC-484 viewer
// reads: Offset pages the newest-first window, and Category narrows to the
// action set for "membership" vs "credential" (an empty/unknown category is no
// filter).
func TestAccessChangeLogStore_SQLite_OffsetAndCategory(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	seed := func(id, action string, ts time.Time) {
		if _, err := conn.Exec(`
			INSERT INTO access_change_log (id, org_id, action, created_at)
			VALUES (?, ?, ?, ?)
		`, id, runmode.LocalDefaultOrgID, action, ts); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	base := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	seed("m1", domain.AccessActionOrgMemberGranted, base)
	seed("m2", domain.AccessActionTeamRoleChanged, base.Add(time.Minute))
	seed("c1", domain.AccessActionCredentialSet, base.Add(2*time.Minute))
	seed("m3", domain.AccessActionOrgMemberRevoked, base.Add(3*time.Minute))

	// Offset skips the newest row(s): newest-first is m3, c1, m2, m1.
	off, _, err := stores.AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("ListByOrg offset: %v", err)
	}
	if len(off) != 2 || off[0].ID != "c1" || off[1].ID != "m2" {
		t.Errorf("offset page = %v, want [c1 m2]", idsOf(off))
	}

	// Category=credential → only the credential_set row.
	cred, _, err := stores.AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Category: domain.AccessCategoryCredential})
	if err != nil {
		t.Fatalf("ListByOrg credential: %v", err)
	}
	if len(cred) != 1 || cred[0].ID != "c1" {
		t.Errorf("credential category = %v, want [c1]", idsOf(cred))
	}

	// Category=membership → the three membership rows, no credential_set.
	mem, _, err := stores.AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Category: domain.AccessCategoryMembership})
	if err != nil {
		t.Fatalf("ListByOrg membership: %v", err)
	}
	if len(mem) != 3 {
		t.Fatalf("membership category = %v, want 3 rows", idsOf(mem))
	}
	for _, r := range mem {
		if r.Action == domain.AccessActionCredentialSet {
			t.Errorf("membership filter leaked credential_set: %v", idsOf(mem))
		}
	}

	// An unrecognized category is no filter — every row comes back.
	none, _, err := stores.AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Category: "bogus"})
	if err != nil {
		t.Fatalf("ListByOrg unknown category: %v", err)
	}
	if len(none) != 4 {
		t.Errorf("unknown category = %v, want all 4", idsOf(none))
	}
}

// TestAccessChangeLogStore_SQLite_RollbackDiscardsRow proves Record composes
// with the surrounding transaction: a WithTx that records a row then returns an
// error rolls the row back, so the log can't diverge from a rolled-back action.
func TestAccessChangeLogStore_SQLite_RollbackDiscardsRow(t *testing.T) {
	conn := newSQLiteForAccessChangeTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	wantErr := errAccessTestRollback
	err := stores.Tx.WithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		if rerr := tx.AccessChangeLog.Record(ctx, runmode.LocalDefaultOrgID, domain.AccessChange{
			Action: domain.AccessActionOrgRoleChanged,
		}); rerr != nil {
			return rerr
		}
		return wantErr // force rollback after the record
	})
	if err != wantErr {
		t.Fatalf("WithTx err = %v, want forced rollback sentinel", err)
	}

	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM access_change_log`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("row count = %d after rollback, want 0", n)
	}
}

func idsOf(rows []domain.AccessChange) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

var errAccessTestRollback = errAccessTest("forced rollback")

type errAccessTest string

func (e errAccessTest) Error() string { return string(e) }

func newSQLiteForAccessChangeTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
