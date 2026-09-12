package delegate

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// runMirror builds the memory mirror runAgent would have built, for a test
// that drives processCompletion / recordNativeResult directly instead of
// going through a launch.
func runMirror(s *Spawner, task domain.Task, conversationID, blueprintRunID, cwd string, inherited *memoryFingerprint) *memoryMirror {
	return s.newMemoryMirror(runmode.LocalDefaultOrgID, conversationID, blueprintRunID, task.EntityID, cwd, inherited)
}

// countingTaskMemory counts what reached the store, so a test can tell "filed
// once" from "filed on every check" — the difference between a mirror that
// tracks the file and one that rewrites the same row per tool call.
type countingTaskMemory struct {
	db.TaskMemoryStore
	upserts int
	touches int
	// failTouches makes the next N attach calls fail, so a test can drive the
	// join row's own retry without the content ever changing.
	failTouches int
}

func (c *countingTaskMemory) UpsertAgentMemorySystem(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error) {
	c.upserts++
	return c.TaskMemoryStore.UpsertAgentMemorySystem(ctx, orgID, conversationID, blueprintRunID, content, source)
}

func (c *countingTaskMemory) RecordEntityTouchSystem(ctx context.Context, orgID, conversationID, entityID, role string) error {
	c.touches++
	if c.failTouches > 0 {
		c.failTouches--
		return errors.New("attach refused")
	}
	return c.TaskMemoryStore.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, role)
}

// mirrorFixture stages a conversation with a real tree and a counting memory
// store, and hands back the mirror an engagement on it would hold.
func mirrorFixture(t *testing.T, suffix string) (*Spawner, string, domain.Task, string, *memoryMirror, *countingTaskMemory) {
	t.Helper()
	s, database, conversationID, taskID := setupAdvanceFixture(t, suffix)
	makeConversationBlueprintStep(t, database, conversationID, taskID)
	task := loadTask(t, s, taskID)
	counting := &countingTaskMemory{TaskMemoryStore: s.taskMemory}
	s.taskMemory = counting
	cwd := t.TempDir()
	return s, conversationID, task, cwd, runMirror(s, task, conversationID, "bpr-"+conversationID, cwd, nil), counting
}

// TestMemoryMirror_FilesEachChangeOnceIsAll pins the mirror's whole rule in one
// pass: a written file is filed, the same file is not filed again, and a
// rewrite is. The count is the assertion that matters — a mirror that re-filed
// on every check would be a write per tool call for the length of a run.
func TestMemoryMirror_FilesEachChangeOnceIsAll(t *testing.T) {
	s, conversationID, task, cwd, mirror, counting := mirrorFixture(t, "mirror-changes")
	ctx := context.Background()

	// Nothing written yet: the agent has been running, it just hasn't decided
	// what to remember.
	if state := mirror.check(ctx); state != memoryFileMissing {
		t.Errorf("check() on an empty tree = %v, want memoryFileMissing", state)
	}
	if counting.upserts != 0 {
		t.Fatalf("upserts = %d before the agent wrote anything, want 0", counting.upserts)
	}

	writeAgentMemory(t, cwd, "the CI failure is a flaky fixture, not the diff")
	if state := mirror.check(ctx); state != memoryFilePresent {
		t.Fatalf("check() after the agent wrote = %v, want memoryFilePresent", state)
	}
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "the CI failure is a flaky fixture, not the diff" {
		t.Errorf("agent_content = %q, want the file the agent just wrote", got)
	}
	if counting.upserts != 1 || counting.touches != 1 {
		t.Errorf("upserts/touches = %d/%d after one write, want 1/1", counting.upserts, counting.touches)
	}

	// Every later tool call re-reads the same bytes and files nothing.
	for range 5 {
		mirror.check(ctx)
	}
	if counting.upserts != 1 {
		t.Errorf("upserts = %d after five checks of an unchanged file, want 1", counting.upserts)
	}

	writeAgentMemory(t, cwd, "re-ran it clean; pushing the real fix")
	mirror.check(ctx)
	if counting.upserts != 2 {
		t.Errorf("upserts = %d after the agent rewrote the file, want 2", counting.upserts)
	}
	if counting.touches != 1 {
		t.Errorf("touches = %d, want 1 — the join row is keyed (conversation, entity), so one success covers every later filing", counting.touches)
	}
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "re-ran it clean; pushing the real fix" {
		t.Errorf("agent_content = %q, want the rewritten file", got)
	}
}

// TestMemoryMirror_RetriesTheEntityAttachUntilItLands: the two writes a filing
// makes fail independently, and the join row is the one an entity read needs —
// a memory row without it is durable and invisible. Tracking it under the
// content digest would mean a single failed attach is never retried, because
// every later look sees the same unchanged file and skips. Nor does the
// conclusion gate's own attach save it: a conversation ended at a boundary
// never reaches a conclusion.
func TestMemoryMirror_RetriesTheEntityAttachUntilItLands(t *testing.T) {
	s, conversationID, task, cwd, mirror, counting := mirrorFixture(t, "mirror-attach-retry")
	ctx := context.Background()
	counting.failTouches = 1

	writeAgentMemory(t, cwd, "what the agent worked out")
	mirror.check(ctx)
	if counting.upserts != 1 {
		t.Fatalf("upserts = %d, want 1 — the content itself landed", counting.upserts)
	}
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "" {
		t.Fatalf("entity read = %q; the fixture is not reproducing the failed attach", got)
	}

	// The file has not changed, so the content is not re-filed — but the
	// attach that failed is tried again.
	mirror.check(ctx)
	if counting.upserts != 1 {
		t.Errorf("upserts = %d, want 1 — an unchanged file must not be re-filed to retry its attach", counting.upserts)
	}
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "what the agent worked out" {
		t.Errorf("entity read = %q, want the mirrored memory — the attach was never retried", got)
	}

	// And once it has landed, later looks leave it alone.
	mirror.check(ctx)
	if counting.touches != 2 {
		t.Errorf("touches = %d, want 2 (one refused, one landed)", counting.touches)
	}
}

// TestMemoryMirror_SettleReassertsTheFileOverALateWriter is the ending's whole
// reason for writing unconditionally.
//
// The memory upsert is keyed by conversation and is not claim-fenced, so an
// engagement that lost its claim still files into the row on its way out — a
// park ahead of the fence check is exactly that. The skip check is in-memory
// state about what THIS mirror wrote, not a read of the row, so the successor
// cannot see that its row was changed under it: mid-run it looks at its own
// unchanged file, matches its own digest, and leaves the loser's narrative
// standing. Its ending is what puts the row back, and only because settle
// declines to consult the digest.
func TestMemoryMirror_SettleReassertsTheFileOverALateWriter(t *testing.T) {
	s, conversationID, task, cwd, successor, _ := mirrorFixture(t, "mirror-late-writer")
	ctx := context.Background()

	// The engagement that owns the conversation files its work.
	writeAgentMemory(t, cwd, "the successor's narrative")
	successor.check(ctx)

	// A zombie on the same conversation, in the tree it still holds, reaches
	// its own ending and files over it.
	zombieCwd := t.TempDir()
	writeAgentMemory(t, zombieCwd, "the zombie's narrative")
	runMirror(s, task, conversationID, "bpr-"+conversationID, zombieCwd, nil).settle(ctx)
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "the zombie's narrative" {
		t.Fatalf("agent_content = %q, want the zombie's — the fixture is not reproducing the clobber", got)
	}

	// Mid-run the successor cannot tell: its file has not changed, so neither
	// has its digest. This is the hazard, asserted rather than described.
	successor.check(ctx)
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "the zombie's narrative" {
		t.Errorf("agent_content = %q; a mid-run check is not expected to notice a row it did not write", got)
	}

	// Its ending is. At an ending the row is what the file says, full stop.
	successor.settle(ctx)
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "the successor's narrative" {
		t.Errorf("agent_content = %q, want the successor's — settle must re-assert its file", got)
	}
}

// TestMemoryMirror_RefusesThePredecessorsFile is the mirror's half of the rule
// the completion gate has always enforced: a blueprint's steps share a tree and
// all write the same filename, so a step that has written nothing must not have
// its predecessor's narrative filed as its own — which the mirror would
// otherwise do on the very first tool call, long before the gate could refuse.
func TestMemoryMirror_RefusesThePredecessorsFile(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "mirror-inherited")
	makeConversationBlueprintStep(t, database, conversationID, taskID)
	task := loadTask(t, s, taskID)
	counting := &countingTaskMemory{TaskMemoryStore: s.taskMemory}
	s.taskMemory = counting
	cwd := t.TempDir()

	writeAgentMemory(t, cwd, "the PREVIOUS step's narrative")
	inherited := fingerprintAgentMemoryFile(cwd)
	if inherited == nil {
		t.Fatal("fingerprint of an existing memory file = nil")
	}
	mirror := runMirror(s, task, conversationID, "bpr-"+conversationID, cwd, inherited)

	if state := mirror.check(context.Background()); state != memoryFileStale {
		t.Errorf("check() on the inherited file = %v, want memoryFileStale", state)
	}
	if counting.upserts != 0 {
		t.Errorf("upserts = %d for a file this step never wrote, want 0", counting.upserts)
	}

	// The same path, now holding this step's own work, is this step's.
	writeAgentMemory(t, cwd, "this step's own narrative")
	mirror.check(context.Background())
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "this step's own narrative" {
		t.Errorf("agent_content = %q, want this step's own narrative", got)
	}
}

// TestMemoryMirror_NativeHookFilesAndPassesTheOutcomeThrough drives the native
// runtime's hook point as the engine drives it — a scripted sequence of
// AfterToolCall calls — and pins both halves of the contract: the file lands in
// conversation_memory, and the tool result the model reads is byte-identical to
// the one dispatched. The hook may rewrite an outcome; this one must not.
func TestMemoryMirror_NativeHookFilesAndPassesTheOutcomeThrough(t *testing.T) {
	s, conversationID, task, cwd, mirror, counting := mirrorFixture(t, "mirror-native-hook")
	ctx := context.Background()

	script := []struct {
		name  string
		write string
		out   agentloop.ToolOutcome
	}{
		{name: "bash", out: agentloop.ToolOutcome{Content: "tests failed"}},
		{name: "write", write: "narrowed it to the fixture", out: agentloop.ToolOutcome{Content: "ok"}},
		{name: "bash", out: agentloop.ToolOutcome{Content: "tests pass", ToolError: "none"}},
	}
	for i, step := range script {
		if step.write != "" {
			writeAgentMemory(t, cwd, step.write)
		}
		got := mirror.afterToolCall(ctx, domain.ToolCall{ID: step.name, Name: step.name}, step.out)
		if !reflect.DeepEqual(got, step.out) {
			t.Errorf("afterToolCall(step %d) rewrote the outcome: %+v, want %+v", i, got, step.out)
		}
	}

	if counting.upserts != 1 {
		t.Errorf("upserts = %d across three tool calls with one write between them, want 1", counting.upserts)
	}
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "narrowed it to the fixture" {
		t.Errorf("agent_content = %q, want what the middle tool call wrote", got)
	}
}

// recordingSink is the inner Sink an activitySink decorates, scripted to
// succeed or to refuse the way a fenced write does.
type recordingSink struct {
	msgs []*domain.Message
	err  error
}

func (r *recordingSink) OnSession(string) error { return nil }

func (r *recordingSink) OnMessage(m *domain.Message) error {
	r.msgs = append(r.msgs, m)
	return r.err
}

// TestMemoryMirror_SDKToolRowsFileTheFile drives the SDK runtime's hook point —
// the activity sink the live driver installs — with a scripted stream. Only a
// tool row means a tool call resolved, so only a tool row is a moment the agent
// could have written its file.
func TestMemoryMirror_SDKToolRowsFileTheFile(t *testing.T) {
	s, conversationID, task, cwd, mirror, counting := mirrorFixture(t, "mirror-sdk-hook")
	inner := &recordingSink{}
	sink := newActivitySink(inner, make(chan struct{}, 8), mirror)

	writeAgentMemory(t, cwd, "what the agent worked out")

	// An assistant row is the model talking, not a tool resolving: nothing has
	// touched the filesystem between it and the last check.
	if err := sink.OnMessage(&domain.Message{ConversationID: conversationID, Role: "assistant", Content: "let me look"}); err != nil {
		t.Fatalf("OnMessage(assistant): %v", err)
	}
	if counting.upserts != 0 {
		t.Errorf("upserts = %d on an assistant row, want 0", counting.upserts)
	}

	if err := sink.OnMessage(&domain.Message{ConversationID: conversationID, Role: "tool", Content: "ok"}); err != nil {
		t.Fatalf("OnMessage(tool): %v", err)
	}
	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "what the agent worked out" {
		t.Errorf("agent_content = %q, want the file the tool row mirrored", got)
	}
	if len(inner.msgs) != 2 {
		t.Errorf("inner sink saw %d rows, want 2 — the mirror must not swallow the transcript", len(inner.msgs))
	}
}

// TestMemoryMirror_SDKFencedRowFilesNothing: a refused transcript write means a
// successor owns the conversation and this engagement may write nothing more
// about it. The memory row is one of those writes.
func TestMemoryMirror_SDKFencedRowFilesNothing(t *testing.T) {
	_, conversationID, _, cwd, mirror, counting := mirrorFixture(t, "mirror-sdk-fenced")
	inner := &recordingSink{err: db.ErrClaimReleased}
	sink := newActivitySink(inner, make(chan struct{}, 8), mirror)

	writeAgentMemory(t, cwd, "notes from an engagement that lost its claim")
	if err := sink.OnMessage(&domain.Message{ConversationID: conversationID, Role: "tool", Content: "ok"}); err == nil {
		t.Fatal("OnMessage returned nil on a refused insert; the driver would keep streaming into a successor's conversation")
	}
	if counting.upserts != 0 {
		t.Errorf("upserts = %d after a fenced row, want 0", counting.upserts)
	}
}

// TestParkConversationOpen_FilesTheAgentsMemoryFile is the boundary this whole
// mechanism exists for: a run parked mid-flight leaves its notes somewhere a
// handler on another pod can read, not only in a blob keyed to one executor's
// tree.
func TestParkConversationOpen_FilesTheAgentsMemoryFile(t *testing.T) {
	s, conversationID, task, cwd, mirror, _ := mirrorFixture(t, "mirror-park")
	writeAgentMemory(t, cwd, "halfway through the rebase when the user stopped me")

	if fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claudeCwd:      cwd,
		reason:         db.ParkIdle(),
		runtime:        domain.ConversationRuntimeSDK,
		mirror:         mirror,
	}, ""); fenced {
		t.Fatal("parkConversationOpen reported a fence trip on an unfenced store")
	}

	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "halfway through the rebase when the user stopped me" {
		t.Errorf("agent_content = %q, want the file as it stood at the park", got)
	}
}

// TestRecordNativeResult_FailureFilesTheAgentsMemoryFile: a failed conversation
// is exactly the one a person will want the agent's account of, and the failure
// is the last moment anyone holds its tree.
func TestRecordNativeResult_FailureFilesTheAgentsMemoryFile(t *testing.T) {
	s, conversationID, task, cwd, mirror, _ := mirrorFixture(t, "mirror-fail")
	writeAgentMemory(t, cwd, "the tool host kept dropping; the work so far is on the branch")

	if fenced := s.recordNativeResult(context.Background(), runmode.LocalDefaultOrgID, conversationID, task,
		runConfig{orgID: runmode.LocalDefaultOrgID, blueprintRunID: "bpr-" + conversationID},
		"bpr-"+conversationID, cwd, "event", "", time.Now(),
		agentloop.Result{Kind: agentloop.ResultFailed, FailureKind: domain.ConversationFailureAgentError}, mirror,
	); fenced {
		t.Fatal("recordNativeResult reported a fence trip on an unfenced store")
	}

	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "the tool host kept dropping; the work so far is on the branch" {
		t.Errorf("agent_content = %q, want the file as it stood at the failure", got)
	}
}

// TestProcessCompletion_ConclusionFilesWhatTheLastTurnWrote is why the final
// check is not redundant with the hooks: the conclusion turn makes no tool call
// after writing the file, so nothing mirrors it and the gate's own read is the
// only thing that will.
func TestProcessCompletion_ConclusionFilesWhatTheLastTurnWrote(t *testing.T) {
	s, conversationID, task, cwd, mirror, counting := mirrorFixture(t, "mirror-conclude")
	writeAgentMemory(t, cwd, "an early note")
	mirror.check(context.Background())

	// The last turn revises the file and concludes without calling another tool.
	writeAgentMemory(t, cwd, "what the run actually concluded")

	parked, fenced := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID,
		"bpr-"+conversationID, "", task, res(`{"outcome":"finish","summary":"done"}`), cwd, mirror, "", "event", "")
	if parked || fenced {
		t.Fatalf("processCompletion(finish) = (parked %v, fenced %v), want (false, false)", parked, fenced)
	}

	if got := memoryContentFor(t, s, task.EntityID, conversationID); got != "what the run actually concluded" {
		t.Errorf("agent_content = %q, want the file the conclusion turn left", got)
	}
	if counting.upserts != 2 {
		t.Errorf("upserts = %d, want 2 — one mirrored mid-run, one at the gate", counting.upserts)
	}
}

// TestMemoryMirror_MemoryMissingFlipsFalseMidRun: the flag has always meant
// "the agent did not write its own memory", and the mirror is what lets it
// answer that while the conversation is still running rather than only once it
// has ended.
func TestMemoryMirror_MemoryMissingFlipsFalseMidRun(t *testing.T) {
	s, conversationID, _, cwd, mirror, _ := mirrorFixture(t, "mirror-missing")

	if conv := loadConversation(t, s, conversationID); !conv.MemoryMissing {
		t.Fatal("memory_missing = false before the agent wrote anything")
	}

	writeAgentMemory(t, cwd, "what I found")
	mirror.check(context.Background())

	conv := loadConversation(t, s, conversationID)
	if conv.MemoryMissing {
		t.Error("memory_missing = true after the mirror filed the agent's file")
	}
	if conv.Status != "running" {
		t.Errorf("status = %q, want running — mirroring is not an ending", conv.Status)
	}
	if conv.EndedAt != nil {
		t.Error("ended_at is set; mirroring a live conversation's file must not end it")
	}
}
