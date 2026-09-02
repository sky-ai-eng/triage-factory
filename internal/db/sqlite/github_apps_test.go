package sqlite_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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

	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
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

// TestGitHubAppsStore_SQLite_BotUserID pins the bot_user_id column round-trip
// (TFAC-474): a set bot user id survives Create→Get, and an unset id (0) is
// written as NULL and scans back as 0 (the resolver's "unknown → plain noreply
// form" fallback).
func TestGitHubAppsStore_SQLite_BotUserID(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const orgWithID = "00000000-0000-0000-0000-0000000000c1"
	const orgWithout = "00000000-0000-0000-0000-0000000000c2"
	seedSQLiteOrgForApps(t, conn, orgWithID)
	seedSQLiteOrgForApps(t, conn, orgWithout)

	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: orgWithID, AppID: "7001", Slug: "acme-bot", ClientID: "Iv1.x",
		ClientSecretRef: "cs", PEMRef: "pem", WebhookSecretRef: "wh",
		BotUserID: 41898282,
	}); err != nil {
		t.Fatalf("CreateForOrg (with id): %v", err)
	}
	got, err := stores.GitHubApps.GetForOrg(ctx, orgWithID)
	if err != nil || got == nil {
		t.Fatalf("GetForOrg (with id) = %+v, %v", got, err)
	}
	if got.BotUserID != 41898282 {
		t.Errorf("BotUserID = %d, want 41898282", got.BotUserID)
	}

	// Unset id → stored NULL → scans back as 0.
	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: orgWithout, AppID: "7002", Slug: "no-bot", ClientID: "Iv1.y",
		ClientSecretRef: "cs", PEMRef: "pem", WebhookSecretRef: "wh",
		// BotUserID intentionally unset (0).
	}); err != nil {
		t.Fatalf("CreateForOrg (without id): %v", err)
	}
	got, err = stores.GitHubApps.GetForOrg(ctx, orgWithout)
	if err != nil || got == nil {
		t.Fatalf("GetForOrg (without id) = %+v, %v", got, err)
	}
	if got.BotUserID != 0 {
		t.Errorf("BotUserID = %d, want 0 (NULL scans as 0)", got.BotUserID)
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

	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
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

// TestGitHubAppsStore_SQLite_SetActive flips the staged/live bit and reads it
// back — the cutover mechanism (TFAC-328). A staged App (active=false) becomes
// live on SetActive(true).
func TestGitHubAppsStore_SQLite_SetActive(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: orgID, AppID: "1", Slug: "staged", ClientID: "Iv1.x",
		ClientSecretRef: "cs", PEMRef: "pem", WebhookSecretRef: "wh",
		Active: false, // staged
	}); err != nil {
		t.Fatalf("CreateForOrg: %v", err)
	}
	got, _ := stores.GitHubApps.GetForOrg(ctx, orgID)
	if got == nil || got.Active {
		t.Fatalf("after staged create got %+v, want Active=false", got)
	}

	if _, err := stores.GitHubApps.SetActive(ctx, orgID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	got, _ = stores.GitHubApps.GetForOrg(ctx, orgID)
	if got == nil || !got.Active {
		t.Fatalf("after SetActive(true) got %+v, want Active=true", got)
	}

	// SetActive on an org with no row is a no-op, no error.
	if _, err := stores.GitHubApps.SetActive(ctx, "00000000-0000-0000-0000-0000000000ff", true); err != nil {
		t.Errorf("SetActive on absent row = %v, want nil", err)
	}
}

// TestGitHubAppsStore_SQLite_DeleteForOrg tears down the registration row AND
// its installations — the switch-to-PAT / discard teardown (TFAC-328).
func TestGitHubAppsStore_SQLite_DeleteForOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)
	seedSQLiteApp(t, conn, orgID, "1", "pem")

	for _, login := range []string{"acme", "globex"} {
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: "inst-" + login, OrgID: orgID, AccountType: "Organization", AccountLogin: login,
		}); err != nil {
			t.Fatalf("seed installation %s: %v", login, err)
		}
	}

	if err := stores.GitHubApps.DeleteForOrg(ctx, orgID); err != nil {
		t.Fatalf("DeleteForOrg: %v", err)
	}
	if got, _ := stores.GitHubApps.GetForOrg(ctx, orgID); got != nil {
		t.Errorf("GetForOrg after delete = %+v, want nil", got)
	}
	if insts, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgID); len(insts) != 0 {
		t.Errorf("installations after delete = %+v, want none", insts)
	}

	// DeleteForOrg on an org with no registration is a no-op, no error.
	if err := stores.GitHubApps.DeleteForOrg(ctx, orgID); err != nil {
		t.Errorf("DeleteForOrg on absent row = %v, want nil", err)
	}
}

// TestGitHubAppsStore_SQLite_InstallationLifecycle pins Upsert →
// MarkInstallationRemoved → Upsert-revive against the active-only read.
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
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, inst); err != nil {
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

	if _, err := stores.GitHubApps.MarkInstallationRemoved(ctx, orgID, "555"); err != nil {
		t.Fatalf("MarkInstallationRemoved: %v", err)
	}
	got, _ = stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if len(got) != 0 {
		t.Fatalf("after remove got %d rows, want 0", len(got))
	}

	// Re-upsert the same installation_id revives the row (removed_at cleared).
	inst.AccountLogin = "acme-renamed"
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, inst); err != nil {
		t.Fatalf("UpsertInstallation (revive): %v", err)
	}
	got, _ = stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if len(got) != 1 || got[0].AccountLogin != "acme-renamed" {
		t.Fatalf("after revive got %+v, want one acme-renamed row", got)
	}
}

// TestGitHubAppsStore_SQLite_InstallationCrossOrg pins the composite
// (org_id, installation_id) key: the same numeric installation_id under two
// orgs on two GitHub deployments — the case that is legal, since GitHub numbers
// installations per deployment and not universally — stays two independent
// rows, and a delete for one org never touches the other's same-id row. The
// same id on ONE deployment is a different question, refused by the uniqueness
// index and pinned in the shared host suite.
func TestGitHubAppsStore_SQLite_InstallationCrossOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const orgA = "00000000-0000-0000-0000-0000000000aa"
	const orgB = "00000000-0000-0000-0000-0000000000bb"
	seedSQLiteOrgForApps(t, conn, orgA)
	seedSQLiteOrgForApps(t, conn, orgB)

	for _, tc := range []struct{ org, login, typ, host string }{
		{orgA, "only-in-a", "User", ""},
		{orgB, "only-in-b", "Organization", "https://git.example.com"},
	} {
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: "900", OrgID: tc.org, AccountType: tc.typ, AccountLogin: tc.login,
			GitHubHost: tc.host,
		}); err != nil {
			t.Fatalf("UpsertInstallation %s: %v", tc.org, err)
		}
	}

	gotA, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgA)
	if len(gotA) != 1 || gotA[0].AccountLogin != "only-in-a" {
		t.Errorf("orgA = %+v, want one only-in-a row (orgB's same-id upsert leaked)", gotA)
	}

	if _, err := stores.GitHubApps.MarkInstallationRemoved(ctx, orgB, "900"); err != nil {
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
		INSERT INTO org_event_sources (org_id, kind, base_url) VALUES (?, 'github', ?)
		ON CONFLICT(org_id, kind) DO UPDATE SET base_url = excluded.base_url
	`, orgID, srv.URL); err != nil {
		t.Fatalf("seed org_event_sources: %v", err)
	}
	// Store the App PEM the backfill reads via SecretStore.GetSystem.
	if err := stores.Secrets.Put(ctx, orgID, pemRef, testRSAPEM(t), "test app pem"); err != nil {
		t.Fatalf("seed pem secret: %v", err)
	}

	// Pre-seed a stale active installation (33) that the mock GitHub no
	// longer reports — the reconcile must soft-remove it.
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
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
	// The reconcile is the second installation writer, so every row it mints
	// records the deployment it was listed from — the org's own base URL, not
	// the public host it would fall back to if nobody stamped one.
	for _, inst := range got {
		if inst.GitHubHost != srv.URL {
			t.Errorf("installation %s GitHubHost = %q; want the base URL it was listed from, %q",
				inst.InstallationID, inst.GitHubHost, srv.URL)
		}
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

// TestGitHubAppsStore_SQLite_ReturnedRowConformance covers the returned-row
// standard end to end: the app row (CreateForOrg, SetActive) and the
// installation row (UpsertInstallation, SetInstallationSuspension,
// MarkInstallationRemoved) both hand back what they persisted.
func TestGitHubAppsStore_SQLite_ReturnedRowConformance(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	seedSQLiteOrgForApps(t, conn, orgID)

	dbtest.RunGitHubAppReturnedRowConformance(t, func(t *testing.T) (db.GitHubAppsStore, string, string) {
		t.Helper()
		return stores.GitHubApps, orgID, ""
	})

	dbtest.RunGitHubInstallationReturnedRowConformance(t, func(t *testing.T) (db.GitHubAppsStore, string, func(string) (*domain.OrgGitHubAppInstallation, error)) {
		t.Helper()
		return stores.GitHubApps, orgID, func(installationID string) (*domain.OrgGitHubAppInstallation, error) {
			return readSQLiteInstallationRaw(ctx, conn, orgID, installationID)
		}
	})
}

// readSQLiteInstallationRaw reads one org_github_app_installations row
// regardless of removed_at — the raw probe RunGitHubInstallationReturnedRowConformance
// needs for MarkInstallationRemoved, whose row is invisible to every store
// read once removed (see GitHubInstallationReturnedRowFactory's doc).
func readSQLiteInstallationRaw(ctx context.Context, conn *sql.DB, orgID, installationID string) (*domain.OrgGitHubAppInstallation, error) {
	var (
		inst        domain.OrgGitHubAppInstallation
		accountID   sql.NullString
		suspendedAt sql.NullTime
		suspendedBy sql.NullString
		selection   sql.NullString
	)
	err := conn.QueryRowContext(ctx, `
		SELECT installation_id, org_id, account_type, account_id, account_login,
		       github_host, installed_at, suspended_at, suspended_by, repository_selection
		  FROM org_github_app_installations
		 WHERE org_id = ? AND installation_id = ?
	`, orgID, installationID).Scan(
		&inst.InstallationID, &inst.OrgID, &inst.AccountType,
		&accountID, &inst.AccountLogin, &inst.GitHubHost, &inst.InstalledAt,
		&suspendedAt, &suspendedBy, &selection,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inst.AccountID = accountID.String
	inst.SuspendedAt = suspendedAt.Time
	inst.SuspendedBy = suspendedBy.String
	inst.RepositorySelection = selection.String
	return &inst, nil
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
