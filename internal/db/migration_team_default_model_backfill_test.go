package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

// The versions either side of the enable-set migration. Seeding happens between
// them, so the fixture rows below are staged exactly the way a shipped build
// wrote them.
const (
	beforeModelEnableSets = 202608260005
	modelEnableSets       = 202608270001
)

// A team default of "" used to resolve to the shipped default; nothing resolves
// it now, so an empty column is a team whose every unpinned step refuses. The
// save that could write one is gone, which leaves the rows already holding it —
// and this is the whole upgrade story for those teams.
//
// The value asserted is the column's own DEFAULT, which is also what the
// deleted fallback resolved to, so a team that cleared its default keeps running
// on exactly the model it was running on before the upgrade.
func TestMigrate_BackfillsClearedTeamDefaultModel(t *testing.T) {
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
	upToErr := goose.UpTo(database, dir, beforeModelEnableSets)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const orgID = "00000000-0000-0000-0000-000000000001"
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('t-cleared', '` + orgID + `', 'cleared', 'Cleared')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('t-picked', '` + orgID + `', 'picked', 'Picked')`,
		`INSERT INTO team_settings (team_id, default_model) VALUES ('t-cleared', '')`,
		`INSERT INTO team_settings (team_id, default_model) VALUES ('t-picked', '` + domain.ModelAliasOpus + `')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.UpTo(database, dir, modelEnableSets)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo model enable sets: %v", upErr)
	}

	defaultModel := func(teamID string) string {
		t.Helper()
		var got string
		if err := database.QueryRow(
			`SELECT default_model FROM team_settings WHERE team_id = ?`, teamID,
		).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", teamID, err)
		}
		return got
	}
	if got := defaultModel("t-cleared"); got != domain.LocalDefaultModel {
		t.Errorf("a cleared team default = %q, want %q — an empty one refuses every unpinned step",
			got, domain.LocalDefaultModel)
	}
	// The backfill is targeted: a team that picked a model keeps it.
	if got := defaultModel("t-picked"); got != domain.ModelAliasOpus {
		t.Errorf("a chosen team default = %q, want %q untouched", got, domain.ModelAliasOpus)
	}
}
