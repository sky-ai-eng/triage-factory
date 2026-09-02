package sqlite_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamPRStore_SQLite runs the shared conformance suite against the SQLite
// impl. The subject team is the local default; the tracked-set outer filter
// the Postgres twin applies has nothing to narrow at N=1, so the suite's
// tracked-set case lives in the Postgres file and this one asserts the
// documented local answer below.
func TestTeamPRStore_SQLite(t *testing.T) {
	dbtest.RunTeamPRStoreConformance(t, func(t *testing.T) (db.TeamPRStore, string, string, string, dbtest.TeamPRSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		// The org's github_base_url is unset here, the common case: the store
		// resolves it to the deployment default (github.com here), which is where a login-claim
		// binding actually lands.
		return stores.TeamPRs, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, "",
			newSQLiteTeamPRSeeder(conn, runmode.LocalDefaultTeamID)
	})

	// The local half of the scoping split: N=1 tracks everything, so a pull
	// request in a repo no team row mentions is still the sole team's. The
	// Postgres file asserts the other half — a tracked-set outer filter that
	// excludes it — and this pins that the difference is deliberate rather
	// than a query that quietly lost a predicate.
	t.Run("local_is_unscoped_by_tracked_set", func(t *testing.T) {
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := newSQLiteTeamPRSeeder(conn, runmode.LocalDefaultTeamID)
		seed.Member(t, "operator")
		seed.PR(t, dbtest.TeamPRFixture{Snapshot: domain.PRSnapshot{
			Number: 1, Author: "operator", State: "OPEN", Repo: "nobody/tracks-this",
		}})

		prs, total, err := stores.TeamPRs.TeamPRs(t.Context(),
			runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, "", db.PRListFilter{}, db.Unwindowed)
		if err != nil {
			t.Fatalf("TeamPRs: %v", err)
		}
		if len(prs) != 1 || total != 1 {
			t.Fatalf("got %d rows / total %d, want 1/1 — local tracks everything", len(prs), total)
		}
	})
}

// newSQLiteTeamPRSeeder stages the conformance fixtures with raw SQL. Team
// membership and identity binding are real rows even at N=1: the member leg is
// a join, and stubbing it would leave the one thing this store does untested.
func newSQLiteTeamPRSeeder(conn *sql.DB, teamID string) dbtest.TeamPRSeeder {
	member := func(t *testing.T, name string) string {
		t.Helper()
		id := uuid.New().String()
		if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, name); err != nil {
			t.Fatalf("seed user %s: %v", name, err)
		}
		if _, err := conn.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`, id, teamID,
		); err != nil {
			t.Fatalf("seed membership for %s: %v", name, err)
		}
		return id
	}
	return dbtest.TeamPRSeeder{
		Member: func(t *testing.T, login string) string {
			t.Helper()
			id := member(t, login)
			if _, err := conn.Exec(`
				INSERT INTO user_github_identities (user_id, github_base_url, login, source)
				VALUES (?, ?, ?, 'connect_oauth')
			`, id, db.EffectiveGitHubHost(""), login); err != nil {
				t.Fatalf("bind identity for %s: %v", login, err)
			}
			return id
		},
		MemberWithoutIdentity: member,
		OtherTeam: func(t *testing.T, name string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
				id, runmode.LocalDefaultOrgID, "seed-"+id[:8], name,
			); err != nil {
				t.Fatalf("seed team %s: %v", name, err)
			}
			return id
		},
		PR: func(t *testing.T, fx dbtest.TeamPRFixture) string {
			t.Helper()
			return seedSQLiteTeamPR(t, conn, fx)
		},
	}
}

func seedSQLiteTeamPR(t *testing.T, conn *sql.DB, fx dbtest.TeamPRFixture) string {
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
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, state,
		                      owning_team_id, created_at, last_polled_at)
		VALUES (?, 'github', ?, 'pr', ?, ?, ?, ?, ?, ?, ?)
	`, entityID, sourceID, snap.Title, snap.URL, string(blob), state, owning, now, now); err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
	return entityID
}
