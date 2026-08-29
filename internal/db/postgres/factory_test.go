package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestFactoryReadStore_Postgres runs the shared conformance suite
// against the Postgres FactoryReadStore impl. Each subtest gets a
// fresh org + team + user + seed prompt, then seeds whatever
// fixtures the subtest needs via raw INSERTs against the admin
// pool. Skips cleanly when Docker isn't available — pgtest.Shared
// handles that.
func TestFactoryReadStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	// Wire both pools against AdminDB so the factory store can read
	// org-wide data without a JWT claims tx. The admin pool is the
	// production wiring choice for this store anyway (system-level
	// view, no per-user identity); using AdminDB for both halves
	// matches that intent in tests. RLS bypass is fine for behavior
	// conformance — the cross-org leakage test below exercises the
	// org_id WHERE filter on its own.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunFactoryReadStoreConformance(t, func(t *testing.T) (db.FactoryReadStore, string, dbtest.FactorySeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgFactoryOrg(t, h)
		promptID := seedPgFactoryPrompt(t, h, orgID, userID)
		seeder := newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)
		return stores.Factory, orgID, seeder
	})
}

// seedPgFactoryOrg builds the auth.user + public.user + org +
// org_membership + default team graph the factory's FK chain needs.
// Mirrors seedPgOrgUserAgent from tasks_test.go but skips the agent
// half — factory reads don't touch the agents table.
func seedPgFactoryOrg(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("factory-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)

	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Factory Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Factory Org "+orgID[:8], "factory-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

// seedPgFactoryPrompt inserts a user-source prompt that conversations can
// FK into. team_id is read from the org's default team (created by
// seedPgFactoryOrg via seedPgDefaultTeam). source='user' satisfies
// prompts_system_has_no_creator (creator must be non-NULL).
func seedPgFactoryPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	promptID := "p_factory_" + uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'Factory Test', 'body', 'user', '', now(), now())
	`, promptID, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return promptID
}

// newPgFactorySeeder builds the FactorySeeder callbacks against the
// admin pool. Every INSERT carries org_id so RLS would pass even if
// the store ran on the app pool — defense-in-depth without test-
// side complication. The default team for the org backs every task
// + run insertion's team_id requirement.
func newPgFactorySeeder(conn *sql.DB, orgID, userID, promptID string) dbtest.FactorySeeder {
	return dbtest.FactorySeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			// Multi-mode visibility is sourced from the team's tracked set
			// (TFAC-516), not task existence, so the conformance entity needs
			// a parseable "owner/repo#N" source_id AND a team_github_repos row
			// registering its repo for the org's default team. Each entity
			// gets its own repo (owner fixed, repo derived from suffix) so a
			// per-entity INSERT can't collide on the (team, owner, repo) PK.
			owner := "tf-test"
			repo := fmt.Sprintf("%s-%s", suffix, id[:8])
			sourceID := fmt.Sprintf("%s/%s#1", owner, repo)
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
			`, id, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			if _, err := conn.Exec(`
				INSERT INTO repositories (org_id, source, owner, repo) VALUES ($1, 'github', $2, $3)
				ON CONFLICT DO NOTHING
			`, orgID, owner, repo); err != nil {
				t.Fatalf("seed entity repository %s/%s: %v", owner, repo, err)
			}
			if _, err := conn.Exec(`
				INSERT INTO team_github_repos (team_id, repository_id, org_id)
				VALUES ((SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1),
				        (SELECT id FROM repositories
				          WHERE org_id = $1 AND lower(owner) = lower($2) AND lower(repo) = lower($3)),
				        $1)
				ON CONFLICT (team_id, repository_id) DO NOTHING
			`, orgID, owner, repo); err != nil {
				t.Fatalf("track entity repo %s/%s: %v", owner, repo, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType, dedupKey string, createdAt, occurredAt time.Time) string {
			t.Helper()
			id := uuid.New().String()
			var occ sql.NullTime
			if !occurredAt.IsZero() {
				occ = sql.NullTime{Time: occurredAt, Valid: true}
			}
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at, occurred_at)
				VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6, $7)
			`, id, orgID, entityID, eventType, dedupKey, createdAt, occ); err != nil {
				t.Fatalf("seed event %s: %v", eventType, err)
			}
			return id
		},
		EventNullEntity: func(t *testing.T, eventType string, createdAt time.Time) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, NULL, $3, '', '{}'::jsonb, $4)
			`, id, orgID, eventType, createdAt); err != nil {
				t.Fatalf("seed system event %s: %v", eventType, err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, dedupKey, primaryEventID, status string, createdAt time.Time) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
				                   status, scoring_status, priority_score, created_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, $6, $7, $8, 'pending', 0.5, $9)
			`, id, orgID, userID, entityID, eventType, dedupKey, primaryEventID, status, createdAt); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		Conversation: func(t *testing.T, taskID, status string) string {
			t.Helper()
			// conversations.blueprint_run_id is NOT NULL — mint a 1-step blueprint_run for
			// the task first (the firing unit), then link the run to it.
			bpID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'user', 'BP', now(), now())
			`, bpID, orgID, userID); err != nil {
				t.Fatalf("seed blueprint: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
				VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', now(), '[]')
			`, brID, orgID, userID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			id := uuid.New().String()
			// "running" is an engagement, not a stored value — mint the
			// claim the real claim path would and leave the column NULL.
			stored := any(status)
			if status == "running" {
				stored = nil
			}
			if _, err := conn.Exec(`
				INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, trigger_type, status, blueprint_run_id)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, 'manual', $6, $7)
			`, id, orgID, userID, taskID, promptID, stored, brID); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			if status == "running" {
				if _, err := conn.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at)
					VALUES ($1, $2, $3, 'factory-seed-executor', 1, now())
				`, uuid.New().String(), orgID, id); err != nil {
					t.Fatalf("seed claim: %v", err)
				}
			}
			return id
		},
		FinishConversation: func(t *testing.T, conversationID, status string, completedAt time.Time) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE conversations SET status = $1, completed_at = $2 WHERE id = $3 AND org_id = $4`,
				status, completedAt.UTC(), conversationID, orgID,
			); err != nil {
				t.Fatalf("finish conversation: %v", err)
			}
		},
		CloseEntity: func(t *testing.T, entityID string, closedAt time.Time) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE entities SET state = 'closed', closed_at = $1 WHERE id = $2 AND org_id = $3`,
				closedAt.UTC(), entityID, orgID,
			); err != nil {
				t.Fatalf("close entity: %v", err)
			}
		},
		SetConversationMemory: func(t *testing.T, conversationID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content)
					VALUES ($1, $2, $3, $4, NULL)
				`, memID, orgID, conversationID, entityID); err != nil {
					t.Fatalf("seed null conversation_memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content)
				VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, conversationID, entityID, content); err != nil {
				t.Fatalf("seed conversation_memory: %v", err)
			}
		},
	}
}

// trackPgRepo registers (owner, repo) as a tracked repo for teamID — the
// multi-mode factory belt's GitHub visibility gate (TFAC-516). The registry
// row comes first, because that is what the tracking row references.
func trackPgRepo(t *testing.T, h *pgtest.Harness, orgID, teamID, owner, repo string) {
	t.Helper()
	pgtest.SeedTrackedRepo(t, h, orgID, teamID, owner, repo)
}

// seedPgGitHubEntityRaw / seedPgJiraEntityRaw insert an active entity with
// an explicit "owner/repo#N" / "KEY-N" source_id and return its ID,
// WITHOUT touching the tracked set — the factory tests below control
// tracking themselves to exercise the tracked-set semi-join's include/
// exclude boundary. (newPgFactorySeeder.Entity auto-tracks; these don't.)
func seedPgGitHubEntityRaw(t *testing.T, h *pgtest.Harness, orgID, owner, repo string, number int) string {
	t.Helper()
	id := uuid.New().String()
	sourceID := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, '', '{}'::jsonb, $5)
	`, id, orgID, sourceID, "PR "+sourceID, time.Now().UTC()); err != nil {
		t.Fatalf("seed github entity %s: %v", sourceID, err)
	}
	return id
}

func seedPgJiraEntityRaw(t *testing.T, h *pgtest.Harness, orgID, projectKey string, number int) string {
	t.Helper()
	id := uuid.New().String()
	sourceID := fmt.Sprintf("%s-%d", projectKey, number)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'jira', $3, 'issue', $4, '', '{}'::jsonb, $5)
	`, id, orgID, sourceID, "Issue "+sourceID, time.Now().UTC()); err != nil {
		t.Fatalf("seed jira entity %s: %v", sourceID, err)
	}
	return id
}

// seedPgFactoryEvent inserts an entity-attached event via the admin pool
// for tests that need a station but not the full newPgFactorySeeder.
func seedPgFactoryEvent(t *testing.T, h *pgtest.Harness, orgID, entityID, eventType string, createdAt time.Time) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
	`, uuid.New().String(), orgID, entityID, eventType, createdAt); err != nil {
		t.Fatalf("seed event %s: %v", eventType, err)
	}
}

// TestFactoryReadStore_Postgres_ExcludesUntrackedEntity pins the TFAC-516
// tracked-set semi-join for GitHub: an entity whose repo is in the team's
// tracked set rides the belt *with no task at all* (the whole point — belt
// density is no longer a side effect of task creation), while an entity on
// an untracked repo is excluded even when it has a station (event) and is
// equally untasked. Matching is case-insensitive, mirroring
// TracksRepoSystem. Runs under AdminDB, so the EXISTS reduces to the
// org-scoped semi-join; the per-team RLS narrowing is exercised in
// TestFactoryReadStore_Postgres_CrossTeamIsolation_RLS.
func TestFactoryReadStore_Postgres_ExcludesUntrackedEntity(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFactoryOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	now := time.Now().UTC()

	// onTracked: repo in the tracked set, NO task — under the old
	// task-existence gate it would have been hidden; now it rides the belt.
	trackPgRepo(t, h, orgID, teamID, "acme", "tracked")
	onTracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "tracked", 1)

	// mixedCase: tracked repo casing differs from the entity's source_id
	// casing — the lower()/lower() match must still surface it.
	trackPgRepo(t, h, orgID, teamID, "acme", "casefold")
	mixedCase := seedPgGitHubEntityRaw(t, h, orgID, "ACME", "CaseFold", 7)

	// onUntracked: no team tracks its repo. It has a station and, like the
	// others, no task — so the tracked-set gate is the only thing on it.
	onUntracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "untracked", 2)
	seedPgFactoryEvent(t, h, orgID, onUntracked, "github:pr:opened", now)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	rows, err := stores.Factory.Entities(context.Background(), orgID, 100, nil)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Entity.ID] = true
	}
	if !got[onTracked] {
		t.Errorf("entity on tracked repo %s missing — tracked-set semi-join must surface untasked entities", onTracked)
	}
	if !got[mixedCase] {
		t.Errorf("entity %s (mixed-case owner/repo) missing — tracked-repo match must be case-insensitive", mixedCase)
	}
	if got[onUntracked] {
		t.Errorf("entity on untracked repo %s leaked through — tracked-set semi-join not applied", onUntracked)
	}
}

// TestFactoryReadStore_Postgres_JiraScopedToTrackedProject is the Jira
// mirror of the GitHub test: a Jira entity whose project key is attached
// to one of the viewer's teams (a jira_project_status_rules row) rides the
// belt; one whose project no team configured is excluded. Pins that the
// belt's Jira side scopes by tracked project, not task existence.
func TestFactoryReadStore_Postgres_JiraScopedToTrackedProject(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFactoryOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	// teamID attaches project SKY; PROJ is attached by no team.
	seedPgJiraRule(t, h, teamID, "SKY")
	onAttached := seedPgJiraEntityRaw(t, h, orgID, "SKY", 1)
	onUnattached := seedPgJiraEntityRaw(t, h, orgID, "PROJ", 2)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	rows, err := stores.Factory.Entities(context.Background(), orgID, 100, nil)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Entity.ID] = true
	}
	if !got[onAttached] {
		t.Errorf("Jira entity on attached project %s missing — tracked-project semi-join dropped a member", onAttached)
	}
	if got[onUnattached] {
		t.Errorf("Jira entity on unattached project %s leaked through — tracked-project semi-join not applied", onUnattached)
	}
}

// TestFactoryReadStore_Postgres_CrossTeamIsolation_RLS proves the per-team
// narrowing the production app pool gets for free: driven through tf_app
// with real JWT claims, a viewer only sees belt entities in their own
// teams' tracked set. This is the one multi-tenant *leak* surface in the
// cleanup (the net-new GitHub-by-repo query), held to the same standard as
// the Jira deck. alice (teamA only) sees teamA's tracked GitHub + Jira
// entities but not teamB's; bob (teamB only) sees the mirror.
func TestFactoryReadStore_Postgres_CrossTeamIsolation_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, alice := seedPgFactoryOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgID)
	bob := seedPgMember(t, h, orgID, "bob", "member")
	teamB := seedPgDefaultTeam(t, h, orgID, bob)

	// teamA tracks acme/a-repo (GitHub) + SKY (Jira); teamB tracks
	// acme/b-repo + BOB. Same org, disjoint tracked sets.
	trackPgRepo(t, h, orgID, teamA, "acme", "a-repo")
	trackPgRepo(t, h, orgID, teamB, "acme", "b-repo")
	seedPgJiraRule(t, h, teamA, "SKY")
	seedPgJiraRule(t, h, teamB, "BOB")

	ghA := seedPgGitHubEntityRaw(t, h, orgID, "acme", "a-repo", 1)
	ghB := seedPgGitHubEntityRaw(t, h, orgID, "acme", "b-repo", 2)
	jiraA := seedPgJiraEntityRaw(t, h, orgID, "SKY", 1)
	jiraB := seedPgJiraEntityRaw(t, h, orgID, "BOB", 2)

	beltFor := func(t *testing.T, userID string) map[string]bool {
		t.Helper()
		ids := map[string]bool{}
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			rows, e := pgstore.NewForTx(tx, pgtest.SecretKey).Factory.Entities(ctx, orgID, 100, nil)
			if e != nil {
				return e
			}
			for _, r := range rows {
				ids[r.Entity.ID] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("belt for %s: %v", userID, err)
		}
		return ids
	}

	aliceSees := beltFor(t, alice)
	if !aliceSees[ghA] || !aliceSees[jiraA] {
		t.Errorf("alice (teamA) can't see her team's tracked entities (gh=%v jira=%v)", aliceSees[ghA], aliceSees[jiraA])
	}
	if aliceSees[ghB] {
		t.Errorf("alice (teamA) saw teamB's GitHub entity %s — tracked-repo scope leaked across the team boundary", ghB)
	}
	if aliceSees[jiraB] {
		t.Errorf("alice (teamA) saw teamB's Jira entity %s — tracked-project scope leaked across the team boundary", jiraB)
	}

	bobSees := beltFor(t, bob)
	if !bobSees[ghB] || !bobSees[jiraB] {
		t.Errorf("bob (teamB) can't see his team's tracked entities (gh=%v jira=%v)", bobSees[ghB], bobSees[jiraB])
	}
	if bobSees[ghA] {
		t.Errorf("bob (teamB) saw teamA's GitHub entity %s — leak", ghA)
	}
	if bobSees[jiraA] {
		t.Errorf("bob (teamB) saw teamA's Jira entity %s — leak", jiraA)
	}
}

// TestFactoryReadStore_Postgres_CountersScopedToTrackedSet pins that the
// station-throughput aggregates (EventCountsSince, TaskCountsSince,
// LifetimeDistinctByEventType) carry the same tracked-set scope as the
// belt. events RLS is org-wide, so without the semi-join an event on a PR
// outside the tracked set would inflate the station header even though
// that PR never appears on the belt; TaskCountsSince re-scopes the same
// way so a task on an untracked entity (e.g. minted before the repo was
// untracked) doesn't inflate "Triggered24h". Runs under AdminDB, so the
// semi-join reduces to the org-scoped tracked-set check.
func TestFactoryReadStore_Postgres_CountersScopedToTrackedSet(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgFactoryOrg(t, h)
	promptID := seedPgFactoryPrompt(t, h, orgID, userID)
	seed := newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)

	now := time.Now().UTC()

	// Tracked entity (seed.Entity auto-registers its repo): one opened
	// event + a task, both in-window — both must count.
	tracked := seed.Entity(t, "ctr-tracked")
	evT := seed.Event(t, tracked, "github:pr:opened", "", now.Add(-10*time.Minute), time.Time{})
	seed.Task(t, tracked, "github:pr:opened", "", evT, "queued", now.Add(-10*time.Minute))

	// Untracked entity: its events AND its task must NOT contribute to any
	// counter — it's off the belt.
	untracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "untracked", 9)
	evU := seed.Event(t, untracked, "github:pr:opened", "", now.Add(-5*time.Minute), time.Time{})
	seed.Event(t, untracked, "github:pr:merged", "", now.Add(-5*time.Minute), time.Time{})
	seed.Task(t, untracked, "github:pr:merged", "", evU, "queued", now.Add(-5*time.Minute))

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	since, err := stores.Factory.EventCountsSince(ctx, orgID, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("EventCountsSince: %v", err)
	}
	if since["github:pr:opened"] != 1 {
		t.Errorf("EventCountsSince[opened] = %d, want 1 (only the tracked entity counts)", since["github:pr:opened"])
	}
	if since["github:pr:merged"] != 0 {
		t.Errorf("EventCountsSince[merged] = %d, want 0 (untracked entity's event must not count)", since["github:pr:merged"])
	}

	taskC, err := stores.Factory.TaskCountsSince(ctx, orgID, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("TaskCountsSince: %v", err)
	}
	if taskC["github:pr:opened"] != 1 {
		t.Errorf("TaskCountsSince[opened] = %d, want 1 (tracked entity's task)", taskC["github:pr:opened"])
	}
	if taskC["github:pr:merged"] != 0 {
		t.Errorf("TaskCountsSince[merged] = %d, want 0 (untracked entity's task must not count)", taskC["github:pr:merged"])
	}

	life, err := stores.Factory.LifetimeDistinctByEventType(ctx, orgID)
	if err != nil {
		t.Fatalf("LifetimeDistinctByEventType: %v", err)
	}
	if life["github:pr:opened"] != 1 {
		t.Errorf("Lifetime[opened] = %d, want 1 (untracked entity excluded)", life["github:pr:opened"])
	}
	if life["github:pr:merged"] != 0 {
		t.Errorf("Lifetime[merged] = %d, want 0 (untracked entity excluded)", life["github:pr:merged"])
	}
}

// TestFactoryReadStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth guarantee: even with the org bind as the only line of defense
// (AdminDB bypasses RLS), org A's queries can't see org B's rows. In
// production the RLS policies add a second layer; this test validates the
// org-bind on its own (the tracked-set semi-join binds org via e.org_id /
// the teams join) so a regression there can't silently rely on RLS to
// compensate. Each entity is auto-tracked by its own org's seeder.
func TestFactoryReadStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA := seedPgFactoryOrg(t, h)
	orgB, userB := seedPgFactoryOrg(t, h)
	promptA := seedPgFactoryPrompt(t, h, orgA, userA)
	promptB := seedPgFactoryPrompt(t, h, orgB, userB)

	seedA := newPgFactorySeeder(h.AdminDB, orgA, userA, promptA)
	seedB := newPgFactorySeeder(h.AdminDB, orgB, userB, promptB)

	now := time.Now().UTC()
	// seed.Entity registers each entity's repo for its own org's default
	// team, so both clear the tracked-set semi-join in their own org — this
	// test isolates the org bind, not the tracked-set membership.
	entA := seedA.Entity(t, "cross-A")
	entB := seedB.Entity(t, "cross-B")
	seedA.Event(t, entA, "github:pr:opened", "", now, time.Time{})
	seedB.Event(t, entB, "github:pr:merged", "", now, time.Time{})

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	// Org A's snapshot must NOT include org B's event.
	countsA, err := stores.Factory.LifetimeDistinctByEventType(ctx, orgA)
	if err != nil {
		t.Fatalf("LifetimeDistinctByEventType orgA: %v", err)
	}
	if countsA["github:pr:merged"] != 0 {
		t.Errorf("orgA saw orgB's merged event — org_id filter leaked")
	}
	if countsA["github:pr:opened"] != 1 {
		t.Errorf("orgA counts[opened] = %d, want 1", countsA["github:pr:opened"])
	}

	// Symmetric.
	countsB, err := stores.Factory.LifetimeDistinctByEventType(ctx, orgB)
	if err != nil {
		t.Fatalf("LifetimeDistinctByEventType orgB: %v", err)
	}
	if countsB["github:pr:opened"] != 0 {
		t.Errorf("orgB saw orgA's opened event — org_id filter leaked")
	}
	if countsB["github:pr:merged"] != 1 {
		t.Errorf("orgB counts[merged] = %d, want 1", countsB["github:pr:merged"])
	}

	// Entities is the other broad read — pin it too.
	entsA, err := stores.Factory.Entities(ctx, orgA, 100, nil)
	if err != nil {
		t.Fatalf("Entities orgA: %v", err)
	}
	for _, e := range entsA {
		if e.Entity.ID == entB {
			t.Errorf("orgA Entities() returned orgB entity %s", entB)
		}
	}
}
