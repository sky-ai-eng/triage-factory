package db

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	_ "modernc.org/sqlite"
)

// A fresh local install's tenant is seeded by an INSERT that names only the
// primary key and takes every other column from its DEFAULT, so the column
// default is what a first run's background jobs spend on.
func TestSeedLocalTenantRows_CarriesBackgroundJobsModel(t *testing.T) {
	database := newTenantlessTestDB(t)
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
