package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	_ "modernc.org/sqlite"
)

// The versions either side of the model-vocabulary rewrite. Seeding happens
// between them, so the fixture rows below are staged exactly the way a shipped
// build wrote them.
const (
	beforeConcreteModelIDs = 202608220001
	concreteModelIDs       = 202608230001
)

// The migration that rewrites stored configuration from the three Claude Code
// tier words to concrete model ids.
//
// It runs on real deployed data, where a team default and a per-prompt
// override each hold whichever of "haiku" / "sonnet" / "opus" a person picked
// — values the raw provider API rejects and the pricing datasheet cannot cost.
// The assertions pin all four rules at once: user pins map, system-source
// prompts land unset (a shipped seed names no model any more), already-concrete
// and empty values are left exactly as they are, and what a past run actually
// executed on is never rewritten.
func TestMigrate_RewritesStoredModelVocabularyToConcreteIDs(t *testing.T) {
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
	upToErr := goose.UpTo(database, dir, beforeConcreteModelIDs)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const (
		orgID   = "00000000-0000-0000-0000-000000000001"
		teamID  = "00000000-0000-0000-0000-000000000010"
		altTeam = "00000000-0000-0000-0000-000000000011"
		userID  = "00000000-0000-0000-0000-000000000100"
	)
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + altTeam + `', '` + orgID + `', 'second', 'Second')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		// The team default a shipped build wrote, and one a person moved up.
		`INSERT INTO team_settings (team_id, default_model) VALUES ('` + teamID + `', 'sonnet')`,
		`INSERT INTO team_settings (team_id, default_model) VALUES ('` + altTeam + `', 'opus')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	seedPrompt := func(id, source, model string) {
		t.Helper()
		var creator any
		if source != "system" {
			creator = userID
		}
		if _, err := database.Exec(
			`INSERT INTO prompts (id, org_id, team_id, name, body, source, model, creator_user_id)
			 VALUES (?, ?, ?, ?, 'body', ?, ?, ?)`,
			id, orgID, teamID, id, source, model, creator,
		); err != nil {
			t.Fatalf("seed prompt %s: %v", id, err)
		}
	}
	// Every value a deployed build could hold, across both provenances.
	seedPrompt("user-haiku", "user", "haiku")
	seedPrompt("user-sonnet", "user", "sonnet")
	seedPrompt("user-opus", "user", "opus")
	seedPrompt("user-unset", "user", "")
	seedPrompt("user-already-concrete", "user", "claude-opus-4-8")
	seedPrompt("imported-haiku", "imported", "haiku")
	seedPrompt("system-haiku", "system", "haiku")
	seedPrompt("system-unset", "system", "")

	// What a past run executed on. Never rewritten: the transcript and the
	// ledger describe a run that already happened.
	if _, err := database.Exec(
		`INSERT INTO conversations (id, org_id, team_id, origin, status, model)
		 VALUES ('conv-historical', ?, ?, 'interactive', 'completed', 'sonnet')`,
		orgID, teamID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, concreteModelIDs)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo concrete model ids: %v", upErr)
	}

	promptModel := func(id string) string {
		t.Helper()
		var got string
		if err := database.QueryRow(`SELECT model FROM prompts WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read prompt %s: %v", id, err)
		}
		return got
	}
	for id, want := range map[string]string{
		"user-haiku":            domain.ModelHaiku,
		"user-sonnet":           domain.ModelSonnet,
		"user-opus":             domain.ModelOpus,
		"user-unset":            "",
		"user-already-concrete": "claude-opus-4-8",
		"imported-haiku":        domain.ModelHaiku,
		// A shipped seed names no model; the row it seeded stops naming one too.
		"system-haiku": "",
		"system-unset": "",
	} {
		if got := promptModel(id); got != want {
			t.Errorf("prompts.model for %s = %q, want %q", id, got, want)
		}
	}

	teamModel := func(id string) string {
		t.Helper()
		var got string
		if err := database.QueryRow(`SELECT default_model FROM team_settings WHERE team_id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read team_settings %s: %v", id, err)
		}
		return got
	}
	if got := teamModel(teamID); got != domain.ModelSonnet {
		t.Errorf("team default = %q, want %q", got, domain.ModelSonnet)
	}
	if got := teamModel(altTeam); got != domain.ModelOpus {
		t.Errorf("second team default = %q, want %q", got, domain.ModelOpus)
	}

	// The rebuild's real payload: the daily-cost-cap surgery materializes a
	// row without naming default_model, so what the column defaults to is what
	// that team runs on.
	const thirdTeam = "00000000-0000-0000-0000-000000000012"
	if _, err := database.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'third', 'Third')`, thirdTeam, orgID,
	); err != nil {
		t.Fatalf("seed third team: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO team_settings (team_id, max_daily_cost_usd) VALUES (?, 5.0)`, thirdTeam,
	); err != nil {
		t.Fatalf("partial insert: %v", err)
	}
	if got := teamModel(thirdTeam); got != domain.DefaultModel {
		t.Errorf("column default materialized %q, want %q", got, domain.DefaultModel)
	}

	// Everything else the rebuild copied is still there, with its defaults.
	var (
		branch   string
		posture  string
		autoMode int
	)
	if err := database.QueryRow(
		`SELECT branch_template, review_posture, auto_mode_enabled FROM team_settings WHERE team_id = ?`,
		teamID,
	).Scan(&branch, &posture, &autoMode); err != nil {
		t.Fatalf("read rebuilt columns: %v", err)
	}
	if branch != domain.DefaultBranchTemplate || posture != domain.DefaultReviewPosture || autoMode != 1 {
		t.Errorf("rebuilt row lost a column: branch=%q posture=%q auto_mode=%d", branch, posture, autoMode)
	}

	var historical string
	if err := database.QueryRow(`SELECT model FROM conversations WHERE id = 'conv-historical'`).Scan(&historical); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if historical != "sonnet" {
		t.Errorf("conversations.model = %q; a past run's record must not be rewritten", historical)
	}
}
