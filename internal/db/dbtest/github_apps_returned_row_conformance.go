package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubAppReturnedRowFactory hands the app-row conformance suite a wired
// store and the (orgID, userID) every write in the suite addresses. userID may
// be "" where the dialect has no users table to satisfy, mirroring
// JiraAppsStoreFactory.
type GitHubAppReturnedRowFactory func(t *testing.T) (store db.GitHubAppsStore, orgID, userID string)

// RunGitHubAppReturnedRowConformance covers the returned-row standard on
// org_github_apps: CreateForOrg and SetActive. Unlike JiraAppsStore's
// UpsertForOrg, CreateForOrg is create-once-then-conflict rather than an
// upsert, so this suite runs both writes against the ONE row CreateForOrg
// mints, in subtest declaration order (Go runs t.Run subtests sequentially) —
// SetActive has nothing to flip until CreateForOrg has run.
func RunGitHubAppReturnedRowConformance(t *testing.T, mk GitHubAppReturnedRowFactory) {
	t.Helper()
	ctx := context.Background()
	store, orgID, userID := mk(t)
	read := func() (*domain.OrgGitHubApp, error) { return store.GetForOrg(ctx, orgID) }

	t.Run("CreateForOrg_returns_the_stored_row", func(t *testing.T) {
		created, err := store.CreateForOrg(ctx, domain.OrgGitHubApp{
			OrgID: orgID, AppID: "ret-1", Slug: "ret-app", ClientID: "Iv1.ret",
			ClientSecretRef: "cs_ref", PEMRef: "pem_ref", WebhookSecretRef: "wh_ref",
			OwnerType: "org", RegisteredByUserID: userID, Active: false,
		})
		if err != nil {
			t.Fatalf("CreateForOrg: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "CreateForOrg", created, read)
		if created.Active {
			t.Error("CreateForOrg returned Active=true for a staged (Active:false) create")
		}
	})

	t.Run("SetActive_returns_the_stored_row", func(t *testing.T) {
		activated, err := store.SetActive(ctx, orgID, true)
		if err != nil {
			t.Fatalf("SetActive: %v", err)
		}
		if activated == nil {
			t.Fatal("SetActive returned nil for an org with a registration")
		}
		AssertWriteReturnedStoredRow(t, "SetActive", *activated, read)
		if !activated.Active {
			t.Error("SetActive(true) returned a row with Active=false")
		}

		// The documented no-op: an org with no registration row flips nothing
		// and errors on nothing — (nil, nil), not db.ErrNoSuchX, matching
		// GetForOrg's own nil-on-absent shape.
		absent, err := store.SetActive(ctx, "00000000-0000-0000-0000-0000000000ff", true)
		if err != nil {
			t.Errorf("SetActive on an absent registration = %v, want nil error", err)
		}
		if absent != nil {
			t.Errorf("SetActive on an absent registration = %+v, want nil", absent)
		}
	})
}

// GitHubInstallationReturnedRowFactory hands the installation-row conformance
// suite a wired store, the orgID every write addresses, and a raw reader for
// one org_github_app_installations row regardless of removed_at.
//
// readInstallationRaw exists because MarkInstallationRemoved's row is, by
// design, invisible to every store read once removed — ListInstallationsForOrg
// is live-rows-only (see GitHubSuspensionSeeder.Row for the same shape, used
// for the same reason). AssertWriteReturnedStoredRow needs a point read to
// compare against for every converted method, so this is that read for the
// one case the public interface has no room for.
type GitHubInstallationReturnedRowFactory func(t *testing.T) (store db.GitHubAppsStore, orgID string, readInstallationRaw func(installationID string) (*domain.OrgGitHubAppInstallation, error))

// RunGitHubInstallationReturnedRowConformance covers the returned-row
// standard on org_github_app_installations: UpsertInstallation,
// SetInstallationSuspension, and MarkInstallationRemoved. Each subtest seeds
// its own installation id so the three don't interfere with each other.
func RunGitHubInstallationReturnedRowConformance(t *testing.T, mk GitHubInstallationReturnedRowFactory) {
	t.Helper()
	ctx := context.Background()
	store, orgID, readRaw := mk(t)

	t.Run("UpsertInstallation_returns_the_stored_row", func(t *testing.T) {
		const instID = "ret-inst-1"
		read := func() (*domain.OrgGitHubAppInstallation, error) { return readRaw(instID) }

		first, err := store.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: instID, OrgID: orgID, AccountType: "Organization", AccountLogin: "acme-ret",
		})
		if err != nil {
			t.Fatalf("UpsertInstallation (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertInstallation (insert)", first, read)
		if first.InstalledAt.IsZero() {
			t.Error("UpsertInstallation returned InstalledAt zero, want the column default")
		}

		renamed, err := store.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: instID, OrgID: orgID, AccountType: "Organization", AccountLogin: "acme-ret-renamed",
		})
		if err != nil {
			t.Fatalf("UpsertInstallation (conflict): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertInstallation (conflict)", renamed, read)
		if renamed.AccountLogin != "acme-ret-renamed" || !renamed.InstalledAt.Equal(first.InstalledAt) {
			t.Errorf("conflict returned %+v, want the new login with the original installed_at", renamed)
		}
	})

	t.Run("SetInstallationSuspension_returns_the_stored_row", func(t *testing.T) {
		const instID = "ret-inst-2"
		if _, err := store.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: instID, OrgID: orgID, AccountType: "Organization", AccountLogin: "beta-ret",
		}); err != nil {
			t.Fatalf("seed UpsertInstallation: %v", err)
		}
		read := func() (*domain.OrgGitHubAppInstallation, error) { return readRaw(instID) }

		suspendedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
		suspended, err := store.SetInstallationSuspension(ctx, orgID, instID, suspendedAt, "octocat")
		if err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}
		if suspended == nil {
			t.Fatal("SetInstallationSuspension returned nil for a live installation")
		}
		AssertWriteReturnedStoredRow(t, "SetInstallationSuspension", *suspended, read)
		if !suspended.Suspended() {
			t.Error("SetInstallationSuspension did not return a suspended row")
		}

		// The documented no-op: an installation the mirror has never seen.
		absent, err := store.SetInstallationSuspension(ctx, orgID, "ret-inst-never-seen", suspendedAt, "octocat")
		if err != nil {
			t.Errorf("SetInstallationSuspension on an unmirrored installation = %v, want nil error", err)
		}
		if absent != nil {
			t.Errorf("SetInstallationSuspension on an unmirrored installation = %+v, want nil", absent)
		}
	})

	t.Run("MarkInstallationRemoved_returns_the_row_it_removed", func(t *testing.T) {
		const instID = "ret-inst-3"
		if _, err := store.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID: instID, OrgID: orgID, AccountType: "Organization", AccountLogin: "gamma-ret",
		}); err != nil {
			t.Fatalf("seed UpsertInstallation: %v", err)
		}
		read := func() (*domain.OrgGitHubAppInstallation, error) { return readRaw(instID) }

		removed, err := store.MarkInstallationRemoved(ctx, orgID, instID)
		if err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		if removed == nil {
			t.Fatal("MarkInstallationRemoved returned nil for a live installation")
		}
		// The raw probe, not ListInstallationsForOrg, because a removed row is
		// gone from every active-only read by design — see readInstallationRaw's
		// doc on GitHubInstallationReturnedRowFactory.
		AssertWriteReturnedStoredRow(t, "MarkInstallationRemoved", *removed, read)
		if removed.AccountLogin != "gamma-ret" {
			t.Errorf("MarkInstallationRemoved returned %+v, want the row it just removed", removed)
		}

		// The documented no-op: already removed.
		again, err := store.MarkInstallationRemoved(ctx, orgID, instID)
		if err != nil {
			t.Errorf("MarkInstallationRemoved on an already-removed row = %v, want nil error", err)
		}
		if again != nil {
			t.Errorf("MarkInstallationRemoved on an already-removed row = %+v, want nil", again)
		}
	})
}
