package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
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

// TestEntityReads_Postgres runs the shared entity and event list conformance
// suite against the Postgres impls. Each subtest gets a fresh org + team +
// user + seed prompt, then seeds its fixtures via raw INSERTs against the
// admin pool. Both pools are wired to AdminDB, so the tracked-set predicate
// reduces to the org's tracking rows; the per-viewer narrowing RLS adds is
// pinned by TestEntityReads_Postgres_CrossTeamIsolation_RLS. Skips cleanly
// when Docker isn't available — pgtest.Shared handles that.
func TestEntityReads_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunEntityReadConformance(t, func(t *testing.T) (db.EntityStore, db.EventStore, string, dbtest.FlowSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgFlowOrg(t, h)
		promptID := seedPgFlowPrompt(t, h, orgID, userID)
		seeder := newPgFlowSeeder(h.AdminDB, orgID, userID, promptID)
		seeder.GroupedEntity = func(t *testing.T, source string) (string, string) {
			t.Helper()
			teamID := firstTeamForOrg(t, h, orgID)
			switch source {
			case "github":
				repo := pgtest.SeedTrackedRepo(t, h, orgID, teamID, "acme", "widget")
				return seedPgGitHubEntityRaw(t, h, orgID, "acme", "widget", 7), repo
			case "jira":
				project := seedPgJiraRule(t, h, teamID, "SKY")
				return seedPgJiraEntityRaw(t, h, orgID, "SKY", 3), project
			}
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, 'ws-test')
				ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id`, orgID)
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name)
				VALUES ($1, 'ws-test', 'lt-eng', 'ENG', 'Engineering')`, teamID)
			id := uuid.New().String()
			pgtest.MustExec(t, h.AdminDB, `
				INSERT INTO entities (id, org_id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
				VALUES ($1, $2, 'linear', 'ENG-1', 'uuid-ENG-1', 'issue', 'ENG-1', '', '{}'::jsonb, $3, 'ws-test', 'lt-eng')`,
				id, orgID, time.Now().UTC())
			return id, "lt-eng"
		}
		return stores.Entities, stores.Events, orgID, seeder
	})
}

// seedPgFlowOrg builds the auth.user + public.user + org + org_membership +
// default team graph the flow seeder's FK chain needs. Mirrors
// seedPgOrgUserAgent from tasks_test.go but skips the agent half — the
// entity reads and the activity node don't touch the agents table.
func seedPgFlowOrg(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("flow-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)

	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Flow Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Flow Org "+orgID[:8], "flow-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// The org is on github.com, the host its pull request entities are
	// scoped to.
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_event_sources (org_id, kind, base_url) VALUES ($1, 'github', 'https://github.com')`, orgID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

// seedPgFlowPrompt inserts a user-source prompt that conversations can
// FK into. team_id is read from the org's default team (created by
// seedPgFlowOrg via seedPgDefaultTeam). source='user' satisfies
// prompts_system_has_no_creator (creator must be non-NULL).
func seedPgFlowPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	promptID := "p_flow_" + uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'Flow Test', 'body', 'user', '', now(), now())
	`, promptID, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return promptID
}

// newPgFlowSeeder builds the FlowSeeder callbacks against the
// admin pool. Every INSERT carries org_id so RLS would pass even if
// the store ran on the app pool — defense-in-depth without test-
// side complication. The default team for the org backs every task
// + run insertion's team_id requirement.
func newPgFlowSeeder(conn *sql.DB, orgID, userID, promptID string) dbtest.FlowSeeder {
	return dbtest.FlowSeeder{
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
			repositoryID := dbseed.GitHubRepository(t, conn, orgID, dbtest.TestGitHubHost, owner, repo)
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
				VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6, 'https://github.com', $7)
			`, id, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC(), repositoryID); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			if _, err := conn.Exec(`
				INSERT INTO team_github_repos (team_id, repository_id, org_id)
				VALUES ((SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1),
				        (SELECT id FROM repositories
				          WHERE org_id = $1 AND host = $4 AND lower(owner) = lower($2) AND lower(repo) = lower($3)),
				        $1)
				ON CONFLICT (team_id, repository_id) DO NOTHING
			`, orgID, owner, repo, dbtest.TestGitHubHost); err != nil {
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
			// One running blueprint_run per task is a schema invariant
			// (blueprint_runs_one_active_run_per_task), so staging a task's next
			// engagement settles the one before it — which is what the task moving on
			// means.
			if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE org_id = $1 AND task_id = $2 AND status = 'running'`, orgID, taskID); err != nil {
				t.Fatalf("settle the task's prior blueprint_run: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, org_id, team_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
				VALUES ($1, $2, (SELECT team_id FROM blueprints WHERE id = $4 AND org_id = $2), $3, $4, $5, 'manual', 'running', '/tmp/wt', now(), '[]')
			`, brID, orgID, userID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			id := uuid.New().String()
			// "running" is an engagement, not a stored value — mint the
			// claim the real claim path would and leave the column NULL.
			resolved, concluded := dbtest.SeedStatus(status)
			stored := any(resolved)
			if status == "running" {
				stored = nil
			}
			if _, err := conn.Exec(`
				INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, trigger_type, status, blueprint_run_id, completed_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, 'manual', $6, $7, CASE WHEN $8 THEN now() END)
			`, id, orgID, userID, taskID, promptID, stored, brID, concluded); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			if status == "running" {
				if _, err := conn.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, lease_expires_at)
					VALUES ($1, $2, $3, 'flow-seed-executor', 1, now(), now() + interval '300 seconds')
				`, uuid.New().String(), orgID, id); err != nil {
					t.Fatalf("seed claim: %v", err)
				}
			}
			return id
		},
		FinishConversation: func(t *testing.T, conversationID, status string, completedAt time.Time) {
			t.Helper()
			var at any
			if !completedAt.IsZero() {
				at = completedAt.UTC()
			}
			if _, err := conn.Exec(
				`UPDATE conversations SET status = $1, completed_at = $2 WHERE id = $3 AND org_id = $4`,
				status, at, conversationID, orgID,
			); err != nil {
				t.Fatalf("finish conversation: %v", err)
			}
		},
		WarmClaim: func(t *testing.T, conversationID string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE claims SET phase = $1 WHERE conversation_id = $2 AND org_id = $3 AND released_at IS NULL`,
				domain.ClaimPhaseWarm, conversationID, orgID,
			); err != nil {
				t.Fatalf("warm claim: %v", err)
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
		SetEntityKind: func(t *testing.T, entityID, kind string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE entities SET kind = $1 WHERE id = $2 AND org_id = $3`, kind, entityID, orgID,
			); err != nil {
				t.Fatalf("set entity kind: %v", err)
			}
		},
		SetConversationMemory: func(t *testing.T, conversationID, content string, source domain.MemorySource) {
			t.Helper()
			memID := uuid.New().String()
			var agentContent any
			if content != dbtest.NullMemorySentinel {
				agentContent = content
			}
			// Raw SQL, deliberately: this seeder's subject is the
			// memory_missing derivation, not the store door's validation.
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, org_id, conversation_id, agent_content, source)
				VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, conversationID, agentContent, string(source)); err != nil {
				t.Fatalf("seed conversation_memory (source=%s): %v", source, err)
			}
		},
	}
}

// trackPgRepo registers (owner, repo) as a tracked repo for teamID — the
// multi-mode entity reads' GitHub visibility gate. The registry row comes
// first, because that is what the tracking row references.
func trackPgRepo(t *testing.T, h *pgtest.Harness, orgID, teamID, owner, repo string) {
	t.Helper()
	pgtest.SeedTrackedRepo(t, h, orgID, teamID, owner, repo)
}

// seedPgGitHubEntityRaw / seedPgJiraEntityRaw insert an active entity with
// an explicit "owner/repo#N" / "KEY-N" source_id and return its ID,
// WITHOUT touching the tracked set — the tests below control tracking
// themselves to exercise the tracked-set semi-join's include/exclude
// boundary. (newPgFlowSeeder.Entity auto-tracks; these don't.)
func seedPgGitHubEntityRaw(t *testing.T, h *pgtest.Harness, orgID, owner, repo string, number int) string {
	t.Helper()
	return seedPgGitHubEntityOnHost(t, h, orgID, dbtest.TestGitHubHost, owner, repo, number)
}

// seedPgGitHubEntityOnHost is seedPgGitHubEntityRaw with the entity's scope —
// the GitHub host it was polled from — named.
func seedPgGitHubEntityOnHost(t *testing.T, h *pgtest.Harness, orgID, host, owner, repo string, number int) string {
	t.Helper()
	id := uuid.New().String()
	sourceID := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	repositoryID := dbseed.GitHubEntityRepository(t, h.AdminDB, orgID, host, sourceID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
		VALUES ($1, $2, 'github', $3, 'pr', $4, '', '{}'::jsonb, $5, $6, $7)
	`, id, orgID, sourceID, "PR "+sourceID, time.Now().UTC(), host, repositoryID); err != nil {
		t.Fatalf("seed github entity %s on %s: %v", sourceID, host, err)
	}
	return id
}

func seedPgJiraEntityRaw(t *testing.T, h *pgtest.Harness, orgID, projectKey string, number int) string {
	t.Helper()
	id := uuid.New().String()
	sourceID := fmt.Sprintf("%s-%d", projectKey, number)
	project := dbseed.JiraProject(t, h.AdminDB, orgID, pgJiraSite, projectKey)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, jira_project_id)
		VALUES ($1, $2, 'jira', $3, $4, 'issue', $5, '', '{}'::jsonb, $6, $7, $8)
	`, id, orgID, sourceID, dbseed.JiraIssueID(sourceID), "Issue "+sourceID, time.Now().UTC(), pgJiraSite, project); err != nil {
		t.Fatalf("seed jira entity %s: %v", sourceID, err)
	}
	return id
}

// seedPgFlowEvent inserts an entity-attached event via the admin pool
// for tests that need an event but not the full newPgFlowSeeder.
func seedPgFlowEvent(t *testing.T, h *pgtest.Harness, orgID, entityID, eventType string, createdAt time.Time) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
	`, uuid.New().String(), orgID, entityID, eventType, createdAt); err != nil {
		t.Fatalf("seed event %s: %v", eventType, err)
	}
}

// TestEntityReads_Postgres_ExcludesUntrackedEntity pins the tracked-set
// semi-join for GitHub: an entity whose repo is in the team's tracked set is
// listed *with no task at all*, while an entity on an untracked repo is
// excluded even when it has an event and is equally untasked. Matching is
// case-insensitive, mirroring TracksRepoSystem. Runs under AdminDB, so the
// EXISTS reduces to the org-scoped semi-join; the per-team RLS narrowing is
// exercised in TestEntityReads_Postgres_CrossTeamIsolation_RLS.
func TestEntityReads_Postgres_ExcludesUntrackedEntity(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFlowOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	now := time.Now().UTC()

	// onTracked: repo in the tracked set, NO task.
	trackPgRepo(t, h, orgID, teamID, "acme", "tracked")
	onTracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "tracked", 1)

	// mixedCase: tracked repo casing differs from the entity's source_id
	// casing — the lower()/lower() match must still surface it.
	trackPgRepo(t, h, orgID, teamID, "acme", "casefold")
	mixedCase := seedPgGitHubEntityRaw(t, h, orgID, "ACME", "CaseFold", 7)

	// onUntracked: no team tracks its repo. It has an event and, like the
	// others, no task — so the tracked-set gate is the only thing on it.
	onUntracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "untracked", 2)
	seedPgFlowEvent(t, h, orgID, onUntracked, "github:pr:opened", now)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got := pgListedEntityIDs(t, stores.Entities, orgID, nil)
	if !got[onTracked] {
		t.Errorf("entity on tracked repo %s missing — tracked-set semi-join must list untasked entities", onTracked)
	}
	if !got[mixedCase] {
		t.Errorf("entity %s (mixed-case owner/repo) missing — tracked-repo match must be case-insensitive", mixedCase)
	}
	if got[onUntracked] {
		t.Errorf("entity on untracked repo %s leaked through — tracked-set semi-join not applied", onUntracked)
	}
}

// TestEntityReads_Postgres_GitHubMatchesTrackingOnItsOwnHost pins the
// host half of the tracked-set semi-join: a pull request entity's scope is the
// GitHub host it was polled from, and it is listed only when a team tracks
// its owner/repo on that host. The same owner/repo tracked on another host is
// a different repository and admits nothing.
func TestEntityReads_Postgres_GitHubMatchesTrackingOnItsOwnHost(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFlowOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	pgtest.SeedTrackedRepoOnHost(t, h, orgID, teamID, dbtest.TestGitHubHost, "acme", "api")
	pgtest.SeedTrackedRepoOnHost(t, h, orgID, teamID, dbtest.TestOtherGitHubHost, "acme", "ghe-only")

	dotcomTracked := seedPgGitHubEntityOnHost(t, h, orgID, dbtest.TestGitHubHost, "acme", "api", 1)
	gheSameSlug := seedPgGitHubEntityOnHost(t, h, orgID, dbtest.TestOtherGitHubHost, "acme", "api", 2)
	gheTracked := seedPgGitHubEntityOnHost(t, h, orgID, dbtest.TestOtherGitHubHost, "ACME", "Ghe-Only", 3)
	dotcomSameSlug := seedPgGitHubEntityOnHost(t, h, orgID, dbtest.TestGitHubHost, "acme", "ghe-only", 4)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got := pgListedEntityIDs(t, stores.Entities, orgID, nil)
	for _, c := range []struct {
		name, id string
		want     bool
	}{
		{"acme/api#1 on github.com, tracked there", dotcomTracked, true},
		{"acme/api#2 on ghe, tracked only on github.com", gheSameSlug, false},
		{"acme/ghe-only#3 on ghe, tracked there", gheTracked, true},
		{"acme/ghe-only#4 on github.com, tracked only on ghe", dotcomSameSlug, false},
	} {
		if got[c.id] != c.want {
			t.Errorf("%s: listed = %v, want %v", c.name, got[c.id], c.want)
		}
	}
}

// TestEntityReads_Postgres_GitHubMatchesTrackingByRepositoryID pins that
// the entity list meets the tracked set on the entity's repository_id and
// reads nothing out of its key. The tracked repository is renamed with no key
// rewritten, and its pull request stays listed; another entity whose key
// spells the tracked repository's new name but which references an untracked
// row stays unlisted.
func TestEntityReads_Postgres_GitHubMatchesTrackingByRepositoryID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFlowOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	trackPgRepo(t, h, orgID, teamID, "acme", "old")
	tracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "old", 1)
	var trackedRepositoryID string
	if err := h.AdminDB.QueryRow(`SELECT repository_id FROM entities WHERE id = $1`, tracked).Scan(&trackedRepositoryID); err != nil {
		t.Fatalf("read repository_id: %v", err)
	}
	dbseed.RenameRepositoryOnly(t, h.AdminDB, trackedRepositoryID, "acme", "new")

	untrackedRepositoryID := dbseed.GitHubRepository(t, h.AdminDB, orgID, dbtest.TestGitHubHost, "acme", "elsewhere")
	keyLies := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
		VALUES ($1, $2, 'github', 'acme/new#2', 'pr', 'PR', '', '{}'::jsonb, now(), $3, $4)
	`, keyLies, orgID, dbtest.TestGitHubHost, untrackedRepositoryID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	// The unfiltered list and the one narrowed to the team read the two
	// variants of the predicate.
	for _, teamIDs := range [][]string{nil, {teamID}} {
		got := pgListedEntityIDs(t, stores.Entities, orgID, teamIDs)
		if !got[tracked] {
			t.Errorf("teams %v: pull request in the renamed tracked repository is not listed; its key still spells acme/old", teamIDs)
		}
		if got[keyLies] {
			t.Errorf("teams %v: entity keyed acme/new#2 in an untracked repository is listed; the key was read, not the id", teamIDs)
		}
	}
}

// TestEntityReads_Postgres_JiraScopedToTrackedProject is the Jira
// mirror of the GitHub test: a Jira entity whose project key is attached
// to one of the viewer's teams (a jira_project_status_rules row) is in the
// tracked set; one whose project no team configured is not. Pins that the
// tracked set's Jira arm scopes by tracked project, not task existence.
func TestEntityReads_Postgres_JiraScopedToTrackedProject(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFlowOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	// teamID attaches project SKY; PROJ is attached by no team.
	seedPgJiraRule(t, h, teamID, "SKY")
	onAttached := seedPgJiraEntityRaw(t, h, orgID, "SKY", 1)
	onUnattached := seedPgJiraEntityRaw(t, h, orgID, "PROJ", 2)

	got, err := pgstore.TrackedEntityIDsForTest(context.Background(), h.AdminDB, orgID, nil)
	if err != nil {
		t.Fatalf("tracked set: %v", err)
	}
	if !got[onAttached] {
		t.Errorf("Jira entity on attached project %s missing — tracked-project semi-join dropped a member", onAttached)
	}
	if got[onUnattached] {
		t.Errorf("Jira entity on unattached project %s leaked through — tracked-project semi-join not applied", onUnattached)
	}
}

// TestEntityReads_Postgres_JiraFollowsTheRegistryAfterAKeyRename: the
// tracked set, and the team activity node that reads it, place a Jira issue
// by the registry row it references, never by its key's prefix. After the
// project's key is renamed the issue stays in its team's tracked set and its
// events stay counted — once the issue is renamed while the registry still
// holds the old key, and again once the registry is re-keyed. An issue in
// another project whose key now carries the renamed project's key is in
// neither.
func TestEntityReads_Postgres_JiraFollowsTheRegistryAfterAKeyRename(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, _ := seedPgFlowOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	project := seedPgJiraRule(t, h, teamID, "OLD")
	issue, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, orgID, pgJiraSite, "OLD-1", "10001", dbseed.JiraIssueProject("OLD-1"), "Renamed", "")
	if err != nil {
		t.Fatalf("seed OLD-1: %v", err)
	}
	keyLies, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, orgID, pgJiraSite, "NEW-9", "10009", dbseed.JiraIssueProject("NEW-9"), "Untracked", "")
	if err != nil {
		t.Fatalf("seed NEW-9: %v", err)
	}
	now := time.Now().UTC()
	seedPgEvent(t, h, orgID, issue.ID, domain.EventJiraIssueAssigned, now.Add(-time.Minute))
	seedPgEvent(t, h, orgID, keyLies.ID, domain.EventJiraIssueAssigned, now.Add(-time.Minute))

	check := func(stage string) {
		t.Helper()
		for _, teamIDs := range [][]string{nil, {teamID}} {
			got, err := pgstore.TrackedEntityIDsForTest(ctx, h.AdminDB, orgID, teamIDs)
			if err != nil {
				t.Fatalf("tracked set (%v): %v", teamIDs, err)
			}
			if !got[issue.ID] {
				t.Errorf("%s, teams %v: the renamed project's issue left the tracked set", stage, teamIDs)
			}
			if got[keyLies.ID] {
				t.Errorf("%s, teams %v: an untracked project's issue keyed NEW is in the tracked set; the key was read, not the row", stage, teamIDs)
			}
		}
		activity, err := stores.TeamActivity.TeamActivity(ctx, orgID, []string{teamID}, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatalf("TeamActivity: %v", err)
		}
		for _, row := range activity.BySource {
			if row.Source == "jira" && (row.Events == nil || *row.Events != 1) {
				t.Errorf("%s: jira events = %v, want the renamed project's one event", stage, row.Events)
			}
		}
	}

	if _, err := stores.Entities.RenameSystem(ctx, orgID, "jira", pgJiraSite, "10001", "NEW-1", ""); err != nil {
		t.Fatalf("RenameSystem: %v", err)
	}
	check("issue renamed, registry still on OLD")
	if _, err := stores.JiraProjects.RefreshSystem(ctx, orgID, project, "id-OLD", "NEW", ""); err != nil {
		t.Fatalf("RefreshSystem: %v", err)
	}
	check("registry re-keyed to NEW")
}

// TestEntityReads_Postgres_LinearScopedToTrackedTeam is the Linear
// mirror: a Linear entity is in the tracked set when its Linear team is one
// the org's teams track (a linear_team_rules row, watched or armed), and not
// otherwise. The per-team narrowing reads the same arm.
func TestEntityReads_Postgres_LinearScopedToTrackedTeam(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgFlowOrg(t, h)
	teamID := firstTeamForOrg(t, h, orgID)

	bindWorkspace := func(workspaceID string) {
		t.Helper()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, $2)
			ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id
		`, orgID, workspaceID); err != nil {
			t.Fatalf("bind linear workspace: %v", err)
		}
	}
	bindWorkspace("ws-test")
	if _, err := h.AdminDB.Exec(`
		INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name)
		VALUES ($1, 'ws-test', 'lt-eng', 'ENG', 'Engineering')
	`, teamID); err != nil {
		t.Fatalf("seed linear team rule: %v", err)
	}
	seed := func(identifier, linearTeamID string, snapshot any) string {
		t.Helper()
		id := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, external_id, kind, title, url, snapshot_json, created_at, scope, linear_team_id)
			VALUES ($1, $2, 'linear', $3, $4, 'issue', $3, '', $5::jsonb, $6, 'ws-test', $7)
		`, id, orgID, identifier, "uuid-"+identifier, snapshot, time.Now().UTC(), linearTeamID); err != nil {
			t.Fatalf("seed linear entity %s: %v", identifier, err)
		}
		return id
	}
	tracked := seed("ENG-1", "lt-eng", `{"identifier":"ENG-1","team_id":"lt-eng"}`)
	untracked := seed("OPS-1", "lt-ops", `{"identifier":"OPS-1","team_id":"lt-ops"}`)
	// A snapshot the source pause cleared: the team is the column's, so the
	// row stays in its team's tracked set.
	cleared := seed("ENG-2", "lt-eng", nil)
	// A snapshot that names a team the row is not in is not where the
	// tracked set looks.
	snapshotOnly := seed("OPS-2", "lt-ops", `{"identifier":"OPS-2","team_id":"lt-eng"}`)

	read := func(teamIDs []string) map[string]bool {
		t.Helper()
		got, err := pgstore.TrackedEntityIDsForTest(context.Background(), h.AdminDB, orgID, teamIDs)
		if err != nil {
			t.Fatalf("tracked set: %v", err)
		}
		return got
	}
	got := read(nil)
	if !got[tracked] {
		t.Error("Linear entity on a tracked Linear team is missing")
	}
	if got[untracked] {
		t.Error("Linear entity on an untracked Linear team leaked through")
	}
	if !got[cleared] {
		t.Error("Linear entity with a cleared snapshot left its tracked team's set")
	}
	if got[snapshotOnly] {
		t.Error("the tracked set took a Linear entity's team from its snapshot rather than its column")
	}
	if perTeam := read([]string{teamID}); !perTeam[tracked] || !perTeam[cleared] {
		t.Error("the per-team filter dropped a Linear entity its team tracks")
	}
	if read([]string{uuid.New().String()})[tracked] {
		t.Error("the per-team filter kept a Linear entity for a team that does not track it")
	}

	// A rule counts only while the org's credential belongs to the workspace
	// it was saved under.
	bindWorkspace("ws-other")
	if read(nil)[tracked] || read([]string{teamID})[tracked] {
		t.Error("a rule from the workspace the org moved off kept its team's issue in the tracked set")
	}
}

// TestEntityReads_Postgres_CrossTeamIsolation_RLS proves the per-team
// narrowing the production app pool gets for free: driven through tf_app with
// real JWT claims, a viewer only sees entities in their own teams' tracked
// set, in the entity list, in the event list, and in the tracked set the
// activity node reads. alice (teamA only) sees teamA's tracked GitHub, Jira,
// and Slack entities but not teamB's; bob (teamB only) sees the mirror.
func TestEntityReads_Postgres_CrossTeamIsolation_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgID, alice := seedPgFlowOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgID)
	bob := seedPgMember(t, h, orgID, "bob", "member")
	teamB := seedPgDefaultTeam(t, h, orgID, bob)

	// teamA tracks acme/a-repo (GitHub), SKY (Jira), and #a (Slack); teamB
	// tracks acme/b-repo, BOB, and #b. Same org, disjoint tracked sets.
	trackPgRepo(t, h, orgID, teamA, "acme", "a-repo")
	trackPgRepo(t, h, orgID, teamB, "acme", "b-repo")
	seedPgJiraRule(t, h, teamA, "SKY")
	seedPgJiraRule(t, h, teamB, "BOB")
	channelA := seedPgSlackChannel(t, h, orgID, "C-A")
	channelB := seedPgSlackChannel(t, h, orgID, "C-B")
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO team_slack_channels (org_id, team_id, slack_channel_id) VALUES ($1, $2, $3), ($1, $4, $5)`,
		orgID, teamA, channelA, teamB, channelB)

	ghA := seedPgGitHubEntityRaw(t, h, orgID, "acme", "a-repo", 1)
	ghB := seedPgGitHubEntityRaw(t, h, orgID, "acme", "b-repo", 2)
	jiraA := seedPgJiraEntityRaw(t, h, orgID, "SKY", 1)
	jiraB := seedPgJiraEntityRaw(t, h, orgID, "BOB", 2)
	slackA := seedPgSlackThread(t, h, orgID, channelA, "C-A")
	slackB := seedPgSlackThread(t, h, orgID, channelB, "C-B")

	now := time.Now().UTC()
	for _, id := range []string{ghA, ghB, jiraA, jiraB, slackA, slackB} {
		seedPgFlowEvent(t, h, orgID, id, domain.EventGitHubPROpened, now)
	}

	type view struct{ listed, eventsOf, tracked map[string]bool }
	viewFor := func(t *testing.T, userID string) view {
		t.Helper()
		v := view{listed: map[string]bool{}, eventsOf: map[string]bool{}}
		err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
			stores := pgstore.NewForTx(tx, pgtest.SecretKey)
			items, _, e := stores.Entities.List(ctx, orgID, db.EntityListFilter{}, db.Unwindowed)
			if e != nil {
				return e
			}
			for _, it := range items {
				v.listed[it.Entity.ID] = true
			}
			evs, _, e := stores.Events.List(ctx, orgID, db.EventListFilter{}, db.Unwindowed)
			if e != nil {
				return e
			}
			for _, ev := range evs {
				v.eventsOf[*ev.Event.EntityID] = true
			}
			v.tracked, e = pgstore.TrackedEntityIDsForTest(ctx, tx, orgID, nil)
			return e
		})
		if err != nil {
			t.Fatalf("view for %s: %v", userID, err)
		}
		return v
	}

	check := func(who string, v view, ours, theirs [3]string) {
		t.Helper()
		for i, source := range []string{"GitHub", "Jira", "Slack"} {
			for name, seen := range map[string]map[string]bool{"entity list": v.listed, "event list": v.eventsOf, "tracked set": v.tracked} {
				if !seen[ours[i]] {
					t.Errorf("%s can't see their team's %s entity in the %s", who, source, name)
				}
				if seen[theirs[i]] {
					t.Errorf("%s saw the other team's %s entity %s in the %s — tracked-set scope leaked across the team boundary", who, source, theirs[i], name)
				}
			}
		}
	}
	check("alice (teamA)", viewFor(t, alice), [3]string{ghA, jiraA, slackA}, [3]string{ghB, jiraB, slackB})
	check("bob (teamB)", viewFor(t, bob), [3]string{ghB, jiraB, slackB}, [3]string{ghA, jiraA, slackA})
}

// TestEntityReads_Postgres_EventListScopedToTrackedSet pins that the event
// list carries the entity list's tracked-set scope. events RLS is org-wide, so
// without the semi-join an event on a pull request outside the tracked set
// would be listed though its entity is not. Runs under AdminDB, so the
// semi-join reduces to the org-scoped tracked-set check.
func TestEntityReads_Postgres_EventListScopedToTrackedSet(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgFlowOrg(t, h)
	promptID := seedPgFlowPrompt(t, h, orgID, userID)
	seed := newPgFlowSeeder(h.AdminDB, orgID, userID, promptID)
	now := time.Now().UTC()

	tracked := seed.Entity(t, "ev-tracked")
	ours := seed.Event(t, tracked, "github:pr:opened", "", now.Add(-10*time.Minute), time.Time{})
	untracked := seedPgGitHubEntityRaw(t, h, orgID, "acme", "untracked", 9)
	seed.Event(t, untracked, "github:pr:merged", "", now.Add(-5*time.Minute), time.Time{})

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	for _, f := range []db.EventListFilter{{}, {EntityIDs: []string{tracked, untracked}}} {
		rows, total, err := stores.Events.List(context.Background(), orgID, f, db.Unwindowed)
		if err != nil {
			t.Fatalf("Events.List(%+v): %v", f, err)
		}
		if total != 1 || len(rows) != 1 || rows[0].Event.ID != ours {
			t.Errorf("Events.List(%+v) = %d rows of %d, want the tracked entity's one event", f, len(rows), total)
		}
	}
}

// TestEntityReads_Postgres_CrossOrgLeakage pins the defense-in-depth
// guarantee: even with the org bind as the only line of defense (AdminDB
// bypasses RLS), org A's reads can't see org B's rows. In production the RLS
// policies add a second layer; this test validates the org bind on its own
// (the tracked-set semi-join binds org via e.org_id / the teams join) so a
// regression there can't silently rely on RLS to compensate. Each entity is
// auto-tracked by its own org's seeder.
func TestEntityReads_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA := seedPgFlowOrg(t, h)
	orgB, userB := seedPgFlowOrg(t, h)
	promptA := seedPgFlowPrompt(t, h, orgA, userA)
	promptB := seedPgFlowPrompt(t, h, orgB, userB)

	seedA := newPgFlowSeeder(h.AdminDB, orgA, userA, promptA)
	seedB := newPgFlowSeeder(h.AdminDB, orgB, userB, promptB)

	now := time.Now().UTC()
	entA := seedA.Entity(t, "cross-A")
	entB := seedB.Entity(t, "cross-B")
	evA := seedA.Event(t, entA, "github:pr:opened", "", now, time.Time{})
	evB := seedB.Event(t, entB, "github:pr:merged", "", now, time.Time{})

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	for _, c := range []struct{ org, ent, ev, otherEnt, otherEv string }{
		{orgA, entA, evA, entB, evB},
		{orgB, entB, evB, entA, evA},
	} {
		got := pgListedEntityIDs(t, stores.Entities, c.org, nil)
		if !got[c.ent] || got[c.otherEnt] {
			t.Errorf("org %s entity list = %v, want its own entity %s and not %s", c.org, got, c.ent, c.otherEnt)
		}
		// Naming the other org's entity does not reach it either.
		if crossed := pgListedEntityIDs(t, stores.Entities, c.org, nil); crossed[c.otherEnt] {
			t.Errorf("org %s listed the other org's entity", c.org)
		}
		evs, _, err := stores.Events.List(ctx, c.org, db.EventListFilter{EntityIDs: []string{c.ent, c.otherEnt}}, db.Unwindowed)
		if err != nil {
			t.Fatalf("Events.List: %v", err)
		}
		if len(evs) != 1 || evs[0].Event.ID != c.ev {
			t.Errorf("org %s event list = %d rows, want its own event %s alone", c.org, len(evs), c.ev)
		}
	}
}

// pgListedEntityIDs reads the whole entity list, narrowed to teamIDs when
// given, and returns the ids it served.
func pgListedEntityIDs(t *testing.T, entities db.EntityStore, orgID string, teamIDs []string) map[string]bool {
	t.Helper()
	items, _, err := entities.List(context.Background(), orgID, db.EntityListFilter{TeamIDs: teamIDs}, db.Unwindowed)
	if err != nil {
		t.Fatalf("Entities.List(teams %v): %v", teamIDs, err)
	}
	got := make(map[string]bool, len(items))
	for _, it := range items {
		got[it.Entity.ID] = true
	}
	return got
}
