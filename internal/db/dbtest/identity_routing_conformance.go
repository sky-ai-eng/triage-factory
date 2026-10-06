package dbtest

import (
	"context"
	"sort"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// IdentityRoutingStores is the pair of stores the claims-free
// GitHub-identity → routing-target chain spans: the reverse identity
// lookup lives on UsersStore, the user→teams-in-org lookup on
// TeamsStore. Both halves are exercised against one wired backend so
// SQLite and Postgres pin to identical results.
type IdentityRoutingStores struct {
	Users db.UsersStore
	Teams db.TeamsStore
}

// IdentityRoutingSeeder stages the tenancy + membership rows the suite
// needs. User/org/team creation and membership enrollment have no store
// methods (row creation is an auth/provisioning concern), so each
// backend implements them against its own schema — the harness only
// consumes the returned IDs. GitHub identity bindings are NOT seeded
// here: the suite writes them through the real UsersStore.UpsertGitHubIdentity
// path so the reverse read is pinned against the same host-normalization
// the writers apply.
type IdentityRoutingSeeder struct {
	// User inserts a user row and returns its ID.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its ID.
	// ownerID must already exist (see User); backends whose orgs table
	// has no owner column may ignore it.
	Org func(t *testing.T, ownerID string) string

	// Team inserts a team row in orgID and returns its ID.
	Team func(t *testing.T, orgID string) string

	// Membership enrolls userID on teamID (team-level memberships).
	Membership func(t *testing.T, userID, teamID string)
}

// IdentityRoutingFactory is what a per-backend test file hands to
// RunIdentityRoutingConformance: the wired store pair plus the seeder.
// Each call returns a fresh, isolated backend so subtests don't leak
// rows into one another.
type IdentityRoutingFactory func(t *testing.T) (IdentityRoutingStores, IdentityRoutingSeeder)

// RunIdentityRoutingConformance is the shared assertion suite for the
// claims-free identity→routing primitives. It pins:
//
//   - UserIDsForGitHubLoginSystem resolves the bound user, returns ALL
//     users when two share a login on one host, returns an empty slice
//     on no binding, normalizes the host (trailing-slash-insensitive,
//     matching the writers), and is host-scoped (same login on another
//     host does not match).
//   - UserIDsForJiraAccountSystem (the Jira twin) pins the identical
//     contract keyed on the Atlassian account id instead of a login.
//   - UserIDsForLinearAccountSystem (the Linear twin) pins it keyed on a
//     workspace and a Linear user id, both matched verbatim, with the
//     workspace as the scope where the others use a host. Linear has no
//     capture route yet to cover its writes through, so the arm also pins
//     the UsersStore Linear surface the reverse lookup reads from: the
//     Get / GetSystem / Upsert / Clear round trip, and the writer's refusals.
//   - TeamIDsForUserInOrgSystem returns exactly the user's teams in
//     that org, excludes their teams in other orgs, and returns an
//     empty slice for a non-member.
//
// Both backends run identical subtests, so any drift between SQLite and
// Postgres fails one of the two per-backend test files immediately.
func RunIdentityRoutingConformance(t *testing.T, mk IdentityRoutingFactory) {
	t.Helper()
	ctx := context.Background()

	const host = "https://github.com"
	const jiraHost = "https://acme.atlassian.net"

	t.Run("UserIDsForGitHubLogin_ResolvesBoundUser", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, host, "octocat")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForGitHubLoginSystem", got, []string{u})
	})

	t.Run("UserIDsForGitHubLogin_ReturnsAllUsersSharingLogin", func(t *testing.T) {
		// The (user_id, github_base_url) PK constrains uniqueness per
		// user, so two TF users can bind the same login on one host —
		// the method must surface both (callers union the teams).
		stores, seed := mk(t)
		u1 := seed.User(t)
		u2 := seed.User(t)
		for _, u := range []string{u1, u2} {
			if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "shared-bot", "", "", "pat"); err != nil {
				t.Fatalf("UpsertGitHubIdentity(%s): %v", u, err)
			}
		}
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, host, "shared-bot")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForGitHubLoginSystem", got, []string{u1, u2})
	})

	t.Run("UserIDsForGitHubLogin_EmptyOnNoBinding", func(t *testing.T) {
		stores, seed := mk(t)
		// Bind a different login so the table is non-empty — the absent
		// row, not an empty table, is what must yield the empty slice.
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, host, "somebody", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, host, "nobody")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForGitHubLoginSystem(no binding) = %v; want empty slice", got)
		}
	})

	t.Run("UserIDsForGitHubLogin_HostNormalization", func(t *testing.T) {
		// Writer stores under the trailing-slash-trimmed host; a reader
		// passing the trailing-slash form must still resolve it (both
		// sides run db.NormalizeGitHubHost).
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, "https://github.com", "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, "https://github.com/", "octocat")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForGitHubLoginSystem(trailing slash)", got, []string{u})
	})

	t.Run("UserIDsForGitHubLogin_HostScoped", func(t *testing.T) {
		// Identity is keyed on (user_id, host); the same login on a
		// different host is a different person and must not match.
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, "https://github.com", "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForGitHubLoginSystem(ctx, "https://ghe.example.com", "octocat")
		if err != nil {
			t.Fatalf("UserIDsForGitHubLoginSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForGitHubLoginSystem(other host) = %v; want empty slice", got)
		}
	})

	const acct = "557058:abc-aidan"

	t.Run("UserIDsForJiraAccount_ResolvesBoundUser", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertJiraIdentity(ctx, u, jiraHost, acct, "Aidan", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForJiraAccountSystem(ctx, jiraHost, acct)
		if err != nil {
			t.Fatalf("UserIDsForJiraAccountSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForJiraAccountSystem", got, []string{u})
	})

	t.Run("UserIDsForJiraAccount_ReturnsAllUsersSharingAccount", func(t *testing.T) {
		// The (user_id, jira_base_url) PK constrains uniqueness per user, so
		// two TF users can bind the same Atlassian account on one host — the
		// method must surface both (callers union the teams).
		stores, seed := mk(t)
		u1 := seed.User(t)
		u2 := seed.User(t)
		for _, u := range []string{u1, u2} {
			if err := stores.Users.UpsertJiraIdentity(ctx, u, jiraHost, "shared-svc", "Shared", "pat"); err != nil {
				t.Fatalf("UpsertJiraIdentity(%s): %v", u, err)
			}
		}
		got, err := stores.Users.UserIDsForJiraAccountSystem(ctx, jiraHost, "shared-svc")
		if err != nil {
			t.Fatalf("UserIDsForJiraAccountSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForJiraAccountSystem", got, []string{u1, u2})
	})

	t.Run("UserIDsForJiraAccount_EmptyOnNoBinding", func(t *testing.T) {
		stores, seed := mk(t)
		// Bind a different account so the table is non-empty — the absent
		// row, not an empty table, is what must yield the empty slice.
		u := seed.User(t)
		if err := stores.Users.UpsertJiraIdentity(ctx, u, jiraHost, "somebody", "Somebody", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForJiraAccountSystem(ctx, jiraHost, "nobody")
		if err != nil {
			t.Fatalf("UserIDsForJiraAccountSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForJiraAccountSystem(no binding) = %v; want empty slice", got)
		}
	})

	t.Run("UserIDsForJiraAccount_HostNormalization", func(t *testing.T) {
		// Writer stores under the trailing-slash-trimmed host; a reader
		// passing the trailing-slash form must still resolve it (both sides
		// run db.NormalizeJiraHost).
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertJiraIdentity(ctx, u, jiraHost, acct, "Aidan", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForJiraAccountSystem(ctx, jiraHost+"/", acct)
		if err != nil {
			t.Fatalf("UserIDsForJiraAccountSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForJiraAccountSystem(trailing slash)", got, []string{u})
	})

	t.Run("UserIDsForJiraAccount_HostScoped", func(t *testing.T) {
		// Identity is keyed on (user_id, host); the same account on a
		// different host is a different binding and must not match.
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertJiraIdentity(ctx, u, jiraHost, acct, "Aidan", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForJiraAccountSystem(ctx, "https://other.atlassian.net", acct)
		if err != nil {
			t.Fatalf("UserIDsForJiraAccountSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForJiraAccountSystem(other host) = %v; want empty slice", got)
		}
	})

	const workspace = "6a0b8c3e-1f2d-4e5a-9b7c-0d1e2f3a4b5c"
	const linearUser = "2d9e4f1a-7b3c-4d5e-8f6a-1b2c3d4e5f60"

	t.Run("UserIDsForLinearAccount_ResolvesBoundUser", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, linearUser, "Aidan", "api_key"); err != nil {
			t.Fatalf("UpsertLinearIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForLinearAccountSystem(ctx, workspace, linearUser)
		if err != nil {
			t.Fatalf("UserIDsForLinearAccountSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForLinearAccountSystem", got, []string{u})
	})

	t.Run("UserIDsForLinearAccount_ReturnsAllUsersSharingAccount", func(t *testing.T) {
		// The (user_id, workspace_id) key constrains uniqueness per user, so
		// two TF users can bind the same Linear user in one workspace — the
		// method must surface both (callers union the teams).
		stores, seed := mk(t)
		u1 := seed.User(t)
		u2 := seed.User(t)
		for _, u := range []string{u1, u2} {
			if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, "shared-linear-user", "Shared", "api_key"); err != nil {
				t.Fatalf("UpsertLinearIdentity(%s): %v", u, err)
			}
		}
		got, err := stores.Users.UserIDsForLinearAccountSystem(ctx, workspace, "shared-linear-user")
		if err != nil {
			t.Fatalf("UserIDsForLinearAccountSystem: %v", err)
		}
		assertSameSet(t, "UserIDsForLinearAccountSystem", got, []string{u1, u2})
	})

	t.Run("UserIDsForLinearAccount_EmptyOnNoBinding", func(t *testing.T) {
		stores, seed := mk(t)
		// Bind a different Linear user so the table is non-empty — the
		// absent row, not an empty table, is what must yield the empty slice.
		u := seed.User(t)
		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, "somebody", "Somebody", "api_key"); err != nil {
			t.Fatalf("UpsertLinearIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForLinearAccountSystem(ctx, workspace, "nobody")
		if err != nil {
			t.Fatalf("UserIDsForLinearAccountSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForLinearAccountSystem(no binding) = %v; want empty slice", got)
		}
		// An org with no Linear workspace resolves nobody.
		got, err = stores.Users.UserIDsForLinearAccountSystem(ctx, "", "somebody")
		if err != nil {
			t.Fatalf("UserIDsForLinearAccountSystem(no workspace): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForLinearAccountSystem(no workspace) = %v; want empty slice", got)
		}
	})

	t.Run("UserIDsForLinearAccount_WorkspaceScoped", func(t *testing.T) {
		// Identity is keyed on (user_id, workspace); the same Linear user id
		// read against another workspace is not this binding.
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, linearUser, "Aidan", "api_key"); err != nil {
			t.Fatalf("UpsertLinearIdentity: %v", err)
		}
		got, err := stores.Users.UserIDsForLinearAccountSystem(ctx, "other-workspace", linearUser)
		if err != nil {
			t.Fatalf("UserIDsForLinearAccountSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("UserIDsForLinearAccountSystem(other workspace) = %v; want empty slice", got)
		}
	})

	t.Run("LinearIdentity_RoundTrip", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)

		// Both readers answer alike, and agree with each other at every step.
		read := func(t *testing.T, ws string) (string, string) {
			t.Helper()
			id, name, err := stores.Users.GetLinearIdentity(ctx, u, ws)
			if err != nil {
				t.Fatalf("GetLinearIdentity: %v", err)
			}
			sysID, sysName, err := stores.Users.GetLinearIdentitySystem(ctx, u, ws)
			if err != nil {
				t.Fatalf("GetLinearIdentitySystem: %v", err)
			}
			if sysID != id || sysName != name {
				t.Errorf("GetLinearIdentitySystem = (%q, %q), GetLinearIdentity = (%q, %q); want equal", sysID, sysName, id, name)
			}
			return id, name
		}

		if id, name := read(t, workspace); id != "" || name != "" {
			t.Fatalf("absent row reads (%q, %q), want empty", id, name)
		}

		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, linearUser, "Aidan", "api_key"); err != nil {
			t.Fatalf("UpsertLinearIdentity: %v", err)
		}
		if id, name := read(t, workspace); id != linearUser || name != "Aidan" {
			t.Errorf("after bind = (%q, %q), want (%q, Aidan)", id, name, linearUser)
		}

		// A second workspace is a second row, not an overwrite of the first.
		if err := stores.Users.UpsertLinearIdentity(ctx, u, "second-workspace", "lin-elsewhere", "Elsewhere", "connect_oauth"); err != nil {
			t.Fatalf("UpsertLinearIdentity(second workspace): %v", err)
		}
		if id, _ := read(t, workspace); id != linearUser {
			t.Errorf("first workspace after second bind = %q, want %q (distinct key)", id, linearUser)
		}

		// A re-bind in the same workspace replaces the identity, and an
		// empty display name stores NULL, which reads back as "".
		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, "lin-rebound", "", "connect_oauth"); err != nil {
			t.Fatalf("UpsertLinearIdentity(rebind): %v", err)
		}
		if id, name := read(t, workspace); id != "lin-rebound" || name != "" {
			t.Errorf("after rebind = (%q, %q), want (lin-rebound, \"\")", id, name)
		}
		if got, err := stores.Users.UserIDsForLinearAccountSystem(ctx, workspace, linearUser); err != nil || len(got) != 0 {
			t.Errorf("reverse lookup of the replaced id = %v, %v; want empty", got, err)
		}

		if err := stores.Users.ClearLinearIdentity(ctx, u, workspace); err != nil {
			t.Fatalf("ClearLinearIdentity: %v", err)
		}
		if id, name := read(t, workspace); id != "" || name != "" {
			t.Errorf("after clear = (%q, %q), want empty", id, name)
		}
		if id, _ := read(t, "second-workspace"); id != "lin-elsewhere" {
			t.Errorf("second workspace after clearing the first = %q, want lin-elsewhere", id)
		}
		// Clearing what is already gone is a no-op, not an error.
		if err := stores.Users.ClearLinearIdentity(ctx, u, workspace); err != nil {
			t.Errorf("ClearLinearIdentity(absent): %v", err)
		}
	})

	t.Run("UpsertLinearIdentity_Refusals", func(t *testing.T) {
		stores, seed := mk(t)
		u := seed.User(t)
		if err := stores.Users.UpsertLinearIdentity(ctx, u, "", linearUser, "Aidan", "api_key"); err == nil {
			t.Error("UpsertLinearIdentity(empty workspace) = nil, want an error")
		}
		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, "", "Aidan", "api_key"); err == nil {
			t.Error("UpsertLinearIdentity(empty linear user id) = nil, want an error")
		}
		// The source set is closed, and Linear has no PAT.
		if err := stores.Users.UpsertLinearIdentity(ctx, u, workspace, linearUser, "Aidan", "pat"); err == nil {
			t.Error("UpsertLinearIdentity(source=pat) = nil, want the source CHECK to refuse it")
		}
		if id, _, err := stores.Users.GetLinearIdentity(ctx, u, workspace); err != nil || id != "" {
			t.Errorf("after refused writes GetLinearIdentity = %q, %v; want no row", id, err)
		}
	})

	t.Run("TeamIDsForUserInOrg_ReturnsUsersTeams", func(t *testing.T) {
		// Member of t1+t2 but not t3 → exactly {t1, t2}.
		stores, seed := mk(t)
		u := seed.User(t)
		org := seed.Org(t, u)
		t1 := seed.Team(t, org)
		t2 := seed.Team(t, org)
		t3 := seed.Team(t, org) // user is NOT enrolled here
		seed.Membership(t, u, t1)
		seed.Membership(t, u, t2)

		got, err := stores.Teams.TeamIDsForUserInOrgSystem(ctx, org, u)
		if err != nil {
			t.Fatalf("TeamIDsForUserInOrgSystem: %v", err)
		}
		assertSameSet(t, "TeamIDsForUserInOrgSystem", got, []string{t1, t2})
		if contains(got, t3) {
			t.Errorf("TeamIDsForUserInOrgSystem leaked non-member team %s: %v", t3, got)
		}
	})

	t.Run("TeamIDsForUserInOrg_ExcludesOtherOrgTeams", func(t *testing.T) {
		// One user, a member of a team in each of two orgs on the same
		// host. The orgID scope must split them — orgA returns only its
		// team, orgB only its own.
		stores, seed := mk(t)
		u := seed.User(t)
		orgA := seed.Org(t, u)
		orgB := seed.Org(t, u)
		tA := seed.Team(t, orgA)
		tB := seed.Team(t, orgB)
		seed.Membership(t, u, tA)
		seed.Membership(t, u, tB)

		gotA, err := stores.Teams.TeamIDsForUserInOrgSystem(ctx, orgA, u)
		if err != nil {
			t.Fatalf("TeamIDsForUserInOrgSystem(orgA): %v", err)
		}
		assertSameSet(t, "TeamIDsForUserInOrgSystem(orgA)", gotA, []string{tA})

		gotB, err := stores.Teams.TeamIDsForUserInOrgSystem(ctx, orgB, u)
		if err != nil {
			t.Fatalf("TeamIDsForUserInOrgSystem(orgB): %v", err)
		}
		assertSameSet(t, "TeamIDsForUserInOrgSystem(orgB)", gotB, []string{tB})
	})

	t.Run("TeamIDsForUserInOrg_EmptyForNonMember", func(t *testing.T) {
		// A user enrolled on no team in the org (here, on no team at
		// all) resolves to the empty slice, not an error.
		stores, seed := mk(t)
		owner := seed.User(t)
		stranger := seed.User(t)
		org := seed.Org(t, owner)
		team := seed.Team(t, org)
		seed.Membership(t, owner, team) // only the owner is enrolled

		got, err := stores.Teams.TeamIDsForUserInOrgSystem(ctx, org, stranger)
		if err != nil {
			t.Fatalf("TeamIDsForUserInOrgSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("TeamIDsForUserInOrgSystem(non-member) = %v; want empty slice", got)
		}
	})
}

// assertSameSet fails the test unless got and want hold the same IDs,
// order-independent. The methods order their rows by id for determinism,
// but the contract is set-membership (callers union teams), so the
// assertion compares as sets to stay robust across the SQLite text /
// Postgres uuid ordering split.
func assertSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Errorf("%s = %v; want %v (set mismatch)", label, got, want)
		return
	}
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("%s = %v; want %v (set mismatch)", label, got, want)
			return
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
