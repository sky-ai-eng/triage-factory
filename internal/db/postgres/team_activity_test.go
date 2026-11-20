package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTeamActivityStore_Postgres runs the shared conformance suite against
// the Postgres impl, reusing the flow seeder and org graph — the node and the
// entity reads share their definition of a team's flow (tracked-set entities,
// team-scoped tasks). Wired against AdminDB like the entity read suite: the
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

	dbtest.RunTeamActivityConformance(t, func(t *testing.T) (db.TeamActivityStore, string, string, dbtest.FlowSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		return stores.TeamActivity, orgID, teamOf(t, orgID), newPgTeamActivitySeeder(h, orgID, userID, promptID)
	})

	// The tracked-set cut's rows are the subject team's tracking rows and no
	// one else's: not a repository another team in the org tracks, not one
	// nobody tracks, and not a Linear team saved under a workspace the org
	// has moved off. The untracked repository's task is the one place the
	// cut and by_source part: by_source counts the team's task wherever its
	// entity is, and the cut has no row for it to land on.
	t.Run("by_tracked_is_only_this_teams_tracked_set", func(t *testing.T) {
		h.Reset(t)
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		seed := newPgTeamActivitySeeder(h, orgID, userID, promptID)
		now := time.Now().UTC()
		subject := teamOf(t, orgID)

		_, repo := seed.TrackedEntity(t, "github")
		seed.TrackedEntity(t, "linear")
		pgtest.MustExec(t, h.AdminDB,
			`UPDATE linear_team_rules SET linear_workspace_id = 'ws-former' WHERE team_id = $1`, subject)

		untracked := seedPgUntrackedEntity(t, h, orgID, "untracked")
		ev := seed.Event(t, untracked, domain.EventGitHubPROpened, "", now.Add(-time.Hour), time.Time{})
		seed.Task(t, untracked, domain.EventGitHubPROpened, "", ev, "queued", now.Add(-time.Hour))

		other := seedPgDefaultTeam(t, h, orgID, userID)
		pgtest.SeedTrackedRepo(t, h, orgID, other, "acme", "theirs")
		theirs := seedPgGitHubEntityRaw(t, h, orgID, "acme", "theirs", 1)
		seed.Event(t, theirs, domain.EventGitHubPROpened, "", now.Add(-time.Hour), time.Time{})

		got, err := stores.TeamActivity.TeamActivity(context.Background(), orgID, []string{subject}, now.Add(-2*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		if len(got.ByTracked) != 1 || got.ByTracked[0].Source != "github" || got.ByTracked[0].UnitID != repo {
			t.Errorf("by_tracked = %+v, want only the subject team's one tracked repository", got.ByTracked)
		}
		for _, row := range got.BySource {
			if row.Source != "github" {
				continue
			}
			if row.Events == nil || *row.Events != 0 {
				t.Errorf("github by_source events = %v, want 0 — neither event is on the team's tracked set", row.Events)
			}
			if row.Tasks != 1 {
				t.Errorf("github by_source tasks = %d, want 1 — the team's task counts wherever its entity is", row.Tasks)
			}
		}
	})

	// Merged rides the events cut's tracked-set predicate, so a pull request
	// merged in a repo this team does not track is not this team's merge —
	// the multi-mode half of the scoping the local twin has no second team
	// to express.
	t.Run("merged_counts_only_the_tracked_set", func(t *testing.T) {
		h.Reset(t)
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		seed := newPgFlowSeeder(h.AdminDB, orgID, userID, promptID)
		now := time.Now().UTC()

		tracked := seed.Entity(t, "tracked")
		seed.Event(t, tracked, domain.EventGitHubPRMerged, "", now.Add(-time.Hour), time.Time{})
		untracked := seedPgUntrackedEntity(t, h, orgID, "untracked")
		seedPgEvent(t, h, orgID, untracked, domain.EventGitHubPRMerged, now.Add(-time.Hour))

		got, err := stores.TeamActivity.TeamActivity(
			context.Background(), orgID, []string{teamOf(t, orgID)}, now.Add(-2*time.Hour), now)
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
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		seed := newPgFlowSeeder(h.AdminDB, orgID, userID, promptID)
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
			context.Background(), orgID, []string{subject}, now.Add(-2*time.Hour), now)
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

	// A team set's flow is the union of its teams' flows: an entity both teams
	// track counts its events once, and its unit is one row of the tracked
	// set; an entity only one of them tracks counts too.
	t.Run("a_team_set_counts_the_union_once", func(t *testing.T) {
		h.Reset(t)
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		seed := newPgFlowSeeder(h.AdminDB, orgID, userID, promptID)
		now := time.Now().UTC()
		first := teamOf(t, orgID)
		second := seedPgDefaultTeam(t, h, orgID, userID)

		shared := seed.Entity(t, "shared")
		seed.Event(t, shared, domain.EventGitHubPROpened, "", now.Add(-time.Hour), time.Time{})
		var sharedRepo string
		if err := h.AdminDB.QueryRow(`SELECT repository_id FROM entities WHERE id = $1`, shared).Scan(&sharedRepo); err != nil {
			t.Fatalf("read repository_id: %v", err)
		}
		pgtest.MustExec(t, h.AdminDB, `INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`, second, sharedRepo, orgID)
		pgtest.SeedTrackedRepo(t, h, orgID, second, "acme", "second-only")
		secondOnly := seedPgGitHubEntityRaw(t, h, orgID, "acme", "second-only", 1)
		seedPgEvent(t, h, orgID, secondOnly, domain.EventGitHubPROpened, now.Add(-time.Hour))

		got, err := stores.TeamActivity.TeamActivity(context.Background(), orgID, []string{first, second}, now.Add(-2*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		for _, row := range got.BySource {
			if row.Source == "github" && (row.Events == nil || *row.Events != 2) {
				t.Errorf("github events = %v, want 2 — the shared entity once and the second team's own", row.Events)
			}
		}
		if len(got.ByTracked) != 2 {
			t.Errorf("by_tracked = %+v, want the shared repository once and the second team's own", got.ByTracked)
		}
		if len(got.ByEventType) != 1 || got.ByEventType[0].Events != 2 || got.ByEventType[0].Entities != 2 {
			t.Errorf("by_event_type = %+v, want opened with 2 events from 2 entities", got.ByEventType)
		}
	})

	// No team is a team set with no flow: zeros, and the by-source rows the
	// shape always carries, never an error.
	// A team's Slack flow is the channels it watches, whoever is primary
	// there: the subject team watches one channel another team is primary
	// in, and that channel's events are the subject's. A channel only the
	// other team watches, and one nobody watches, are not.
	t.Run("slack_counts_the_channels_the_team_watches", func(t *testing.T) {
		h.Reset(t)
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		seed := newPgTeamActivitySeeder(h, orgID, userID, promptID)
		now := time.Now().UTC().Truncate(time.Millisecond)
		subject := teamOf(t, orgID)

		thread, watched := seed.TrackedEntity(t, "slack")
		seed.Event(t, thread, domain.EventSlackMessage, "", now.Add(-time.Hour), time.Time{})
		last := now.Add(-30 * time.Minute)
		seed.Event(t, thread, domain.EventSlackMessage, "", last, time.Time{})

		other := seedPgDefaultTeam(t, h, orgID, userID)
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id, is_primary)
			VALUES ($1, $2, $3, true)`, orgID, other, watched)
		theirs := seedPgSlackChannel(t, h, orgID, "C-THEIRS")
		pgtest.MustExec(t, h.AdminDB, `
			INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id) VALUES ($1, $2, $3)`,
			orgID, other, theirs)
		seedPgEvent(t, h, orgID, seedPgSlackThread(t, h, orgID, theirs, "C-THEIRS"), domain.EventSlackMessage, now.Add(-time.Hour))
		nobodys := seedPgSlackChannel(t, h, orgID, "C-NOBODY")
		seedPgEvent(t, h, orgID, seedPgSlackThread(t, h, orgID, nobodys, "C-NOBODY"), domain.EventSlackMessage, now.Add(-time.Hour))

		got, err := stores.TeamActivity.TeamActivity(context.Background(), orgID, []string{subject}, now.Add(-2*time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		for _, row := range got.BySource {
			if row.Source == "slack" && (row.Events == nil || *row.Events != 2) {
				t.Errorf("slack by_source events = %v, want 2 — the watched channel's events only", row.Events)
			}
		}
		if len(got.ByTracked) != 1 {
			t.Fatalf("by_tracked = %+v, want only the watched channel", got.ByTracked)
		}
		unit := got.ByTracked[0]
		if unit.Source != "slack" || unit.UnitID != watched || unit.Events != 2 {
			t.Errorf("by_tracked = %+v, want slack %s with 2 events", unit, watched)
		}
		if unit.LastEventAt == nil || !unit.LastEventAt.Equal(last) {
			t.Errorf("last_event_at = %v, want %v", unit.LastEventAt, last)
		}
	})
}

// newPgTeamActivitySeeder is the flow seeder plus TrackedEntity, which
// registers each new unit in the org's first team's tracked set.
func newPgTeamActivitySeeder(h *pgtest.Harness, orgID, userID, promptID string) dbtest.FlowSeeder {
	seed := newPgFlowSeeder(h.AdminDB, orgID, userID, promptID)
	seed.TrackedEntity = func(t *testing.T, source string) (string, string) {
		t.Helper()
		teamID := firstTeamForOrg(t, h, orgID)
		key := "K" + strings.ToUpper(uuid.New().String()[:6])
		switch source {
		case "github":
			repo := strings.ToLower(key)
			unit := pgtest.SeedTrackedRepo(t, h, orgID, teamID, "acme", repo)
			return seedPgGitHubEntityRaw(t, h, orgID, "acme", repo, 1), unit
		case "jira":
			unit := seedPgJiraRule(t, h, teamID, key)
			return seedPgJiraEntityRaw(t, h, orgID, key, 1), unit
		case "linear":
			unit := "lt-" + strings.ToLower(key)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, 'ws-test')
				ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id`, orgID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name)
				VALUES ($1, 'ws-test', $2, $3, $3)`, teamID, unit, key)
			id := uuid.New().String()
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO entities (id, org_id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
				VALUES ($1, $2, 'linear', $3, $4, 'issue', $3, '', '{}'::jsonb, $5, 'ws-test', $6)`,
				id, orgID, key+"-1", "uuid-"+id, time.Now().UTC(), unit)
			return id, unit
		case "slack":
			channel := "C" + key
			unit := seedPgSlackChannel(t, h, orgID, channel)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id) VALUES ($1, $2, $3)`,
				orgID, teamID, unit)
			return seedPgSlackThread(t, h, orgID, unit, channel), unit
		}
		t.Fatalf("TrackedEntity: no tracked set for source %q", source)
		return "", ""
	}
	return seed
}

// seedPgUntrackedEntity inserts an active GitHub PR entity whose repo is in
// no team's tracked set — the counterpart to FlowSeeder.Entity, which
// registers one. Returns the entity id.
func seedPgUntrackedEntity(t *testing.T, h *pgtest.Harness, orgID, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	sourceID := fmt.Sprintf("tf-test/%s-%s#1", suffix, id[:8])
	repositoryID := dbseed.GitHubEntityRepository(t, h.AdminDB, orgID, "https://github.com", sourceID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, now(), 'https://github.com', $6)
	`, id, orgID, sourceID, "Untracked "+suffix, "https://example/"+sourceID, repositoryID); err != nil {
		t.Fatalf("seed untracked entity %s: %v", suffix, err)
	}
	return id
}

// seedPgSlackChannel inserts a channel's registry row and returns its id.
func seedPgSlackChannel(t *testing.T, h *pgtest.Harness, orgID, channel string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO slack_channels (org_id, channel_id, workspace_id) VALUES ($1, $2, 'T-test')
		RETURNING id::text`, orgID, channel).Scan(&id); err != nil {
		t.Fatalf("seed slack channel %s: %v", channel, err)
	}
	return id
}

// seedPgSlackThread inserts an active thread entity in the channel whose
// registry row is channelRow, and returns its id.
func seedPgSlackThread(t *testing.T, h *pgtest.Harness, orgID, channelRow, channel string) string {
	t.Helper()
	id := uuid.New().String()
	ts := "1700000000.000100"
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, scope, slack_channel_id, slack_thread_ts)
		VALUES ($1, $2, 'slack', $3, 'thread', 'test thread', $4, $5, $6)`,
		id, orgID, domain.SlackSourceID(channel, ts), domain.SlackScope, channelRow, ts)
	return id
}

// seedPgEvent inserts one entity-attached event. FlowSeeder.Event is
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
