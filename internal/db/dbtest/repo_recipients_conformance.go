package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// RepoRecipientsSeeder stages the tenancy, role, and tracking rows the
// RepoUpdateRecipientsSystem suite needs. Row creation for users / orgs /
// teams / memberships has no store method (a provisioning concern), so
// each backend implements the seeder against its own schema — the
// harness only consumes the returned IDs. Tracking rows are seeded
// directly too: the suite pins the read, not ReplaceForTeam's write path
// (which has its own conformance cases).
type RepoRecipientsSeeder struct {
	// User inserts a user row and returns its ID.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its ID.
	Org func(t *testing.T, ownerID string) string

	// Team inserts a team row in orgID and returns its ID.
	Team func(t *testing.T, orgID string) string

	// TeamMembership enrolls userID on teamID (team-level memberships).
	TeamMembership func(t *testing.T, userID, teamID string)

	// OrgMembership enrolls userID in orgID with the given org role
	// ('owner' | 'admin' | 'member').
	OrgMembership func(t *testing.T, userID, orgID, role string)

	// TrackRepo inserts a team_github_repos row for teamID.
	TrackRepo func(t *testing.T, teamID, owner, repo string)

	// ArchiveTeam soft-deletes teamID (sets teams.deleted_at).
	ArchiveTeam func(t *testing.T, teamID string)
}

// RepoRecipientsFactory hands the suite a wired TeamGitHubReposStore plus
// the seeder. Each call returns a fresh, isolated backend.
type RepoRecipientsFactory func(t *testing.T) (db.TeamGitHubReposStore, RepoRecipientsSeeder)

// RunRepoRecipientsConformance is the shared assertion suite for
// TeamGitHubReposStore.RepoUpdateRecipientsSystem — the websocket
// delivery scope for repository_updated events. It pins the visibility
// contract the method mirrors from the REST read (handleRepositories):
// org admins/owners always receive; members receive only through a live
// team that tracks the repo; nobody else does. Both backends run the
// identical subtests so SQLite and Postgres pin to one result.
func RunRepoRecipientsConformance(t *testing.T, mk RepoRecipientsFactory) {
	t.Helper()
	ctx := context.Background()

	recipients := func(t *testing.T, s db.TeamGitHubReposStore, orgID, owner, repo string) []string {
		t.Helper()
		got, err := s.RepoUpdateRecipientsSystem(ctx, orgID, owner, repo)
		if err != nil {
			t.Fatalf("RepoUpdateRecipientsSystem(%s, %s/%s): %v", orgID, owner, repo, err)
		}
		return got
	}

	t.Run("RepoRecipients_TrackingTeamMemberIn_NonTrackingOut", func(t *testing.T) {
		// The core disclosure boundary: a member of a same-org team that
		// does NOT track the repo must not receive its events — that is
		// exactly the client the acceptance criterion says gets nothing.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		teamA := seed.Team(t, org)
		teamB := seed.Team(t, org)
		userA := seed.User(t)
		userB := seed.User(t)
		seed.OrgMembership(t, userA, org, "member")
		seed.OrgMembership(t, userB, org, "member")
		seed.TeamMembership(t, userA, teamA)
		seed.TeamMembership(t, userB, teamB)
		seed.TrackRepo(t, teamA, "Acme", "api")

		assertSameSet(t, "recipients(tracked repo)", recipients(t, s, org, "Acme", "api"), []string{userA})
		// Case-insensitive on both fields, matching TracksRepoSystem.
		assertSameSet(t, "recipients(case-folded)", recipients(t, s, org, "acme", "API"), []string{userA})
		// An untracked repo in the same org reaches nobody (no admins here).
		if got := recipients(t, s, org, "acme", "web"); len(got) != 0 {
			t.Errorf("recipients(untracked repo) = %v; want empty slice", got)
		}
	})

	t.Run("RepoRecipients_OrgAdminsAlwaysIn_PlainMembersOut", func(t *testing.T) {
		// Org admins/owners see the org-wide union on the REST read, so
		// they receive events for every repo — teamless or not. An org
		// 'member' with no tracking team gets nothing.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		seed.OrgMembership(t, founder, org, "owner")
		admin := seed.User(t)
		seed.OrgMembership(t, admin, org, "admin")
		plain := seed.User(t)
		seed.OrgMembership(t, plain, org, "member")
		tracker := seed.User(t)
		seed.OrgMembership(t, tracker, org, "member")
		seed.TeamMembership(t, tracker, team)
		seed.TrackRepo(t, team, "acme", "api")

		assertSameSet(t, "recipients(admins+tracker)",
			recipients(t, s, org, "acme", "api"), []string{founder, admin, tracker})
	})

	t.Run("RepoRecipients_AdminWhoAlsoTracks_AppearsOnce", func(t *testing.T) {
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		seed.OrgMembership(t, founder, org, "admin")
		seed.TeamMembership(t, founder, team)
		seed.TrackRepo(t, team, "acme", "api")

		got := recipients(t, s, org, "acme", "api")
		if len(got) != 1 || got[0] != founder {
			t.Errorf("recipients(admin who also tracks) = %v; want exactly [%s]", got, founder)
		}
	})

	t.Run("RepoRecipients_ArchivedTeamExcluded", func(t *testing.T) {
		// Archiving force-stops a team; its members must not keep
		// receiving repo events through it — parity with
		// TeamIDsForUserInOrgSystem's deleted_at filter.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		member := seed.User(t)
		seed.OrgMembership(t, member, org, "member")
		seed.TeamMembership(t, member, team)
		seed.TrackRepo(t, team, "acme", "api")

		assertSameSet(t, "recipients(live team)", recipients(t, s, org, "acme", "api"), []string{member})
		seed.ArchiveTeam(t, team)
		if got := recipients(t, s, org, "acme", "api"); len(got) != 0 {
			t.Errorf("recipients(archived team) = %v; want empty slice", got)
		}
	})

	t.Run("RepoRecipients_OrgScoped", func(t *testing.T) {
		// Two orgs can track the same (owner, repo) slug; each org's
		// emission must resolve only its own users — neither the other
		// org's tracking members nor its admins.
		s, seed := mk(t)
		founder1 := seed.User(t)
		org1 := seed.Org(t, founder1)
		team1 := seed.Team(t, org1)
		user1 := seed.User(t)
		seed.OrgMembership(t, user1, org1, "member")
		seed.TeamMembership(t, user1, team1)
		seed.TrackRepo(t, team1, "acme", "api")

		founder2 := seed.User(t)
		org2 := seed.Org(t, founder2)
		team2 := seed.Team(t, org2)
		seed.OrgMembership(t, founder2, org2, "owner")
		user2 := seed.User(t)
		seed.OrgMembership(t, user2, org2, "member")
		seed.TeamMembership(t, user2, team2)
		seed.TrackRepo(t, team2, "acme", "api")

		assertSameSet(t, "recipients(org1)", recipients(t, s, org1, "acme", "api"), []string{user1})
		assertSameSet(t, "recipients(org2)", recipients(t, s, org2, "acme", "api"), []string{founder2, user2})
	})
}
