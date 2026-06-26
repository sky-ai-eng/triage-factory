package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedProjectForCurator inserts a minimal project row via raw SQL.
// Package db tests can't depend on internal/db/sqlite (import cycle:
// sqlite imports db for the interface), so the curator_test fixtures
// keep their own seed/read helpers. The store-level contract is
// covered by the dbtest conformance suite running against both
// backends.
func seedProjectForCurator(t *testing.T, database *sql.DB) string {
	t.Helper()
	id := uuid.New().String()
	now := time.Now().UTC()
	if _, err := database.Exec(`
		INSERT INTO projects (id, name, description, pinned_repos, team_id, created_at, updated_at)
		VALUES (?, 'Curator test project', '', '[]', ?, ?, ?)
	`, id, runmode.LocalDefaultTeamID, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

func deleteProjectForCurator(t *testing.T, database *sql.DB, id string) {
	t.Helper()
	if _, err := database.Exec(`DELETE FROM projects WHERE id = ?`, id); err != nil {
		t.Fatalf("delete project %q: %v", id, err)
	}
}

func readProjectCuratorSessionID(t *testing.T, database *sql.DB, id string) string {
	t.Helper()
	var sessionID sql.NullString
	if err := database.QueryRow(`SELECT curator_session_id FROM projects WHERE id = ?`, id).Scan(&sessionID); err != nil {
		t.Fatalf("read project %q: %v", id, err)
	}
	return sessionID.String
}

func TestCreateCuratorRequest_RoundtripDefaults(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, err := CreateCuratorRequest(database, projectID, "what's up")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("empty request id")
	}

	got, err := GetCuratorRequest(database, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected request, got nil")
	}
	if got.Status != "queued" {
		t.Errorf("status = %q, want queued", got.Status)
	}
	if got.UserInput != "what's up" {
		t.Errorf("user_input = %q", got.UserInput)
	}
	if got.StartedAt != nil || got.FinishedAt != nil {
		t.Errorf("started/finished should be nil for queued; got %v / %v", got.StartedAt, got.FinishedAt)
	}
	if got.IsTerminal() {
		t.Errorf("queued should not be terminal")
	}
}

func TestMarkCuratorRequestRunning_SecondCallNoOps(t *testing.T) {
	// Pin: the goroutine's pickup is the only legitimate queued→
	// running transition. A second pickup attempt (e.g., a duplicate
	// dispatch) must error out so the caller knows the row was
	// already claimed, rather than silently re-stamping started_at.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	id, _ := CreateCuratorRequest(database, projectID, "hi")

	if err := MarkCuratorRequestRunning(database, id); err != nil {
		t.Fatalf("first running: %v", err)
	}
	err := MarkCuratorRequestRunning(database, id)
	if err != sql.ErrNoRows {
		t.Errorf("second running call: want sql.ErrNoRows, got %v", err)
	}
}

func TestMarkCuratorRequestCancelledIfActive_TerminalRowsLeftAlone(t *testing.T) {
	// The cancel endpoint and the project-delete handler both call
	// this from outside the per-project goroutine. The status filter
	// must keep them from clobbering a row the goroutine just
	// finished cleanly.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, _ := CreateCuratorRequest(database, projectID, "x")
	if _, err := CompleteCuratorRequest(database, id, "done", "", 0.42, 1500, 3); err != nil {
		t.Fatalf("complete: %v", err)
	}

	flipped, err := MarkCuratorRequestCancelledIfActive(database, id, "user cancelled")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if flipped {
		t.Errorf("done row should not flip to cancelled")
	}

	got, _ := GetCuratorRequest(database, id)
	if got.Status != "done" {
		t.Errorf("status = %q, want done", got.Status)
	}
	if got.CostUSD != 0.42 || got.NumTurns != 3 {
		t.Errorf("accounting clobbered: %+v", got)
	}
}

func TestMarkCuratorRequestCancelledIfActive_FlipsRunningRow(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	id, _ := CreateCuratorRequest(database, projectID, "x")
	_ = MarkCuratorRequestRunning(database, id)

	flipped, err := MarkCuratorRequestCancelledIfActive(database, id, "user cancelled")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !flipped {
		t.Fatal("running row should flip to cancelled")
	}
	got, _ := GetCuratorRequest(database, id)
	if got.Status != "cancelled" || got.ErrorMsg != "user cancelled" {
		t.Errorf("post-cancel: %+v", got)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at not stamped on cancel")
	}
}

func TestInFlightCuratorRequestForProject_PrefersRunning(t *testing.T) {
	// When both queued + running rows exist (transient state during
	// goroutine pickup), the cancel endpoint should target the
	// running one — otherwise we cancel the next message and let
	// the active one keep going.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	queuedID, _ := CreateCuratorRequest(database, projectID, "first")
	_ = MarkCuratorRequestRunning(database, queuedID)
	_, _ = CreateCuratorRequest(database, projectID, "second") // queued

	got, err := InFlightCuratorRequestForProject(database, projectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got == nil || got.ID != queuedID {
		t.Errorf("expected running row %q, got %+v", queuedID, got)
	}
}

func TestInFlightCuratorRequestForProject_NoneReturnsNil(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	id, _ := CreateCuratorRequest(database, projectID, "x")
	_, _ = CompleteCuratorRequest(database, id, "done", "", 0, 0, 0)

	got, err := InFlightCuratorRequestForProject(database, projectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for project with only terminal rows, got %+v", got)
	}
}

// TestCompleteCuratorRequest_DoesNotClobberCancelled is the load-
// bearing race-protection test: a row that was cancelled (e.g. by
// the user via the DELETE endpoint) while the goroutine was running
// agentproc must NOT be silently flipped back to done by the
// goroutine's terminal write. The status filter on the UPDATE is
// what enforces this.
func TestCompleteCuratorRequest_DoesNotClobberCancelled(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, _ := CreateCuratorRequest(database, projectID, "x")
	_ = MarkCuratorRequestRunning(database, id)

	// Mimic the cancel handler racing ahead of the goroutine's
	// completion write.
	flipped, err := MarkCuratorRequestCancelledIfActive(database, id, "user cancelled")
	if err != nil || !flipped {
		t.Fatalf("seed cancel: flipped=%v err=%v", flipped, err)
	}

	// Now the goroutine tries to write done. Must be a no-op.
	flipped, err = CompleteCuratorRequest(database, id, "done", "", 0.5, 1000, 2)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if flipped {
		t.Error("CompleteCuratorRequest flipped a cancelled row — clobbered the user's cancel")
	}

	got, _ := GetCuratorRequest(database, id)
	if got.Status != "cancelled" {
		t.Errorf("post-race status = %q, want cancelled", got.Status)
	}
	if got.ErrorMsg != "user cancelled" {
		t.Errorf("error_msg = %q, want 'user cancelled'", got.ErrorMsg)
	}
	// Accounting from the racing completion call must not have
	// landed: the row is cancelled, not done, and a half-cancelled
	// half-completed row would be confusing in the UI.
	if got.CostUSD != 0 || got.NumTurns != 0 {
		t.Errorf("accounting leaked into cancelled row: %+v", got)
	}
}

// TestCompleteCuratorRequest_RollsUpTokensFromMessages pins the TFAC-473
// roll-up: CompleteCuratorRequest SETs the four token columns to the
// absolute SUM over curator_messages (the same shape runs uses over
// run_messages), so a completed request carries the turn's full token
// breakdown without the caller threading any counts.
func TestCompleteCuratorRequest_RollsUpTokensFromMessages(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, _ := CreateCuratorRequest(database, projectID, "x")
	if err := MarkCuratorRequestRunning(database, id); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// Two token-bearing assistant rows — written by the streaming sink
	// before the terminal completion write in production.
	for _, m := range []*domain.CuratorMessage{
		{RequestID: id, Role: "assistant", Subtype: "text", Content: "a",
			InputTokens: intPtr(100), OutputTokens: intPtr(20), CacheReadTokens: intPtr(1000), CacheCreationTokens: intPtr(7)},
		{RequestID: id, Role: "assistant", Subtype: "text", Content: "b",
			InputTokens: intPtr(50), OutputTokens: intPtr(5), CacheReadTokens: intPtr(500), CacheCreationTokens: intPtr(3)},
	} {
		if _, err := InsertCuratorMessage(database, m); err != nil {
			t.Fatalf("insert msg: %v", err)
		}
	}

	if flipped, err := CompleteCuratorRequest(database, id, "done", "", 0.1, 1000, 2); err != nil || !flipped {
		t.Fatalf("complete: flipped=%v err=%v", flipped, err)
	}

	// The legacy package-level reader doesn't project the token columns
	// (the CuratorStore read path does), so assert them directly.
	var in, out, cr, cc int
	if err := database.QueryRow(`
		SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
		FROM curator_requests WHERE id = ?
	`, id).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if in != 150 || out != 25 || cr != 1500 || cc != 10 {
		t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — SUM over curator_messages", in, out, cr, cc)
	}
}

// TestMarkCuratorRequestCancelledIfActive_RollsUpTokensFromMessages pins that
// the cancel path also refreshes the denormalized token columns from the
// curator_messages SUM — a request cancelled mid-turn still reflects the
// tokens it streamed rather than stranding them at 0 (TFAC-473).
func TestMarkCuratorRequestCancelledIfActive_RollsUpTokensFromMessages(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	id, _ := CreateCuratorRequest(database, projectID, "x")
	if err := MarkCuratorRequestRunning(database, id); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	for _, m := range []*domain.CuratorMessage{
		{RequestID: id, Role: "assistant", Subtype: "text", Content: "a",
			InputTokens: intPtr(100), OutputTokens: intPtr(20), CacheReadTokens: intPtr(1000), CacheCreationTokens: intPtr(7)},
		{RequestID: id, Role: "assistant", Subtype: "text", Content: "b",
			InputTokens: intPtr(50), OutputTokens: intPtr(5), CacheReadTokens: intPtr(500), CacheCreationTokens: intPtr(3)},
	} {
		if _, err := InsertCuratorMessage(database, m); err != nil {
			t.Fatalf("insert msg: %v", err)
		}
	}

	flipped, err := MarkCuratorRequestCancelledIfActive(database, id, "user cancelled")
	if err != nil || !flipped {
		t.Fatalf("cancel: flipped=%v err=%v", flipped, err)
	}

	var in, out, cr, cc int
	if err := database.QueryRow(`
		SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
		FROM curator_requests WHERE id = ?
	`, id).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if in != 150 || out != 25 || cr != 1500 || cc != 10 {
		t.Errorf("token cols = (%d,%d,%d,%d), want (150,25,1500,10) — cancel did not roll up the curator_messages SUM", in, out, cr, cc)
	}
}

func TestProjectDelete_CascadesCuratorRows(t *testing.T) {
	// FK ON DELETE CASCADE drives the cleanup contract: removing the
	// project takes its requests + messages with it. Without this,
	// orphaned rows would build up over time.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	requestID, _ := CreateCuratorRequest(database, projectID, "x")
	if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
		RequestID: requestID,
		Role:      "assistant",
		Subtype:   "text",
		Content:   "hello",
	}); err != nil {
		t.Fatalf("insert msg: %v", err)
	}

	deleteProjectForCurator(t, database, projectID)

	if got, _ := GetCuratorRequest(database, requestID); got != nil {
		t.Errorf("request survived project delete: %+v", got)
	}
	msgs, err := ListCuratorMessagesByRequest(database, requestID)
	if err != nil {
		t.Fatalf("list msgs: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no messages after cascade, got %d", len(msgs))
	}
}

func TestSetProjectCuratorSessionID_PersistsOnProjectRow(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	if err := SetProjectCuratorSessionID(database, projectID, "sess-curator-123"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	if got := readProjectCuratorSessionID(t, database, projectID); got != "sess-curator-123" {
		t.Errorf("curator_session_id = %q", got)
	}
}

func TestListCuratorMessagesByRequestIDs_GroupsByRequest(t *testing.T) {
	// Pin the batched-fetch contract: messages are returned in a map
	// keyed by request_id, each list ordered chronologically. This is
	// what lets the history handler render N requests with a single
	// IN-list query instead of N per-request round trips.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	r1, _ := CreateCuratorRequest(database, projectID, "first")
	r2, _ := CreateCuratorRequest(database, projectID, "second")
	r3, _ := CreateCuratorRequest(database, projectID, "third — no replies")

	// Two messages on r1, one on r2, none on r3.
	for _, content := range []string{"r1-a", "r1-b"} {
		if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
			RequestID: r1, Role: "assistant", Subtype: "text", Content: content,
		}); err != nil {
			t.Fatalf("seed r1 msg %q: %v", content, err)
		}
	}
	if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
		RequestID: r2, Role: "assistant", Subtype: "text", Content: "r2-a",
	}); err != nil {
		t.Fatalf("seed r2 msg: %v", err)
	}

	got, err := ListCuratorMessagesByRequestIDs(database, []string{r1, r2, r3})
	if err != nil {
		t.Fatalf("batch fetch: %v", err)
	}

	if len(got[r1]) != 2 {
		t.Errorf("r1: got %d messages, want 2", len(got[r1]))
	}
	if got[r1][0].Content != "r1-a" || got[r1][1].Content != "r1-b" {
		t.Errorf("r1 ordering wrong: %+v", got[r1])
	}
	if len(got[r2]) != 1 || got[r2][0].Content != "r2-a" {
		t.Errorf("r2: got %+v, want one r2-a", got[r2])
	}
	// r3 had no messages — must NOT be present in the map. Callers
	// substitute an empty slice when rendering JSON; checking for
	// "missing key === no messages" keeps the helper allocation-free
	// for empty-stream requests.
	if _, ok := got[r3]; ok {
		t.Errorf("r3 should be absent from map (no messages), got %+v", got[r3])
	}
}

func TestListCuratorMessagesByRequestIDs_EmptyInputReturnsEmptyMap(t *testing.T) {
	// The handler iterates the result by request id; a nil map
	// would force every caller to nil-check before lookup. Pin the
	// non-nil empty contract.
	database := newTestDB(t)
	got, err := ListCuratorMessagesByRequestIDs(database, nil)
	if err != nil {
		t.Fatalf("empty fetch: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty map for nil input")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestInsertCuratorMessage_RoundtripsToolCallsAndTokens(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	requestID, _ := CreateCuratorRequest(database, projectID, "x")

	in := &domain.CuratorMessage{
		RequestID: requestID,
		Role:      "assistant",
		Subtype:   "tool_use",
		Content:   "calling Read",
		ToolCalls: []domain.ToolCall{{ID: "t1", Name: "Read", Input: map[string]any{"file_path": "/x"}}},
		Model:     "sonnet-4-6",
	}
	five := 5
	twelve := 12
	in.InputTokens = &five
	in.OutputTokens = &twelve

	id, err := InsertCuratorMessage(database, in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Errorf("expected non-zero auto id")
	}

	out, err := ListCuratorMessagesByRequest(database, requestID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	got := out[0]
	if got.Role != "assistant" || got.Subtype != "tool_use" {
		t.Errorf("role/subtype: %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "Read" {
		t.Errorf("tool_calls roundtrip: %+v", got.ToolCalls)
	}
	if got.InputTokens == nil || *got.InputTokens != 5 {
		t.Errorf("input tokens: %+v", got.InputTokens)
	}
}

func TestResetCuratorForProject_WipesEverythingAndClearsSession(t *testing.T) {
	// Reset: wipe pending-context, wipe requests (cascading messages),
	// clear curator_session_id. The next message starts fresh, which
	// is the whole point — `--resume` binds the original session's
	// flags so changes to the allowlist or envelope only take effect
	// against a brand-new session.
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)

	if err := SetProjectCuratorSessionID(database, projectID, "stale-session"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	requestID, err := CreateCuratorRequest(database, projectID, "first turn")
	if err != nil {
		t.Fatalf("seed request: %v", err)
	}
	// Force-finish the request so the in-flight check passes. The
	// reset must work on a project with terminal history.
	if _, err := CompleteCuratorRequest(database, requestID, "done", "", 0, 0, 0); err != nil {
		t.Fatalf("finish request: %v", err)
	}
	if _, err := InsertCuratorMessage(database, &domain.CuratorMessage{
		RequestID: requestID,
		Role:      "assistant",
		Content:   "hello back",
	}); err != nil {
		t.Fatalf("seed msg: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO curator_pending_context
			(project_id, curator_session_id, change_type, baseline_value)
		VALUES (?, ?, ?, ?)
	`, projectID, "stale-session", "pinned_repos", `[]`); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	if err := ResetCuratorForProject(database, projectID); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// curator_session_id back to NULL → reads as empty string.
	if got := readProjectCuratorSessionID(t, database, projectID); got != "" {
		t.Errorf("session id = %q, want cleared", got)
	}

	// All curator_requests for this project gone → cascades messages.
	requests, err := ListCuratorRequestsByProject(database, projectID)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(requests) != 0 {
		t.Errorf("expected 0 requests after reset, got %d", len(requests))
	}
	msgs, err := ListCuratorMessagesByRequest(database, requestID)
	if err != nil {
		t.Fatalf("list msgs: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected messages cascaded, got %d", len(msgs))
	}

	// Pending-context rows for the wiped session also gone.
	var pendingCount int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM curator_pending_context WHERE project_id = ?`, projectID,
	).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCount != 0 {
		t.Errorf("expected pending wiped, got %d", pendingCount)
	}
}

func TestResetCuratorForProject_RefusesWhenInFlight(t *testing.T) {
	database := newTestDB(t)
	projectID := seedProjectForCurator(t, database)
	if _, err := CreateCuratorRequest(database, projectID, "running…"); err != nil {
		t.Fatalf("seed req: %v", err)
	}
	// Default status from CreateCuratorRequest is "queued" — that's
	// in-flight by the reset's definition, exactly what should block.

	err := ResetCuratorForProject(database, projectID)
	if err == nil || err != ErrCuratorInFlight {
		t.Errorf("expected ErrCuratorInFlight, got %v", err)
	}

	// Verify nothing was wiped — the TX rolled back atomically.
	requests, _ := ListCuratorRequestsByProject(database, projectID)
	if len(requests) != 1 {
		t.Errorf("requests should not have been deleted: got %d", len(requests))
	}
}
