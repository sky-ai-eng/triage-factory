package postgres_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTeamPRStore_Postgres runs the shared conformance suite against the
// Postgres impl. Wired against AdminDB like the team activity suite: the
// population is subject-team-scoped rather than viewer-scoped, so the read's
// answer must not depend on the auth path — the RLS arm that hides another
// team's tracked set from a non-member is the app pool's, and the handler
// tests own it.
func TestTeamPRStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTeamPRStoreConformance(t, func(t *testing.T) (db.TeamPRStore, string, string, string, dbtest.TeamPRSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, _ := seedPgOrgAndUserForDashboard(t, h)
		teamID := firstTeamForOrg(t, h, orgID)
		// github_base_url unset, the common case: the store resolves it to
		// the public host, which is where a login-claim binding lands.
		return stores.TeamPRs, orgID, teamID, "", newPgTeamPRSeeder(h, orgID, teamID)
	})

	// The tracked set is the outer filter, and local mode has no untracked
	// repo to be outside of — so this is the half of the population rule only
	// multi can state: a member's own pull request in a repo nobody attached
	// to the team is not the team's work.
	t.Run("tracked_set_is_the_outer_filter", func(t *testing.T) {
		h.Reset(t)
		orgID, _ := seedPgOrgAndUserForDashboard(t, h)
		teamID := firstTeamForOrg(t, h, orgID)
		seed := newPgTeamPRSeeder(h, orgID, teamID)
		seed.Member(t, "member-a")

		seed.PR(t, dbtest.TeamPRFixture{Snapshot: domain.PRSnapshot{
			Number: 1, Author: "member-a", State: "OPEN", Repo: "tf-test/tracked",
		}})
		seedPgUntrackedTeamPR(t, h, orgID, domain.PRSnapshot{
			Number: 2, Author: "member-a", State: "OPEN", Repo: "tf-test/untracked",
		}, "")
		// Even the owning-team leg sits inside the tracked set: a stamp is
		// about whose work it is, not about widening what the team can see.
		seedPgUntrackedTeamPR(t, h, orgID, domain.PRSnapshot{
			Number: 3, Author: "tf-bot", State: "OPEN", Repo: "tf-test/untracked-owned",
		}, teamID)

		prs, total, err := stores.TeamPRs.TeamPRs(t.Context(), orgID, teamID, "", db.PRListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("TeamPRs: %v", err)
		}
		if len(prs) != 1 || total != 1 || prs[0].Number != 1 {
			t.Fatalf("got %+v (total %d), want exactly #1 — the untracked repos are outside the team's set",
				prs, total)
		}
	})

	// Entities are org-wide, so the org_id predicate is the only thing
	// keeping another tenant's pull requests out of this team's list. Nothing
	// else can assert it: SQLite is N=1 against a single sentinel org.
	t.Run("another_orgs_pull_requests_are_invisible", func(t *testing.T) {
		h.Reset(t)
		orgID, _ := seedPgOrgAndUserForDashboard(t, h)
		teamID := firstTeamForOrg(t, h, orgID)
		seed := newPgTeamPRSeeder(h, orgID, teamID)
		seed.Member(t, "member-a")
		seed.PR(t, dbtest.TeamPRFixture{Snapshot: domain.PRSnapshot{
			Number: 1, Author: "member-a", State: "OPEN", Repo: "tf-test/mine",
		}})

		// A second tenant whose team tracks a repo of its own, with a pull
		// request by a login that happens to match ours.
		otherOrg, _ := seedPgOrgAndUserForDashboard(t, h)
		otherTeam := firstTeamForOrg(t, h, otherOrg)
		otherSeed := newPgTeamPRSeeder(h, otherOrg, otherTeam)
		otherSeed.Member(t, "member-a")
		otherSeed.PR(t, dbtest.TeamPRFixture{Snapshot: domain.PRSnapshot{
			Number: 99, Author: "member-a", State: "OPEN", Repo: "tf-test/theirs",
		}})

		prs, total, err := stores.TeamPRs.TeamPRs(t.Context(), orgID, teamID, "", db.PRListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("TeamPRs: %v", err)
		}
		if len(prs) != 1 || total != 1 || prs[0].Number != 1 {
			t.Fatalf("got %+v (total %d), want exactly #1 — cross-tenant leak", prs, total)
		}
	})
}

// newPgTeamPRSeeder stages the conformance fixtures. Every seeded pull request
// lands in a repo registered in the SUBJECT team's tracked set, since that is
// what makes an entity this team's to see at all; the untracked cases are
// seeded by seedPgUntrackedTeamPR instead.
func newPgTeamPRSeeder(h *pgtest.Harness, orgID, teamID string) dbtest.TeamPRSeeder {
	member := func(t *testing.T, name string) string {
		t.Helper()
		id := uuid.New().String()
		h.SeedAuthUser(t, id, fmt.Sprintf("team-prs-%s@test.local", id[:8]))
		if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, id, name); err != nil {
			t.Fatalf("seed user %s: %v", name, err)
		}
		if _, err := h.AdminDB.Exec(
			`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, id,
		); err != nil {
			t.Fatalf("seed org membership for %s: %v", name, err)
		}
		if _, err := h.AdminDB.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, id, teamID,
		); err != nil {
			t.Fatalf("seed team membership for %s: %v", name, err)
		}
		return id
	}
	return dbtest.TeamPRSeeder{
		Member: func(t *testing.T, login string) string {
			t.Helper()
			id := member(t, login)
			if _, err := h.AdminDB.Exec(`
				INSERT INTO user_github_identities (user_id, github_base_url, login, source)
				VALUES ($1, $2, $3, 'connect_oauth')
			`, id, db.EffectiveGitHubHost(""), login); err != nil {
				t.Fatalf("bind identity for %s: %v", login, err)
			}
			return id
		},
		MemberWithoutIdentity: member,
		OtherTeam: func(t *testing.T, name string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := h.AdminDB.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
				id, orgID, "seed-"+id[:8], name,
			); err != nil {
				t.Fatalf("seed team %s: %v", name, err)
			}
			return id
		},
		PR: func(t *testing.T, fx dbtest.TeamPRFixture) string {
			t.Helper()
			id := seedPgTeamPREntity(t, h, orgID, fx)
			trackPgTeamRepo(t, h, orgID, teamID, repoOf(fx.Snapshot))
			return id
		},
	}
}

// seedPgUntrackedTeamPR seeds a pull request whose repo is registered for the
// org but attached to no team — the "outside the tracked set" fixture.
func seedPgUntrackedTeamPR(t *testing.T, h *pgtest.Harness, orgID string, snap domain.PRSnapshot, owningTeam string) string {
	t.Helper()
	return seedPgTeamPREntity(t, h, orgID, dbtest.TeamPRFixture{Snapshot: snap, OwningTeam: owningTeam})
}

func seedPgTeamPREntity(t *testing.T, h *pgtest.Harness, orgID string, fx dbtest.TeamPRFixture) string {
	t.Helper()
	snap := fx.Snapshot
	if snap.Repo == "" {
		snap.Repo = "tf-test/team-prs"
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	state := fx.EntityState
	if state == "" {
		state = "active"
	}
	var owning any
	if fx.OwningTeam != "" {
		owning = fx.OwningTeam
	}
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("%s#%d", snap.Repo, snap.Number)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, state,
		                      owning_team_id, created_at, last_polled_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, $6::jsonb, $7, $8, $9, $9)
	`, entityID, orgID, sourceID, snap.Title, snap.URL, string(blob), state, owning, now); err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
	return entityID
}

// trackPgTeamRepo registers owner/repo for the org and attaches it to the
// team, which is what puts an entity inside the team's tracked set.
func trackPgTeamRepo(t *testing.T, h *pgtest.Harness, orgID, teamID, ownerRepo string) {
	t.Helper()
	owner, repo, ok := splitOwnerRepo(ownerRepo)
	if !ok {
		t.Fatalf("fixture repo %q is not owner/repo", ownerRepo)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO repositories (org_id, source, owner, repo) VALUES ($1, 'github', $2, $3)
		ON CONFLICT DO NOTHING
	`, orgID, owner, repo); err != nil {
		t.Fatalf("seed repository %s: %v", ownerRepo, err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO team_github_repos (team_id, repository_id, org_id)
		VALUES ($1,
		        (SELECT id FROM repositories
		          WHERE org_id = $2 AND lower(owner) = lower($3) AND lower(repo) = lower($4)),
		        $2)
		ON CONFLICT (team_id, repository_id) DO NOTHING
	`, teamID, orgID, owner, repo); err != nil {
		t.Fatalf("track repo %s for team %s: %v", ownerRepo, teamID, err)
	}
}

func repoOf(snap domain.PRSnapshot) string {
	if snap.Repo == "" {
		return "tf-test/team-prs"
	}
	return snap.Repo
}

func splitOwnerRepo(s string) (owner, repo string, ok bool) {
	for i := range len(s) {
		if s[i] == '/' {
			return s[:i], s[i+1:], s[:i] != "" && s[i+1:] != ""
		}
	}
	return "", "", false
}
