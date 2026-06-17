package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTeamAgentStore_Postgres runs the shared conformance suite. The
// factory pre-seeds (user, org, team, agent) so all FKs satisfy.
func TestTeamAgentStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunTeamAgentStoreConformance(t, func(t *testing.T) (db.TeamAgentStore, string, string, string) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForAgents(t, h)
		teamID := seedPgTeam(t, h, orgID, "default")
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		agentID, err := stores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Bot"})
		if err != nil {
			t.Fatalf("seed agent: %v", err)
		}
		return stores.TeamAgents, orgID, teamID, agentID
	})
}

// TestTeamAgentStore_Postgres_NonMemberCannotToggle pins the RLS
// gate: a team member can toggle their own team's bot, but cannot
// toggle a different team's bot in the same org.
func TestTeamAgentStore_Postgres_NonMemberCannotToggle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID := seedPgOrgForAgents(t, h)
	platformID := seedPgTeam(t, h, orgID, "platform")
	mobileID := seedPgTeam(t, h, orgID, "mobile")

	// dave is a member of the org (so org-access predicate passes for
	// joined queries) but only joins platform; toggling mobile must
	// fail the team_agents_update RLS predicate (user_in_team gate).
	daveID := seedPgMember(t, h, orgID, "dave", "member")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		daveID, platformID,
	); err != nil {
		t.Fatalf("dave platform membership: %v", err)
	}

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	agentID, err := adminStores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Org Bot"})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Pre-seed team_agents rows for both teams.
	for _, teamID := range []string{platformID, mobileID} {
		if err := adminStores.TeamAgents.AddForTeam(context.Background(), orgID, teamID, agentID); err != nil {
			t.Fatalf("AddForTeam %s: %v", teamID, err)
		}
	}

	// dave toggles platform — allowed (he's a member).
	err = h.WithUser(t, daveID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE team_agents SET enabled = FALSE WHERE team_id = $1 AND agent_id = $2`,
			platformID, agentID,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("platform toggle matched %d rows; want 1 (dave is a platform member)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dave toggle own team: %v", err)
	}

	// dave toggles mobile — refused (not a member). RLS filters the
	// row out of the UPDATE's USING set; 0 rows affected, no error.
	err = h.WithUser(t, daveID, orgID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE team_agents SET enabled = FALSE WHERE team_id = $1 AND agent_id = $2`,
			mobileID, agentID,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			return fmt.Errorf("non-member toggle matched %d rows; want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dave toggle other team: %v", err)
	}

	// Sanity: mobile's team_agents row is still enabled.
	var enabled bool
	if err := h.AdminDB.QueryRow(
		`SELECT enabled FROM team_agents WHERE team_id = $1`, mobileID,
	).Scan(&enabled); err != nil {
		t.Fatalf("re-read mobile: %v", err)
	}
	if !enabled {
		t.Error("mobile bot disabled by non-member; RLS didn't gate")
	}
}

// TestTeamAgentStore_Postgres_BlocksCrossOrgInsert pins the fix
// from migration 202605120004. Without the agents.org_id = teams.org_id
// predicate on team_agents_insert, a member of org A's team could
// INSERT (team_id=team-in-A, agent_id=guessed-agent-uuid-from-B) and
// the policy would accept it because user_in_team(team_id) is true.
// The subsequent JOIN that resolves "this team's bot" would silently
// dispatch to the wrong tenant's agent.
//
// The regression check: an org-A team member directly INSERTs a
// team_agents row pointing at org B's agent and expects RLS refusal.
func TestTeamAgentStore_Postgres_BlocksCrossOrgInsert(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	teamA := seedPgTeam(t, h, orgA, "team-a")
	aliceID := seedPgMember(t, h, orgA, "alice", "member")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		aliceID, teamA,
	); err != nil {
		t.Fatalf("alice team-a membership: %v", err)
	}

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := adminStores.Agents.Create(context.Background(), orgA, domain.Agent{DisplayName: "Bot A"}); err != nil {
		t.Fatalf("seed agent A: %v", err)
	}
	if _, err := adminStores.Agents.Create(context.Background(), orgB, domain.Agent{DisplayName: "Bot B"}); err != nil {
		t.Fatalf("seed agent B: %v", err)
	}
	var agentBID string
	if err := h.AdminDB.QueryRow(`SELECT id FROM agents WHERE org_id = $1`, orgB).Scan(&agentBID); err != nil {
		t.Fatalf("read agent B id: %v", err)
	}

	// Alice (member of org-A's team-a) attempts to INSERT a team_agents
	// row that points at org B's agent. RLS must filter this row out
	// of the INSERT's WITH CHECK — Postgres surfaces the policy
	// refusal as ERROR 42501 "new row violates row-level security
	// policy" rather than a 0-rows-affected outcome on INSERT.
	err := h.WithUser(t, aliceID, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO team_agents (team_id, agent_id, enabled) VALUES ($1, $2, TRUE)`,
			teamA, agentBID,
		)
		return err
	})
	if err == nil {
		t.Fatal("cross-org team_agents INSERT succeeded; team member can reach another org's agent via team_agents")
	}

	// Sanity: no team_agents row exists for team-a after the failed
	// attempt — the policy refused at INSERT time, no row was written.
	var count int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM team_agents WHERE team_id = $1`, teamA,
	).Scan(&count); err != nil {
		t.Fatalf("re-read team-a: %v", err)
	}
	if count != 0 {
		t.Errorf("team_agents row count for team-a=%d after failed cross-org INSERT; want 0", count)
	}
}

// TestTeamAgentStore_Postgres_BlocksOtherOrgWhileInActiveOrg pins the
// fix from migration 202605120005. Without the tf.team_in_current_org
// predicate, a user with memberships in two orgs could SELECT / UPDATE
// team_agents rows in org B while their JWT claims org_id = A — a
// tenancy break vs every other table in the schema (compare
// teams_select / memberships_select which both require
// t.org_id = tf.current_org_id()).
//
// The scenario: Charlie belongs to teams in both orgA and orgB. He
// makes a request with JWT claims org_id = orgA, but the SQL path
// addresses orgB's team. Without the org pin, RLS lets it through
// because user_in_team(team_b) is true. The fix refuses.
func TestTeamAgentStore_Postgres_BlocksOtherOrgWhileInActiveOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	teamA := seedPgTeam(t, h, orgA, "team-a")
	teamB := seedPgTeam(t, h, orgB, "team-b")

	// Charlie is a member of both orgs and both teams.
	charlieID := seedPgMember(t, h, orgA, "charlie", "member")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`,
		orgB, charlieID,
	); err != nil {
		t.Fatalf("charlie orgB membership: %v", err)
	}
	for _, teamID := range []string{teamA, teamB} {
		if _, err := h.AdminDB.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
			charlieID, teamID,
		); err != nil {
			t.Fatalf("charlie membership %s: %v", teamID, err)
		}
	}

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	for _, orgID := range []string{orgA, orgB} {
		if _, err := adminStores.Agents.Create(context.Background(), orgID, domain.Agent{DisplayName: "Bot"}); err != nil {
			t.Fatalf("seed agent for %s: %v", orgID, err)
		}
	}
	agentB := mustAgentIDForOrg(t, h, orgB)
	for _, p := range []struct{ team, agent string }{{teamA, mustAgentIDForOrg(t, h, orgA)}, {teamB, agentB}} {
		if err := adminStores.TeamAgents.AddForTeam(context.Background(), "", p.team, p.agent); err != nil {
			t.Fatalf("seed team_agents %s/%s: %v", p.team, p.agent, err)
		}
	}

	// Charlie's session is on orgA. He attempts to UPDATE orgB's
	// team_agents row. RLS must filter the row out of the USING set
	// (the team_in_current_org predicate fails because teamB.org_id
	// != orgA). Expected outcome: 0 rows affected.
	err := h.WithUser(t, charlieID, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE team_agents SET enabled = FALSE WHERE team_id = $1 AND agent_id = $2`,
			teamB, agentB,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			return fmt.Errorf("cross-org UPDATE while in orgA context matched %d rows on teamB; want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-org UPDATE assertion: %v", err)
	}

	// And the orgB team_agents row is still enabled.
	var enabled bool
	if err := h.AdminDB.QueryRow(
		`SELECT enabled FROM team_agents WHERE team_id = $1`, teamB,
	).Scan(&enabled); err != nil {
		t.Fatalf("re-read teamB: %v", err)
	}
	if !enabled {
		t.Error("teamB bot disabled via cross-org write; RLS didn't gate on current_org_id")
	}

	// Same predicate must apply to SELECT — Charlie in orgA can't
	// READ orgB's team_agents row even though he is a member of teamB.
	err = h.WithUser(t, charlieID, orgA, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM team_agents WHERE team_id = $1`, teamB,
		).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("cross-org SELECT while in orgA context returned %d rows on teamB; want 0", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-org SELECT assertion: %v", err)
	}
}

// TestTeamAgentStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for team_agents with the same 3-subtest shape used across
// the store suite. team_agents has no org_id column — the policies
// gate via team_in_current_org + user_in_team — so the cross-org
// write denial fires when an orgB-claimed user tries to INSERT a
// team_agents row pointing at orgA's team. Mirrors the existing
// BlocksCrossOrgInsert test, structured under the canonical 3-subtest
// rubric.
func TestTeamAgentStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	teamA := seedPgTeam(t, h, orgA, "team-a")
	teamB := seedPgTeam(t, h, orgB, "team-b")

	aliceID := seedPgMember(t, h, orgA, "alice", "member")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		aliceID, teamA,
	); err != nil {
		t.Fatalf("alice team-a membership: %v", err)
	}
	bobID := seedPgMember(t, h, orgB, "bob", "member")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		bobID, teamB,
	); err != nil {
		t.Fatalf("bob team-b membership: %v", err)
	}

	adminStores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := adminStores.Agents.Create(context.Background(), orgA, domain.Agent{DisplayName: "Bot A"}); err != nil {
		t.Fatalf("seed agent A: %v", err)
	}
	if _, err := adminStores.Agents.Create(context.Background(), orgB, domain.Agent{DisplayName: "Bot B"}); err != nil {
		t.Fatalf("seed agent B: %v", err)
	}
	agentA := mustAgentIDForOrg(t, h, orgA)
	agentB := mustAgentIDForOrg(t, h, orgB)
	if err := adminStores.TeamAgents.AddForTeam(context.Background(), orgA, teamA, agentA); err != nil {
		t.Fatalf("seed team_agents A: %v", err)
	}
	if err := adminStores.TeamAgents.AddForTeam(context.Background(), orgB, teamB, agentB); err != nil {
		t.Fatalf("seed team_agents B: %v", err)
	}
	ctx := context.Background()

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, aliceID, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).TeamAgents.GetForTeam(ctx, orgA, teamA, agentA)
			if err != nil {
				return fmt.Errorf("GetForTeam: %w", err)
			}
			if got == nil {
				t.Errorf("alice GetForTeam(orgA, teamA, agentA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		// bob in orgB asking about orgA's team_agents — RLS's
		// team_in_current_org predicate fails (teamA.org_id != orgB).
		err := h.WithUser(t, bobID, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).TeamAgents.GetForTeam(ctx, orgA, teamA, agentA)
			if err != nil {
				return fmt.Errorf("GetForTeam: %w", err)
			}
			if got != nil {
				t.Errorf("bob GetForTeam(orgA, teamA, agentA) returned %+v; RLS USING filter leaked orgA's team_agents row to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob in orgB attempts a direct INSERT of a team_agents row
		// for orgA's team. team_agents_insert WITH CHECK requires
		// team_in_current_org(team_id) AND user_in_team(team_id);
		// both fail for bob/teamA. Expect 42501. The store's
		// AddForTeam goes through the admin pool (production
		// bootstrap), so the policy is exercised via raw SQL — same
		// shape as TestTeamAgentStore_Postgres_BlocksCrossOrgInsert.
		err := h.WithUser(t, bobID, orgB, func(tx *sql.Tx) error {
			_, e := tx.Exec(
				`INSERT INTO team_agents (team_id, agent_id, enabled) VALUES ($1, $2, TRUE)`,
				teamA, agentA,
			)
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// mustAgentIDForOrg reads the deterministic agent id back. Used by
// the cross-org test which needs both orgs' agent ids without going
// through GetForOrg (which is itself app-pool-gated and would need a
// JWT claims setup).
func mustAgentIDForOrg(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`SELECT id FROM agents WHERE org_id = $1`, orgID).Scan(&id); err != nil {
		t.Fatalf("read agent id for org %s: %v", orgID, err)
	}
	return id
}

// seedPgTeam inserts a teams row under the given org and returns its id.
func seedPgTeam(t *testing.T, h *pgtest.Harness, orgID, slug string) string {
	t.Helper()
	teamID := uuid.New().String()
	if _, err := h.AdminDB.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES ($1, $2, $3, $4)`,
		teamID, orgID, slug, slug+" team",
	); err != nil {
		t.Fatalf("seed team %s: %v", slug, err)
	}
	return teamID
}
