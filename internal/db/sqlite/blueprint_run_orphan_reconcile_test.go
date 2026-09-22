package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// childConversationStatusDB reads a single conversations row's STORED status, with
// SQL NULL (the mid-flight state) as "".
func childConversationStatusDB(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	var s sql.NullString
	if err := conn.QueryRow(`SELECT status FROM conversations WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read conversation %s status: %v", id, err)
	}
	return s.String
}

// TestMarkRunStatus_ParksOrphanedChild_OnTerminal pins the atomic
// guarantee: flipping a blueprint_run to a terminal status must park any
// still-mid-flight child conversation in the same call, so a cancel that
// raced the dispatcher can't strand a child mid-flight (which keeps the
// dispatcher on phantom work and pins its feature branch in a worktree,
// requeuing forever).
func TestMarkRunStatus_ParksOrphanedChild_OnTerminal(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "orphan-atomic")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "oa-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "oa-bp", "Orphan Atomic BP")
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, "oa-bp", []string{"oa-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID := insertBlueprintRunForTest(t, conn, domain.BlueprintRun{
		ID: "oa-br", BlueprintID: "oa-bp", TaskID: task.ID, WorktreePath: "/tmp/wt-oa",
	})
	step0 := 0
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "oa-child", TaskID: task.ID, PromptID: "oa-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = NULL WHERE id = 'oa-child'`); err != nil {
		t.Fatalf("set child running: %v", err)
	}
	// The racing dispatcher already claimed the child; the cancel must
	// release the engagement along with the status flip, not leave it for
	// the janitor.
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id, lease_expires_at)
		VALUES ('oa-claim', 'oa-child', 'exec-oa', strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
	`); err != nil {
		t.Fatalf("seed oa-child claim: %v", err)
	}

	changed, err := stores.Blueprints.MarkRunStatus(ctx, org, brID, domain.BlueprintRunStatusCancelled, "user_cancelled", nil)
	if err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}
	if !changed {
		t.Fatal("MarkRunStatus reported no change")
	}

	if got := childConversationStatusDB(t, conn, "oa-child"); got != "open" {
		t.Errorf("child conversation status = %q, want open (must not strand a child under a terminal parent)", got)
	}
	var parkedAt any
	if err := conn.QueryRow(`SELECT parked_at FROM conversations WHERE id = 'oa-child'`).Scan(&parkedAt); err != nil {
		t.Fatalf("read parked_at: %v", err)
	}
	if parkedAt == nil {
		t.Error("parked child conversation has NULL parked_at; the retention sweep keys off it")
	}
	var releasedAt any
	var outcome string
	if err := conn.QueryRow(`SELECT released_at, COALESCE(outcome, '') FROM claims WHERE id = 'oa-claim'`).Scan(&releasedAt, &outcome); err != nil {
		t.Fatalf("read oa-claim: %v", err)
	}
	if releasedAt == nil || outcome != "cancelled" {
		t.Errorf("parked child's claim = (released=%v, outcome=%q), want (released, cancelled)", releasedAt != nil, outcome)
	}
}

// TestMarkRunStatus_LeavesTerminalChild pins that a clean finish (the common
// path, where the finishing step is already 'completed') does not clobber the
// child's terminal status/outcome.
func TestMarkRunStatus_LeavesTerminalChild(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "orphan-finish")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "of-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "of-bp", "Orphan Finish BP")
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, "of-bp", []string{"of-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID := insertBlueprintRunForTest(t, conn, domain.BlueprintRun{
		ID: "of-br", BlueprintID: "of-bp", TaskID: task.ID, WorktreePath: "/tmp/wt-of",
	})
	step0 := 0
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "of-child", TaskID: task.ID, PromptID: "of-p0", Status: "completed",
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = 'completed', outcome = 'finish' WHERE id = 'of-child'`); err != nil {
		t.Fatalf("set child completed: %v", err)
	}

	if _, err := stores.Blueprints.MarkRunStatus(ctx, org, brID, domain.BlueprintRunStatusCompleted, "", nil); err != nil {
		t.Fatalf("MarkRunStatus: %v", err)
	}

	if got := childConversationStatusDB(t, conn, "of-child"); got != "completed" {
		t.Errorf("child conversation status = %q, want completed (a terminal child must not be re-cancelled)", got)
	}
	var outcome string
	if err := conn.QueryRow(`SELECT COALESCE(outcome,'') FROM conversations WHERE id = 'of-child'`).Scan(&outcome); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcome != "finish" {
		t.Errorf("child outcome = %q, want finish (must not be clobbered)", outcome)
	}
}

// TestReconcileOrphanedConversations heals the exact desync: a child conversation
// left 'running' under an already-terminal blueprint_run. The boot sweep must
// cancel it (and only it), leaving children under a still-running parent alone.
func TestReconcileOrphanedConversations(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// Desynced: terminal (cancelled) blueprint_run, child still running.
	taskA := seedEntityEventTask(t, conn, "recon-a")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "ra-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "ra-bp", "Recon A BP")
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, "ra-bp", []string{"ra-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps A: %v", err)
	}
	brA := insertBlueprintRunForTest(t, conn, domain.BlueprintRun{
		ID: "ra-br", BlueprintID: "ra-bp", TaskID: taskA.ID, WorktreePath: "/tmp/wt-ra",
	})
	step0 := 0
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "ra-child", TaskID: taskA.ID, PromptID: "ra-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brA, BlueprintStepIndex: &step0,
	})
	// Force the desync directly (bypassing MarkRunStatus's atomic cancel) to
	// mimic a DB broken before the fix landed.
	if _, err := conn.Exec(`UPDATE conversations SET status = NULL WHERE id = 'ra-child'`); err != nil {
		t.Fatalf("set child A running: %v", err)
	}
	if _, err := conn.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = 0 WHERE id = 'ra-br'`); err != nil {
		t.Fatalf("set br A cancelled: %v", err)
	}
	// ra-child streamed two token-bearing messages before the crash; the boot
	// sweep must roll them up onto the run, not leave the columns at 0 (TFAC-473).
	if _, err := conn.Exec(`
		INSERT INTO messages (conversation_id, role, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES ('ra-child', 'assistant', 100, 20, 1000, 7),
		       ('ra-child', 'assistant',  50,  5,  500, 3)
	`); err != nil {
		t.Fatalf("seed ra-child messages: %v", err)
	}

	// A second orphan that never started: a mid-flight child under the same
	// terminal parent that no claim ever picked up (the parent went terminal
	// first). Must also be parked — a claimable step under a non-running parent
	// is never actually claimed, so it would sit in the queue forever.
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "ra-child-queued", TaskID: taskA.ID, PromptID: "ra-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brA, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = NULL WHERE id = 'ra-child-queued'`); err != nil {
		t.Fatalf("set queued orphan mid-flight: %v", err)
	}

	// Healthy: running blueprint_run, child running — must be left alone.
	taskB := seedEntityEventTask(t, conn, "recon-b")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rb-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rb-bp", "Recon B BP")
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, "rb-bp", []string{"rb-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps B: %v", err)
	}
	brB := insertBlueprintRunForTest(t, conn, domain.BlueprintRun{
		ID: "rb-br", BlueprintID: "rb-bp", TaskID: taskB.ID, WorktreePath: "/tmp/wt-rb",
	})
	insertConversationForTest(t, conn, domain.Conversation{
		ID: "rb-child", TaskID: taskB.ID, PromptID: "rb-p0", Status: "running",
		Model: "claude-sonnet-4-6", BlueprintRunID: brB, BlueprintStepIndex: &step0,
	})
	if _, err := conn.Exec(`UPDATE conversations SET status = NULL WHERE id = 'rb-child'`); err != nil {
		t.Fatalf("set child B running: %v", err)
	}
	// A genuinely running child holds an active claim (ClaimNextConversation mints
	// it); without one, the claim-desync requeue arm would rightly treat the
	// row as stranded.
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id, lease_expires_at)
		VALUES ('rb-claim', 'rb-child', 'exec-b', strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
	`); err != nil {
		t.Fatalf("seed rb-child claim: %v", err)
	}

	n, _, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedConversations: %v", err)
	}
	if n != 2 {
		t.Errorf("reconciled count = %d, want 2 (both mid-flight orphans under the terminal parent)", n)
	}
	if got := childConversationStatusDB(t, conn, "ra-child"); got != "open" {
		t.Errorf("orphan child status = %q, want open", got)
	}
	if got := childConversationStatusDB(t, conn, "ra-child-queued"); got != "open" {
		t.Errorf("unclaimed orphan child status = %q, want open", got)
	}
	// The running orphan's streamed tokens still read through the ledger —
	// the sweep is a status flip only, nothing to roll up or lose.
	{
		var in, out, cr, cc int
		if err := conn.QueryRow(`
			SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
			FROM messages WHERE conversation_id = 'ra-child'
		`).Scan(&in, &out, &cr, &cc); err != nil {
			t.Fatalf("read ra-child tokens: %v", err)
		}
		if in != 150 || out != 25 || cr != 1500 || cc != 10 {
			t.Errorf("orphan child ledger tokens = (%d,%d,%d,%d), want (150,25,1500,10)", in, out, cr, cc)
		}
	}
	if got := childConversationStatusDB(t, conn, "rb-child"); got != "" {
		t.Errorf("healthy child status = %q, want none (must not touch mid-flight children under a running parent)", got)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, _, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 {
		t.Errorf("second ReconcileOrphanedConversations = (%d, %v), want (0, nil)", n2, err)
	}
}

// TestReconcileOrphanedConversations_HealsClaimDesyncs pins the janitor arm on the
// SQLite side: a terminal conversation with a dangling active claim gets the
// claim released with the status-mapped outcome, while every healthy shape
// is untouched. A mid-flight conversation with no active claim is NOT a
// desync any more — that shape IS the claimable state — so nothing heals it
// and it stays exactly as it is.
func TestReconcileOrphanedConversations_HealsClaimDesyncs(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "desync")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "ds-p0", Name: "p0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "ds-bp", "Desync BP")
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, "ds-bp", []string{"ds-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID := insertBlueprintRunForTest(t, conn, domain.BlueprintRun{
		ID: "ds-br", BlueprintID: "ds-bp", TaskID: task.ID, WorktreePath: "/tmp/wt-ds",
	})
	step0 := 0
	seedChild := func(id, status string) {
		t.Helper()
		insertConversationForTest(t, conn, domain.Conversation{
			ID: id, TaskID: task.ID, PromptID: "ds-p0", Status: "running",
			Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
		})
		if _, err := conn.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatalf("set %s status: %v", id, err)
		}
	}
	activeClaim := func(claimID, convID string) {
		t.Helper()
		if _, err := conn.Exec(`
			INSERT INTO claims (id, conversation_id, executor_id, lease_expires_at)
			VALUES (?, ?, 'exec-ds', strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
		`, claimID, convID); err != nil {
			t.Fatalf("seed claim %s: %v", claimID, err)
		}
	}

	// Dangling claims on terminal rows (the crash-after-flip shape).
	seedChild("ds-done", "completed")
	activeClaim("ds-done-cl", "ds-done")
	seedChild("ds-failed", "failed")
	activeClaim("ds-failed-cl", "ds-failed")
	// Mid-flight row with no claim: under the derived model this is simply
	// a claimable conversation, so the sweep must leave it (and its stale
	// placement stamp, which the next claim re-earns) alone.
	seedChild("ds-stranded", "")
	if _, err := conn.Exec(`UPDATE conversations SET preferred_executor_id = 'exec-dead' WHERE id = 'ds-stranded'`); err != nil {
		t.Fatalf("stamp placement: %v", err)
	}
	// Healthy shapes: mid-flight with a live claim; mid-flight with none.
	seedChild("ds-healthy", "")
	activeClaim("ds-healthy-cl", "ds-healthy")
	seedChild("ds-queued", "")

	n, _, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx)
	if err != nil {
		t.Fatalf("ReconcileOrphanedConversations: %v", err)
	}
	if n != 2 {
		t.Errorf("healed count = %d, want 2 (two released claims)", n)
	}

	claimState := func(id string) (released bool, outcome string) {
		t.Helper()
		var rel any
		if err := conn.QueryRow(`SELECT released_at, COALESCE(outcome, '') FROM claims WHERE id = ?`, id).Scan(&rel, &outcome); err != nil {
			t.Fatalf("read claim %s: %v", id, err)
		}
		return rel != nil, outcome
	}
	if rel, out := claimState("ds-done-cl"); !rel || out != "completed" {
		t.Errorf("completed row's claim = (released=%v, outcome=%q), want (true, completed)", rel, out)
	}
	if rel, out := claimState("ds-failed-cl"); !rel || out != "failed" {
		t.Errorf("failed row's claim = (released=%v, outcome=%q), want (true, failed)", rel, out)
	}
	if got := childConversationStatusDB(t, conn, "ds-stranded"); got != "" {
		t.Errorf("mid-flight claimless row status = %q, want no stored status (already claimable)", got)
	}
	if rel, _ := claimState("ds-healthy-cl"); rel {
		t.Error("healthy engaged row's live claim was released")
	}
	if got := childConversationStatusDB(t, conn, "ds-healthy"); got != "" {
		t.Errorf("healthy engaged row status = %q, want no stored status", got)
	}
	if got := childConversationStatusDB(t, conn, "ds-queued"); got != "" {
		t.Errorf("claimless row status = %q, want no stored status (already claimable, nothing to heal)", got)
	}

	// Idempotent: a second sweep finds nothing.
	if n2, _, err := stores.ConversationQueue.ReconcileOrphanedConversations(ctx); err != nil || n2 != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", n2, err)
	}
}
