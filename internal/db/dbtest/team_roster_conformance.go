package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamRosterStores is the pair of stores one team-roster read spans: the
// roster itself lives on TeamsStore, and the identity bindings it enriches
// with are written through UsersStore — through the real writers, so the
// read is pinned against the same host normalization the capture paths apply
// rather than against hand-inserted rows.
type TeamRosterStores struct {
	Teams db.TeamsStore
	Users db.UsersStore
}

// TeamRosterSeeder stages the tenancy + membership rows the suite needs. User
// / org / team creation and membership enrollment have no store methods (row
// creation is an auth/provisioning concern), so each backend implements them
// against its own schema and the harness only consumes the returned IDs.
type TeamRosterSeeder struct {
	// User inserts a user row with displayName and returns its ID. The name
	// is the suite's input, not a label: the roster orders by it, and the
	// paging cases deliberately hand out colliding names.
	User func(t *testing.T, displayName string) string

	// Org inserts an org row owned by ownerID and returns its ID.
	Org func(t *testing.T, ownerID string) string

	// Team inserts a team row in orgID and returns its ID.
	Team func(t *testing.T, orgID string) string

	// Membership enrolls userID on teamID with a team role
	// ("admin" | "member" | "viewer").
	Membership func(t *testing.T, userID, teamID, role string)
}

// TeamRosterFactory is what a per-backend test file hands to
// RunTeamRosterConformance: the wired stores plus the seeder. Each call
// returns a fresh, isolated backend so subtests don't leak rows.
type TeamRosterFactory func(t *testing.T) (TeamRosterStores, TeamRosterSeeder)

// RunTeamRosterConformance is the shared assertion suite for
// TeamsStore.ListMembers. It pins:
//
//   - the row shape: user id, display name, team role, and the host-scoped
//     GitHub / Jira readiness pointers (nil when unbound).
//   - the ordering: display name, then user id. The tiebreaker is the half
//     that matters under paging, so the paging case gives every member the
//     SAME display name — without the id tiebreaker those pages drop and
//     repeat rows, and with it the walk is exact.
//   - paging: (items, total, err), where total is the unpaged count on every
//     page and an offset past the end is an empty page rather than an error.
//   - host scoping: an unset github_base_url resolves to the deployment default (that is
//     where the capture paths bind), an unset jira_base_url matches nothing,
//     and a binding on another host never leaks into this roster.
//   - the N=1 shape local mode runs on: one member, role admin, identities
//     populated — the answer the assignee picker and the predicate editor
//     read there, from the same query the multi-team roster uses.
//
// Both backends run identical subtests, so any drift between SQLite and
// Postgres fails one of the two per-backend test files immediately.
func RunTeamRosterConformance(t *testing.T, mk TeamRosterFactory) {
	t.Helper()
	ctx := context.Background()

	const ghHost = "https://github.com"
	const jiraHost = "https://acme.atlassian.net"

	// team stages an org with a team and returns both, plus the stores.
	team := func(t *testing.T) (TeamRosterStores, TeamRosterSeeder, string) {
		t.Helper()
		stores, seed := mk(t)
		owner := seed.User(t, "owner")
		org := seed.Org(t, owner)
		return stores, seed, seed.Team(t, org)
	}

	byID := func(members []domain.TeamMember) map[string]domain.TeamMember {
		out := make(map[string]domain.TeamMember, len(members))
		for _, m := range members {
			out[m.UserID] = m
		}
		return out
	}

	t.Run("ListMembers_RolesAndOrder", func(t *testing.T) {
		stores, seed, teamID := team(t)
		// Seeded out of alphabetical order so the ORDER BY is doing the work.
		carol := seed.User(t, "Carol")
		alice := seed.User(t, "Alice")
		bob := seed.User(t, "Bob")
		seed.Membership(t, carol, teamID, "viewer")
		seed.Membership(t, alice, teamID, "admin")
		seed.Membership(t, bob, teamID, "member")

		members, total, err := stores.Teams.ListMembers(ctx, teamID, "", "", db.Unwindowed)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		wantOrder := []string{alice, bob, carol}
		if len(members) != len(wantOrder) {
			t.Fatalf("members = %d, want %d", len(members), len(wantOrder))
		}
		for i, want := range wantOrder {
			if members[i].UserID != want {
				t.Errorf("members[%d].user_id = %s, want %s (display-name order)", i, members[i].UserID, want)
			}
		}
		roles := map[string]string{alice: "admin", bob: "member", carol: "viewer"}
		for id, m := range byID(members) {
			if m.Role != roles[id] {
				t.Errorf("role for %s = %q, want %q", id, m.Role, roles[id])
			}
			if m.GitHubUsername != nil || m.JiraAccountID != nil {
				t.Errorf("member %s has an identity binding, want nil/nil (nothing bound)", id)
			}
		}
	})

	t.Run("ListMembers_PagesWithoutDroppingTiedRows", func(t *testing.T) {
		stores, seed, teamID := team(t)
		// Every member shares one display name: the worst case for offset
		// paging, and the one the id tiebreaker exists for.
		want := map[string]bool{}
		for range 5 {
			u := seed.User(t, "Same Name")
			seed.Membership(t, u, teamID, "member")
			want[u] = true
		}

		seen := map[string]bool{}
		for offset := 0; offset < 6; offset += 2 {
			page, total, err := stores.Teams.ListMembers(ctx, teamID, "", "",
				db.ListOpts{Limit: 2, Offset: offset})
			if err != nil {
				t.Fatalf("ListMembers(offset=%d): %v", offset, err)
			}
			if total != 5 {
				t.Errorf("total at offset %d = %d, want 5 (unpaged count on every page)", offset, total)
			}
			wantLen := map[int]int{0: 2, 2: 2, 4: 1}[offset]
			if len(page) != wantLen {
				t.Errorf("page at offset %d has %d rows, want %d", offset, len(page), wantLen)
			}
			for _, m := range page {
				if seen[m.UserID] {
					t.Errorf("member %s repeated across pages", m.UserID)
				}
				seen[m.UserID] = true
			}
		}
		for id := range want {
			if !seen[id] {
				t.Errorf("member %s never appeared in any page", id)
			}
		}
	})

	t.Run("ListMembers_OffsetPastEndIsEmpty", func(t *testing.T) {
		stores, seed, teamID := team(t)
		u := seed.User(t, "Only")
		seed.Membership(t, u, teamID, "admin")

		page, total, err := stores.Teams.ListMembers(ctx, teamID, "", "",
			db.ListOpts{Limit: 10, Offset: 10})
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		if len(page) != 0 {
			t.Errorf("page = %d rows, want 0 past the end", len(page))
		}
		if total != 1 {
			t.Errorf("total = %d, want 1 (the count doesn't move with the window)", total)
		}
	})

	t.Run("ListMembers_EmptyTeam", func(t *testing.T) {
		stores, _, teamID := team(t)
		members, total, err := stores.Teams.ListMembers(ctx, teamID, "", "", db.Unwindowed)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		if len(members) != 0 || total != 0 {
			t.Errorf("ListMembers(empty team) = %d rows / total %d, want 0 / 0", len(members), total)
		}
	})

	t.Run("ListMembers_IdentityReadiness", func(t *testing.T) {
		stores, seed, teamID := team(t)
		bound := seed.User(t, "Bound")
		unbound := seed.User(t, "Unbound")
		seed.Membership(t, bound, teamID, "admin")
		seed.Membership(t, unbound, teamID, "member")
		if err := stores.Users.UpsertGitHubIdentity(ctx, bound, ghHost, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := stores.Users.UpsertJiraIdentity(ctx, bound, jiraHost, "acct-1", "Bound", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}

		// githubBaseURL is passed UNSET, as an org on the deployment's default
		// GitHub has it: the impl must resolve that to the default, which is
		// where the binding above landed.
		members, _, err := stores.Teams.ListMembers(ctx, teamID, "", jiraHost, db.Unwindowed)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		rows := byID(members)
		if got := rows[bound].GitHubUsername; got == nil || *got != "octocat" {
			t.Errorf("bound github_username = %v, want octocat", got)
		}
		if got := rows[bound].JiraAccountID; got == nil || *got != "acct-1" {
			t.Errorf("bound jira_account_id = %v, want acct-1", got)
		}
		if rows[unbound].GitHubUsername != nil || rows[unbound].JiraAccountID != nil {
			t.Errorf("unbound member reads %v / %v, want nil / nil (Not connected)",
				rows[unbound].GitHubUsername, rows[unbound].JiraAccountID)
		}
	})

	t.Run("ListMembers_IdentityIsHostScoped", func(t *testing.T) {
		stores, seed, teamID := team(t)
		u := seed.User(t, "Enterprise")
		seed.Membership(t, u, teamID, "admin")
		// Bound on a GHES host and a Jira site the org does not use.
		if err := stores.Users.UpsertGitHubIdentity(ctx, u, "https://ghe.corp.example", "corp-login", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := stores.Users.UpsertJiraIdentity(ctx, u, "https://other.atlassian.net", "acct-other", "Enterprise", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}

		members, _, err := stores.Teams.ListMembers(ctx, teamID, "", "", db.Unwindowed)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		if len(members) != 1 {
			t.Fatalf("members = %d, want 1", len(members))
		}
		if members[0].GitHubUsername != nil {
			t.Errorf("github_username = %v, want nil (bound on another host)", *members[0].GitHubUsername)
		}
		if members[0].JiraAccountID != nil {
			t.Errorf("jira_account_id = %v, want nil (unset jira host matches nothing)", *members[0].JiraAccountID)
		}
	})

	t.Run("ListMembers_ExcludesOtherTeams", func(t *testing.T) {
		stores, seed, teamID := team(t)
		mine := seed.User(t, "Mine")
		theirs := seed.User(t, "Theirs")
		seed.Membership(t, mine, teamID, "admin")
		other := seed.Team(t, seed.Org(t, theirs))
		seed.Membership(t, theirs, other, "admin")

		members, total, err := stores.Teams.ListMembers(ctx, teamID, "", "", db.Unwindowed)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		if total != 1 || len(members) != 1 || members[0].UserID != mine {
			t.Fatalf("ListMembers = %v (total %d), want only %s", members, total, mine)
		}
	})

	t.Run("ListMembers_N1SyntheticMember", func(t *testing.T) {
		// The shape local mode answers with: one implicit user, enrolled on
		// the sole team as an admin, with whatever identities they captured.
		// It runs on both backends because the row is real in both — the
		// local tenant seed writes exactly this membership.
		stores, seed, teamID := team(t)
		you := seed.User(t, "You")
		seed.Membership(t, you, teamID, "admin")
		if err := stores.Users.UpsertGitHubIdentity(ctx, you, ghHost, "solo-dev", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := stores.Users.UpsertJiraIdentity(ctx, you, jiraHost, "acct-solo", "You", "pat"); err != nil {
			t.Fatalf("UpsertJiraIdentity: %v", err)
		}

		members, total, err := stores.Teams.ListMembers(ctx, teamID, "", jiraHost,
			db.ListOpts{Limit: 200, Offset: 0})
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		if total != 1 || len(members) != 1 {
			t.Fatalf("ListMembers = %d rows (total %d), want 1 / 1", len(members), total)
		}
		m := members[0]
		if m.UserID != you {
			t.Errorf("user_id = %s, want %s", m.UserID, you)
		}
		if m.DisplayName != "You" {
			t.Errorf("display_name = %q, want You", m.DisplayName)
		}
		if m.Role != "admin" {
			t.Errorf("role = %q, want admin (the sole user admins their team)", m.Role)
		}
		if m.GitHubUsername == nil || *m.GitHubUsername != "solo-dev" {
			t.Errorf("github_username = %v, want solo-dev", m.GitHubUsername)
		}
		if m.JiraAccountID == nil || *m.JiraAccountID != "acct-solo" {
			t.Errorf("jira_account_id = %v, want acct-solo", m.JiraAccountID)
		}
	})
	t.Run("MemberIDsSystem_EveryRole_OrgScoped", func(t *testing.T) {
		// The websocket audience for a private-root knowledge change: every
		// member of the team, whatever their role, and nobody outside it.
		stores, seed := mk(t)
		owner := seed.User(t, "owner")
		org := seed.Org(t, owner)
		teamID := seed.Team(t, org)

		admin := seed.User(t, "Admin")
		member := seed.User(t, "Member")
		// A viewer reads the KB page too, so a stale listing is exactly what
		// excluding them would produce.
		viewer := seed.User(t, "Viewer")
		seed.Membership(t, admin, teamID, "admin")
		seed.Membership(t, member, teamID, "member")
		seed.Membership(t, viewer, teamID, "viewer")

		sibling := seed.Team(t, org)
		outsider := seed.User(t, "Outsider")
		seed.Membership(t, outsider, sibling, "admin")

		got, err := stores.Teams.MemberIDsSystem(ctx, org, teamID)
		if err != nil {
			t.Fatalf("MemberIDsSystem: %v", err)
		}
		assertSameSet(t, "MemberIDsSystem", got, []string{admin, member, viewer})

		// The org id is checked in the WHERE clause, so a real team id read
		// under the wrong tenant resolves to nobody rather than to its own
		// members.
		otherOrg := seed.Org(t, seed.User(t, "elsewhere"))
		crossed, err := stores.Teams.MemberIDsSystem(ctx, otherOrg, teamID)
		if err != nil {
			t.Fatalf("MemberIDsSystem(cross-org): %v", err)
		}
		if len(crossed) != 0 {
			t.Errorf("MemberIDsSystem(cross-org) = %v; want empty", crossed)
		}
	})

	t.Run("MemberIDsSystem_EmptyTeam", func(t *testing.T) {
		stores, seed := mk(t)
		org := seed.Org(t, seed.User(t, "owner"))
		got, err := stores.Teams.MemberIDsSystem(ctx, org, seed.Team(t, org))
		if err != nil {
			t.Fatalf("MemberIDsSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("MemberIDsSystem(empty team) = %v; want empty slice", got)
		}
	})
}
