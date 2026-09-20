package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestEntityStore_CloseTerminal_Postgres runs the shared terminating-close
// suite against the Postgres impl. Fixtures are seeded through the harness's
// admin connection (BYPASSRLS) and the stores are wired on the same pool —
// both methods under test are admin-pool only, so that is the pool they run
// on in production too. Skips cleanly when Docker isn't available.
func TestEntityStore_CloseTerminal_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunEntityTerminalCloseConformance(t, func(t *testing.T) (db.EntityStore, db.TaskStore, string, dbtest.EntityTerminalCloseSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := seedPgOrgUserAgent(t, h)
		return stores.Entities, stores.Tasks, orgID, newPgTerminalCloseSeeder(h.AdminDB, orgID, userID)
	})
}

// newPgTerminalCloseSeeder extends the cancel-intent seeder with the entity
// side the terminating-close suite reads and moves.
func newPgTerminalCloseSeeder(conn *sql.DB, orgID, userID string) dbtest.EntityTerminalCloseSeeder {
	base, entityForTask := newPgCloseIntentSeeder(conn, orgID, userID)
	return dbtest.EntityTerminalCloseSeeder{
		TaskCloseCancelIntentSeeder: base,
		EntityForTask: func(t *testing.T, taskID string) string {
			t.Helper()
			id, ok := entityForTask[taskID]
			if !ok {
				t.Fatalf("no seeded entity for task %s", taskID)
			}
			return id
		},
		TaskOnEntity: func(t *testing.T, entityID, eventType string) string {
			t.Helper()
			taskID := uuid.New().String()
			eventID := uuid.New().String()
			now := time.Now().UTC()
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
			`, eventID, orgID, entityID, eventType, now); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
				                   status, scoring_status, priority_score, created_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, '', $6, 'queued', 'pending', 0.5, $7)
			`, taskID, orgID, userID, entityID, eventType, eventID, now); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			entityForTask[taskID] = entityID
			return taskID
		},
		EntityRow: func(t *testing.T, entityID string) (string, int64) {
			t.Helper()
			var state string
			var seq int64
			if err := conn.QueryRow(`SELECT state, poll_seq FROM entities WHERE id = $1 AND org_id = $2`, entityID, orgID).Scan(&state, &seq); err != nil {
				t.Fatalf("read entity %s: %v", entityID, err)
			}
			return state, seq
		},
		BumpPollSeq: func(t *testing.T, entityID string) {
			t.Helper()
			if _, err := conn.Exec(`UPDATE entities SET poll_seq = poll_seq + 1 WHERE id = $1 AND org_id = $2`, entityID, orgID); err != nil {
				t.Fatalf("bump poll_seq for %s: %v", entityID, err)
			}
		},
		CloseEntityRaw: func(t *testing.T, entityID string) {
			t.Helper()
			if _, err := conn.Exec(`UPDATE entities SET state = 'closed', closed_at = now() WHERE id = $1 AND org_id = $2`, entityID, orgID); err != nil {
				t.Fatalf("close entity %s around the guard: %v", entityID, err)
			}
		},
	}
}

// TestEntityStore_CloseTerminal_Postgres_SerializesWithTaskMint pins the lock
// order the two writers share. A mint holding the entity's row lock blocks a
// terminating close until it commits, and the close then walks the task it
// minted; a close holding the lock blocks a mint until it commits, and the
// mint then refuses. Neither order can strand a task on a closed entity.
//
// The mint side is driven as raw SQL on an explicit transaction so the test
// can hold the lock open; the store's own mint takes the same statements in
// the same order (an advisory lock per entity, then the FOR UPDATE read,
// then the insert), and the reverse arm uses the real store method.
func TestEntityStore_CloseTerminal_Postgres_SerializesWithTaskMint(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	orgID, userID, _ := seedPgOrgUserAgent(t, h)
	seed := newPgTerminalCloseSeeder(h.AdminDB, orgID, userID)
	const ci = domain.EventGitHubPRCICheckFailed
	const review = domain.EventGitHubPRReviewChangesRequested

	t.Run("mint_then_close_sees_the_minted_task", func(t *testing.T) {
		taskA := seed.Task(t)
		entityID := seed.EntityForTask(t, taskA)
		eventID := seed.Event(t, taskA)
		_, seq := seed.EntityRow(t, entityID)

		// The mint, mid-transaction: it has locked the entity row and
		// inserted its task, and has not committed.
		mint, err := h.AdminDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin mint tx: %v", err)
		}
		var state string
		if err := mint.QueryRow(`SELECT state FROM entities WHERE org_id = $1 AND id = $2 FOR UPDATE`, orgID, entityID).Scan(&state); err != nil || state != "active" {
			t.Fatalf("mint: lock entity: state=%q err=%v", state, err)
		}
		minted := uuid.New().String()
		if _, err := mint.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
			                   status, scoring_status, priority_score, created_at)
			VALUES ($1, $2, $3,
			        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
			        'team', $4, $5, '', $6, 'queued', 'pending', 0.5, now())
		`, minted, orgID, userID, entityID, review, eventID); err != nil {
			t.Fatalf("mint: insert task: %v", err)
		}

		// The close arrives while the mint holds the lock: it must block.
		type outcome struct {
			res db.TerminalCloseResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := stores.Entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci, review}, "entity_closed", domain.EventGitHubPRMerged, eventID)
			done <- outcome{res, err}
		}()
		select {
		case o := <-done:
			t.Fatalf("close returned while the mint held the entity lock: %+v / %v", o.res, o.err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := mint.Commit(); err != nil {
			t.Fatalf("mint: commit: %v", err)
		}
		var o outcome
		select {
		case o = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("close did not proceed after the mint committed")
		}
		if o.err != nil || !o.res.Closed {
			t.Fatalf("close after the mint: res=%+v err=%v", o.res, o.err)
		}
		if len(o.res.ClosedTaskIDs) != 2 {
			t.Errorf("ClosedTaskIDs = %v, want both the seeded task and the one minted under the lock", o.res.ClosedTaskIDs)
		}
		if got := seed.TaskStatus(t, minted); got != "done" {
			t.Errorf("minted task status = %q, want done — the close that waited must see it", got)
		}
	})

	t.Run("close_then_mint_refuses", func(t *testing.T) {
		taskA := seed.Task(t)
		entityID := seed.EntityForTask(t, taskA)
		eventID := seed.Event(t, taskA)

		// The close, mid-transaction: it holds the entity row lock and has
		// flipped the entity, and has not committed.
		closeTx, err := h.AdminDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin close tx: %v", err)
		}
		var state string
		if err := closeTx.QueryRow(`SELECT state FROM entities WHERE org_id = $1 AND id = $2 FOR UPDATE`, orgID, entityID).Scan(&state); err != nil {
			t.Fatalf("close: lock entity: %v", err)
		}
		if _, err := closeTx.Exec(`UPDATE entities SET state = 'closed', closed_at = now() WHERE org_id = $1 AND id = $2 AND state = 'active'`, orgID, entityID); err != nil {
			t.Fatalf("close: flip entity: %v", err)
		}

		type outcome struct {
			task *domain.Task
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			task, _, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, review, "", eventID, 0.5, time.Now().UTC())
			done <- outcome{task, err}
		}()
		select {
		case o := <-done:
			t.Fatalf("mint returned while the close held the entity lock: %+v / %v", o.task, o.err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := closeTx.Commit(); err != nil {
			t.Fatalf("close: commit: %v", err)
		}
		var o outcome
		select {
		case o = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("mint did not proceed after the close committed")
		}
		if !errors.Is(o.err, db.ErrEntityClosed) {
			t.Fatalf("mint after the close: task=%+v err=%v, want ErrEntityClosed", o.task, o.err)
		}
		if active, err := stores.Tasks.FindActiveByEntitySystem(ctx, orgID, entityID); err != nil || len(active) != 1 {
			t.Errorf("active tasks = %d (err %v), want only the seeded one — the refused mint wrote nothing", len(active), err)
		}
	})
}
