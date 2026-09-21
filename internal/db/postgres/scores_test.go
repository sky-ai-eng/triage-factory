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
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestScoreStore_Postgres runs the shared conformance suite against
// the Postgres ScoreStore impl. ScoreStore wires against the admin
// pool in production (the scorer is a system service operating across
// users), so the test uses the harness's AdminDB directly — same
// privilege envelope as production.
//
// Each subtest gets a fresh org + user + entities + tasks seeded via
// raw SQL on AdminDB, since TaskStore hasn't migrated yet (wave 3a).
func TestScoreStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	dbtest.RunScoreStoreConformance(t, func(t *testing.T) dbtest.ScoreFixture {
		t.Helper()
		h.Reset(t)
		orgID, userID := seedPgOrgAndUser(t, h)
		return dbtest.ScoreFixture{
			Store: stores.Scores,
			OrgID: orgID,
			Seed: func(t *testing.T, n int) []string {
				t.Helper()
				return seedPgTasks(t, h.AdminDB, orgID, userID, n)
			},
			ReDerive: stores.TaskReDerive,
			ScoreRevision: func(t *testing.T, taskID string) int64 {
				t.Helper()
				return readPgScoreRevision(t, h.AdminDB, taskID)
			},
			QueueRows: func(t *testing.T, taskID string) []dbtest.ReDeriveQueueRow {
				t.Helper()
				return readPgReDeriveRows(t, h.AdminDB, taskID)
			},
		}
	})
}

// readPgScoreRevision reads tasks.score_revision, the one column no domain
// read projects.
func readPgScoreRevision(t *testing.T, conn *sql.DB, taskID string) int64 {
	t.Helper()
	var rev int64
	if err := conn.QueryRow(`SELECT score_revision FROM tasks WHERE id = $1`, taskID).Scan(&rev); err != nil {
		t.Fatalf("read score_revision of %s: %v", taskID, err)
	}
	return rev
}

// readPgReDeriveRows reads a task's task_rederive_queue rows, oldest first,
// in the shape the conformance suites compare.
func readPgReDeriveRows(t *testing.T, conn *sql.DB, taskID string) []dbtest.ReDeriveQueueRow {
	t.Helper()
	rows, err := conn.Query(`
		SELECT id, status, attempt, requested_revision, COALESCE(unique_key, '')
		FROM task_rederive_queue WHERE task_id = $1 ORDER BY id
	`, taskID)
	if err != nil {
		t.Fatalf("read task_rederive_queue rows of %s: %v", taskID, err)
	}
	defer rows.Close()
	out := []dbtest.ReDeriveQueueRow{}
	for rows.Next() {
		var r dbtest.ReDeriveQueueRow
		if err := rows.Scan(&r.ID, &r.Status, &r.Attempt, &r.RequestedRevision, &r.UniqueKey); err != nil {
			t.Fatalf("scan task_rederive_queue row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read task_rederive_queue rows: %v", err)
	}
	return out
}

// TestScoreStore_Postgres_ResetStaleScoring_OrgScoped pins the half of
// the crash-recovery contract the conformance suite can't reach: SQLite
// is N=1 so its harness only ever has one org, while in multi mode the
// per-org runners fire concurrently. A reset that missed its org_id
// predicate would strip the 'in_progress' claim out from under another
// tenant's live cycle, whose tasks would then be scored twice and, worse,
// re-picked while its LLM call was still in flight.
func TestScoreStore_Postgres_ResetStaleScoring_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgA, userA := seedPgOrgAndUser(t, h)
	orgB, userB := seedPgOrgAndUser(t, h)
	residueA := seedPgTasks(t, h.AdminDB, orgA, userA, 2)
	inFlightB := seedPgTasks(t, h.AdminDB, orgB, userB, 3)

	ctx := context.Background()
	if err := stores.Scores.MarkScoring(ctx, orgA, residueA); err != nil {
		t.Fatalf("MarkScoring orgA: %v", err)
	}
	if err := stores.Scores.MarkScoring(ctx, orgB, inFlightB); err != nil {
		t.Fatalf("MarkScoring orgB: %v", err)
	}

	n, err := stores.Scores.ResetStaleScoring(ctx, orgA)
	if err != nil {
		t.Fatalf("ResetStaleScoring: %v", err)
	}
	if n != len(residueA) {
		t.Errorf("ResetStaleScoring(orgA) = %d, want %d", n, len(residueA))
	}

	tasksB, err := stores.Scores.UnscoredTasks(ctx, orgB)
	if err != nil {
		t.Fatalf("UnscoredTasks orgB: %v", err)
	}
	if len(tasksB) != 0 {
		t.Errorf("orgB has %d unscored tasks after orgA's reset, want 0 — its in-flight claims must be untouched", len(tasksB))
	}
	tasksA, err := stores.Scores.UnscoredTasks(ctx, orgA)
	if err != nil {
		t.Fatalf("UnscoredTasks orgA: %v", err)
	}
	if len(tasksA) != len(residueA) {
		t.Errorf("orgA has %d unscored tasks, want %d", len(tasksA), len(residueA))
	}
}

// TestScoreStore_Postgres_ReDeriveAdmission_OrgScoped is the multi-tenant
// half of the re-evaluation contract, which SQLite's N=1 harness can't
// express: the per-org scoring runners write concurrently, so one org's
// score write must admit obligations for its own tasks alone, and the
// operator surface for one org must list none of another's.
func TestScoreStore_Postgres_ReDeriveAdmission_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgA, userA := seedPgOrgAndUser(t, h)
	orgB, userB := seedPgOrgAndUser(t, h)
	tasksA := seedPgTasks(t, h.AdminDB, orgA, userA, 2)
	tasksB := seedPgTasks(t, h.AdminDB, orgB, userB, 2)

	ctx := context.Background()
	updates := make([]domain.TaskScoreUpdate, len(tasksA))
	for i, id := range tasksA {
		updates[i] = domain.TaskScoreUpdate{ID: id, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r"}
	}
	if err := stores.Scores.UpdateTaskScores(ctx, orgA, updates); err != nil {
		t.Fatalf("UpdateTaskScores(orgA): %v", err)
	}

	for _, id := range tasksA {
		rows := readPgReDeriveRows(t, h.AdminDB, id)
		if len(rows) != 1 || rows[0].Status != workitem.StatusReady || rows[0].RequestedRevision != 1 {
			t.Errorf("orgA task %s queue rows = %+v, want one ready row at revision 1", id, rows)
		}
	}
	for _, id := range tasksB {
		if rows := readPgReDeriveRows(t, h.AdminDB, id); len(rows) != 0 {
			t.Errorf("orgB task %s has queue rows %+v after orgA's save", id, rows)
		}
		if rev := readPgScoreRevision(t, h.AdminDB, id); rev != 0 {
			t.Errorf("orgB task %s score_revision = %d after orgA's save", id, rev)
		}
	}

	h2 := stores.TaskReDerive.(db.WorkKindHandle)
	if _, total, err := workitem.List(ctx, h2.Conn(), h2.Kind(), orgB, "", 10, 0); err != nil || total != 0 {
		t.Errorf("orgB lists %d re-evaluations (err %v), want none", total, err)
	}
	if _, total, err := workitem.List(ctx, h2.Conn(), h2.Kind(), orgA, "", 10, 0); err != nil || total != len(tasksA) {
		t.Errorf("orgA lists %d re-evaluations (err %v), want %d", total, err, len(tasksA))
	}
}

// seedPgOrgAndUser creates the org + auth.user + public.user +
// membership rows required by tasks' creator_user_id FK and the RLS
// helpers. ScoreStore runs against AdminDB which bypasses RLS, but
// the FK constraints still fire and the harness needs a coherent
// (org, user) pair for tasks.creator_user_id to satisfy them.
func seedPgOrgAndUser(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("conformance-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)

	// public.users.id FKs to auth.users(id) — seed users before orgs
	// because orgs.owner_user_id NOT NULL references users(id).
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Conformance Org "+orgID[:8], "conf-"+orgID[:8], userID,
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

// seedPgTasks inserts n rows of (entity + event + task) inside the
// given org. Returns the task IDs. All rows hold org_id = orgID and
// creator_user_id = userID so the composite FKs from D3 are satisfied.
func seedPgTasks(t *testing.T, conn *sql.DB, orgID, userID string, n int) []string {
	t.Helper()
	now := time.Now().UTC()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		entityID := uuid.New().String()
		taskID := uuid.New().String()
		eventID := uuid.New().String()
		sourceID := fmt.Sprintf("conformance-pr-%d-%d", i, now.UnixNano())
		// "github:pr:opened" is in the seeded events_catalog
		// (domain.EventGitHubPROpened) — stable FK target.
		eventType := "github:pr:opened"

		if _, err := conn.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
		`, entityID, orgID, sourceID, fmt.Sprintf("Conformance PR %d", i), "https://example/pr/"+sourceID, now); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
		`, eventID, orgID, entityID, eventType, now); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		// team_id resolved inline from the org's first team.
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
			                   status, scoring_status, created_at)
			VALUES ($1, $2, $3, (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1), 'team', $4, $5, '', $6, 'queued', 'pending', $7)
		`, taskID, orgID, userID, entityID, eventType, eventID, now); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		ids = append(ids, taskID)
	}
	return ids
}
