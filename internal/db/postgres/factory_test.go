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
	stores := pgstore.New(h.AdminDB, h.AdminDB)

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

// seedPgFactoryPrompt inserts a user-source prompt that runs can FK
// into. team_id is read from the org's default team (created by
// seedPgFactoryOrg via seedPgDefaultTeam). source='user' satisfies
// prompts_system_has_no_creator (creator must be non-NULL).
func seedPgFactoryPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	promptID := "p_factory_" + uuid.New().String()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, kind, allowed_tools, visibility, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'Factory Test', 'body', 'user', 'leaf', '', 'team', now(), now())
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
			sourceID := fmt.Sprintf("factory-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
			`, id, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
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
		Run: func(t *testing.T, taskID, status string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO runs (id, org_id, creator_user_id, team_id, visibility, task_id, prompt_id, trigger_type, status)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, 'manual', $6)
			`, id, orgID, userID, taskID, promptID, status); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			return id
		},
		CloseEntity: func(t *testing.T, entityID string, closedAt time.Time) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE entities SET state = 'closed', closed_at = $1 WHERE id = $2 AND org_id = $3`,
				closedAt, entityID, orgID,
			); err != nil {
				t.Fatalf("close entity: %v", err)
			}
		},
		SetRunMemory: func(t *testing.T, runID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO run_memory (id, org_id, run_id, entity_id, agent_content)
					VALUES ($1, $2, $3, $4, NULL)
				`, memID, orgID, runID, entityID); err != nil {
					t.Fatalf("seed null run_memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO run_memory (id, org_id, run_id, entity_id, agent_content)
				VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, runID, entityID, content); err != nil {
				t.Fatalf("seed run_memory: %v", err)
			}
		},
	}
}

// TestFactoryReadStore_Postgres_ExcludesUntaskedEntity pins the
// multi-mode membership semi-join: an entity a team has tasked (over
// any task status) rides the factory snapshot, while one no team ever
// tasked is excluded — even when it has events (a station). This is
// the Postgres-only contract; SQLite (local mode) applies no
// membership filter. Runs under AdminDB, so the EXISTS reduces to the
// org-scoped semi-join; the RLS team-scoping it inherits in production
// is a property of tasks_select, exercised by the RLS suite.
func TestFactoryReadStore_Postgres_ExcludesUntaskedEntity(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgFactoryOrg(t, h)
	promptID := seedPgFactoryPrompt(t, h, orgID, userID)
	seed := newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)

	now := time.Now().UTC()
	// withTask: a rule cared about it — earns membership.
	withTask := seed.Entity(t, "memb-tasked")
	ev := seed.Event(t, withTask, "github:pr:opened", "", now, time.Time{})
	seed.Task(t, withTask, "github:pr:opened", "", ev, "queued", now)

	// noTask: never matched a rule. Give it an event so the only thing
	// keeping it out is membership, not a missing station.
	noTask := seed.Entity(t, "memb-untasked")
	seed.Event(t, noTask, "github:pr:opened", "", now, time.Time{})

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	rows, err := stores.Factory.Entities(context.Background(), orgID, 100)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Entity.ID] = true
	}
	if !got[withTask] {
		t.Errorf("tasked entity %s missing — membership semi-join dropped a member", withTask)
	}
	if got[noTask] {
		t.Errorf("untasked entity %s leaked through — membership semi-join not applied", noTask)
	}
}

// TestFactoryReadStore_Postgres_KeepsEntityAfterTaskTerminal pins that
// membership spans all task statuses: an entity whose only task has
// gone terminal (done) still rides the snapshot. "Has had a task but
// has none active now" must stay in the factory — membership is
// monotonic, orthogonal to task lifecycle.
func TestFactoryReadStore_Postgres_KeepsEntityAfterTaskTerminal(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgFactoryOrg(t, h)
	promptID := seedPgFactoryPrompt(t, h, orgID, userID)
	seed := newPgFactorySeeder(h.AdminDB, orgID, userID, promptID)

	now := time.Now().UTC()
	ent := seed.Entity(t, "terminal-task")
	ev := seed.Event(t, ent, "github:pr:opened", "", now, time.Time{})
	seed.Task(t, ent, "github:pr:opened", "", ev, "done", now)

	stores := pgstore.New(h.AdminDB, h.AdminDB)
	rows, err := stores.Factory.Entities(context.Background(), orgID, 100)
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Entity.ID == ent {
			found = true
		}
	}
	if !found {
		t.Errorf("entity %s with a terminal (done) task dropped — membership must span all task statuses", ent)
	}
}

// TestFactoryReadStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth guarantee: even with the org_id filter as the only line of
// defense (AdminDB bypasses RLS), org A's queries can't see org B's
// rows. In production the RLS policies add a second layer; this test
// validates the WHERE-clause filter on its own so a regression there
// can't silently rely on RLS to compensate.
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
	entA := seedA.Entity(t, "cross-A")
	entB := seedB.Entity(t, "cross-B")
	seedA.Event(t, entA, "github:pr:opened", "", now, time.Time{})
	seedB.Event(t, entB, "github:pr:merged", "", now, time.Time{})

	stores := pgstore.New(h.AdminDB, h.AdminDB)
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
	entsA, err := stores.Factory.Entities(ctx, orgA, 100)
	if err != nil {
		t.Fatalf("Entities orgA: %v", err)
	}
	for _, e := range entsA {
		if e.Entity.ID == entB {
			t.Errorf("orgA Entities() returned orgB entity %s", entB)
		}
	}
}
