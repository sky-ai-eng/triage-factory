package db

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	_ "modernc.org/sqlite"
)

// The versions either side of the background-jobs-model column. Seeding happens
// between them, so the fixture row below is staged exactly the way a shipped
// build wrote it.
const (
	beforeBackgroundJobsModel = 202608240002
	backgroundJobsModel       = 202608260001
)

// The three system jobs skip their cycle when the org has no background-jobs
// model, and there is no fallback for them to fall back to. An existing local
// install therefore has to come out of this migration with the column already
// filled in, or upgrading would silently stop scoring, classifying and
// profiling until the operator found a setting nobody told them about.
//
// SQLite fills a NOT NULL DEFAULT column for every existing row when it is
// added, which is what makes the DEFAULT the seed. This pins that: it is the
// whole upgrade story for a shipped install.
func TestMigrate_SeedsExistingOrgsBackgroundJobsModel(t *testing.T) {
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
	upToErr := goose.UpTo(database, dir, beforeBackgroundJobsModel)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const orgID = "00000000-0000-0000-0000-000000000001"
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO org_settings (org_id) VALUES ('` + orgID + `')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.UpTo(database, dir, backgroundJobsModel)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo background jobs model: %v", upErr)
	}

	var got string
	if err := database.QueryRow(
		`SELECT background_jobs_model FROM org_settings WHERE org_id = ?`, orgID,
	).Scan(&got); err != nil {
		t.Fatalf("read background_jobs_model: %v", err)
	}
	if got != domain.LocalBackgroundJobsModel {
		t.Errorf("background_jobs_model = %q, want %q — an install that predates the column must keep running its background jobs", got, domain.LocalBackgroundJobsModel)
	}
}

// A fresh local install never runs the migration above against a row: the
// tenant is seeded afterwards, by an INSERT that names only the primary key and
// takes every other column from its DEFAULT. Same value, different path in, and
// it is the one a first run actually takes.
func TestSeedLocalTenantRows_CarriesBackgroundJobsModel(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := SeedLocalTenantRows(context.Background(), database); err != nil {
		t.Fatalf("SeedLocalTenantRows: %v", err)
	}

	var got string
	if err := database.QueryRow(`SELECT background_jobs_model FROM org_settings`).Scan(&got); err != nil {
		t.Fatalf("read background_jobs_model: %v", err)
	}
	if got != domain.LocalBackgroundJobsModel {
		t.Errorf("background_jobs_model = %q, want the local pre-fill %q", got, domain.LocalBackgroundJobsModel)
	}
}

// The pre-fill is dispatched verbatim and validated against the deployment's
// universe on every save, so a value that universe does not offer would be one
// the settings UI refuses to re-save and every background job refuses to run — a
// local install seeded into a state it cannot get out of without editing the DB.
//
// Asked of the LOCAL universe specifically: this column default is SQLite's, and
// SQLite is local, whose runtime is the SDK subprocess.
func TestLocalBackgroundJobsModel_IsOffered(t *testing.T) {
	if !modelcatalog.UniverseFor(false).Offers(domain.LocalBackgroundJobsModel) {
		t.Errorf("LocalBackgroundJobsModel %q is not a model a local deployment offers", domain.LocalBackgroundJobsModel)
	}
}
