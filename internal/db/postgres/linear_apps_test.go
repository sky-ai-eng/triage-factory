package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestLinearAppsStore_Postgres_ReturnedRowConformance runs the shared suite
// under the registrant's claims on the app pool, for the reason
// TestJiraAppsStore_Postgres_ReturnedRowConformance gives: a RETURNING on a
// BYPASSRLS connection hands back its row whatever the SELECT policy says, so
// only a claims-carrying transaction tests the upsert's read-back.
func TestLinearAppsStore_Postgres_ReturnedRowConformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).LinearApps
		dbtest.RunLinearAppsReturnedRowConformance(t, func(t *testing.T) (db.LinearAppsStore, string, string) {
			t.Helper()
			return store, orgID, userID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestLinearInstallsStore_Postgres_Conformance runs the installs suite on the
// admin pool, where every write to org_linear_installs goes. Each subtest gets
// two fresh orgs, so the workspace uniqueness it proves spans orgs.
func TestLinearInstallsStore_Postgres_Conformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	dbtest.RunLinearInstallsConformance(t, func(t *testing.T) (db.LinearInstallsStore, string, string, string) {
		t.Helper()
		orgA, userID := seedPgOrgAndUserForGitHubApps(t, h)
		orgB, _ := seedPgOrgAndUserForGitHubApps(t, h)
		return stores.LinearInstalls, orgA, orgB, userID
	})
}

// TestLinearInstallsStore_Postgres_AppPoolCannotWrite pins the posture the
// install callback depends on: members read their org's install, and tf_app
// writes nothing, so the admin pool is the only writer.
func TestLinearInstallsStore_Postgres_AppPoolCannotWrite(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)

	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_linear_installs (org_id, workspace_id, workspace_url_key, app_user_id, app_client_id, installed_at)
		VALUES ($1, 'ws-rls', 'rls', 'app-user', 'client', now())
	`, orgID); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		var ws string
		if err := tx.QueryRow(`SELECT workspace_id FROM org_linear_installs WHERE org_id = $1`, orgID).Scan(&ws); err != nil {
			t.Errorf("member read of the org's install: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser (read): %v", err)
	}

	err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE org_linear_installs SET removed_at = now(), removed_reason = 'disconnected' WHERE org_id = $1`, orgID)
		return err
	})
	if err != nil {
		// An UPDATE whose USING is false filters rather than raises.
		t.Fatalf("app-pool update raised: %v", err)
	}
	var removed sql.NullTime
	if err := h.AdminDB.QueryRow(`SELECT removed_at FROM org_linear_installs WHERE org_id = $1`, orgID).Scan(&removed); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if removed.Valid {
		t.Error("an org owner on the app pool removed the install; only the admin pool may write")
	}

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO org_linear_installs (org_id, workspace_id, workspace_url_key, app_user_id, app_client_id, installed_at)
			VALUES ($1, 'ws-other', 'other', 'app-user', 'client', now())
			ON CONFLICT (org_id) DO NOTHING
		`, orgID)
		return err
	}); err == nil {
		t.Error("an app-pool insert into org_linear_installs succeeded; RLS should refuse it")
	}
}
