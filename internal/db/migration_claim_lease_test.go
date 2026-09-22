package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// The migration that gives claims a lease, and the version just before it, at
// which the test seeds the previous shape.
const (
	claimLeaseFile  = "migrations-sqlite/202609230001_claim_lease.sql"
	claimLeasePrior = 202609220002
)

// TestMigrate_ClaimLease pins the upgrade: a claim live across it carries an
// already-expired lease in the layout the fence compares against, a released
// claim carries none, the expiry index exists, and a second Migrate is a
// no-op.
func TestMigrate_ClaimLease(t *testing.T) {
	database := openMigrationsTestDB(t)
	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations-sqlite", claimLeasePrior); err != nil {
		t.Fatalf("goose UpTo %d: %v", claimLeasePrior, err)
	}
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const (
		orgID  = "00000000-0000-0000-0000-000000000001"
		teamID = "00000000-0000-0000-0000-000000000010"
		userID = "00000000-0000-0000-0000-000000000100"
	)
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES ('c-live', '` + orgID + `', '` + teamID + `', 'interactive', NULL)`,
		`INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES ('c-done', '` + orgID + `', '` + teamID + `', 'interactive', 'completed')`,
		`INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
			VALUES ('cl-live', '` + orgID + `', 'c-live', 'exec-1', 1)`,
		`INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, released_at, outcome)
			VALUES ('cl-done', '` + orgID + `', 'c-done', 'exec-1', 1, strftime('%Y-%m-%d %H:%M:%f','now'), 'completed')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// CAST to TEXT: the column is declared DATETIME, so the driver would
	// otherwise hand back a time.Time rendered in its own layout, and the
	// layout stored is exactly what this test is about.
	var live sql.NullString
	if err := database.QueryRow(`SELECT CAST(lease_expires_at AS TEXT) FROM claims WHERE id = 'cl-live'`).Scan(&live); err != nil {
		t.Fatalf("read the live claim's lease: %v", err)
	}
	if !live.Valid {
		t.Fatal("a claim live across the upgrade carries no lease; the live-claim invariant is already broken")
	}
	// The layout is the one the fence and the renewal compare against, so a
	// value Go cannot parse under it is one SQL would not compare either.
	expiry, err := time.Parse("2006-01-02 15:04:05.000", live.String)
	if err != nil {
		t.Fatalf("lease_expires_at %q is not the fence's layout: %v", live.String, err)
	}
	if !expiry.Before(time.Now().UTC()) {
		t.Errorf("lease_expires_at = %s, want an already-lapsed lease", expiry)
	}

	var released sql.NullString
	if err := database.QueryRow(`SELECT CAST(lease_expires_at AS TEXT) FROM claims WHERE id = 'cl-done'`).Scan(&released); err != nil {
		t.Fatalf("read the released claim's lease: %v", err)
	}
	if released.Valid {
		t.Errorf("released claim gained lease_expires_at = %q, want NULL: release never touches the column", released.String)
	}

	var idx int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_claims_live_expiry'
	`).Scan(&idx); err != nil {
		t.Fatalf("scan for the expiry index: %v", err)
	}
	if idx != 1 {
		t.Errorf("idx_claims_live_expiry present = %d, want 1", idx)
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var after sql.NullString
	if err := database.QueryRow(`SELECT CAST(lease_expires_at AS TEXT) FROM claims WHERE id = 'cl-live'`).Scan(&after); err != nil {
		t.Fatalf("re-read the live claim's lease: %v", err)
	}
	if after.String != live.String {
		t.Errorf("second Migrate rewrote the lease: %q -> %q", live.String, after.String)
	}
}
