package agentproc

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSDK_LiveSmoke is a live end-to-end smoke test against the real
// Agent SDK runtime. Skipped by default; opt in with
//
//	TF_TEST_SDK_LIVE=1 go test ./internal/agentproc -run TestSDK_LiveSmoke -v
//
// First run installs the SDK + Node deps under ~/.triagefactory/sdk
// (a few hundred MB, ~30s). Subsequent runs are fast.
//
// Bills against whatever auth the user's environment resolves —
// typically the local Claude Code OAuth login (Pro/Max subscription).
//
// Exists as a manual verification gate while we migrate the runtime
// from the `claude` CLI to the SDK; remove or fold into a higher-level
// integration suite once the migration ships.
func TestSDK_LiveSmoke(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sink := &captureSink{}
	outcome, err := Run(ctx, RunOptions{
		Cwd:      t.TempDir(),
		Message:  "Reply with exactly the word PONG and nothing else.",
		Model:    "haiku",
		MaxTurns: 1,
		TraceID:  "live-smoke",
	}, sink)
	if err != nil {
		t.Fatalf("Run failed: %v\nstderr: %s", err, outcomeStderr(outcome))
	}
	if outcome == nil || outcome.Result == nil {
		t.Fatalf("expected terminal Result, got %+v\nstderr: %s", outcome, outcomeStderr(outcome))
	}
	if outcome.SessionID == "" {
		t.Errorf("expected non-empty session id (system/init missing or stream.go regressed)")
	}
	if !strings.Contains(outcome.Result.Result, "PONG") {
		t.Errorf("expected PONG in result, got %q", outcome.Result.Result)
	}
	if assistantMsg := findFirstAssistant(sink.messages); assistantMsg == nil {
		t.Errorf("expected at least one assistant message in the sink")
	}
	t.Logf("live SDK smoke ok: session=%s cost_usd=%.6f turns=%d", outcome.SessionID, outcome.Result.CostUSD, outcome.Result.NumTurns)
}

// TestSDK_LiveSmoke_Interactive exercises the streaming-input bridge
// end-to-end against the real SDK: an initial turn, a second message
// continuing the SAME process (no replay), and stable session id across
// both. Gated like TestSDK_LiveSmoke (TF_TEST_SDK_LIVE=1).
//
// Mirrors the spike's 01-interrupt-live.mjs steering flow minus the
// interrupt (covered by TestSDK_LiveSmoke_InteractiveInterrupt).
func TestSDK_LiveSmoke_Interactive(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:     t.TempDir(),
		Message: "Reply with exactly the word PING and nothing else.",
		Model:   "haiku",
		TraceID: "live-interactive",
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	first := sink.waitAssistant(t, 60*time.Second)
	t.Logf("first turn: %q", first.Content)

	sid := lr.SessionID()
	if sid == "" {
		t.Fatal("expected a session id after the first turn")
	}

	// Continue the same process with a second message.
	if err := lr.Send(ctx, "Now reply with exactly the word PONG and nothing else."); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	second := sink.waitAssistant(t, 60*time.Second)
	t.Logf("second turn: %q", second.Content)

	if lr.SessionID() != sid {
		t.Errorf("session id changed across turns: %q -> %q", sid, lr.SessionID())
	}

	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
}

// TestSDK_LiveSmoke_InteractiveOpeningBlocks sends the opening turn as block
// content rather than a string and asks the model to read one block back: the
// end-to-end proof that Go's blocks payload survives the wrapper, the SDK and
// the API as the message's content. Gated like TestSDK_LiveSmoke.
func TestSDK_LiveSmoke_InteractiveOpeningBlocks(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd: t.TempDir(),
		OpeningBlocks: []ContentBlock{
			{Type: "text", Text: "MARKER-ALPHA: ZEBRAFISH"},
			{Type: "text", Text: "Reply with exactly the word that follows MARKER-ALPHA's colon in the first block, and nothing else."},
		},
		Model:   "haiku",
		TraceID: "live-opening-blocks",
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	reply := sink.waitAssistant(t, 60*time.Second)
	t.Logf("opening-blocks turn: %q", reply.Content)
	if !strings.Contains(reply.Content, "ZEBRAFISH") {
		t.Errorf("assistant reply = %q; the model did not read the first block back, so the blocks did not arrive as content", reply.Content)
	}
}

// TestSDK_LiveSmoke_InteractiveInterrupt asserts interrupt() lands: the
// in-flight turn ends with an error_during_execution result. Mirrors
// spike/takeover/01-interrupt-live.mjs.
func TestSDK_LiveSmoke_InteractiveInterrupt(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:     t.TempDir(),
		Message: "Write a very long, detailed essay of at least 3000 words about the full history of computing. Take your time and be thorough.",
		Model:   "haiku",
		TraceID: "live-interrupt",
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	// Let the turn get underway, then interrupt mid-flight. The assistant
	// message only flushes at stop_reason, so we wait on the session id
	// (emitted early in the stream) rather than an assistant turn.
	waitSession(t, lr, 60*time.Second)
	time.Sleep(3 * time.Second)

	if err := lr.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt failed: %v", err)
	}

	// Graceful close drains the interrupted turn's result.
	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
	<-lr.Done()

	res := lr.Result()
	if res == nil {
		t.Fatal("expected a Result after interrupt")
	}
	if res.Subtype != "error_during_execution" {
		t.Errorf("interrupt result subtype = %q, want error_during_execution", res.Subtype)
	}
	t.Logf("interrupt ok: subtype=%s is_error=%v", res.Subtype, res.IsError)
}

// TestSDK_LiveSmoke_QueuedTurnRunsAfterTheCurrentOne pins the SDK behavior
// the live driver's turn-boundary check rests on: a message sent while a turn
// is running is queued and run as the NEXT turn, with its own result — it is
// neither folded into the running turn nor dropped when that turn ends. The
// count of owed turns is sampled at each result, the same moment the driver
// reads it: two owed after both sends, one left when the first turn's result
// lands (the steered turn is still to come), none after the second.
func TestSDK_LiveSmoke_QueuedTurnRunsAfterTheCurrentOne(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var (
		mu       sync.Mutex
		owedAt   []int
		resultsC = make(chan *Result, 8)
	)
	sink := newLiveSink()
	var lr *LiveRun
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:     t.TempDir(),
		Message: "Count from 1 to 60, one number per line, with no other text.",
		Model:   "haiku",
		TraceID: "live-queued-turn",
		OnResult: func(r *Result) {
			mu.Lock()
			owedAt = append(owedAt, lr.QueuedTurns())
			mu.Unlock()
			resultsC <- r
		},
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	// Steer while the first turn is still being produced.
	waitSession(t, lr, 60*time.Second)
	if err := lr.Send(ctx, "Now reply with exactly the word PONG and nothing else."); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if got := lr.QueuedTurns(); got != 2 {
		t.Errorf("QueuedTurns with a steer queued behind the running turn = %d, want 2", got)
	}

	for i := range 2 {
		select {
		case r := <-resultsC:
			t.Logf("result %d: subtype=%s is_error=%v", i+1, r.Subtype, r.IsError)
		case <-time.After(90 * time.Second):
			t.Fatalf("result %d never arrived", i+1)
		}
	}
	first := sink.waitAssistant(t, time.Second)
	second := sink.waitAssistant(t, time.Second)
	t.Logf("first turn: %q", first.Content)
	t.Logf("second turn: %q", second.Content)
	if !strings.Contains(second.Content, "PONG") {
		t.Errorf("the queued message did not produce its own turn; second assistant message = %q", second.Content)
	}
	mu.Lock()
	got := append([]int(nil), owedAt...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Errorf("QueuedTurns sampled at each result = %v, want [1 0]", got)
	}
	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
}

// TestSDK_LiveSmoke_QueuedTurnSurvivesAnInterrupt is the pause half of the
// same contract: a message queued behind a running turn is still run when
// that turn is ended by interrupt() rather than by the model — the pause ends
// the turn, not the queue. The driver leans on this to stay with a paused
// process that has a steer owed rather than parking it.
func TestSDK_LiveSmoke_QueuedTurnSurvivesAnInterrupt(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	resultsC := make(chan *Result, 8)
	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:      t.TempDir(),
		Message:  "Write a very long, detailed essay of at least 3000 words about the full history of computing. Take your time and be thorough.",
		Model:    "haiku",
		TraceID:  "live-queued-interrupt",
		OnResult: func(r *Result) { resultsC <- r },
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	waitSession(t, lr, 60*time.Second)
	time.Sleep(2 * time.Second)
	if err := lr.Send(ctx, "Now reply with exactly the word PONG and nothing else."); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if err := lr.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt failed: %v", err)
	}

	var got []*Result
	for i := range 2 {
		select {
		case r := <-resultsC:
			t.Logf("result %d: subtype=%s is_error=%v interrupted=%v owed_after=%d", i+1, r.Subtype, r.IsError, r.Interrupted, lr.QueuedTurns())
			got = append(got, r)
		case <-time.After(90 * time.Second):
			t.Fatalf("result %d never arrived", i+1)
		}
	}
	if !got[0].Interrupted {
		t.Errorf("first result should be the interrupted turn, got subtype=%s", got[0].Subtype)
	}
	if got[1].Interrupted || got[1].IsError {
		t.Errorf("second result should be the queued turn's own, got %+v", got[1])
	}
	if owed := lr.QueuedTurns(); owed != 0 {
		t.Errorf("QueuedTurns after both results = %d, want 0", owed)
	}
	// The interrupted essay may or may not have flushed an assistant message;
	// the queued turn's reply is the one that must be there.
	var sawPong bool
	for range 2 {
		select {
		case m := <-sink.asst:
			t.Logf("assistant: %.60q", m.Content)
			if strings.Contains(m.Content, "PONG") {
				sawPong = true
			}
		case <-time.After(time.Second):
		}
	}
	if !sawPong {
		t.Error("the queued message did not produce its turn after the interrupt")
	}
	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
}

// TestSDK_LiveSmoke_InteractivePermission asserts the canUseTool bridge:
// a Write attempt surfaces a permission_request to the handler, and a
// deny is honored (the file is never written). Mirrors
// spike/takeover/03-permission.mjs.
func TestSDK_LiveSmoke_InteractivePermission(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	cwd := t.TempDir()
	target := filepath.Join(cwd, "secret.txt")

	permCh := make(chan PermissionRequest, 8)
	handler := func(req PermissionRequest) PermissionDecision {
		permCh <- req
		return PermissionDecision{Behavior: "deny", Message: "denied by test"}
	}

	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:     cwd,
		Message: "Use the Write tool to create a file named secret.txt containing the text 'hello'. Do it now without asking.",
		Model:   "sonnet-4-6",
		TraceID: "live-perm",
	}, sink, handler)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	select {
	case req := <-permCh:
		t.Logf("permission requested: tool=%s input=%v", req.ToolName, req.Input)
	case <-time.After(120 * time.Second):
		t.Fatal("expected a permission_request for the Write tool")
	}

	// Deny honored — the file must not have been created.
	if _, err := os.Stat(target); err == nil {
		t.Errorf("file %s was written despite a denied permission", target)
	}
}

// TestSDK_LiveSmoke_AllowlistShortCircuitsHandler pins the load-bearing
// assumption behind the §F permission model: an --allowedTools match
// short-circuits to allow and canUseTool is NEVER invoked for it, while an
// off-allowlist tool DOES round-trip through the handler. If this ever
// regressed (the SDK started consulting canUseTool for allowlisted tools),
// wiring delegate's autonomous runs onto a nil handler would silently
// deny-all the allowlist. Mirrors spike/takeover/03-permission.mjs +
// 03b. Gated like the other live smokes (TF_TEST_SDK_LIVE=1).
func TestSDK_LiveSmoke_AllowlistShortCircuitsHandler(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	cwd := t.TempDir()
	allowedTarget := filepath.Join(cwd, "allowed.txt")

	var mu sync.Mutex
	var seen []string
	offAllowlist := make(chan string, 8)
	handler := func(req PermissionRequest) PermissionDecision {
		mu.Lock()
		seen = append(seen, req.ToolName)
		mu.Unlock()
		select {
		case offAllowlist <- req.ToolName:
		default:
		}
		// Deny the off-allowlist tool so the run stays bounded.
		return PermissionDecision{Behavior: "deny", Message: "denied by test"}
	}

	// Write is on the allowlist (match → allow, no callback); Bash is NOT
	// (off-allowlist → must reach the handler). Do the Write first so the
	// allowlist path is exercised before the agent reaches the denied Bash.
	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:          cwd,
		Message:      "First use the Write tool to create a file named allowed.txt containing 'hi'. After that, use the Bash tool to run `echo nope`. Do both without asking.",
		Model:        "sonnet-4-6",
		AllowedTools: "Write,Read,Glob,Grep",
		TraceID:      "live-allowlist",
	}, sink, handler)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	defer func() { _ = lr.Close() }()

	// Wait for the off-allowlist tool to reach the handler.
	select {
	case tool := <-offAllowlist:
		t.Logf("off-allowlist tool routed to handler: %s", tool)
	case <-time.After(120 * time.Second):
		t.Fatal("expected an off-allowlist tool (Bash) to reach the handler")
	}

	// The load-bearing assertions: the allowlisted Write was allowed
	// without ever consulting the handler, and the file was written.
	mu.Lock()
	for _, tool := range seen {
		if tool == "Write" {
			mu.Unlock()
			t.Fatalf("allowlisted Write reached canUseTool (handler saw %v) — allowlist no longer short-circuits", seen)
		}
	}
	mu.Unlock()
	if _, statErr := os.Stat(allowedTarget); statErr != nil {
		t.Errorf("allowed.txt was not written despite Write being allowlisted (allow short-circuit failed): %v", statErr)
	}
}

// denyAllPermissions is a PermissionHandler that denies everything —
// suitable for smoke tests that don't expect any tool use.
func denyAllPermissions(req PermissionRequest) PermissionDecision {
	return PermissionDecision{Behavior: "deny", Message: "denied (smoke test)"}
}

// waitSession blocks until the run has captured a session id or the
// deadline passes.
func waitSession(t *testing.T, lr *LiveRun, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if lr.SessionID() != "" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for session id")
		case <-lr.Done():
			if lr.SessionID() == "" {
				t.Fatalf("run finished before a session id: %v", lr.Err())
			}
			return
		case <-tick.C:
		}
	}
}

// liveSink is a concurrency-safe Sink for the interactive smoke tests:
// the reader goroutine writes while the test goroutine reads. It signals
// each completed assistant turn (a flushed assistant message with text)
// over a channel so a test can wait for it.
type liveSink struct {
	mu       sync.Mutex
	messages []*domain.Message
	asst     chan *domain.Message
}

func newLiveSink() *liveSink {
	return &liveSink{asst: make(chan *domain.Message, 64)}
}

func (s *liveSink) OnSession(string) error { return nil }

func (s *liveSink) OnMessage(m *domain.Message) error {
	s.mu.Lock()
	s.messages = append(s.messages, m)
	s.mu.Unlock()
	if m.Role == "assistant" && m.Content != "" {
		select {
		case s.asst <- m:
		default:
		}
	}
	return nil
}

func (s *liveSink) waitAssistant(t *testing.T, d time.Duration) *domain.Message {
	t.Helper()
	select {
	case m := <-s.asst:
		return m
	case <-time.After(d):
		t.Fatalf("timed out waiting for an assistant turn")
		return nil
	}
}

func outcomeStderr(o *Outcome) string {
	if o == nil {
		return ""
	}
	return o.Stderr
}

func findFirstAssistant(msgs []*domain.Message) *domain.Message {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return m
		}
	}
	return nil
}

// TestSDK_LiveSmoke_OpeningTurnIsTheAssembledRows is the acceptance for the
// opening a delegated SDK conversation actually sends: the rows a launch mints
// — the task-memory envelope around one prior memory, then the task context —
// assembled into one block-content user message.
//
// Four things it proves that a unit test cannot. The blocks survive Go, the
// wrapper, the SDK and the API as ONE turn (one assistant reply, one result,
// not four turns or a silently joined string). The session file the SDK writes
// records them as a block array, which is what a `--resume` replays. The resume
// really does replay them. And the model reads the envelope as the framing of
// what follows rather than as prose, which is the whole reason the boundaries
// are preserved rather than joined.
//
// Gated like TestSDK_LiveSmoke.
func TestSDK_LiveSmoke_OpeningTurnIsTheAssembledRows(t *testing.T) {
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The shape RowsToMessages produces over one memory row and a task-context
	// row. Spelled literally rather than assembled here: agentproc sits below
	// the packages that own rows and assembly, and what this test is for is the
	// wire, not the composition.
	const taskContext = "<task_context>\nPull request owner/repo#7 — \"Fix the flaky poller test\"\n</task_context>"
	envelopeOpen := domain.MemoryEnvelopeOpen(1)
	blocks := []ContentBlock{
		{Type: "text", Text: envelopeOpen},
		{Type: "text", Text: "A prior conversation rewrote the poller's fixture and left the assertion alone."},
		{Type: "text", Text: domain.MemoryEnvelopeClose},
		{Type: "text", Text: taskContext + "\n\nBefore doing anything else: reply with the first line of the first content block you were given, verbatim, and nothing else. Use no tools."},
	}

	cwd := t.TempDir()
	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:           cwd,
		OpeningBlocks: blocks,
		Model:         "haiku",
		TraceID:       "live-opening-rows",
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}

	reply := sink.waitAssistant(t, 90*time.Second)
	t.Logf("opening turn reply: %q", reply.Content)
	if !strings.Contains(reply.Content, `<system-note kind="task-memory">`) {
		t.Errorf("assistant reply = %q; the model did not quote the envelope's first line, so the opening did not arrive as the framing of what followed", reply.Content)
	}

	sid := lr.SessionID()
	if sid == "" {
		t.Fatal("no session id; nothing to read the transcript from")
	}
	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
	<-lr.Done()

	// Exactly one turn: one assistant message and one result for one send.
	if got := countAssistants(sink.snapshot()); got != 1 {
		t.Errorf("assistant messages = %d, want 1 — the opening must be one turn, not one per block", got)
	}
	if lr.Result() == nil {
		t.Error("expected a terminal Result for the opening turn")
	}

	// The session file is what --resume replays, so the block array has to be
	// in it: a joined string here would be a resume that reads a different
	// opening than the one that was sent.
	entry := firstUserEntry(t, sid)
	content, ok := entry["content"].([]any)
	if !ok {
		t.Fatalf("the session's first user entry carries %T content, want a block array: %#v", entry["content"], entry)
	}
	if len(content) != len(blocks) {
		t.Fatalf("the session records %d content blocks, want %d: %#v", len(content), len(blocks), content)
	}
	for i, raw := range content {
		b, _ := raw.(map[string]any)
		if b["type"] != "text" || b["text"] != blocks[i].Text {
			t.Errorf("session block %d = %#v, want %q", i, raw, blocks[i].Text)
		}
	}

	// --resume replays it: a fresh process on the same session is still
	// answering about blocks it never sent.
	resumed := newLiveSink()
	rlr, err := RunInteractive(ctx, RunOptions{
		Cwd:       cwd,
		SessionID: sid,
		Message:   "Quote the first line of the very first content block of this conversation, verbatim, and nothing else. Use no tools.",
		Model:     "haiku",
		TraceID:   "live-opening-rows-resume",
	}, resumed, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive (resume) failed: %v", err)
	}
	defer func() { _ = rlr.Close() }()

	replayed := resumed.waitAssistant(t, 90*time.Second)
	t.Logf("resumed turn reply: %q", replayed.Content)
	if !strings.Contains(replayed.Content, `<system-note kind="task-memory">`) {
		t.Errorf("resumed reply = %q; --resume did not replay the opening blocks", replayed.Content)
	}
}

// snapshot copies the sink's messages under its lock, for the assertions that
// run after the process is gone.
func (s *liveSink) snapshot() []*domain.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*domain.Message(nil), s.messages...)
}

func countAssistants(msgs []*domain.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" && m.Content != "" {
			n++
		}
	}
	return n
}

// firstUserEntry decodes the first `type: "user"` line of the session
// transcript the SDK wrote for sessionID.
//
// Found by walking ~/.claude/projects for the session's own file rather than by
// re-deriving the cwd-encoded directory name: that encoding belongs to
// internal/worktree, which imports this package, so the test locates the file
// by the one name it already knows.
func firstUserEntry(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	var found string
	if err := filepath.WalkDir(filepath.Join(home, ".claude", "projects"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil //nolint:nilerr // a missing or unreadable subtree just isn't the session
		}
		if d.Name() == sessionID+".jsonl" {
			found = path
		}
		return nil
	}); err != nil {
		t.Fatalf("walk the session directory: %v", err)
	}
	if found == "" {
		t.Fatalf("no session transcript named %s.jsonl under ~/.claude/projects", sessionID)
	}
	raw, err := os.ReadFile(found) //nolint:gosec // a path this test just found by name
	if err != nil {
		t.Fatalf("read the session transcript: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry["type"] != "user" {
			continue
		}
		msg, ok := entry["message"].(map[string]any)
		if !ok {
			continue
		}
		return msg
	}
	t.Fatalf("no user entry in the session transcript at %s", found)
	return nil
}
