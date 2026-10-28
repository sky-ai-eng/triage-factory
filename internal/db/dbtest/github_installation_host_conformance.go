package dbtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
//     in the schema uses, so a trailing slash cannot produce a second
//     spelling of one host;
//   - the column never reads back empty — an installation written without a
//     host names no GitHub and is refused;
//   - it is part of the row's key: the same installation id on two hosts is two
//     installations, in one org as across two, so neither write takes over the
//     other's row, and a removal or a suspension addresses one of them;
//   - and the mirror of that, which the column is half the key of: on ONE
//     deployment, an installation belongs to exactly one org, refused by a
//     unique index over live rows rather than by any caller's filter;
//   - an account login is held once per org per deployment, not once per org:
//     the same login on two hosts names two accounts, and one host still
//     refuses the login twice;
//   - ListInstallationsOnHost(System) reads one deployment's live rows,
//     normalizes its host argument as the write does, and answers an empty
//     host with nothing.
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

	t.Run("UnsetHostIsRefused", func(t *testing.T) {
		// An installation lives on one GitHub, and a struct without a host
		// names none: it is refused and nothing is written, whatever the
		// deployment's default GitHub is — never a row under "" or under a
		// host the writer did not name.
		ghbase.SetDefaultBaseURLForTest(t, "https://ghe.default.test")
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", "", "acme")); !errors.Is(err, db.ErrInstallationHostRequired) {
			t.Fatalf("UpsertInstallation with no host = %v; want ErrInstallationHostRequired", err)
		}
		if rows, err := store.ListInstallationsForOrgSystem(ctx, org); err != nil || len(rows) != 0 {
			t.Errorf("installations after a refused write = %+v, %v; want none", rows, err)
		}
		if got, err := store.InstallationOwnerSystem(ctx, "", "456"); err != nil || got != "" {
			t.Errorf("InstallationOwnerSystem(\"\", 456) = (%q, %v); want no owner", got, err)
		}
	})

	t.Run("SameInstallationIDOnTwoHostsInOneOrg", func(t *testing.T) {
		// GitHub numbers installations per deployment, so 456 on two hosts is
		// two installations even in one org. The second write is a row of its
		// own: it neither takes over the first row's host and account nor
		// collides with it, and a removal or a suspension names the host and
		// touches only that host's row.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghbase.DefaultBaseURL(), "acme")); err != nil {
			t.Fatalf("UpsertInstallation (default host): %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes, "globex")); err != nil {
			t.Fatalf("UpsertInstallation (same id on GHES): %v", err)
		}
		byHost := func(t *testing.T) map[string]domain.OrgGitHubAppInstallation {
			t.Helper()
			insts, err := store.ListInstallationsForOrgSystem(ctx, org)
			if err != nil {
				t.Fatalf("ListInstallationsForOrgSystem: %v", err)
			}
			out := map[string]domain.OrgGitHubAppInstallation{}
			for _, inst := range insts {
				out[inst.GitHubHost] = inst
			}
			return out
		}
		got := byHost(t)
		if len(got) != 2 || got[ghbase.DefaultBaseURL()].AccountLogin != "acme" || got[ghes].AccountLogin != "globex" {
			t.Fatalf("live installations = %+v; want 456 for acme on %s and 456 for globex on %s",
				got, ghbase.DefaultBaseURL(), ghes)
		}

		if _, err := store.SetInstallationSuspension(ctx, org, ghes, "456", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "octocat"); err != nil {
			t.Fatalf("SetInstallationSuspension: %v", err)
		}
		got = byHost(t)
		if !got[ghes].Suspended() || got[ghbase.DefaultBaseURL()].Suspended() {
			t.Errorf("suspending 456 on %s: suspended = (default %v, ghes %v); want only the GHES row",
				ghes, got[ghbase.DefaultBaseURL()].Suspended(), got[ghes].Suspended())
		}

		removed, err := store.MarkInstallationRemoved(ctx, org, ghes, "456")
		if err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		if removed == nil || removed.GitHubHost != ghes {
			t.Errorf("MarkInstallationRemoved returned %+v; want the GHES row", removed)
		}
		if got := only(t, store, org); got.GitHubHost != ghbase.DefaultBaseURL() || got.AccountLogin != "acme" {
			t.Errorf("live installation after removing the GHES one = %+v; want 456 for acme on %s", got, ghbase.DefaultBaseURL())
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
			t.Fatalf("UpsertInstallation (default host): %v", err)
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
		// org that once had it.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))

		if got, err := store.InstallationOwnerSystem(ctx, ghes, "999"); err != nil || got != "" {
			t.Fatalf("InstallationOwnerSystem on an unknown installation = (%q, %v); want (\"\", nil)", got, err)
		}

		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if _, err := store.MarkInstallationRemoved(ctx, org, ghes, "456"); err != nil {
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
		// One installation belongs to exactly one org per host, and the
		// index enforces it rather than a filter somewhere upstream. The grant is
		// not divisible, installation.deleted fires once, the rate budget is
		// shared, and the account owner cannot enumerate who rides their
		// installation — two orgs on one installation is not a state
		// anything downstream can represent.
		//
		// The refusal is the whole assertion, but so is what it leaves behind:
		// the org that already held the installation keeps its row exactly
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
		// nothing, so the id becoming another org's is an ordinary bind
		// rather than a collision — the same answer InstallationOwnerSystem
		// already gives for a removed row.
		store, seed := mk(t)
		owner := seed.User(t)
		orgA, orgB := seed.Org(t, owner), seed.Org(t, owner)

		if _, err := store.UpsertInstallation(ctx, install(orgA, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation (org A): %v", err)
		}
		if _, err := store.MarkInstallationRemoved(ctx, orgA, ghes, "456"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(orgB, "456", ghes, "other")); err != nil {
			t.Fatalf("UpsertInstallation (org B, after org A's removal): %v", err)
		}
		if got := only(t, store, orgB).AccountLogin; got != "other" {
			t.Errorf("org B AccountLogin = %q; want %q", got, "other")
		}
	})

	t.Run("SameLoginOnTwoHostsCoexists", func(t *testing.T) {
		// A login is unique within one GitHub deployment, so the same login on
		// two hosts names two accounts, and the second installation lands beside
		// the first rather than colliding with it.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghbase.DefaultBaseURL(), "acme")); err != nil {
			t.Fatalf("UpsertInstallation (default host): %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(org, "789", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation (same login on another host): %v", err)
		}
		insts, err := store.ListInstallationsForOrgSystem(ctx, org)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		hosts := map[string]string{}
		for _, inst := range insts {
			hosts[inst.InstallationID] = inst.GitHubHost
		}
		if len(insts) != 2 || hosts["456"] != ghbase.DefaultBaseURL() || hosts["789"] != ghes {
			t.Errorf("live installations = %+v; want 456 on %s and 789 on %s, both live", insts, ghbase.DefaultBaseURL(), ghes)
		}
	})

	t.Run("SameLoginTwiceOnOneHostIsRefused", func(t *testing.T) {
		// The host joined the key; it did not loosen it. On one deployment an
		// account holds at most one live installation, and a row that has no
		// account id yet, as these do, is held to its login.
		store, seed := mk(t)
		org := seed.Org(t, seed.User(t))
		if _, err := store.UpsertInstallation(ctx, install(org, "456", ghes, "acme")); err != nil {
			t.Fatalf("UpsertInstallation: %v", err)
		}
		if _, err := store.UpsertInstallation(ctx, install(org, "789", ghes+"/", "acme")); err == nil {
			t.Fatal("UpsertInstallation (same login, same host) succeeded; want a uniqueness refusal")
		}
		if got := only(t, store, org); got.InstallationID != "456" {
			t.Errorf("live installation = %q after the refused write; want the incumbent 456", got.InstallationID)
		}
	})

	t.Run("ListInstallationsOnHostReadsOneHost", func(t *testing.T) {
		store, seed := mk(t)
		owner := seed.User(t)
		org, other := seed.Org(t, owner), seed.Org(t, owner)
		for _, inst := range []domain.OrgGitHubAppInstallation{
			install(org, "456", ghbase.DefaultBaseURL(), "acme"),
			install(org, "789", ghes, "acme"),
			install(org, "790", ghes, "globex"),
			install(org, "791", ghes, "initech"), // removed below
			install(other, "800", ghes, "umbrella"),
		} {
			if _, err := store.UpsertInstallation(ctx, inst); err != nil {
				t.Fatalf("UpsertInstallation(%s): %v", inst.InstallationID, err)
			}
		}
		if _, err := store.MarkInstallationRemoved(ctx, org, ghes, "791"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}

		ids := func(insts []domain.OrgGitHubAppInstallation) []string {
			out := make([]string, 0, len(insts))
			for _, inst := range insts {
				out = append(out, inst.InstallationID)
			}
			return out
		}
		reads := map[string]func(host string) ([]domain.OrgGitHubAppInstallation, error){
			"ListInstallationsOnHost": func(host string) ([]domain.OrgGitHubAppInstallation, error) {
				return store.ListInstallationsOnHost(ctx, org, host)
			},
			"ListInstallationsOnHostSystem": func(host string) ([]domain.OrgGitHubAppInstallation, error) {
				return store.ListInstallationsOnHostSystem(ctx, org, host)
			},
		}
		for name, read := range reads {
			for _, tc := range []struct {
				host string
				want []string
			}{
				// The org's live rows on that host, ordered by login; the removed
				// row and the other org's row on the same host are not among them.
				{ghes, []string{"789", "790"}},
				// The argument normalizes as the write does.
				{ghes + "/", []string{"789", "790"}},
				{ghbase.DefaultBaseURL(), []string{"456"}},
				// A host the org holds nothing on.
				{"https://ghe.elsewhere.test", []string{}},
				// No host is no match, not the deployment default.
				{"", []string{}},
				{"/", []string{}},
			} {
				got, err := read(tc.host)
				if err != nil {
					t.Fatalf("%s(%q): %v", name, tc.host, err)
				}
				if g := ids(got); !reflect.DeepEqual(g, tc.want) {
					t.Errorf("%s(%q) = %v; want %v", name, tc.host, g, tc.want)
				}
				for _, inst := range got {
					if inst.OrgID != org {
						t.Errorf("%s(%q) returned org %q's row %s", name, tc.host, inst.OrgID, inst.InstallationID)
					}
				}
			}
		}
	})
}
