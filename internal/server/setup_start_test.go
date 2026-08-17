package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newTenantlessServer returns a server backed by a freshly-migrated but
// UNPROVISIONED SQLite DB — the genuine fresh-install shape now that
// nothing provisions at boot. Unlike newTestServer (which seeds the
// tenant + agent via BootstrapSchemaForTest), this uses Migrate directly
// so there are zero tenant rows, exercising the first-run + provision
// paths. Returns the raw DB so tests can assert row state.
func newTenantlessServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(database, sqlitestore.New(database)), database
}

func countTable(t *testing.T, database *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestIntegrationsStatus_TenantlessNotConfigured pins the first-run
// detection: a tenant-less install reports configured=false so the
// AuthGate routes to the "Start your factory" screen. The server
// must serve the endpoint cleanly with zero tenant rows.
func TestIntegrationsStatus_TenantlessNotConfigured(t *testing.T) {
	keyring.MockInit()
	s, database := newTenantlessServer(t)

	if n := countTable(t, database, "orgs"); n != 0 {
		t.Fatalf("tenant-less server has %d orgs rows; want 0", n)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/integrations/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["configured"] != false {
		t.Errorf("configured=%v; want false (no tenant provisioned yet)", body["configured"])
	}
}

// TestSetupStart_ProvisionsAndSeeds pins the explicit provision action:
// POST /api/setup/start on an empty DB creates the synthetic tenant +
// materializes the shipped defaults through the org template, and the
// status endpoint flips to configured=true.
func TestSetupStart_ProvisionsAndSeeds(t *testing.T) {
	keyring.MockInit()
	s, database := newTenantlessServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/setup/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup/start: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["provisioned"] != true {
		t.Errorf("provisioned=%v; want true", resp["provisioned"])
	}
	if resp["already_provisioned"] != false {
		t.Errorf("already_provisioned=%v; want false on first provision", resp["already_provisioned"])
	}

	// Tenant + agent rows now exist.
	for _, table := range []string{"orgs", "teams", "users", "agents", "team_agents"} {
		if n := countTable(t, database, table); n != 1 {
			t.Errorf("%s row count=%d after provision; want 1", table, n)
		}
	}
	// Shipped defaults materialized through the template (not zero).
	var handlers int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND team_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
	).Scan(&handlers); err != nil {
		t.Fatalf("count handlers: %v", err)
	}
	if handlers == 0 {
		t.Error("no shipped event handlers after setup/start")
	}

	// Status now reports configured.
	statusRec := doJSON(t, s, http.MethodGet, "/api/integrations/status", nil)
	var status map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status["configured"] != true {
		t.Errorf("configured=%v after provision; want true", status["configured"])
	}
}

// TestSetupStart_PartialProvisionRecovery pins that setup/start completes a
// crash-mid-provision state (CreateLocalTenant committed, binary died before
// BootstrapNewOrg ran) without requiring a clean-slate reinstall. The endpoint
// must not treat "org row exists, no agent" as "already_provisioned".
func TestSetupStart_PartialProvisionRecovery(t *testing.T) {
	keyring.MockInit()
	s, database := newTenantlessServer(t)

	// Simulate crash-mid-provision: tenant rows committed, agent never created.
	stores := sqlitestore.New(database)
	if err := stores.Orgs.CreateLocalTenant(t.Context()); err != nil {
		t.Fatalf("CreateLocalTenant (partial): %v", err)
	}
	if n := countTable(t, database, "agents"); n != 0 {
		t.Fatalf("partial state has %d agent rows; want 0", n)
	}

	// Endpoint must complete the provision, not return already_provisioned.
	rec := doJSON(t, s, http.MethodPost, "/api/setup/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup/start after partial: got=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["provisioned"] != true {
		t.Errorf("provisioned=%v; want true", resp["provisioned"])
	}
	if resp["already_provisioned"] != false {
		t.Errorf("already_provisioned=%v; want false (completed a partial, not a no-op)", resp["already_provisioned"])
	}

	// Full provision now complete.
	for _, table := range []string{"agents", "team_agents"} {
		if n := countTable(t, database, table); n != 1 {
			t.Errorf("%s=%d after partial provision recovery; want 1", table, n)
		}
	}
}

// TestSetupStart_NonResurrection is the durability guarantee the whole
// ticket exists for: provision, delete a shipped (system) handler, then
// re-call setup/start. Because the endpoint no-ops once a tenant exists,
// the deleted handler stays deleted — it does not resurrect (the analog
// of "survives a restart" now that boot seeds nothing).
func TestSetupStart_NonResurrection(t *testing.T) {
	keyring.MockInit()
	s, database := newTenantlessServer(t)

	if rec := doJSON(t, s, http.MethodPost, "/api/setup/start", nil); rec.Code != http.StatusOK {
		t.Fatalf("first setup/start: got=%d body=%s", rec.Code, rec.Body.String())
	}

	// Pick a shipped handler and hard-delete it (the user's "fully delete
	// a system default" gesture).
	var victimSlug string
	if err := database.QueryRow(
		`SELECT system_slug FROM event_handlers WHERE org_id = ? AND team_id = ? AND source = 'system' AND system_slug IS NOT NULL LIMIT 1`,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
	).Scan(&victimSlug); err != nil {
		t.Fatalf("pick victim handler: %v", err)
	}
	if _, err := database.Exec(
		`DELETE FROM event_handlers WHERE org_id = ? AND team_id = ? AND system_slug = ?`,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, victimSlug,
	); err != nil {
		t.Fatalf("delete victim: %v", err)
	}

	// Re-call the provision endpoint — must be a no-op (tenant exists).
	rec := doJSON(t, s, http.MethodPost, "/api/setup/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second setup/start: got=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["already_provisioned"] != true {
		t.Errorf("already_provisioned=%v on re-call; want true (no-op)", resp["already_provisioned"])
	}

	// The deleted handler stays deleted.
	var resurrected int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND team_id = ? AND system_slug = ?`,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, victimSlug,
	).Scan(&resurrected); err != nil {
		t.Fatalf("count resurrected: %v", err)
	}
	if resurrected != 0 {
		t.Errorf("deleted system handler %q came back after re-provision (count=%d); want it to stay deleted", victimSlug, resurrected)
	}
}
