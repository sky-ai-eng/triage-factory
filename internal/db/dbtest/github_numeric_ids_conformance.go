package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubNumericIDStores is the pair of stores that persist GitHub's numeric
// ids: the installation mirror on GitHubAppsStore and the per-user identity
// binding on UsersStore. Both halves run against one wired backend so SQLite
// and Postgres pin to identical results.
type GitHubNumericIDStores struct {
	Users      db.UsersStore
	GitHubApps db.GitHubAppsStore
}

// GitHubNumericIDSeeder stages the rows the suite needs and reads back the one
// column no store method surfaces. User and org creation have no store methods
// (row creation is an auth/provisioning concern), so each backend implements
// them against its own schema.
type GitHubNumericIDSeeder struct {
	// User inserts a user row and returns its ID.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its ID. ownerID
	// must already exist (see User); backends whose orgs table has no owner
	// column may ignore it.
	Org func(t *testing.T, ownerID string) string

	// GitHubUserID reads user_github_identities.github_user_id for the (user,
	// host) binding, "" for NULL or an absent row. It is a raw-SQL probe
	// rather than a store method on purpose: nothing in the product reads
	// this column yet — it is written now so the identity records WHICH
	// account it bound, not only what that account is currently called — and
	// adding a reader no caller wants would be worse than a test-local probe.
	// Implementations must normalize the host the way the writers do
	// (db.NormalizeGitHubHost).
	GitHubUserID func(t *testing.T, userID, githubBaseURL string) string
	GitHubEmail  func(t *testing.T, userID, githubBaseURL string) string
}

// GitHubNumericIDFactory is what a per-backend test file hands to
// RunGitHubNumericIDConformance. Each call returns a fresh, isolated backend so
// subtests don't leak rows into one another.
type GitHubNumericIDFactory func(t *testing.T) (GitHubNumericIDStores, GitHubNumericIDSeeder)

// RunGitHubNumericIDConformance is the shared assertion suite for the numeric
// GitHub ids stored alongside the renameable handles they outlive. It pins:
//
//   - org_github_app_installations.account_id round-trips through
//     UpsertInstallation → ListInstallationsForOrgSystem;
//   - an installation mirrored without one reads back as "" and is not an
//     error — the un-backfilled row is a supported state, not a gap;
//   - the id fills in opportunistically on a later write that names it, and a
//     later write that does NOT name it leaves the stored id alone;
//   - account_login is overwritten when GitHub reports a new one for a known
//     account_id, because the login is what the UI renders and a rename has to
//     reach it;
//   - user_github_identities.github_user_id round-trips through
//     UpsertGitHubIdentity under the same fill-in-never-erase rule, and the
//     reverse (host, login) → user lookup resolves regardless of the
//     capitalisation either side was captured with.
func RunGitHubNumericIDConformance(t *testing.T, mk GitHubNumericIDFactory) {
	t.Helper()
	ctx := context.Background()

	const host = "https://github.com"

	// install builds one mirrored installation for org, so each subtest names
	// only the fields it is actually about.
	install := func(orgID, installationID, accountID, login string) domain.OrgGitHubAppInstallation {
		return domain.OrgGitHubAppInstallation{
			InstallationID: installationID,
			OrgID:          orgID,
			AccountType:    "Organization",
			AccountID:      accountID,
			AccountLogin:   login,
		}
	}

	// only returns the single installation the org has, failing the test if it
	// has any other number — every subtest below asserts against exactly one.
	only := func(t *testing.T, stores GitHubNumericIDStores, orgID string) domain.OrgGitHubAppInstallation {
		t.Helper()
		insts, err := stores.GitHubApps.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if len(insts) != 1 {
			t.Fatalf("ListInstallationsForOrgSystem = %d installations; want 1", len(insts))
		}
		return insts[0]
	}

	t.Run("InstallationAccountIDRoundTrips", func(t *testing.T) {
		stores, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "1234", "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		got := only(t, stores, org)
		if got.AccountID != "1234" {
			t.Errorf("AccountID = %q; want %q", got.AccountID, "1234")
		}
		if got.AccountLogin != "acme" {
			t.Errorf("AccountLogin = %q; want %q", got.AccountLogin, "acme")
		}
	})

	t.Run("InstallationWithoutAccountIDReadsEmpty", func(t *testing.T) {
		// The negative space: a row mirrored before ids were captured. It must
		// read back cleanly as "" — no error, no sentinel — so resolution by
		// login is unaffected by the column existing.
		stores, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "", "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if got := only(t, stores, org); got.AccountID != "" {
			t.Errorf("AccountID = %q for an installation mirrored without one; want \"\"", got.AccountID)
		}
	})

	t.Run("InstallationAccountIDBackfillsOpportunistically", func(t *testing.T) {
		// The backfill is the next write that names the account, not a job that
		// walks every org: an id-less row picks the id up from the reconcile or
		// webhook that follows.
		stores, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "", "acme")); err != nil {
			t.Fatalf("UpsertInstallation (no id): %v", err)
		}
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "1234", "acme")); err != nil {
			t.Fatalf("UpsertInstallation (with id): %v", err)
		}
		if got := only(t, stores, org); got.AccountID != "1234" {
			t.Errorf("AccountID = %q after a write that named it; want %q", got.AccountID, "1234")
		}
	})

	t.Run("InstallationAccountIDSurvivesAWriteThatOmitsIt", func(t *testing.T) {
		// Fill in, never erase: a payload that carried no account id must not
		// unlearn one the mirror already has.
		stores, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "1234", "acme")); err != nil {
			t.Fatalf("UpsertInstallation (with id): %v", err)
		}
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "", "acme")); err != nil {
			t.Fatalf("UpsertInstallation (no id): %v", err)
		}
		if got := only(t, stores, org); got.AccountID != "1234" {
			t.Errorf("AccountID = %q after a write that omitted it; want the stored %q", got.AccountID, "1234")
		}
	})

	t.Run("InstallationLoginUpdatesForAKnownAccountID", func(t *testing.T) {
		// The mirror image of the rule above, and the reason the two fields
		// update on different rules: account_login is what the UI renders, so a
		// rename has to land on it.
		stores, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "1234", "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if _, err := stores.GitHubApps.UpsertInstallation(ctx, install(org, "456", "1234", "acme-corp")); err != nil {
			t.Fatalf("UpsertInstallation (renamed): %v", err)
		}
		got := only(t, stores, org)
		if got.AccountLogin != "acme-corp" {
			t.Errorf("AccountLogin = %q after a rename; want the new %q", got.AccountLogin, "acme-corp")
		}
		if got.AccountID != "1234" {
			t.Errorf("AccountID = %q after a rename; want the unchanged %q", got.AccountID, "1234")
		}
	})

	t.Run("IdentityGitHubUserIDRoundTrips", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if got := seed.GitHubUserID(t, u, host); got != "583231" {
			t.Errorf("github_user_id = %q; want %q", got, "583231")
		}
		// The binding still resolves by login — the id is recorded alongside
		// the handle, it does not replace it.
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, host, "octocat")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForGitHubLoginSystem", got, []string{u})
	})

	t.Run("IdentityReadReturnsTheRowByIdAndNilForNone", func(t *testing.T) {
		// The bind ceremony compares identities by numeric id, so the read has
		// to hand the id back beside the login — and answer nil, not a zero
		// row, for a user with nothing linked on that host, since "nothing
		// linked" is a refusal of its own there.
		stores, seed := mk(t)
		u := seed.User(t)
		if got, err := stores.Users.GetGitHubIdentity(ctx, u, host); err != nil || got != nil {
			t.Fatalf("GetGitHubIdentity(unlinked) = %+v, %v; want nil, nil", got, err)
		}
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "", "connect_oauth"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		got, err := stores.Users.GetGitHubIdentity(ctx, u, host)
		if err != nil || got == nil {
			t.Fatalf("GetGitHubIdentity = %+v, %v", got, err)
		}
		if got.Login != "octocat" || got.GitHubUserID != "583231" || got.Source != "connect_oauth" || got.VerifiedAt.IsZero() {
			t.Errorf("GetGitHubIdentity = %+v; want octocat/583231/connect_oauth with a verification time", *got)
		}
		if got, err := stores.Users.GetGitHubIdentity(ctx, u, "https://elsewhere.example"); err != nil || got != nil {
			t.Errorf("GetGitHubIdentity(other host) = %+v, %v; want nil — identity is host-scoped", got, err)
		}
	})

	t.Run("IdentityEmailRoundTripsAndSurvivesAnOmission", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "octocat@example.com", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if got := seed.GitHubEmail(t, u, host); got != "octocat@example.com" {
			t.Errorf("email = %q; want octocat@example.com", got)
		}
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "", "login_claim"); err != nil {
			t.Fatalf("UpsertGitHubIdentity without email: %v", err)
		}
		if got := seed.GitHubEmail(t, u, host); got != "octocat@example.com" {
			t.Errorf("email = %q after omitted capture; want stored address", got)
		}
	})

	t.Run("IdentityRebindWithoutEmailClearsPriorAccountsEmail", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "octocat@example.com", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "hubber", "999", "", "login_claim"); err != nil {
			t.Fatalf("UpsertGitHubIdentity rebind: %v", err)
		}
		if got := seed.GitHubEmail(t, u, host); got != "" {
			t.Errorf("email = %q after rebind without one; want empty rather than prior account's address", got)
		}
	})

	t.Run("IdentityWithoutGitHubUserIDReadsEmpty", func(t *testing.T) {
		// A host that reported no id binds anyway: the login is what proves the
		// token, and the id fills in on a later capture.
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if got := seed.GitHubUserID(t, u, host); got != "" {
			t.Errorf("github_user_id = %q for a binding captured without one; want \"\"", got)
		}
	})

	t.Run("IdentityGitHubUserIDBackfillsAndSurvivesAnOmission", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity (no id): %v", err)
		}
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "", "connect_oauth"); err != nil {
			t.Fatalf("UpsertGitHubIdentity (with id): %v", err)
		}
		if got := seed.GitHubUserID(t, u, host); got != "583231" {
			t.Errorf("github_user_id = %q after a capture that named it; want %q", got, "583231")
		}
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity (no id, again): %v", err)
		}
		if got := seed.GitHubUserID(t, u, host); got != "583231" {
			t.Errorf("github_user_id = %q after a capture that omitted it; want the stored %q", got, "583231")
		}
	})

	t.Run("IdentityRebindReplacesGitHubUserID", func(t *testing.T) {
		// Re-binding to a different account carries that account's id, so
		// fill-in-never-erase can't pin a stale one.
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "583231", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "hubber", "999", "", "connect_oauth"); err != nil {
			t.Fatalf("UpsertGitHubIdentity (re-bind): %v", err)
		}
		if got := seed.GitHubUserID(t, u, host); got != "999" {
			t.Errorf("github_user_id = %q after re-binding to another account; want %q", got, "999")
		}
	})

	t.Run("IdentityReverseLookupIsCaseInsensitive", func(t *testing.T) {
		// GitHub logins are case-insensitive and the writers persist them as
		// captured, so a verbatim compare misses on capitalisation — and a miss
		// here reads as "this person is on no team", not as an error.
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "OctoCat", "583231", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		for _, probe := range []string{"OctoCat", "octocat", "OCTOCAT"} {
			got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, host, probe)
			if err != nil {
				t.Fatalf("UserIDsForGitHubLoginSystem(%q): %v", probe, err)
			}
			assertSameSet(t, "UserIDsForGitHubLoginSystem("+probe+")", got, []string{u})
		}
		// Case folding must not turn the lookup into a prefix / fuzzy match.
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, host, "octocat2")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem(other login): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForGitHubLoginSystem(octocat2) = %v; want empty slice", got)
		}
	})
}
