package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The migration that gives conversations their boundary columns (202609120002).
// It adds two nullable columns and one partial index and backfills nothing, so
// what there is to pin is that it is additive: rows written by a deployed build
// survive it reading "never ended", the columns then round-trip every value in
// the vocabulary, and the anti-join index the boundary reads rely on exists.
func TestMigrate_ConversationsGainTheirBoundaryColumns(t *testing.T) {
	database := openMigrationsTestDB(t)

	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		gooseMu.Unlock()
		t.Fatalf("SetDialect: %v", err)
	}
	// Stop one version short, so the rows below are staged exactly the way a
	// deployed build wrote them — with no column to end them in.
	upToErr := goose.UpTo(database, dir, 202609120001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
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
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	// origin='interactive' sidesteps the blueprint parentage CHECK, which is
	// irrelevant to what this migration touches.
	seed := func(id, status string) {
		t.Helper()
		if _, err := database.Exec(
			`INSERT INTO conversations (id, org_id, team_id, origin, status)
			 VALUES (?, ?, ?, 'interactive', ?)`,
			id, orgID, teamID, status,
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("legacy-open", "open")
	seed("legacy-completed", "completed")

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202609120002)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo boundaries: %v", upErr)
	}

	boundaryOf := func(id string) (sql.NullString, sql.NullString) {
		t.Helper()
		var at, reason sql.NullString
		if err := database.QueryRow(
			`SELECT ended_at, ended_reason FROM conversations WHERE id = ?`, id).Scan(&at, &reason); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return at, reason
	}

	// No backfill: a conversation that ended before this shipped ended for a
	// reason nothing recorded, and a guessed one is a wrong answer printed to
	// a person later.
	for _, id := range []string{"legacy-open", "legacy-completed"} {
		at, reason := boundaryOf(id)
		if at.Valid || reason.Valid {
			t.Errorf("%s reads (%v, %v) after the migration, want both NULL — nothing recorded why it ended",
				id, at, reason)
		}
	}

	// Every value the Go vocabulary can produce survives a write and a read,
	// alongside the timestamp it was stamped with.
	stamp := time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC)
	for _, r := range domain.AllEndedReasons() {
		id := "ended-" + string(r)
		seed(id, "completed")
		if _, err := database.Exec(
			`UPDATE conversations SET ended_at = ?, ended_reason = ? WHERE id = ?`,
			stamp, string(r), id,
		); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
		var at sql.NullTime
		var reason sql.NullString
		if err := database.QueryRow(
			`SELECT ended_at, ended_reason FROM conversations WHERE id = ?`, id).Scan(&at, &reason); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if !at.Valid || !at.Time.Equal(stamp) || reason.String != string(r) {
			t.Errorf("%s reads (%v, %q), want (%v, %q)", id, at, reason.String, stamp, r)
		}
	}

	// The anti-join the provisioner's sweep and the drivable gate read.
	var indexSQL string
	if err := database.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_conversations_task_ended'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("idx_conversations_task_ended not created: %v", err)
	}
	if indexSQL == "" {
		t.Error("idx_conversations_task_ended exists but carries no definition")
	}
}
