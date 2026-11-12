package dbtest

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ConversationFrameRecipientsSeeder stages the tenancy, membership, and
// conversation rows the ConversationFrameRecipientsSystem suite needs. None
// of them has a store method that writes them alone, so each backend
// implements the seeder against its own schema and the harness consumes
// only the returned ids.
type ConversationFrameRecipientsSeeder struct {
	// User inserts a user row and returns its id.
	User func(t *testing.T) string

	// Org inserts an org row owned by ownerID and returns its id.
	Org func(t *testing.T, ownerID string) string

	// Team inserts a team row in orgID and returns its id.
	Team func(t *testing.T, orgID string) string

	// OrgMembership enrolls userID in orgID with the given org role
	// ('owner' | 'admin' | 'member').
	OrgMembership func(t *testing.T, userID, orgID, role string)

	// RemoveOrgMembership deletes userID's org_memberships row in orgID and
	// nothing else.
	RemoveOrgMembership func(t *testing.T, userID, orgID string)

	// TeamMembership enrolls userID on teamID with the given team role
	// ('admin' | 'member' | 'viewer').
	TeamMembership func(t *testing.T, userID, teamID, role string)

	// RemoveTeamMembership deletes userID's memberships row on teamID.
	RemoveTeamMembership func(t *testing.T, userID, teamID string)

	// ArchiveTeam soft-deletes teamID (sets teams.deleted_at).
	ArchiveTeam func(t *testing.T, teamID string)

	// Conversation inserts a manual conversation in orgID on teamID, created
	// by creatorID, with the given visibility, and returns its id.
	Conversation func(t *testing.T, orgID, teamID, creatorID, visibility string) string
}

// ConversationFrameRecipientsFactory hands the suite a wired
// ConversationStore plus the seeder. Each call returns a fresh, isolated
// backend.
type ConversationFrameRecipientsFactory func(t *testing.T) (db.ConversationStore, ConversationFrameRecipientsSeeder)

// RunConversationFrameRecipientsConformance is the shared assertion suite
// for ConversationStore.ConversationFrameRecipientsSystem, the delivery
// scope of the websocket frames about one conversation. It pins the method
// to conversations_select, the REST read's visibility, so a frame reaches
// exactly the people who could read the row it describes. Both backends
// run the identical subtests so SQLite and Postgres pin to one result.
func RunConversationFrameRecipientsConformance(t *testing.T, mk ConversationFrameRecipientsFactory) {
	t.Helper()
	ctx := context.Background()

	recipients := func(t *testing.T, s db.ConversationStore, orgID, conversationID string) (bool, []string) {
		t.Helper()
		orgWide, got, err := s.ConversationFrameRecipientsSystem(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("ConversationFrameRecipientsSystem(%s, %s): %v", orgID, conversationID, err)
		}
		return orgWide, got
	}
	// assertUsers checks the answer is a team or private audience of
	// exactly want, sorted ascending, and never nil.
	assertUsers := func(t *testing.T, label string, orgWide bool, got []string, want ...string) {
		t.Helper()
		if orgWide {
			t.Errorf("%s: orgWide = true; want a recipient list", label)
		}
		if got == nil {
			t.Errorf("%s: userIDs = nil; want a non-nil slice", label)
		}
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: userIDs = %v; want %v, sorted ascending", label, got, want)
		}
	}

	t.Run("TeamConversation_TeamMembersOnly", func(t *testing.T) {
		// The disclosure boundary: a team-visible conversation reaches every
		// member of its team, a viewer included because the read checks no
		// role, and nobody else in the org. An org admin who is not on the
		// team is left out because the read has no admin arm.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		teamA := seed.Team(t, org)
		teamB := seed.Team(t, org)
		member := seed.User(t)
		viewer := seed.User(t)
		otherTeam := seed.User(t)
		admin := seed.User(t)
		seed.OrgMembership(t, member, org, "member")
		seed.OrgMembership(t, viewer, org, "member")
		seed.OrgMembership(t, otherTeam, org, "member")
		seed.OrgMembership(t, admin, org, "admin")
		seed.TeamMembership(t, member, teamA, "member")
		seed.TeamMembership(t, viewer, teamA, "viewer")
		seed.TeamMembership(t, otherTeam, teamB, "member")
		conv := seed.Conversation(t, org, teamA, member, "team")

		orgWide, got := recipients(t, s, org, conv)
		assertUsers(t, "team conversation", orgWide, got, member, viewer)
	})

	t.Run("PrivateConversation_CreatorOnly", func(t *testing.T) {
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		creator := seed.User(t)
		teammate := seed.User(t)
		seed.OrgMembership(t, creator, org, "member")
		seed.OrgMembership(t, teammate, org, "member")
		seed.TeamMembership(t, creator, team, "member")
		seed.TeamMembership(t, teammate, team, "member")
		conv := seed.Conversation(t, org, team, creator, "private")

		orgWide, got := recipients(t, s, org, conv)
		assertUsers(t, "private conversation", orgWide, got, creator)
	})

	t.Run("OrgConversation_OrgWide", func(t *testing.T) {
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		creator := seed.User(t)
		seed.OrgMembership(t, creator, org, "admin")
		seed.TeamMembership(t, creator, team, "member")
		conv := seed.Conversation(t, org, team, creator, "org")

		orgWide, got := recipients(t, s, org, conv)
		if !orgWide {
			t.Errorf("org conversation: orgWide = false, userIDs = %v; want orgWide = true", got)
		}
	})

	t.Run("DeletedTeam_Nobody", func(t *testing.T) {
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		member := seed.User(t)
		seed.OrgMembership(t, member, org, "member")
		seed.TeamMembership(t, member, team, "member")
		conv := seed.Conversation(t, org, team, member, "team")
		seed.ArchiveTeam(t, team)

		orgWide, got := recipients(t, s, org, conv)
		assertUsers(t, "conversation on a deleted team", orgWide, got)
	})

	t.Run("MissingConversation_Nobody", func(t *testing.T) {
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)

		orgWide, got := recipients(t, s, org, uuid.NewString())
		assertUsers(t, "missing conversation", orgWide, got)
	})

	t.Run("LeftOrg_OutOfAudience", func(t *testing.T) {
		// The read requires org membership on top of each arm, so a user
		// whose org membership is gone reads neither a private conversation
		// they created nor their team's conversation, whatever rows they
		// still hold. The founder owns the org and the member who stays is
		// the team's admin, because Postgres refuses to leave an org without
		// an owner or a team without an admin.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		seed.OrgMembership(t, founder, org, "owner")
		team := seed.Team(t, org)
		stays := seed.User(t)
		leaves := seed.User(t)
		seed.OrgMembership(t, stays, org, "member")
		seed.OrgMembership(t, leaves, org, "member")
		seed.TeamMembership(t, stays, team, "admin")
		seed.TeamMembership(t, leaves, team, "member")
		teamConv := seed.Conversation(t, org, team, stays, "team")
		privateConv := seed.Conversation(t, org, team, leaves, "private")
		seed.RemoveOrgMembership(t, leaves, org)

		orgWide, got := recipients(t, s, org, teamConv)
		assertUsers(t, "team conversation", orgWide, got, stays)
		orgWide, got = recipients(t, s, org, privateConv)
		assertUsers(t, "private conversation of a creator who left", orgWide, got)
	})

	t.Run("RemovedMember_LeavesNextAnswer", func(t *testing.T) {
		// The answer is read fresh, so a membership removed between two
		// frames is out of the second one's audience. The member who stays
		// is the team's admin, because Postgres refuses a removal that
		// leaves a team without one.
		s, seed := mk(t)
		founder := seed.User(t)
		org := seed.Org(t, founder)
		team := seed.Team(t, org)
		stays := seed.User(t)
		leaves := seed.User(t)
		seed.OrgMembership(t, stays, org, "member")
		seed.OrgMembership(t, leaves, org, "member")
		seed.TeamMembership(t, stays, team, "admin")
		seed.TeamMembership(t, leaves, team, "member")
		conv := seed.Conversation(t, org, team, stays, "team")

		orgWide, got := recipients(t, s, org, conv)
		assertUsers(t, "before removal", orgWide, got, stays, leaves)
		seed.RemoveTeamMembership(t, leaves, team)
		orgWide, got = recipients(t, s, org, conv)
		assertUsers(t, "after removal", orgWide, got, stays)
	})
}
