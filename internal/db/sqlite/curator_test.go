package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCuratorStore_SQLite_FullTurn pins the per-turn write set the
// curator goroutine produces against SQLite. Mirrors the Postgres
// attribution test but without RLS — SQLite has no auth concept and
// the assertion is purely behavioral.
func TestCuratorStore_SQLite_FullTurn(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Drive each lifecycle write through SyntheticClaimsWithTx so the
	// test exercises the production goroutine code path.
	var requestID string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, runmode.LocalDefaultOrgID, projectID, runmode.LocalDefaultUserID, "hello")
		if err != nil {
			return err
		}
		requestID = id
		return nil
	}); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, runmode.LocalDefaultOrgID, requestID)
	}); err != nil {
		t.Fatalf("MarkRequestRunning: %v", err)
	}

	// Second MarkRunning should return sql.ErrNoRows because the
	// status filter (status = 'queued') no longer matches.
	err = stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, runmode.LocalDefaultOrgID, requestID)
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second MarkRequestRunning err = %v, want sql.ErrNoRows", err)
	}

	// One token-bearing message — the streaming sink writes these rows
	// before the terminal completion write, and CompleteRequest rolls them
	// up onto curator_requests (TFAC-473).
	ip := func(n int) *int { return &n }
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID:           requestID,
			Role:                "assistant",
			Subtype:             "text",
			Content:             "ack",
			InputTokens:         ip(11),
			OutputTokens:        ip(22),
			CacheReadTokens:     ip(33),
			CacheCreationTokens: ip(44),
		})
		return err
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	// CompleteRequest flips terminal once; second call returns
	// flipped=false because the row is already terminal.
	var flipped bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.CompleteRequest(ctx, runmode.LocalDefaultOrgID, requestID, "done", "", 0.01, 100, 1)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	}); err != nil {
		t.Fatalf("first CompleteRequest: %v", err)
	}
	if !flipped {
		t.Error("first CompleteRequest flipped=false, want true")
	}
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.CompleteRequest(ctx, runmode.LocalDefaultOrgID, requestID, "done", "", 0.02, 200, 2)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	}); err != nil {
		t.Fatalf("second CompleteRequest: %v", err)
	}
	if flipped {
		t.Error("second CompleteRequest flipped=true, want false (already terminal)")
	}

	// GetRequest under the same claims sees the row.
	var seen *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, runmode.LocalDefaultOrgID, requestID)
		if err != nil {
			return err
		}
		seen = r
		return nil
	}); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if seen == nil {
		t.Fatal("GetRequest returned nil for existing row")
	}
	if seen.Status != "done" {
		t.Errorf("status = %q, want done", seen.Status)
	}
	if seen.CreatorUserID != runmode.LocalDefaultUserID {
		t.Errorf("CreatorUserID = %q, want %q", seen.CreatorUserID, runmode.LocalDefaultUserID)
	}
	// Token breakdown rolled up from curator_messages by the first (winning)
	// CompleteRequest and read back via the store's scan path; the second
	// (no-op, already-terminal) call leaves it intact — the status filter
	// blocks the re-roll-up entirely. TFAC-473.
	if seen.InputTokens != 11 || seen.OutputTokens != 22 ||
		seen.CacheReadTokens != 33 || seen.CacheCreationTokens != 44 {
		t.Errorf("token breakdown = (%d,%d,%d,%d), want (11,22,33,44) — SUM over curator_messages, lost or clobbered",
			seen.InputTokens, seen.OutputTokens, seen.CacheReadTokens, seen.CacheCreationTokens)
	}
}

// TestCuratorStore_SQLite_CancelRollsUpTokens pins that the store's cancel
// path (the production curator runtime's terminal write for a user/shutdown
// cancel) refreshes the denormalized token columns from the curator_messages
// SUM, same as CompleteRequest — a request cancelled mid-turn reflects the
// tokens it streamed rather than stranding them at 0 (TFAC-473).
func TestCuratorStore_SQLite_CancelRollsUpTokens(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	user := runmode.LocalDefaultUserID

	projectID, err := stores.Projects.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	ip := func(n int) *int { return &n }
	var requestID string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, org, user, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, org, projectID, user, "hello")
		if err != nil {
			return err
		}
		requestID = id
		if err := ts.Curator.MarkRequestRunning(ctx, org, requestID); err != nil {
			return err
		}
		_, err = ts.Curator.InsertMessage(ctx, org, &domain.CuratorMessage{
			RequestID: requestID, Role: "assistant", Subtype: "text", Content: "ack",
			InputTokens: ip(11), OutputTokens: ip(22), CacheReadTokens: ip(33), CacheCreationTokens: ip(44),
		})
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var flipped bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, org, user, func(ts db.TxStores) error {
		f, err := ts.Curator.MarkRequestCancelledIfActive(ctx, org, requestID, "user cancelled")
		flipped = f
		return err
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !flipped {
		t.Fatal("MarkRequestCancelledIfActive flipped=false, want true")
	}

	var seen *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, org, user, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, org, requestID)
		seen = r
		return err
	}); err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if seen == nil || seen.Status != "cancelled" {
		t.Fatalf("want cancelled row, got %+v", seen)
	}
	if seen.InputTokens != 11 || seen.OutputTokens != 22 ||
		seen.CacheReadTokens != 33 || seen.CacheCreationTokens != 44 {
		t.Errorf("token breakdown = (%d,%d,%d,%d), want (11,22,33,44) — cancel did not roll up curator_messages",
			seen.InputTokens, seen.OutputTokens, seen.CacheReadTokens, seen.CacheCreationTokens)
	}
}

// TestCuratorStore_SQLite_PendingContextRoundTrip pins the consume →
// finalize and consume → revert flows the goroutine uses for pending
// context-change rows. The consume path is the most complex SQL in
// the store (UPDATE-first locking) and needs separate coverage from
// the higher-level fixtures.
func TestCuratorStore_SQLite_PendingContextRoundTrip(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Seed a pending context row directly via the package-level helper
	// (the projects handler calls this on PATCH; the goroutine never
	// inserts pending rows itself, only consumes them).
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["foo/bar"]`); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	requestID, err := db.CreateCuratorRequest(conn, projectID, "consume me")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	var (
		project *domain.Project
		pending []domain.CuratorPendingContext
	)
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		p, ps, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID)
		if err != nil {
			return err
		}
		project = p
		pending = ps
		return nil
	}); err != nil {
		t.Fatalf("ConsumePendingContext: %v", err)
	}
	if project == nil || project.ID != projectID {
		t.Fatalf("Consume returned project %+v, want id=%s", project, projectID)
	}
	if len(pending) != 1 {
		t.Fatalf("Consume returned %d pending rows, want 1", len(pending))
	}
	if pending[0].ChangeType != domain.ChangeTypePinnedRepos {
		t.Errorf("pending row change_type = %q, want %q", pending[0].ChangeType, domain.ChangeTypePinnedRepos)
	}

	// Revert un-consumes the rows.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.RevertPendingContext(ctx, runmode.LocalDefaultOrgID, requestID)
	}); err != nil {
		t.Fatalf("RevertPendingContext: %v", err)
	}
	all, err := stores.Curator.ListPendingContext(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(all) != 1 || all[0].ConsumedAt != nil {
		t.Errorf("after revert, expected 1 unconsumed row; got %+v", all)
	}

	// Re-consume + finalize purges them.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		if _, _, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID); err != nil {
			return err
		}
		return ts.Curator.FinalizePendingContext(ctx, runmode.LocalDefaultOrgID, requestID)
	}); err != nil {
		t.Fatalf("consume+finalize: %v", err)
	}
	all, err = stores.Curator.ListPendingContext(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("after finalize, expected 0 rows; got %d", len(all))
	}
}

// TestCuratorStore_SQLite_RevertCleansAuditRow pins the compound
// revert-and-delete-audit-row path that the goroutine's
// revertPendingFor helper drives on terminal cancel/fail. The audit
// row is the `context_change` curator_messages entry the dispatch
// loop persists when it renders a pending-context note into the
// user's message — if the turn doesn't complete successfully, the
// chat history must not show a phantom "context noted" entry for a
// delta the agent never absorbed.
func TestCuratorStore_SQLite_RevertCleansAuditRow(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["foo/bar"]`); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	requestID, err := db.CreateCuratorRequest(conn, projectID, "msg")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	// Drive consume + audit-row insert under the same identity the
	// goroutine would use — this mirrors the dispatch sequence in
	// session.go around the context-change rendering.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		if _, _, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID); err != nil {
			return err
		}
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "system",
			Subtype:   "context_change",
			Content:   "pinned_repos changed",
		})
		return err
	}); err != nil {
		t.Fatalf("consume + audit insert: %v", err)
	}

	// Audit row should be present before revert.
	auditCount := countMessages(t, conn, requestID, "context_change")
	if auditCount != 1 {
		t.Fatalf("pre-revert audit row count = %d, want 1", auditCount)
	}

	// Revert + DeleteMessagesBySubtype — the exact pair revertPendingFor runs.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		if err := ts.Curator.RevertPendingContext(ctx, runmode.LocalDefaultOrgID, requestID); err != nil {
			return err
		}
		return ts.Curator.DeleteMessagesBySubtype(ctx, runmode.LocalDefaultOrgID, requestID, "context_change")
	}); err != nil {
		t.Fatalf("revert + audit delete: %v", err)
	}

	// Audit row gone.
	if got := countMessages(t, conn, requestID, "context_change"); got != 0 {
		t.Errorf("post-revert audit row count = %d, want 0", got)
	}
	// Pending row re-armed (un-consumed).
	pending, err := stores.Curator.ListPendingContext(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ConsumedAt != nil {
		t.Errorf("expected 1 unconsumed pending row after revert; got %+v", pending)
	}

	// Other-subtype messages on the same request must NOT be touched.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "assistant",
			Subtype:   "text",
			Content:   "should survive",
		})
		return err
	}); err != nil {
		t.Fatalf("insert assistant message: %v", err)
	}
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.DeleteMessagesBySubtype(ctx, runmode.LocalDefaultOrgID, requestID, "context_change")
	}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if got := countMessages(t, conn, requestID, "text"); got != 1 {
		t.Errorf("text subtype message count = %d, want 1 (DeleteMessagesBySubtype clobbered an unrelated subtype)", got)
	}
}

func countMessages(t *testing.T, conn *sql.DB, requestID, subtype string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM curator_messages WHERE request_id = ? AND subtype = ?`,
		requestID, subtype,
	).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestProjectStore_SQLite_SetCuratorSessionID verifies the new
// ProjectStore method used by the curator sink on first-session
// capture. Idempotent set-then-read.
func TestProjectStore_SQLite_SetCuratorSessionID(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := stores.Projects.SetCuratorSessionID(ctx, runmode.LocalDefaultOrgID, projectID, "sess-xyz"); err != nil {
		t.Fatalf("SetCuratorSessionID: %v", err)
	}
	got, err := stores.Projects.Get(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CuratorSessionID != "sess-xyz" {
		t.Errorf("CuratorSessionID = %q, want sess-xyz", got.CuratorSessionID)
	}

	// Idempotent re-set.
	if err := stores.Projects.SetCuratorSessionID(ctx, runmode.LocalDefaultOrgID, projectID, "sess-xyz"); err != nil {
		t.Errorf("re-SetCuratorSessionID should be idempotent, got %v", err)
	}

	// Missing project: silently no-op (nil error), not sql.ErrNoRows.
	// Pinned by the interface doc — diverges intentionally from
	// Update/Delete's not-found semantics because the curator sink
	// has nothing useful to do with an error when the project was
	// deleted mid-turn.
	if err := stores.Projects.SetCuratorSessionID(ctx, runmode.LocalDefaultOrgID, "00000000-0000-0000-0000-000000000ghost", "sess-x"); err != nil {
		t.Errorf("SetCuratorSessionID on missing project should be best-effort nil, got %v", err)
	}
}

// TestCuratorStore_SQLite_InsertPendingContext_Coalesces pins the
// coalescing contract on the partial-unique index: a second insert
// against the same (project, session, change_type) tuple while the
// first row is still unconsumed must drop on ON CONFLICT DO NOTHING
// — the earliest baseline is the truer "snapshot before the first
// unconsumed change" anchor.
func TestCuratorStore_SQLite_InsertPendingContext_Coalesces(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["a/b"]`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["c/d"]`); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	rows, err := stores.Curator.ListPendingContext(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after coalesce, got %d", len(rows))
	}
	if rows[0].BaselineValue != `["a/b"]` {
		t.Errorf("baseline = %q, want %q (earliest wins)", rows[0].BaselineValue, `["a/b"]`)
	}
}

// TestCuratorStore_SQLite_ListPendingContext_MixedConsumed verifies the
// list returns every row regardless of consumed state, in created_at
// order — the project-bundle export needs both halves of the lifecycle.
func TestCuratorStore_SQLite_ListPendingContext_MixedConsumed(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["x"]`); err != nil {
		t.Fatalf("insert pinned: %v", err)
	}
	requestID, err := db.CreateCuratorRequest(conn, projectID, "consume me")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	// Consume the first row so it transitions to consumed.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, _, err := ts.Curator.ConsumePendingContext(ctx, runmode.LocalDefaultOrgID, projectID, requestID)
		return err
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	// Insert a fresh unconsumed row for a different change_type.
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypeJiraProjectKey, `null`); err != nil {
		t.Fatalf("insert jira: %v", err)
	}

	rows, err := stores.Curator.ListPendingContext(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (1 consumed + 1 pending), got %d (%+v)", len(rows), rows)
	}
	if rows[0].ConsumedAt == nil {
		t.Errorf("expected first row to be consumed, got %+v", rows[0])
	}
	if rows[1].ConsumedAt != nil {
		t.Errorf("expected second row to be pending, got %+v", rows[1])
	}
}

// TestCuratorStore_SQLite_DeletePendingContextForSession scopes deletion
// to (project, session): rows tied to a different session must stay.
func TestCuratorStore_SQLite_DeletePendingContextForSession(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p", CuratorSessionID: "sess-1"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1", domain.ChangeTypePinnedRepos, `["a"]`); err != nil {
		t.Fatalf("insert sess-1: %v", err)
	}
	if err := stores.Curator.InsertPendingContext(ctx, runmode.LocalDefaultOrgID, projectID, "sess-2", domain.ChangeTypePinnedRepos, `["b"]`); err != nil {
		t.Fatalf("insert sess-2: %v", err)
	}

	if err := stores.Curator.DeletePendingContextForSession(ctx, runmode.LocalDefaultOrgID, projectID, "sess-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	rows, err := stores.Curator.ListPendingContext(ctx, runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row left (sess-2), got %d (%+v)", len(rows), rows)
	}
	if rows[0].CuratorSessionID != "sess-2" {
		t.Errorf("wrong session survived: %q", rows[0].CuratorSessionID)
	}
}

// TestCuratorStore_SQLite_CancelOrphanedNonTerminalRequests pins the
// startup recovery contract: BOTH queued and running rows get
// cancelled because neither can survive a process restart in a useful
// state. Terminal rows are untouched.
func TestCuratorStore_SQLite_CancelOrphanedNonTerminalRequests(t *testing.T) {
	conn := newSQLiteForCuratorTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	projectID, err := stores.Projects.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		domain.Project{Name: "p"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	create := func(input string) string {
		t.Helper()
		var id string
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
			rid, err := ts.Curator.CreateRequest(ctx, runmode.LocalDefaultOrgID, projectID, runmode.LocalDefaultUserID, input)
			if err != nil {
				return err
			}
			id = rid
			return nil
		}); err != nil {
			t.Fatalf("CreateRequest %s: %v", input, err)
		}
		return id
	}

	runningID := create("running")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, runmode.LocalDefaultOrgID, runningID)
	}); err != nil {
		t.Fatalf("MarkRequestRunning: %v", err)
	}
	// The running request streamed a token-bearing message before the "crash";
	// the boot sweep must roll it up onto the cancelled row (TFAC-473).
	ip := func(n int) *int { return &n }
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.InsertMessage(ctx, runmode.LocalDefaultOrgID, &domain.CuratorMessage{
			RequestID: runningID, Role: "assistant", Subtype: "text", Content: "partial",
			InputTokens: ip(100), OutputTokens: ip(20), CacheReadTokens: ip(1000), CacheCreationTokens: ip(7),
		})
		return err
	}); err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}

	queuedID := create("queued")

	doneID := create("done")
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.CompleteRequest(ctx, runmode.LocalDefaultOrgID, doneID, "done", "", 0.1, 100, 1)
		return err
	}); err != nil {
		t.Fatalf("CompleteRequest: %v", err)
	}

	n, err := stores.Curator.CancelOrphanedNonTerminalRequests(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("flipped %d rows, want 2 (running + queued)", n)
	}

	getStatus := func(id string) string {
		var status string
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
			r, err := ts.Curator.GetRequest(ctx, runmode.LocalDefaultOrgID, id)
			if err != nil {
				return err
			}
			status = r.Status
			return nil
		}); err != nil {
			t.Fatalf("GetRequest %s: %v", id, err)
		}
		return status
	}
	if got := getStatus(runningID); got != "cancelled" {
		t.Errorf("running row status = %q, want cancelled", got)
	}
	if got := getStatus(queuedID); got != "cancelled" {
		t.Errorf("queued row status = %q, want cancelled", got)
	}
	if got := getStatus(doneID); got != "done" {
		t.Errorf("done row status = %q, want done (untouched)", got)
	}
	// The running orphan's token cache rolled up from curator_messages in the
	// same sweep; the queued orphan had no messages, so it stays 0 (TFAC-473).
	{
		var in, out, cr, cc int
		if err := conn.QueryRow(`
			SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
			FROM curator_requests WHERE id = ?
		`, runningID).Scan(&in, &out, &cr, &cc); err != nil {
			t.Fatalf("read running tokens: %v", err)
		}
		if in != 100 || out != 20 || cr != 1000 || cc != 7 {
			t.Errorf("running orphan token cols = (%d,%d,%d,%d), want (100,20,1000,7) — boot sweep did not roll up curator_messages", in, out, cr, cc)
		}
	}
}

func newSQLiteForCuratorTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
