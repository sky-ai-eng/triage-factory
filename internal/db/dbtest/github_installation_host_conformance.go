package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// GitHubInstallationHostSeeder stages the rows the host suite needs. User and
// org creation have no store methods (row creation is an auth/provisioning
// concern), so each backend implements them against its own schema.
type GitHubInstallationHostSeeder struct {
	// User inserts a user row and returns its ID.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its ID. ownerID must
	// already exist (see User); backends whose orgs table has no owner column
	// may ignore it.
	Org func(t *testing.T, ownerID string) string
}

// GitHubInstallationHostFactory is what a per-backend test file hands to
// RunGitHubInstallationHostConformance. Each call returns a fresh, isolated
// backend so subtests don't leak rows into one another.
type GitHubInstallationHostFactory func(t *testing.T) (db.GitHubAppsStore, GitHubInstallationHostSeeder)

// RunGitHubInstallationHostConformance is the shared assertion suite for the
// GitHub deployment denormalized onto the installation row. It pins:
//
//   - github_host round-trips through UpsertInstallation →
//     ListInstallationsForOrgSystem, path component and all;
//   - it is normalized on write to the same string every other GitHub host key
//     in the schema uses, so a trailing slash and an unset base URL cannot
//     produce a second spelling of one host;
//   - the column never reads back empty — an installation written without a
//     host is one whose org configured none, which is the public host;
//   - it follows the org when a later write reports a different one, like the
//     login and unlike the account id;
//   - the negative space this column exists FOR: two orgs on different GitHub
//     deployments may hold the same numeric installation_id, because that id is
//     unique per deployment and not universally;
//   - and the mirror of that, which the column is half the key of: on ONE
//     deployment, an installation belongs to exactly one workspace, refused by a
//     unique index over live rows rather than by any caller's filter.
func RunGitHubInstallationHostConformance(t *testing.T, mk GitHubInstallationHostFactory) {
	t.Helper()
	ctx := context.Background()

	// A GHES base with a path: the shape a bare-origin derivation would
	// silently truncate, and therefore the one worth carrying through the suite.
	const ghes = "https://git.example.com/github"

	install := func(orgID, installationID, host, login string) domain.OrgGitHubAppInstallation {
		return domain.OrgGitHubAppInstallation{
			InstallationID: installationID,
			OrgID:          orgID,
			AccountType:    "Organization",
			AccountLogin:   login,
			GitHubHost:     host,
		}
	}

	// only returns the single installation the org has, failing the test if it
	// has any other number.
	only := func(t *testing.T, store db.GitHubAppsStore, orgID string) domain.OrgGitHubAppInstallation {
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

	t.Run("HostRoundTrips", func(t *testing.T) {
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if got := only(t, store, org).GitHubHost; got != ghes {
			t.Errorf("GitHubHost = %q; want the stored %q — a truncated path keys a different GitHub", got, ghes)
		}
	})

	t.Run("HostIsNormalizedOnWrite", func(t *testing.T) {
		// One GitHub must be one string. A base URL that differs only by a
		// trailing slash is the same deployment, and storing both spellings
		// would make the column useless for the comparisons it exists to enable.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes+"/", "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if got := only(t, store, org).GitHubHost; got != ghes {
			t.Errorf("GitHubHost = %q for a base URL with a trailing slash; want the normalized %q", got, ghes)
		}
	})

	t.Run("UnsetHostResolvesToTheDeploymentDefault", func(t *testing.T) {
		// The NOT NULL column's floor: an org that configured no base URL is on
		// the deployment's default GitHub, so a struct built without a host is
		// a row under that host — never a NULL and never the empty string,
		// which is not a host anything can be compared against. Pinned under a
		// non-github.com default so the assertion cannot pass on the literal:
		// the store must read the resolved default, not spell github.com.
		ghbase.SetDefaultBaseURLForTest(t, "https://ghe.default.test")
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", "", "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if got := only(t, store, org).GitHubHost; got != "https://ghe.default.test" {
			t.Errorf("GitHubHost = %q for an installation written without one; want the deployment default %q", got, "https://ghe.default.test")
		}
		// And the lookup normalizes an empty host the same way, so a caller
		// asking about "the default" finds the row the writer keyed.
		if got, err := store.InstallationOwnerSystem(ctx, "", "456"); err != nil || got != org {
			t.Errorf("InstallationOwnerSystem(\"\", 456) = (%q, %v); want (%q, nil)", got, err, org)
		}
	})

	t.Run("HostFollowsALaterWrite", func(t *testing.T) {
		// Both writers derive the host from the org's current base URL, so an
		// installation the current host reports is on the current host. It takes
		// the login's overwrite rule rather than the account id's fill-in-only
		// one for that reason: there is no such thing as a writer that saw the
		// installation but not which GitHub it came from.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghbase.DefaultBaseURL(), "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation (moved host): %v", err)
		}
		if got := only(t, store, org).GitHubHost; got != ghes {
			t.Errorf("GitHubHost = %q after a write reporting another deployment; want %q", got, ghes)
		}
	})

	t.Run("SameInstallationIDOnDifferentHosts", func(t *testing.T) {
		// The negative space, and the whole reason the column exists. GitHub
		// numbers installations per deployment, so a self-host aggregating orgs
		// across two GHES instances will see id 456 twice, meaning two unrelated
		// installations. Nothing may refuse the second one or collapse it onto
		// the first.
		store, seed := mk(t)
		owner := seed.User(t)
		orgA, orgB := seed.Org(t, owner), seed.Org(t, owner)

		if _, err := store.UpsertInstallation(ctx, install(orgA, "456", ghbase.DefaultBaseURL(), "acme")); err != nil {
			t.Fatalf("UpsertInstallation (public host): %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(orgB, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation (GHES, same installation id): %v", err)
		}

		gotA, gotB := only(t, store, orgA), only(t, store, orgB)
		if gotA.GitHubHost != ghbase.DefaultBaseURL() {
			t.Errorf("org A GitHubHost = %q; want %q", gotA.GitHubHost, ghbase.DefaultBaseURL())
		}
		if gotB.GitHubHost != ghes {
			t.Errorf("org B GitHubHost = %q; want %q", gotB.GitHubHost, ghes)
		}
		if gotA.InstallationID != "456" || gotB.InstallationID != "456" {
			t.Errorf("installation ids = %q / %q; want both %q — neither row may have been rewritten",
				gotA.InstallationID, gotB.InstallationID, "456")
		}
	})

	t.Run("InstallationOwnerIsHostScoped", func(t *testing.T) {
		// The uniqueness gate of the bind ceremony, and it asks the same
		// host-scoped question the column exists for: an id alone names nothing,
		// because two deployments number installations independently.
		store, seed := mk(t)
		owner := seed.User(t)
		orgA := seed.Org(t, owner)

		if _, err := store.UpsertInstallation(ctx, install(orgA, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}

		got, err := store.InstallationOwnerSystem(ctx, ghes, "456")
		if err != nil {
			t.Fatalf("InstallationOwnerSystem: %v", err)
		}
		if got != orgA {
			t.Errorf("owner of (%s, 456) = %q; want %q", ghes, got, orgA)
		}

		// A trailing slash is the same deployment, so it must find the same row
		// — the lookup normalizes its argument exactly as the write does.
		if got, err = store.InstallationOwnerSystem(ctx, ghes+"/", "456"); err != nil {
			t.Fatalf("InstallationOwnerSystem (trailing slash): %v", err)
		}
		if got != orgA {
			t.Errorf("owner of (%s/, 456) = %q; want %q — the host argument must normalize", ghes, got, orgA)
		}

		// The same number on another deployment is another installation, and
		// nobody holds it.
		if got, err = store.InstallationOwnerSystem(ctx, ghbase.DefaultBaseURL(), "456"); err != nil {
			t.Fatalf("InstallationOwnerSystem (other host): %v", err)
		}
		if got != "" {
			t.Errorf("owner of (%s, 456) = %q; want \"\" — that id is another deployment's",
				ghbase.DefaultBaseURL(), got)
		}
	})

	t.Run("InstallationOwnerIgnoresRemovedAndUnknown", func(t *testing.T) {
		// An uninstalled installation reaches nothing, so it owns nothing:
		// re-binding it is an ordinary new bind, not a collision with the
		// workspace that once had it.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))

		if got, err := store.InstallationOwnerSystem(ctx, ghes, "999"); err != nil || got != "" {
			t.Fatalf("InstallationOwnerSystem on an unknown installation = (%q, %v); want (\"\", nil)", got, err)
		}

		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if _, err := store.MarkInstallationRemoved(ctx, org, "456"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		got, err := store.InstallationOwnerSystem(ctx, ghes, "456")
		if err != nil {
			t.Fatalf("InstallationOwnerSystem: %v", err)
		}
		if got != "" {
			t.Errorf("owner of a removed installation = %q; want \"\"", got)
		}
	})

	t.Run("SameInstallationIDOnOneHostIsRefused", func(t *testing.T) {
		// One installation belongs to exactly one workspace per host, and the
		// index enforces it rather than a filter somewhere upstream. The grant is
		// not divisible, installation.deleted fires once, the rate budget is
		// shared, and the account owner cannot enumerate who rides their
		// installation — two workspaces on one installation is not a state
		// anything downstream can represent.
		//
		// The refusal is the whole assertion, but so is what it leaves behind:
		// the workspace that already held the installation keeps its row exactly
		// as it was, so a rejected claim costs the incumbent nothing.
		store, seed := mk(t)
		owner := seed.User(t)
		orgA, orgB := seed.Org(t, owner), seed.Org(t, owner)

		if _, err := store.UpsertInstallation(ctx, install(orgA, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation (org A): %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(orgB, "456", ghes, "other")); err == nil {
			t.Fatal("UpsertInstallation (org B, same host + installation id) succeeded; want a uniqueness refusal")
		}

		gotA := only(t, store, orgA)
		if gotA.GitHubHost != ghes || gotA.AccountLogin != "acme" {
			t.Errorf("org A row = (%q, %q) after org B's refused claim; want (%q, %q) — the incumbent is untouched",
				gotA.GitHubHost, gotA.AccountLogin, ghes, "acme")
		}
		insts, err := store.ListInstallationsForOrgSystem(ctx, orgB)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem (org B): %v", err)
		}
		if len(insts) != 0 {
			t.Errorf("org B holds %d installations; want 0 — a refused claim writes nothing", len(insts))
		}
	})

	t.Run("RemovedInstallationFreesItsIDOnThatHost", func(t *testing.T) {
		// The uniqueness index is partial on live rows, and this is what that
		// buys. An uninstalled installation reaches nothing and therefore holds
		// nothing, so the id becoming another workspace's is an ordinary bind
		// rather than a collision — the same answer InstallationOwnerSystem
		// already gives for a removed row.
		store, seed := mk(t)
		owner := seed.User(t)
		orgA, orgB := seed.Org(t, owner), seed.Org(t, owner)

		if _, err := store.UpsertInstallation(ctx, install(orgA, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation (org A): %v", err)
		}
		if _, err := store.MarkInstallationRemoved(ctx, orgA, "456"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(orgB, "456", ghes, "other")); err != nil {
			t.Fatalf("UpsertInstallation (org B, after org A's removal): %v", err)
		}
		if got := only(t, store, orgB).AccountLogin; got != "other" {
			t.Errorf("org B AccountLogin = %q; want %q", got, "other")
		}
	})
}
