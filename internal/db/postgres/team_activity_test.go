package postgres_test

import (
	"context"
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

	// Merged rides the events cut's tracked-set predicate, so a pull request
	// merged in a repo this team does not track is not this team's merge —
	// the multi-mode half of the scoping the local twin has no second team
	// to express.
	t.Run("merged_counts_only_the_tracked_set", func(t *testing.T) {
		h.Reset(t)
		orgID, userID := seedPgFactoryOrg(t, h)
		promptID := seedPgFactoryPrompt(t, h, orgID, userID)
		seed := newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)
		now := time.Now().UTC()

		tracked := seed.Entity(t, "tracked")
		seed.Event(t, tracked, domain.EventGitHubPRMerged, "", now.Add(-time.Hour), time.Time{})
		untracked := seedPgUntrackedEntity(t, h, orgID, "untracked")
		seedPgEvent(t, h, orgID, untracked, domain.EventGitHubPRMerged, now.Add(-time.Hour))

		got, err := stores.TeamActivity.TeamActivity(
			context.Background(), orgID, teamOf(t, orgID), now.Add(-2*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		merged, events := 0, 0
		for _, d := range got.ByDay {
			merged += d.Merged
			events += d.Events
		}
		if merged != 1 {
			t.Errorf("merged = %d, want 1 — the untracked repo's merge is not this team's", merged)
		}
		if events != 1 {
			t.Errorf("events = %d, want 1 — merged must ride the same tracked-set gate as events", events)
		}
	})

	// Failed scopes on the conversation's own team_id: a run another team
	// owns is that team's failure, even though both teams live in one org
	// and the node reads under an RLS-bypassed pool.
	t.Run("failed_counts_only_the_owning_team", func(t *testing.T) {
		h.Reset(t)
		orgID, userID := seedPgFactoryOrg(t, h)
		promptID := seedPgFactoryPrompt(t, h, orgID, userID)
		seed := newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)
		now := time.Now().UTC()
		subject := teamOf(t, orgID)

		entityID := seed.Entity(t, "owned")
		ev := seed.Event(t, entityID, domain.EventGitHubPRCICheckFailed, "", now.Add(-time.Hour), time.Time{})
		taskID := seed.Task(t, entityID, domain.EventGitHubPRCICheckFailed, "", ev, "queued", now.Add(-time.Hour))

		ours := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, ours, domain.StatusFailed, now.Add(-30*time.Minute))
		theirs := seed.Conversation(t, taskID, domain.StatusFailed)
		seed.FinishConversation(t, theirs, domain.StatusFailed, now.Add(-30*time.Minute))
		// The seeder mints against the org's first team, so the second team
		// is created only once every fixture row is placed — then one run is
		// reassigned, leaving the two differing in nothing but ownership.
		other := seedPgDefaultTeam(t, h, orgID, userID)
		if _, err := h.AdminDB.Exec(
			`UPDATE conversations SET team_id = $1 WHERE id = $2`, other, theirs,
		); err != nil {
			t.Fatalf("reassign conversation to the other team: %v", err)
		}

		got, err := stores.TeamActivity.TeamActivity(
			context.Background(), orgID, subject, now.Add(-2*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		failed := 0
		for _, d := range got.ByDay {
			failed += d.Failed
		}
		if failed != 1 {
			t.Errorf("failed = %d, want 1 — the other team's run is not this team's failure", failed)
		}
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

// seedPgUntrackedEntity inserts an active GitHub PR entity whose repo is in
// no team's tracked set — the counterpart to FactorySeeder.Entity, which
// registers one. Returns the entity id.
func seedPgUntrackedEntity(t *testing.T, h *pgtest.Harness, orgID, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	sourceID := fmt.Sprintf("tf-test/%s-%s#1", suffix, id[:8])
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, now())
	`, id, orgID, sourceID, "Untracked "+suffix, "https://example/"+sourceID); err != nil {
		t.Fatalf("seed untracked entity %s: %v", suffix, err)
	}
	return id
}

// seedPgEvent inserts one entity-attached event. FactorySeeder.Event is
// closed over the seeder's own org graph; this one takes any entity.
func seedPgEvent(t *testing.T, h *pgtest.Harness, orgID, entityID, eventType string, createdAt time.Time) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
	`, uuid.New().String(), orgID, entityID, eventType, createdAt.UTC()); err != nil {
		t.Fatalf("seed event %s: %v", eventType, err)
	}
}
