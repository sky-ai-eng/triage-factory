package sqlite_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func seedSQLiteOrgForApps(t *testing.T, conn *sql.DB, orgID string) {
	t.Helper()
	// BootstrapSchemaForTest pre-seeds the LocalDefaultOrgID org, so this
	// is idempotent for that id and inserts for any other.
	if _, err := conn.Exec(`INSERT OR IGNORE INTO orgs (id, slug, name) VALUES (?, ?, ?)`,
		orgID, "app-org-"+orgID, "App Org"); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
}

func seedSQLiteApp(t *testing.T, conn *sql.DB, orgID, appID, pemRef string) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT INTO org_github_apps
			(org_id, app_id, slug, client_id, client_secret_ref, pem_ref, webhook_secret_ref)
		VALUES (?, ?, 'tf-app', 'Iv1.x', 'cs_ref', ?, 'wh_ref')
	`, orgID, appID, pemRef); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}
}

// TestGitHubAppsStore_SQLite_CreateGetRoundTrip pins the CreateForOrg →
// GetForOrg round-trip with a focus on owner_type (TFAC-325): an App
// registered under an organization reads back as "org".
func TestGitHubAppsStore_SQLite_CreateGetRoundTrip(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	// Empty table → nil, no error.
	got, err := stores.GitHubApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("GetForOrg on empty table = %+v, want nil", got)
	}

	if err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID:            orgID,
		AppID:            "4242",
		Slug:             "tf-roundtrip",
		ClientID:         "Iv1.roundtrip",
		ClientSecretRef:  "cs_ref",
		PEMRef:           "pem_ref",
		WebhookSecretRef: "wh_ref",
		OwnerType:        "org",
	}); err != nil {
		t.Fatalf("CreateForOrg: %v", err)
	}

	got, err = stores.GitHubApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	if got == nil {
		t.Fatal("GetForOrg returned nil after Create")
	}
	if got.OwnerType != "org" {
		t.Errorf("OwnerType = %q, want org", got.OwnerType)
	}
	if got.AppID != "4242" || got.Slug != "tf-roundtrip" {
		t.Errorf("round-trip mismatch: app_id=%q slug=%q", got.AppID, got.Slug)
	}
}

// TestGitHubAppsStore_SQLite_OwnerTypeDefaultsToUser pins that an App
// created without an explicit OwnerType reads back as "user" — the store
// folds the empty value (NormalizedOwnerType) so the persisted value is
// never an empty string, matching the column default.
func TestGitHubAppsStore_SQLite_OwnerTypeDefaultsToUser(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	if err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID:            orgID,
		AppID:            "9001",
		Slug:             "tf-default",
		ClientID:         "Iv1.default",
		ClientSecretRef:  "cs",
		PEMRef:           "pem",
		WebhookSecretRef: "wh",
		// OwnerType intentionally unset.
	}); err != nil {
		t.Fatalf("CreateForOrg: %v", err)
	}

	got, err := stores.GitHubApps.GetForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	if got == nil || got.OwnerType != "user" {
		t.Fatalf("OwnerType = %+v, want user (unset folds to default)", got)
	}
}

// TestGitHubAppsStore_SQLite_InstallationLifecycle pins Upsert →
// MarkRemoved → Upsert-revive against the active-only read.
func TestGitHubAppsStore_SQLite_InstallationLifecycle(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	inst := domain.OrgGitHubAppInstallation{
		InstallationID: "555",
		OrgID:          orgID,
		AccountType:    "Organization",
		AccountLogin:   "acme",
	}
	if err := stores.GitHubApps.UpsertInstallation(ctx, inst); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}

	got, err := stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("ListInstallationsForOrg: %v", err)
	}
	if len(got) != 1 || got[0].InstallationID != "555" || got[0].AccountLogin != "acme" {
		t.Fatalf("after upsert got %+v, want one acme/555 row", got)
	}
	if got[0].InstalledAt.IsZero() {
		t.Error("InstalledAt is zero; want defaulted CURRENT_TIMESTAMP")
	}

	if err := stores.GitHubApps.MarkInstallationRemoved(ctx, orgID, "555"); err != nil {
		t.Fatalf("MarkInstallationRemoved: %v", err)
	}
	got, _ = stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if len(got) != 0 {
		t.Fatalf("after remove got %d rows, want 0", len(got))
	}

	// Re-upsert the same installation_id revives the row (removed_at cleared).
	inst.AccountLogin = "acme-renamed"
	if err := stores.GitHubApps.UpsertInstallation(ctx, inst); err != nil {
		t.Fatalf("UpsertInstallation (revive): %v", err)
	}
	got, _ = stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if len(got) != 1 || got[0].AccountLogin != "acme-renamed" {
		t.Fatalf("after revive got %+v, want one acme-renamed row", got)
	}
}

// TestGitHubAppsStore_SQLite_InstallationCrossOrg pins the composite
// (org_id, installation_id) key: the same numeric installation_id under
// two orgs (legal across distinct GitHub hosts) stays two independent
// rows, and a delete for one org never touches the other's same-id row.
func TestGitHubAppsStore_SQLite_InstallationCrossOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const orgA = "00000000-0000-0000-0000-0000000000aa"
	const orgB = "00000000-0000-0000-0000-0000000000bb"
	seedSQLiteOrgForApps(t, conn, orgA)
	seedSQLiteOrgForApps(t, conn, orgB)

	for _, tc := range []struct{ org, login, typ string }{
		{orgA, "only-in-a", "User"},
		{orgB, "only-in-b", "Organization"},
	} {
		if err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: "900", OrgID: tc.org, AccountType: tc.typ, AccountLogin: tc.login,
		}); err != nil {
			t.Fatalf("UpsertInstallation %s: %v", tc.org, err)
		}
	}

	gotA, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgA)
	if len(gotA) != 1 || gotA[0].AccountLogin != "only-in-a" {
		t.Errorf("orgA = %+v, want one only-in-a row (orgB's same-id upsert leaked)", gotA)
	}

	if err := stores.GitHubApps.MarkInstallationRemoved(ctx, orgB, "900"); err != nil {
		t.Fatalf("MarkInstallationRemoved orgB: %v", err)
	}
	if got, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgA); len(got) != 1 {
		t.Errorf("orgA lost its row to orgB's delete: %+v", got)
	}
	if got, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgB); len(got) != 0 {
		t.Errorf("orgB still active after its own delete: %+v", got)
	}
}

// TestGitHubAppsStore_SQLite_Backfill exercises the full backfill: read
// app row + base URL, mint a JWT from the keychain-stored PEM, list
// installations against a mock GitHub, and upsert them.
func TestGitHubAppsStore_SQLite_Backfill(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/app/installations" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"id": 11, "account": {"login": "org-eng", "type": "Organization"}, "created_at": "2026-01-01T00:00:00Z"},
			{"id": 22, "account": {"login": "org-mkt", "type": "Organization"}, "created_at": "2026-01-02T00:00:00Z"}
		]`))
	}))
	defer srv.Close()

	seedSQLiteOrgForApps(t, conn, orgID)
	const pemRef = "github_app_777_pem"
	seedSQLiteApp(t, conn, orgID, "777", pemRef)
	if _, err := conn.Exec(`
		INSERT INTO org_settings (org_id, github_base_url) VALUES (?, ?)
		ON CONFLICT(org_id) DO UPDATE SET github_base_url = excluded.github_base_url
	`, orgID, srv.URL); err != nil {
		t.Fatalf("seed org_settings: %v", err)
	}
	// Store the App PEM the backfill reads via SecretStore.GetSystem.
	if err := stores.Secrets.Put(ctx, orgID, pemRef, testRSAPEM(t), "test app pem"); err != nil {
		t.Fatalf("seed pem secret: %v", err)
	}

	// Pre-seed a stale active installation (33) that the mock GitHub no
	// longer reports — the reconcile must soft-remove it.
	if err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID: "33", OrgID: orgID, AccountType: "User", AccountLogin: "departed",
	}); err != nil {
		t.Fatalf("seed stale installation: %v", err)
	}

	if err := stores.GitHubApps.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
		t.Fatalf("BackfillInstallationsFromAPI: %v", err)
	}

	got, err := stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("ListInstallationsForOrg: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("backfill left %d active rows, want 2 (stale 33 reconciled away): %+v", len(got), got)
	}
	// Ordered by account_login: org-eng (11), org-mkt (22); "departed" gone.
	if got[0].InstallationID != "11" || got[1].InstallationID != "22" {
		t.Errorf("installation ids = %q, %q; want 11, 22", got[0].InstallationID, got[1].InstallationID)
	}
}

// TestGitHubAppsStore_SQLite_BackfillNoApp is a no-op (and no error) when
// the org has no registered App.
func TestGitHubAppsStore_SQLite_BackfillNoApp(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	if err := stores.GitHubApps.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
		t.Fatalf("BackfillInstallationsFromAPI with no App = %v; want nil", err)
	}
}

// testRSAPEM returns a fresh PKCS#1 RSA private key in PEM form for
// JWT signing in the backfill path.
func testRSAPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}
