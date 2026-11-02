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

	// TrackRepo inserts a team_github_repos row for teamID referencing the
	// registry row for owner/repo on host, minting that row if absent, and
	// returns the registry row's id.
	TrackRepo func(t *testing.T, teamID, host, owner, repo string) string

	// RenameRepo renames the registry row repositoryID to owner/repo and
	// moves nothing else (dbseed.RenameRepositoryOnly).
	RenameRepo func(t *testing.T, repositoryID, owner, repo string)

	// RetireRepo sets the registry row's retired_at.
	RetireRepo func(t *testing.T, repositoryID string)

	// ArchiveTeam soft-deletes teamID (sets teams.deleted_at).
	ArchiveTeam func(t *testing.T, teamID string)

	// RestoreTeam clears teamID's teams.deleted_at.
	RestoreTeam func(t *testing.T, teamID string)
}

// RepoRecipientsFactory hands the suite a wired TeamGitHubReposStore plus
// the seeder. Each call returns a fresh, isolated backend.
type RepoRecipientsFactory func(t *testing.T) (db.TeamGitHubReposStore, RepoRecipientsSeeder)

// RunRepoRecipientsConformance is the shared assertion suite for
// TeamGitHubReposStore.RepoUpdateRecipientsSystem — the websocket
// delivery scope for repository_updated events. It pins the visibility
// contract the method mirrors from the REST read (handleRepositories):
// org admins/owners always receive; members receive only through a live team
// that tracks the registry row (an archived team tracks nothing there, as in
// the REST read); nobody else does. The row is named by its id, so a rename moves nothing
// and a same-named row on another host is another repository. Both
// backends run the identical subtests so SQLite and Postgres pin to one
// result.
func RunRepoRecipientsConformance(t *testing.T, mk RepoRecipientsFactory) {
	t.Helper()
	ctx := context.Background()

	recipients := func(t *testing.T, s db.TeamGitHubReposStore, orgID, repositoryID string) []string {
		t.Helper()
		got, err := s.RepoUpdateRecipientsSystem(ctx, orgID, repositoryID)
		if err != nil {
			t.Fatalf("RepoUpdateRecipientsSystem(%s, %s): %v", orgID, repositoryID, err)
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
		api := seed.TrackRepo(t, teamA, TestGitHubHost, "Acme", "api")
		web := seed.TrackRepo(t, teamB, TestGitHubHost, "acme", "web")

		assertSameSet(t, "recipients(api)", recipients(t, s, org, api), []string{userA})
		assertSameSet(t, "recipients(web)", recipients(t, s, org, web), []string{userB})
		// An id naming no registry row reaches nobody (no admins here).
		if got := recipients(t, s, org, "00000000-0000-0000-0000-000000000000"); len(got) != 0 {
			t.Errorf("recipients(no such row) = %v; want empty slice", got)
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
		api := seed.TrackRepo(t, team, TestGitHubHost, "acme", "api")

		assertSameSet(t, "recipients(admins+tracker)",
			recipients(t, s, org, api), []string{founder, admin, tracker})
	})

	t.Run("RepoRecipients_AdminWhoAlsoTracks_AppearsOnce", func(t *testing.T) {
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		seed.OrgMembership(t, founder, org, "admin")
		seed.TeamMembership(t, founder, team)
		api := seed.TrackRepo(t, team, TestGitHubHost, "acme", "api")

		got := recipients(t, s, org, api)
		if len(got) != 1 || got[0] != founder {
			t.Errorf("recipients(admin who also tracks) = %v; want exactly [%s]", got, founder)
		}
	})

	t.Run("RepoRecipients_ArchivedTeamLeavesUntilRestored", func(t *testing.T) {
		// Each team tracks one repository. An archived team tracks nothing in
		// the REST read's scoping (repoTrackedByATeam), so its member no longer
		// sees its repository on GET /api/repos and leaves that repository's
		// audience; the live team's audience is untouched, and a restore puts
		// the member back.
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
		api := seed.TrackRepo(t, teamA, TestGitHubHost, "acme", "api")
		web := seed.TrackRepo(t, teamB, TestGitHubHost, "acme", "web")

		assertSameSet(t, "recipients(web, live team)", recipients(t, s, org, web), []string{userB})
		seed.ArchiveTeam(t, teamB)
		if got := recipients(t, s, org, web); len(got) != 0 {
			t.Errorf("recipients(web, archived team) = %v; want empty slice", got)
		}
		assertSameSet(t, "recipients(api, other team archived)", recipients(t, s, org, api), []string{userA})
		seed.RestoreTeam(t, teamB)
		assertSameSet(t, "recipients(web, restored team)", recipients(t, s, org, web), []string{userB})
	})

	t.Run("RepoRecipients_OrgScoped", func(t *testing.T) {
		// Two orgs can track the same (owner, repo) slug; each org's
		// emission must resolve only its own users — neither the other
		// org's tracking members nor its admins, even when it names the
		// other org's row.
		s, seed := mk(t)
		founder1 := seed.User(t)
		org1 := seed.Org(t, founder1)
		team1 := seed.Team(t, org1)
		user1 := seed.User(t)
		seed.OrgMembership(t, user1, org1, "member")
		seed.TeamMembership(t, user1, team1)
		api1 := seed.TrackRepo(t, team1, TestGitHubHost, "acme", "api")

		founder2 := seed.User(t)
		org2 := seed.Org(t, founder2)
		team2 := seed.Team(t, org2)
		seed.OrgMembership(t, founder2, org2, "owner")
		user2 := seed.User(t)
		seed.OrgMembership(t, user2, org2, "member")
		seed.TeamMembership(t, user2, team2)
		api2 := seed.TrackRepo(t, team2, TestGitHubHost, "acme", "api")

		assertSameSet(t, "recipients(org1)", recipients(t, s, org1, api1), []string{user1})
		assertSameSet(t, "recipients(org2)", recipients(t, s, org2, api2), []string{founder2, user2})
		if got := recipients(t, s, org1, api2); len(got) != 0 {
			t.Errorf("recipients(org1, org2's row) = %v; want empty slice", got)
		}
	})

	t.Run("RepoRecipients_HostScoped", func(t *testing.T) {
		// One owner/repo on two GitHub hosts is two repositories, two
		// registry rows. A member whose team tracks it on one host is not
		// told about the other's updates; org admins are in on every
		// host, as they are for every repository.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		seed.OrgMembership(t, founder, org, "owner")
		dotcomTeam := seed.Team(t, org)
		gheTeam := seed.Team(t, org)
		dotcomUser := seed.User(t)
		gheUser := seed.User(t)
		seed.OrgMembership(t, dotcomUser, org, "member")
		seed.OrgMembership(t, gheUser, org, "member")
		seed.TeamMembership(t, dotcomUser, dotcomTeam)
		seed.TeamMembership(t, gheUser, gheTeam)
		dotcom := seed.TrackRepo(t, dotcomTeam, TestGitHubHost, "acme", "api")
		ghe := seed.TrackRepo(t, gheTeam, TestOtherGitHubHost, "acme", "api")

		assertSameSet(t, "recipients(github.com)", recipients(t, s, org, dotcom), []string{founder, dotcomUser})
		assertSameSet(t, "recipients(ghe)", recipients(t, s, org, ghe), []string{founder, gheUser})
	})

	t.Run("RepoRecipients_FollowTheRowThroughARenameAndARetire", func(t *testing.T) {
		// The event names the row by id, so a rename between the write that
		// emitted it and this read keeps its audience. GET /api/repos leaves a
		// retired row out, so its tracking teams' members are not its
		// audience; org admins stay in, as they are for every row.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		seed.OrgMembership(t, founder, org, "owner")
		team := seed.Team(t, org)
		member := seed.User(t)
		seed.OrgMembership(t, member, org, "member")
		seed.TeamMembership(t, member, team)
		api := seed.TrackRepo(t, team, TestGitHubHost, "acme", "api")

		seed.RenameRepo(t, api, "acme", "api-v2")
		assertSameSet(t, "recipients(renamed row)", recipients(t, s, org, api), []string{founder, member})
		seed.RetireRepo(t, api)
		assertSameSet(t, "recipients(retired row)", recipients(t, s, org, api), []string{founder})
	})
}
