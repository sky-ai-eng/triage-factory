package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestGitHubAppsStore_Postgres_RoundTrip exercises Create + Get through
// the app pool with matching claims. RLS gates writes via
// tf.user_is_org_admin(org_id).
func TestGitHubAppsStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)

		got, err := stores.GitHubApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg (empty): %w", err)
		}
		if got != nil {
			t.Error("GetForOrg on empty table returned non-nil")
		}

		app := domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              "42",
			Slug:               "tf-test",
			ClientID:           "Iv1.abc123",
			ClientSecretRef:    "github_app_42_client_secret",
			PEMRef:             "github_app_42_pem",
			WebhookSecretRef:   "github_app_42_webhook_secret",
			OwnerType:          "org",
			RegisteredByUserID: userID,
			Active:             true,
		}
		if _, err := stores.GitHubApps.CreateForOrg(ctx, app); err != nil {
			return fmt.Errorf("CreateForOrg: %w", err)
		}

		got, err = stores.GitHubApps.GetForOrg(ctx, orgID)
		if err != nil {
			return fmt.Errorf("GetForOrg: %w", err)
		}
		if got == nil {
			t.Fatal("GetForOrg returned nil after Create")
		}
		if got.AppID != "42" {
			t.Errorf("AppID = %q, want 42", got.AppID)
		}
		if got.Slug != "tf-test" {
			t.Errorf("Slug = %q, want tf-test", got.Slug)
		}
		if got.ClientID != "Iv1.abc123" {
			t.Errorf("ClientID = %q, want Iv1.abc123", got.ClientID)
		}
		if got.OwnerType != "org" {
			t.Errorf("OwnerType = %q, want org", got.OwnerType)
		}
		if !got.Active {
			t.Error("Active = false, want true (CreateForOrg writes the flag verbatim)")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_BotUserID pins the bot_user_id column round-trip
// (TFAC-474) on the admin/system path: a set id survives Create→GetForOrgSystem,
// and an unset id (0) is written as NULL and scans back as 0. Wires both pools
// to AdminDB (BYPASSRLS) so the test exercises the SQL independent of the auth
// path, like the installation-lifecycle test.
func TestGitHubAppsStore_Postgres_BotUserID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgWithID, _ := seedPgOrgAndUserForGitHubApps(t, h)
	orgWithout, _ := seedPgOrgAndUserForGitHubApps(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: orgWithID, AppID: "7001", Slug: "acme-bot", ClientID: "Iv1.x",
		ClientSecretRef: "cs", PEMRef: "pem", WebhookSecretRef: "wh",
		BotUserID: 41898282,
	}); err != nil {
		t.Fatalf("CreateForOrg (with id): %v", err)
	}
	got, err := stores.GitHubApps.GetForOrgSystem(ctx, orgWithID)
	if err != nil || got == nil {
		t.Fatalf("GetForOrgSystem (with id) = %+v, %v", got, err)
	}
	if got.BotUserID != 41898282 {
		t.Errorf("BotUserID = %d, want 41898282", got.BotUserID)
	}

	// An unset id stores NULL (CreateForOrg maps 0 → NULL) and scans back as 0.
	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: orgWithout, AppID: "7002", Slug: "no-bot", ClientID: "Iv1.y",
		ClientSecretRef: "cs", PEMRef: "pem", WebhookSecretRef: "wh",
	}); err != nil {
		t.Fatalf("CreateForOrg (without id): %v", err)
	}
	got, err = stores.GitHubApps.GetForOrgSystem(ctx, orgWithout)
	if err != nil || got == nil {
		t.Fatalf("GetForOrgSystem (without id) = %+v, %v", got, err)
	}
	if got.BotUserID != 0 {
		t.Errorf("BotUserID = %d, want 0 (NULL scans as 0)", got.BotUserID)
	}
}

// TestGitHubAppsStore_Postgres_ConflictOnDuplicate verifies that a
// second CreateForOrg for the same org returns ErrGitHubAppExists.
func TestGitHubAppsStore_Postgres_ConflictOnDuplicate(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)

		app := domain.OrgGitHubApp{
			OrgID:            orgID,
			AppID:            "100",
			Slug:             "first",
			ClientID:         "Iv1.first",
			ClientSecretRef:  "ref_cs",
			PEMRef:           "ref_pem",
			WebhookSecretRef: "ref_wh",
		}
		if _, err := stores.GitHubApps.CreateForOrg(ctx, app); err != nil {
			return fmt.Errorf("first Create: %w", err)
		}

		// Wrap the duplicate insert in a savepoint so the constraint
		// violation doesn't abort the outer tx (Postgres puts the tx
		// in an error state on any unhandled error).
		if _, err := tx.ExecContext(ctx, "SAVEPOINT dup"); err != nil {
			return fmt.Errorf("savepoint: %w", err)
		}
		app.AppID = "200"
		app.Slug = "second"
		_, err := stores.GitHubApps.CreateForOrg(ctx, app)
		if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT dup"); rerr != nil {
			return fmt.Errorf("rollback savepoint: %w", rerr)
		}
		if err == nil {
			t.Error("second CreateForOrg should have failed with ErrGitHubAppExists")
			return nil
		}
		var exists *db.ErrGitHubAppExists
		if !errors.As(err, &exists) {
			t.Errorf("second CreateForOrg error type = %T, want *db.ErrGitHubAppExists", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_CrossOrgRLSDenied proves that a user
// from org B cannot read or write org A's org_github_apps row.
func TestGitHubAppsStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA := seedPgOrgAndUserForGitHubApps(t, h)
	orgB, userB := seedPgOrgAndUserForGitHubApps(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed a row in orgA via the admin pool.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO org_github_apps (org_id, app_id, slug, client_id, client_secret_ref, pem_ref, webhook_secret_ref)
		VALUES ($1, '99', 'tf-a', 'Iv1.a', 'ref_cs', 'ref_pem', 'ref_wh')
	`, orgA); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}

	// User A can read their own org's app.
	if err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		got, err := stores.GitHubApps.GetForOrg(ctx, orgA)
		if err != nil {
			return fmt.Errorf("userA GetForOrg(orgA): %w", err)
		}
		if got == nil {
			t.Error("userA GetForOrg(orgA) = nil, want non-nil")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser userA/orgA: %v", err)
	}

	// User B with claims for orgB cannot see orgA's row.
	if err := h.WithUser(t, userB, orgB, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		got, err := stores.GitHubApps.GetForOrg(ctx, orgA)
		if err != nil {
			return fmt.Errorf("userB GetForOrg(orgA): %w", err)
		}
		if got != nil {
			t.Error("userB GetForOrg(orgA) should be nil (cross-org), got non-nil")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser userB/orgB: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_NonAdminWriteDenied proves that a
// non-admin member (role='member') cannot INSERT into org_github_apps.
func TestGitHubAppsStore_Postgres_NonAdminWriteDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, _ := seedPgOrgAndUserForGitHubApps(t, h)
	memberID := seedPgMember(t, h, orgID, "non-admin", "member")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)
		_, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
			OrgID:            orgID,
			AppID:            "77",
			Slug:             "member-attempt",
			ClientID:         "Iv1.m",
			ClientSecretRef:  "r1",
			PEMRef:           "r2",
			WebhookSecretRef: "r3",
		})
		return err
	})
	if err == nil {
		t.Fatal("non-admin CreateForOrg should have been rejected by RLS")
	}
	pgtest.AssertRLSViolation(t, err)
}

// TestGitHubAppsStore_Postgres_InstallationLifecycle exercises the
// admin-pool installation writes (tf_app is denied all writes to
// org_github_app_installations): Upsert → MarkInstallationRemoved → Upsert-revive.
// Wires both pools to AdminDB (BYPASSRLS) so the test exercises the SQL
// independent of the auth path.
func TestGitHubAppsStore_Postgres_InstallationLifecycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgOrgAndUserForGitHubApps(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	if len(got) != 1 || got[0].InstallationID != "555" {
		t.Fatalf("after upsert got %+v, want one /555 row", got)
	}
	if got[0].InstalledAt.IsZero() {
		t.Error("InstalledAt is zero; want defaulted now()")
	}

	if _, err := stores.GitHubApps.MarkInstallationRemoved(ctx, orgID, "555"); err != nil {
		t.Fatalf("MarkInstallationRemoved: %v", err)
	}
	if got, _ = stores.GitHubApps.ListInstallationsForOrg(ctx, orgID); len(got) != 0 {
		t.Fatalf("after remove got %d rows, want 0", len(got))
	}

	inst.AccountLogin = "acme-renamed"
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, inst); err != nil {
		t.Fatalf("UpsertInstallation (revive): %v", err)
	}
	got, _ = stores.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if len(got) != 1 || got[0].AccountLogin != "acme-renamed" {
		t.Fatalf("after revive got %+v, want one acme-renamed row", got)
	}
}

// TestGitHubAppsStore_Postgres_InstallationCrossOrg proves an upsert
// under org A never surfaces under org B — the org_id binding on the
// row + the read's org_id filter keep tenants isolated.
func TestGitHubAppsStore_Postgres_InstallationCrossOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _ := seedPgOrgAndUserForGitHubApps(t, h)
	orgB, _ := seedPgOrgAndUserForGitHubApps(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Same numeric installation_id under both orgs, on two GitHub deployments —
	// the case that is legal, because GitHub numbers installations per
	// deployment and not universally. (One host is not: the uniqueness index
	// refuses it, and the shared host suite pins that.) The composite
	// (org_id, installation_id) PK must keep these as two independent rows, not
	// let one overwrite the other.
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID: "900", OrgID: orgA, AccountType: "User", AccountLogin: "only-in-a",
	}); err != nil {
		t.Fatalf("UpsertInstallation orgA: %v", err)
	}
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID: "900", OrgID: orgB, AccountType: "Organization", AccountLogin: "only-in-b",
		GitHubHost: "https://git.example.com",
	}); err != nil {
		t.Fatalf("UpsertInstallation orgB (same id, other deployment): %v", err)
	}

	gotA, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgA)
	if len(gotA) != 1 || gotA[0].AccountLogin != "only-in-a" {
		t.Errorf("orgA = %+v, want one only-in-a row (orgB's same-id upsert leaked)", gotA)
	}
	gotB, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgB)
	if len(gotB) != 1 || gotB[0].AccountLogin != "only-in-b" {
		t.Errorf("orgB = %+v, want one only-in-b row", gotB)
	}

	// A delete for orgB's id=900 must not touch orgA's id=900.
	if _, err := stores.GitHubApps.MarkInstallationRemoved(ctx, orgB, "900"); err != nil {
		t.Fatalf("MarkInstallationRemoved orgB: %v", err)
	}
	if got, _ := stores.GitHubApps.ListInstallationsForOrg(ctx, orgA); len(got) != 1 {
		t.Errorf("orgA lost its row to orgB's delete: %+v", got)
	}
}

// TestGitHubAppsStore_Postgres_SetActiveAndDelete exercises the staged/live
// flip (SetActive) and the full teardown (DeleteForOrg: row + installations) —
// the either/or switch mechanics (TFAC-328). Both pools wire to AdminDB so the
// SQL is exercised independent of the auth path.
func TestGitHubAppsStore_Postgres_SetActiveAndDelete(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := stores.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: orgID, AppID: "1", Slug: "staged", ClientID: "Iv1.x",
		ClientSecretRef: "cs", PEMRef: "pem", WebhookSecretRef: "wh",
		RegisteredByUserID: userID, Active: false, // staged
	}); err != nil {
		t.Fatalf("CreateForOrg (staged): %v", err)
	}
	if got, _ := stores.GitHubApps.GetForOrg(ctx, orgID); got == nil || got.Active {
		t.Fatalf("after staged create got %+v, want Active=false", got)
	}

	if _, err := stores.GitHubApps.SetActive(ctx, orgID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if got, _ := stores.GitHubApps.GetForOrg(ctx, orgID); got == nil || !got.Active {
		t.Fatalf("after SetActive(true) got %+v, want Active=true", got)
	}

	// Seed installations, then tear the whole registration down.
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
		t.Errorf("installations survived DeleteForOrg = %+v, want none", insts)
	}

	// Both ops are no-ops (no error) on an org with no registration.
	if _, err := stores.GitHubApps.SetActive(ctx, orgID, true); err != nil {
		t.Errorf("SetActive on absent row = %v, want nil", err)
	}
	if err := stores.GitHubApps.DeleteForOrg(ctx, orgID); err != nil {
		t.Errorf("DeleteForOrg on absent row = %v, want nil", err)
	}
}

// TestGitHubAppsStore_Postgres_AppReturnedRowConformance runs the shared
// app-row returned-row suite (CreateForOrg, SetActive) against the app pool
// under the registrant's claims — the RLS-relevant half, same reasoning as
// TestJiraAppsStore_Postgres_ReturnedRowConformance.
func TestGitHubAppsStore_Postgres_AppReturnedRowConformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForGitHubApps(t, h)

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		store := pgstore.NewForTx(tx, pgtest.SecretKey).GitHubApps
		dbtest.RunGitHubAppReturnedRowConformance(t, func(t *testing.T) (db.GitHubAppsStore, string, string) {
			t.Helper()
			return store, orgID, userID
		})
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestGitHubAppsStore_Postgres_InstallationReturnedRowConformance runs the
// shared installation-row returned-row suite (UpsertInstallation,
// SetInstallationSuspension, MarkInstallationRemoved) on the admin pool — the
// only pool these writes ever use, since tf_app is denied all writes to
// org_github_app_installations by RLS (see gitHubAppsStore's doc).
func TestGitHubAppsStore_Postgres_InstallationReturnedRowConformance(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgOrgAndUserForGitHubApps(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	readRaw := func(installationID string) (*domain.OrgGitHubAppInstallation, error) {
		var (
			inst        domain.OrgGitHubAppInstallation
			accountID   sql.NullString
			suspendedAt sql.NullTime
			suspendedBy sql.NullString
			selection   sql.NullString
		)
		err := h.AdminDB.QueryRowContext(ctx, `
			SELECT installation_id, org_id, account_type, account_id, account_login,
			       github_host, installed_at, suspended_at, suspended_by, repository_selection
			  FROM org_github_app_installations
			 WHERE org_id = $1 AND installation_id = $2
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

	dbtest.RunGitHubInstallationReturnedRowConformance(t, func(t *testing.T) (db.GitHubAppsStore, string, func(string) (*domain.OrgGitHubAppInstallation, error)) {
		t.Helper()
		return stores.GitHubApps, orgID, readRaw
	})
}

func seedPgOrgAndUserForGitHubApps(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("ghapp-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "GH App Test User"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "GH App Org "+orgID[:8], "ghapp-"+orgID[:8], userID); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}
