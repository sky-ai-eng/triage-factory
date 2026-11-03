package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// OrgMemberListSeeder stages the tenancy, membership and sign-in identity rows
// the org roster suite reads. None of them has a store writer (they are auth
// and provisioning concerns), so each backend writes them against its own
// schema and the suite consumes the returned ids.
type OrgMemberListSeeder struct {
	// User inserts a user row with displayName and returns its id.
	User func(t *testing.T, displayName string) string
	// Org inserts an org owned by ownerID, with ownerID its owner member, and
	// returns its id.
	Org func(t *testing.T, ownerID string) string
	// OrgMember enrolls userID in orgID with the org role "member".
	OrgMember func(t *testing.T, userID, orgID string)
	// Team inserts a team in orgID and returns its id.
	Team func(t *testing.T, orgID string) string
	// TeamMember enrolls userID on teamID with the team role "member".
	TeamMember func(t *testing.T, userID, teamID string)
	// SignInEmail gives userID a sign-in identity holding email, verified or
	// not.
	SignInEmail func(t *testing.T, userID, email string, verified bool)
}

// OrgMemberListFactory returns a fresh, isolated backend's org memberships
// store and its seeder.
type OrgMemberListFactory func(t *testing.T) (db.OrgMembershipsStore, OrgMemberListSeeder)

// RunOrgMemberListConformance pins OrgMembershipsStore.ListWithIdentity's
// filters: the display-name search, the email arm and the verified-only rule
// it keeps, the literal treatment of LIKE wildcards in a needle, the team
// exclusion, and that the total and the window follow the filter while the
// order stays display name then user id.
//
// Postgres runs it. The org roster is a multi-mode surface, and the SQLite
// impl refuses the read with ErrNotApplicableInLocal rather than answering
// for an org that local mode does not have.
func RunOrgMemberListConformance(t *testing.T, mk OrgMemberListFactory) {
	t.Helper()
	ctx := context.Background()

	list := func(t *testing.T, store db.OrgMembershipsStore, orgID string, f db.OrgMemberFilter, opts db.ListOpts) ([]string, int) {
		t.Helper()
		members, total, err := store.ListWithIdentity(ctx, orgID, "", "", "", f, opts)
		if err != nil {
			t.Fatalf("ListWithIdentity(%+v): %v", f, err)
		}
		out := make([]string, len(members))
		for i, m := range members {
			out[i] = m.UserID
		}
		return out, total
	}
	expect := func(t *testing.T, label string, got []string, total int, want ...string) {
		t.Helper()
		if total != len(want) {
			t.Errorf("%s total = %d, want %d (counted under the filter)", label, total, len(want))
		}
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v (display-name order)", label, got, want)
			}
		}
	}

	t.Run("SearchMatchesDisplayName", func(t *testing.T) {
		store, seed := mk(t)
		owner := seed.User(t, "Zed Owner")
		org := seed.Org(t, owner)
		alice := seed.User(t, "Alice Anders")
		alan := seed.User(t, "Alan")
		bob := seed.User(t, "Bob")
		for _, u := range []string{alice, alan, bob} {
			seed.OrgMember(t, u, org)
		}

		got, total := list(t, store, org, db.OrgMemberFilter{Search: "AL"}, db.Unwindowed)
		expect(t, "search AL", got, total, alan, alice)
		got, total = list(t, store, org, db.OrgMemberFilter{Search: " bob "}, db.Unwindowed)
		expect(t, "search bob", got, total, bob)
		got, total = list(t, store, org, db.OrgMemberFilter{}, db.Unwindowed)
		expect(t, "no search", got, total, alan, alice, bob, owner)

		// The window cuts the searched set; the total stays its count.
		got, total = list(t, store, org, db.OrgMemberFilter{Search: "al"}, db.ListOpts{Limit: 1, Offset: 1})
		if len(got) != 1 || got[0] != alice || total != 2 {
			t.Errorf("search al, second page = %v / %d, want [%s] / 2", got, total, alice)
		}
		got, total = list(t, store, org, db.OrgMemberFilter{Search: "al"}, db.ListOpts{CountOnly: true})
		if len(got) != 0 || total != 2 {
			t.Errorf("search al, count only = %v / %d, want no rows / 2", got, total)
		}
	})

	t.Run("SearchMatchesVerifiedEmailsOnlyWhenAsked", func(t *testing.T) {
		store, seed := mk(t)
		owner := seed.User(t, "Owner")
		org := seed.Org(t, owner)
		bob := seed.User(t, "Bob")
		dan := seed.User(t, "Dan")
		outsider := seed.User(t, "Outsider")
		seed.OrgMember(t, bob, org)
		seed.OrgMember(t, dan, org)
		seed.SignInEmail(t, bob, "bob@corp.example", true)
		// Unverified: an identity can claim any address, so it is not one the
		// search may find the claimant by.
		seed.SignInEmail(t, dan, "dan@corp.example", false)
		// Verified, but not a member of this org.
		seed.SignInEmail(t, outsider, "outsider@corp.example", true)

		got, total := list(t, store, org, db.OrgMemberFilter{Search: "corp.example"}, db.Unwindowed)
		expect(t, "search an address, emails not asked for", got, total)
		got, total = list(t, store, org, db.OrgMemberFilter{Search: "CORP.example", SearchEmails: true}, db.Unwindowed)
		expect(t, "search an address", got, total, bob)
		// The email arm sits beside the display name, not in place of it.
		got, total = list(t, store, org, db.OrgMemberFilter{Search: "dan", SearchEmails: true}, db.Unwindowed)
		expect(t, "search a name with emails on", got, total, dan)
	})

	t.Run("SearchIsLiteral", func(t *testing.T) {
		store, seed := mk(t)
		owner := seed.User(t, "Owner")
		org := seed.Org(t, owner)
		percent := seed.User(t, "100% done")
		digits := seed.User(t, "1000 done")
		under := seed.User(t, "Under")
		other := seed.User(t, "Other")
		for _, u := range []string{percent, digits, under, other} {
			seed.OrgMember(t, u, org)
		}
		seed.SignInEmail(t, under, "a_b@x.example", true)
		seed.SignInEmail(t, other, "axb@x.example", true)

		got, total := list(t, store, org, db.OrgMemberFilter{Search: "100%"}, db.Unwindowed)
		expect(t, "search 100%", got, total, percent)
		got, total = list(t, store, org, db.OrgMemberFilter{Search: "%"}, db.Unwindowed)
		expect(t, "search %", got, total, percent)
		got, total = list(t, store, org, db.OrgMemberFilter{Search: "a_b", SearchEmails: true}, db.Unwindowed)
		expect(t, "search a_b", got, total, under)
	})

	t.Run("ExcludingTeam", func(t *testing.T) {
		store, seed := mk(t)
		owner := seed.User(t, "Owner")
		org := seed.Org(t, owner)
		team := seed.Team(t, org)
		sibling := seed.Team(t, org)
		alice := seed.User(t, "Alice")
		bob := seed.User(t, "Bob")
		carol := seed.User(t, "Carol")
		for _, u := range []string{alice, bob, carol} {
			seed.OrgMember(t, u, org)
		}
		seed.TeamMember(t, alice, team)
		seed.TeamMember(t, owner, team)
		// On another team only: still someone this team could add.
		seed.TeamMember(t, bob, sibling)

		got, total := list(t, store, org, db.OrgMemberFilter{ExcludingTeamID: team}, db.Unwindowed)
		expect(t, "excluding the team", got, total, bob, carol)
		got, total = list(t, store, org, db.OrgMemberFilter{ExcludingTeamID: team, Search: "o"}, db.Unwindowed)
		expect(t, "excluding the team, searching o", got, total, bob, carol)
		got, total = list(t, store, org, db.OrgMemberFilter{ExcludingTeamID: team, Search: "alice"}, db.Unwindowed)
		expect(t, "excluding the team, searching a member of it", got, total)
	})
}
