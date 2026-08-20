package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubSuspensionSeeder stages the rows the suspension suite needs and reads
// back the one state no store method surfaces. User and org creation have no
// store methods (row creation is an auth/provisioning concern), so each backend
// implements them against its own schema.
type GitHubSuspensionSeeder struct {
	// User inserts a user row and returns its ID.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its ID. ownerID must
	// already exist (see User); backends whose orgs table has no owner column
	// may ignore it.
	Org func(t *testing.T, ownerID string) string

	// Row reads one installation's lifecycle columns straight from the table,
	// removed or not. It is a raw-SQL probe rather than a store method because
	// every store read is deliberately live-rows-only: the suspended → removed
	// arrow asserts what a REMOVED row retained, which no reader in the product
	// wants to see, and adding one nobody calls would be worse than a
	// test-local probe.
	Row func(t *testing.T, orgID, installationID string) GitHubInstallationRow
}

// GitHubInstallationRow is the raw lifecycle state of one mirrored
// installation. The timestamps are reported as "is it set", not as values:
// the dialects store and return them differently, and every assertion here is
// about presence.
type GitHubInstallationRow struct {
	Exists      bool
	Removed     bool
	Suspended   bool
	SuspendedBy string
}

// GitHubSuspensionFactory is what a per-backend test file hands to
// RunGitHubInstallationSuspensionConformance. Each call returns a fresh,
// isolated backend so subtests don't leak rows into one another.
type GitHubSuspensionFactory func(t *testing.T) (db.GitHubAppsStore, GitHubSuspensionSeeder)

// RunGitHubInstallationSuspensionConformance is the shared assertion suite for
// installation suspension — the state in which an App's grant survives but
// every token minted from it is refused. It walks every arrow of the
// lifecycle, in both dialects:
//
//   - active → suspended, by the webhook's targeted write
//     (SetInstallationSuspension) and by the reconcile's whole-row write
//     (UpsertInstallation), since a deployment that missed a delivery converges
//     only through the second;
//   - suspended → active, leaving no residue in either column;
//   - suspended → removed, which retains the suspension columns rather than
//     tidying them away — the row records what was true of the installation
//     when it went;
//   - removed → active, where a re-install clears removed_at AND the
//     suspension in one statement, because the installation a user just
//     re-installed is not the suspended one they removed.
//
// Plus the invariant every reader depends on: a suspended installation is
// distinguishable from a live one in ListInstallationsForOrgSystem output, and
// a suspension does not soft-remove the row or disturb the org's other
// installations.
func RunGitHubInstallationSuspensionConformance(t *testing.T, mk GitHubSuspensionFactory) {
	t.Helper()
	ctx := context.Background()

	// suspendedAt is a fixed, non-zero instant — the suite asserts presence and
	// clearing, never a particular value, so one timestamp serves throughout.
	suspendedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	install := func(orgID, installationID, login string) domain.OrgGitHubAppInstallation {
		return domain.OrgGitHubAppInstallation{
			InstallationID: installationID,
			OrgID:          orgID,
			AccountType:    "Organization",
			AccountID:      "1234",
			AccountLogin:   login,
		}
	}

	// live returns the org's single live installation, failing if it has any
	// other number.
	live := func(t *testing.T, store db.GitHubAppsStore, orgID string) domain.OrgGitHubAppInstallation {
		t.Helper()
		insts, err := store.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if len(insts) != 1 {
			t.Fatalf("ListInstallationsForOrgSystem = %d installations; want 1", len(insts))
		}
		return insts[0]
	}

	// seedActive stages one live, unsuspended installation and returns its org.
	seedActive := func(t *testing.T, store db.GitHubAppsStore, seed GitHubSuspensionSeeder) string {
		t.Helper()
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		return org
	}

	t.Run("ActiveToSuspendedByWebhook", func(t *testing.T) {
		store, seed := mk(t)
		org := seedActive(t, store, seed)

		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}

		// The invariant a suspended installation exists to serve: a reader of
		// the live mirror can tell it apart from a working one.
		got := live(t, store, org)
		if !got.Suspended() {
			t.Error("Suspended() = false after a suspend; want true")
		}
		if !got.SuspendedAt.Equal(suspendedAt) {
			t.Errorf("SuspendedAt = %s; want %s", got.SuspendedAt, suspendedAt)
		}
		if got.SuspendedBy != "octocat" {
			t.Errorf("SuspendedBy = %q; want %q", got.SuspendedBy, "octocat")
		}
		// Negative space: a suspension is not a soft removal. The row is still
		// live, which is exactly why the state needed columns of its own.
		if row := seed.Row(t, org, "456"); row.Removed {
			t.Error("a suspend marked the installation removed; want the row left live")
		}
	})

	t.Run("ActiveToSuspendedByReconcile", func(t *testing.T) {
		// The half that converges a deployment that missed a delivery — GitHub
		// does not re-deliver installation.suspend, and local mode receives no
		// deliveries at all, so the /app/installations reconcile is the only
		// path that ever corrects it there.
		store, seed := mk(t)
		org := seedActive(t, store, seed)

		suspended := install(org, "456", "acme")
		suspended.SuspendedAt, suspended.SuspendedBy = suspendedAt, "octocat"
		if _, err := store.UpsertInstallation(ctx, suspended); err != nil {
			t.Fatalf("UpsertInstallation (reconcile sees a suspension): %v", err)
		}

		got := live(t, store, org)
		if !got.Suspended() || got.SuspendedBy != "octocat" {
			t.Errorf("after a reconcile that reported a suspension: Suspended()=%v SuspendedBy=%q; want true, %q",
				got.Suspended(), got.SuspendedBy, "octocat")
		}
	})

	t.Run("SuspendedToActive", func(t *testing.T) {
		store, seed := mk(t)
		org := seedActive(t, store, seed)
		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}

		if _, err := store.SetInstallationSuspension(ctx, org, "456", time.Time{}, ""); err != nil {
			t.Fatalf("SetInstallationSuspension (unsuspend): %v", err)
		}

		// Exactly the prior state, no residue: the suspender's login must go
		// with the timestamp, or the row reads as suspended-by-someone forever.
		got := live(t, store, org)
		if got.Suspended() {
			t.Errorf("Suspended() = true after an unsuspend (SuspendedAt=%s); want false", got.SuspendedAt)
		}
		if got.SuspendedBy != "" {
			t.Errorf("SuspendedBy = %q after an unsuspend; want \"\"", got.SuspendedBy)
		}
	})

	t.Run("SuspendedToActiveByReconcile", func(t *testing.T) {
		// Same arrow, discovered by pull: GitHub reports the installation with
		// no suspension, and the reconcile's whole-row write is an assertion
		// that there is none.
		store, seed := mk(t)
		org := seedActive(t, store, seed)
		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}

		if _, err := store.UpsertInstallation(ctx, install(org, "456", "acme")); err != nil {
			t.Fatalf("UpsertInstallation (reconcile sees no suspension): %v", err)
		}
		if got := live(t, store, org); got.Suspended() {
			t.Errorf("Suspended() = true after a reconcile reporting none (SuspendedAt=%s); want false", got.SuspendedAt)
		}
	})

	t.Run("SuspendedToRemovedRetainsSuspension", func(t *testing.T) {
		store, seed := mk(t)
		org := seedActive(t, store, seed)
		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}

		if _, err := store.MarkInstallationRemoved(ctx, org, "456"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}

		insts, err := store.ListInstallationsForOrgSystem(ctx, org)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if len(insts) != 0 {
			t.Fatalf("ListInstallationsForOrgSystem = %d installations after the removal; want 0", len(insts))
		}
		// The removal stamps removed_at and touches nothing else: the row keeps
		// recording what was true of the installation when it went.
		row := seed.Row(t, org, "456")
		if !row.Removed {
			t.Error("removed_at not set after MarkInstallationRemoved")
		}
		if !row.Suspended || row.SuspendedBy != "octocat" {
			t.Errorf("removed row: Suspended=%v SuspendedBy=%q; want the suspension retained (true, %q)",
				row.Suspended, row.SuspendedBy, "octocat")
		}
	})

	t.Run("RemovedToActiveClearsSuspension", func(t *testing.T) {
		// The arrow to get right: a re-install must not inherit the previous
		// installation's suspension. removed_at and both suspension columns
		// clear in the one statement, so there is no window in which the
		// revived row reads as suspended.
		store, seed := mk(t)
		org := seedActive(t, store, seed)
		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}
		if _, err := store.MarkInstallationRemoved(ctx, org, "456"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}

		if _, err := store.UpsertInstallation(ctx, install(org, "456", "acme")); err != nil {
			t.Fatalf("UpsertInstallation (re-install): %v", err)
		}

		got := live(t, store, org)
		if got.Suspended() {
			t.Errorf("a re-installed installation reads as suspended (SuspendedAt=%s); want the suspension cleared", got.SuspendedAt)
		}
		if got.SuspendedBy != "" {
			t.Errorf("SuspendedBy = %q after a re-install; want \"\"", got.SuspendedBy)
		}
		if row := seed.Row(t, org, "456"); row.Removed {
			t.Error("removed_at still set after a re-install; want it cleared")
		}
	})

	t.Run("SuspensionIsPerInstallation", func(t *testing.T) {
		// Negative space: suspending one account's installation says nothing
		// about the org's others — an enterprise App has one installation per
		// GitHub org, and the rest keep minting.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		for _, inst := range []domain.OrgGitHubAppInstallation{
			{InstallationID: "456", OrgID: org, AccountType: "Organization", AccountID: "1234", AccountLogin: "acme"},
			{InstallationID: "789", OrgID: org, AccountType: "Organization", AccountID: "5678", AccountLogin: "beta"},
		} {
			if _, err := store.UpsertInstallation(ctx, inst); err != nil {
				t.Fatalf("UpsertInstallation(%s): %v", inst.InstallationID, err)
			}
		}

		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}

		insts, err := store.ListInstallationsForOrgSystem(ctx, org)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if len(insts) != 2 {
			t.Fatalf("ListInstallationsForOrgSystem = %d installations; want both still live", len(insts))
		}
		for _, got := range insts {
			want := got.InstallationID == "456"
			if got.Suspended() != want {
				t.Errorf("installation %s (%s): Suspended() = %v; want %v",
					got.InstallationID, got.AccountLogin, got.Suspended(), want)
			}
		}
	})

	t.Run("SuspendingAnUnknownInstallationIsANoOp", func(t *testing.T) {
		// A suspend for an installation the mirror has never seen (a missed
		// created delivery) writes nothing and errors on nothing — the next
		// reconcile mints the row with the suspension already on it.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))

		if _, err := store.SetInstallationSuspension(ctx, org, "456", suspendedAt, "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension on an unmirrored installation: %v", err)
		}
		if row := seed.Row(t, org, "456"); row.Exists {
			t.Error("a suspend for an unmirrored installation created a row; want no write")
		}
	})
}
