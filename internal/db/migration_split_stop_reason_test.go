package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

// The migration that splits conversations.stop_reason (202608160005). It runs
// on real deployed data, where the column holds one of two unrelated things
// depending on which writer touched the row last: a park's reason, or the
// MODEL's stop reason written straight through by the terminal write.
//
// Renaming alone would carry the second kind forward under a name that lies
// about it — and this value is rendered to a person on the run station — so
// the migration clears anything outside the park vocabulary. The assertions
// below pin both halves: every real park reason survives, every model stop
// reason goes.
//
// The exclusion list in the SQL is spelled out as literals because SQL cannot
// import a Go const. The final assertion is what keeps the two in step: it
// walks domain.AllParkReasons() and fails if the migration cleared one, so a
// reason added in Go and forgotten in the migration is caught here rather than
// by a user whose park reason silently vanished on upgrade.
func TestMigrate_SplitsStopReasonIntoParkReasonAndPerTurnStopReason(t *testing.T) {
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
	// Stop one version short of the split, so the rows below are staged the
	// way a deployed build wrote them.
	upToErr := goose.UpTo(database, dir, 202608160004)
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
		// conversations.creator_user_id defaults to this id and is a FK.
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	seed := func(id, stopReason string) {
		t.Helper()
		var reason any
		if stopReason != "" {
			reason = stopReason
		}
		if _, err := database.Exec(
			`INSERT INTO conversations (id, org_id, team_id, origin, status, stop_reason)
			 VALUES (?, ?, ?, 'interactive', 'open', ?)`,
			id, orgID, teamID, reason,
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// Every park reason a shipped build could have written.
	for _, r := range domain.AllParkReasons() {
		seed("park-"+string(r), string(r))
	}
	// The model's stop reasons, written by the terminal write on the same
	// column. These are the values a rename alone would relabel.
	seed("model-end-turn", "end_turn")
	seed("model-max-tokens", "max_tokens")
	seed("model-tool-use", "tool_use")
	// A provider spelling no build here has ever classified. The exclusion is
	// written against the closed set for exactly this row: the model
	// vocabulary is open, so it can never be enumerated correctly.
	seed("model-unheard-of", "pause_turn")
	// Never parked, never concluded.
	seed("never-set", "")

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202608160005)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo split: %v", upErr)
	}

	parkReasonOf := func(id string) string {
		t.Helper()
		var got sql.NullString
		if err := database.QueryRow(`SELECT park_reason FROM conversations WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return got.String
	}

	for _, id := range []string{"model-end-turn", "model-max-tokens", "model-tool-use", "model-unheard-of"} {
		if got := parkReasonOf(id); got != "" {
			t.Errorf("%s park_reason = %q, want cleared — a model stop reason under a column called park_reason "+
				"is a new lie, and this value is what the run station prints", id, got)
		}
	}
	if got := parkReasonOf("never-set"); got != "" {
		t.Errorf("never-parked row park_reason = %q, want empty", got)
	}
	// The half that matters most: no real park reason is collateral damage of
	// the clean-up. Derived from the vocabulary, so the SQL's literal list
	// cannot fall behind it.
	for _, r := range domain.AllParkReasons() {
		if got := parkReasonOf("park-" + string(r)); got != string(r) {
			t.Errorf("park reason %q survived as %q — the migration's exclusion list has fallen behind "+
				"domain.AllParkReasons()", r, got)
		}
	}

	// The model's half of the split lands on the messages table, per turn.
	if _, err := database.Exec(
		`INSERT INTO messages (org_id, conversation_id, role, content, stop_reason)
		 VALUES (?, 'park-idle', 'assistant', 'work', 'max_tokens')`, orgID,
	); err != nil {
		t.Fatalf("insert a per-turn stop reason: %v", err)
	}
	var perTurn string
	if err := database.QueryRow(
		`SELECT stop_reason FROM messages WHERE conversation_id = 'park-idle'`).Scan(&perTurn); err != nil {
		t.Fatalf("read messages.stop_reason: %v", err)
	}
	if perTurn != "max_tokens" {
		t.Errorf("messages.stop_reason = %q, want max_tokens", perTurn)
	}
}
