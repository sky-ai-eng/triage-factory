package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTeamActivityStore_Postgres runs the shared conformance suite against
// the Postgres impl, reusing the factory seeder and org graph — the node and
// the belt share their definition of a team's flow (tracked-set entities,
// team-scoped tasks). Wired against AdminDB like the factory suite: the
// node's predicates are subject-team-scoped rather than viewer-scoped, so
// behavior is independent of the auth path and the RLS arms are covered by
// the app-pool suites that own them.
func TestTeamActivityStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	teamOf := func(t *testing.T, orgID string) string {
		t.Helper()
		var teamID string
		if err := h.AdminDB.QueryRow(
			`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`, orgID,
		).Scan(&teamID); err != nil {
			t.Fatalf("resolve default team: %v", err)
		}
		return teamID
	}

	dbtest.RunTeamActivityConformance(t, func(t *testing.T) (db.TeamActivityStore, string, string, dbtest.FactorySeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgFactoryOrg(t, h)
		promptID := seedPgFactoryPrompt(t, h, orgID, userID)
		return stores.TeamActivity, orgID, teamOf(t, orgID), newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)
	})

	// The tracked-set rule's side of the slack divergence: with no tracked
	// set there is no team-scoped definition of a slack event, so its count
	// is nil (undefined) while the pollable sources answer a defined zero.
	t.Run("slack_events_are_undefined_not_zero", func(t *testing.T) {
		h.Reset(t)
		orgID, _ := seedPgFactoryOrg(t, h)
		now := time.Now().UTC()
		got, err := stores.TeamActivity.TeamActivity(context.Background(), orgID, teamOf(t, orgID), now.Add(-time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		for _, row := range got.BySource {
			switch row.Source {
			case "github", "jira":
				if row.Events == nil {
					t.Errorf("%s events undefined, want a defined zero", row.Source)
				}
			case "slack":
				if row.Events != nil {
					t.Errorf("slack events = %d, want undefined (nil)", *row.Events)
				}
			}
		}
	})
}
