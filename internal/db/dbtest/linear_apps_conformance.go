package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// LinearAppsStoreFactory hands the conformance suite a wired store, the orgID
// to scope every call, and the user id to attribute a registration to (empty
// where the dialect has no users table to satisfy).
type LinearAppsStoreFactory func(t *testing.T) (store db.LinearAppsStore, orgID, userID string)

// RunLinearAppsReturnedRowConformance covers the returned-row standard on
// UpsertForOrg, the RunJiraAppsReturnedRowConformance shape: the write hands
// back the row it persisted, including the registered_at its conflict arm
// preserves.
func RunLinearAppsReturnedRowConformance(t *testing.T, mk LinearAppsStoreFactory) {
	t.Helper()

	t.Run("UpsertForOrg_returns_the_stored_row", func(t *testing.T) {
		store, orgID, userID := mk(t)
		ctx := context.Background()
		read := func() (*domain.OrgLinearApp, error) { return store.GetForOrg(ctx, orgID) }

		first, err := store.UpsertForOrg(ctx, domain.OrgLinearApp{
			OrgID: orgID, ClientID: "lin-ret-1",
			ClientSecretRef: "linear_oauth_client_secret", RegisteredByUserID: userID,
		})
		if err != nil {
			t.Fatalf("UpsertForOrg (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertForOrg (insert)", first, read)
		if first.RegisteredAt.IsZero() {
			t.Error("UpsertForOrg returned a row with no registered_at, the column the insert defaults")
		}

		replaced, err := store.UpsertForOrg(ctx, domain.OrgLinearApp{
			OrgID: orgID, ClientID: "lin-ret-2",
			ClientSecretRef: "linear_oauth_client_secret", RegisteredByUserID: userID,
		})
		if err != nil {
			t.Fatalf("UpsertForOrg (replace): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertForOrg (replace)", replaced, read)
		if replaced.ClientID != "lin-ret-2" || !replaced.RegisteredAt.Equal(first.RegisteredAt) {
			t.Errorf("replace returned %+v, want the new client id with the original registered_at", replaced)
		}

		if err := store.DeleteForOrg(ctx, orgID); err != nil {
			t.Fatalf("DeleteForOrg: %v", err)
		}
		if got, err := read(); err != nil || got != nil {
			t.Fatalf("GetForOrg after delete = (%+v, %v), want (nil, nil)", got, err)
		}
		if err := store.DeleteForOrg(ctx, orgID); err != nil {
			t.Fatalf("DeleteForOrg with no row: %v", err)
		}
	})
}

// LinearInstallsStoreFactory hands the suite a wired store and two orgs that
// share nothing, so one workspace can be offered to both. userID is the admin
// to attribute an install to.
type LinearInstallsStoreFactory func(t *testing.T) (store db.LinearInstallsStore, orgA, orgB, userID string)

// RunLinearInstallsConformance covers org_linear_installs on both backends:
// the returned-row standard on UpsertSystem and MarkRemovedSystem, one live
// install of a workspace across orgs, and a removed install releasing its
// workspace for a re-install anywhere.
func RunLinearInstallsConformance(t *testing.T, mk LinearInstallsStoreFactory) {
	t.Helper()

	install := func(orgID, workspace, userID string) domain.OrgLinearInstall {
		return domain.OrgLinearInstall{
			OrgID:             orgID,
			WorkspaceID:       workspace,
			WorkspaceURLKey:   workspace + "-key",
			AppUserID:         "app-user-" + workspace,
			AppClientID:       "client-1",
			InstalledByUserID: userID,
			InstalledAt:       time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		}
	}

	t.Run("upsert_returns_the_stored_row_and_replaces_in_place", func(t *testing.T) {
		store, orgA, _, userID := mk(t)
		ctx := context.Background()
		read := func() (*domain.OrgLinearInstall, error) { return store.GetForOrgSystem(ctx, orgA) }

		if got, err := read(); err != nil || got != nil {
			t.Fatalf("GetForOrgSystem before any install = (%+v, %v), want (nil, nil)", got, err)
		}
		first, err := store.UpsertSystem(ctx, install(orgA, "ws-1", userID))
		if err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem (insert)", first, read)
		if !first.Live() || first.RemovedReason != "" {
			t.Errorf("a fresh install reads %+v, want it live with no reason", first)
		}

		// The same org installing again replaces its row: one row per org,
		// whichever workspace.
		again, err := store.UpsertSystem(ctx, install(orgA, "ws-2", userID))
		if err != nil {
			t.Fatalf("UpsertSystem (replace): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpsertSystem (replace)", again, read)
		if again.WorkspaceID != "ws-2" || again.AppUserID != "app-user-ws-2" {
			t.Errorf("replace returned %+v, want the new workspace and app user", again)
		}
	})

	t.Run("a_live_workspace_is_held_against_other_orgs", func(t *testing.T) {
		store, orgA, orgB, userID := mk(t)
		ctx := context.Background()

		if _, err := store.UpsertSystem(ctx, install(orgA, "ws-shared", userID)); err != nil {
			t.Fatalf("UpsertSystem org A: %v", err)
		}
		_, err := store.UpsertSystem(ctx, install(orgB, "ws-shared", userID))
		if !errors.Is(err, db.ErrWorkspaceInstalledElsewhere) {
			t.Fatalf("org B installing org A's live workspace = %v, want ErrWorkspaceInstalledElsewhere", err)
		}
		if got, err := store.GetForOrgSystem(ctx, orgB); err != nil || got != nil {
			t.Fatalf("org B after the refused install = (%+v, %v), want no row", got, err)
		}
		// The refusal holds when org B already has a row of its own, too: the
		// conflict arm's update is what collides then.
		if _, err := store.UpsertSystem(ctx, install(orgB, "ws-b", userID)); err != nil {
			t.Fatalf("UpsertSystem org B own workspace: %v", err)
		}
		if _, err := store.UpsertSystem(ctx, install(orgB, "ws-shared", userID)); !errors.Is(err, db.ErrWorkspaceInstalledElsewhere) {
			t.Fatalf("org B moving onto org A's live workspace = %v, want ErrWorkspaceInstalledElsewhere", err)
		}
		if got, err := store.GetForOrgSystem(ctx, orgB); err != nil || got == nil || got.WorkspaceID != "ws-b" {
			t.Fatalf("org B after the refused move = (%+v, %v), want its own workspace untouched", got, err)
		}
	})

	t.Run("a_removed_install_releases_its_workspace", func(t *testing.T) {
		store, orgA, orgB, userID := mk(t)
		ctx := context.Background()

		if _, err := store.UpsertSystem(ctx, install(orgA, "ws-free", userID)); err != nil {
			t.Fatalf("UpsertSystem org A: %v", err)
		}
		removed, err := store.MarkRemovedSystem(ctx, orgA, domain.LinearInstallRemovedDisconnected)
		if err != nil {
			t.Fatalf("MarkRemovedSystem: %v", err)
		}
		if removed == nil {
			t.Fatal("MarkRemovedSystem on a live install returned no row")
		}
		AssertWriteReturnedStoredRow(t, "MarkRemovedSystem", *removed, func() (*domain.OrgLinearInstall, error) {
			return store.GetForOrgSystem(ctx, orgA)
		})
		if removed.Live() || removed.RemovedReason != domain.LinearInstallRemovedDisconnected {
			t.Errorf("removed row = %+v, want removed with reason %q", removed, domain.LinearInstallRemovedDisconnected)
		}
		if again, err := store.MarkRemovedSystem(ctx, orgA, domain.LinearInstallRemovedRevoked); err != nil || again != nil {
			t.Fatalf("MarkRemovedSystem on a removed install = (%+v, %v), want (nil, nil)", again, err)
		}
		if got, err := store.GetForOrgSystem(ctx, orgA); err != nil || got == nil || got.RemovedReason != domain.LinearInstallRemovedDisconnected {
			t.Fatalf("the second removal rewrote the first: (%+v, %v)", got, err)
		}

		// Org B may now install the workspace org A let go of.
		taken, err := store.UpsertSystem(ctx, install(orgB, "ws-free", userID))
		if err != nil {
			t.Fatalf("org B installing a released workspace: %v", err)
		}
		if !taken.Live() {
			t.Errorf("org B's install = %+v, want live", taken)
		}
		// And org A re-installing now collides with org B's live row.
		if _, err := store.UpsertSystem(ctx, install(orgA, "ws-free", userID)); !errors.Is(err, db.ErrWorkspaceInstalledElsewhere) {
			t.Fatalf("org A re-installing org B's live workspace = %v, want ErrWorkspaceInstalledElsewhere", err)
		}
	})

	t.Run("a_reinstall_revives_a_removed_row", func(t *testing.T) {
		store, orgA, _, userID := mk(t)
		ctx := context.Background()

		if _, err := store.UpsertSystem(ctx, install(orgA, "ws-back", userID)); err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		if _, err := store.MarkRemovedSystem(ctx, orgA, domain.LinearInstallRemovedRevoked); err != nil {
			t.Fatalf("MarkRemovedSystem: %v", err)
		}
		back, err := store.UpsertSystem(ctx, install(orgA, "ws-back", userID))
		if err != nil {
			t.Fatalf("UpsertSystem (re-install): %v", err)
		}
		if !back.Live() || back.RemovedReason != "" {
			t.Errorf("re-install = %+v, want live with the removal cleared", back)
		}
	})
}
