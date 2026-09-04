package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The SDK path's reaction to a claim fence trip. The fence itself is a store
// property (internal/db, both dialects); what these pin is the half that lives
// here — that a refused write makes the engagement stop rather than carry on
// writing into a conversation somebody else now owns.
//
// A released claim is staged by making the store refuse, not by releasing a
// real one: the injected refusal counts which fenced door each write came
// through, which a real release cannot, and it fires on exactly the call
// under test rather than on every claim-keyed write a fixture happens to make.

// fencedConversationStore refuses the claim-fenced writes, passing everything
// else through to the real store.
type fencedConversationStore struct {
	db.ConversationStore
	inserts   int
	completes int
	cancels   int
	// sessions counts fenced session writes; unfencedSessions counts the ones
	// that went round the fence, which must stay zero once a claim is in scope.
	sessions         int
	unfencedSessions int
}

func (f *fencedConversationStore) SetSessionForClaimSystem(context.Context, string, string, string, string) (*domain.Conversation, error) {
	f.sessions++
	return nil, db.ErrClaimReleased
}

func (f *fencedConversationStore) SetSessionSystem(ctx context.Context, orgID, conversationID, sessionID string) (*domain.Conversation, error) {
	f.unfencedSessions++
	return f.ConversationStore.SetSessionSystem(ctx, orgID, conversationID, sessionID)
}

func (f *fencedConversationStore) InsertMessageForClaimSystem(context.Context, string, string, *domain.Message) (int64, error) {
	f.inserts++
	return 0, db.ErrClaimReleased
}

func (f *fencedConversationStore) CompleteForClaimSystem(context.Context, string, string, string, string, float64, int, int, string, string, string, string) (*domain.Conversation, error) {
	f.completes++
	return nil, db.ErrClaimReleased
}

func (f *fencedConversationStore) MarkFailedIfActiveForClaimSystem(context.Context, string, string, string, string) (bool, error) {
	return false, db.ErrClaimReleased
}

func (f *fencedConversationStore) ParkOpenForClaimSystem(context.Context, string, string, string, db.Park) (bool, error) {
	f.cancels++
	return false, db.ErrClaimReleased
}

// TestConversationSink_FenceTripKillsTheRunAndRecordsIt: a refused transcript write
// stops the agent and marks the engagement fenced, so runAgent abandons the
// run instead of writing a terminal for it.
func TestConversationSink_FenceTripKillsTheRunAndRecordsIt(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-sink")
	_ = database
	s.conversations = &fencedConversationStore{ConversationStore: s.conversations}

	// The run's cancel handle, as the dispatcher registers it — the sink's
	// only way to kill a live agent.
	killed := false
	s.mu.Lock()
	s.cancels[conversationID] = func() { killed = true }
	s.mu.Unlock()

	sink := newConversationSink(s, runmode.LocalDefaultOrgID, conversationID, "claim-1", "event", "")
	err := sink.OnMessage(&domain.Message{ConversationID: conversationID, Role: "assistant", Content: "hello"})
	if !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("OnMessage = %v, want ErrClaimReleased surfaced to the runtime", err)
	}
	if !sink.fenceTripped() {
		t.Error("sink did not record the fence trip; runAgent would go on to write a terminal")
	}
	if !killed {
		t.Error("sink did not kill the agent after being fenced out")
	}
}

// TestConversationSink_UnfencedWithoutAClaim: a sink with no claim in scope keeps the
// pre-fence write path, so the paths that never claimed a run are unaffected.
func TestConversationSink_UnfencedWithoutAClaim(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-sink-noclaim")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	sink := newConversationSink(s, runmode.LocalDefaultOrgID, conversationID, "", "event", "")
	if err := sink.OnMessage(&domain.Message{ConversationID: conversationID, Role: "assistant", Content: "hello"}); err != nil {
		t.Fatalf("OnMessage: %v", err)
	}
	if stub.inserts != 0 {
		t.Errorf("claimless sink took the fenced path %d times", stub.inserts)
	}
	if sink.fenceTripped() {
		t.Error("claimless sink reported a fence trip")
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 1 {
		t.Errorf("messages = %d, want the row written through the unfenced path", n)
	}
}

// TestConversationSink_SessionFenceTripDiscardsTheSessionID: the session id is the
// resume coordinate, so a refused write is not a write to retry — it is a
// write that must never land. The engagement stops, and the id dies with it;
// falling back to the unfenced door would be the corruption itself.
func TestConversationSink_SessionFenceTripDiscardsTheSessionID(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-session")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	killed := false
	s.mu.Lock()
	s.cancels[conversationID] = func() { killed = true }
	s.mu.Unlock()

	sink := newConversationSink(s, runmode.LocalDefaultOrgID, conversationID, "claim-1", "event", "")
	err := sink.OnSession("sess-zombie")
	if !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("OnSession = %v, want ErrClaimReleased surfaced to the runtime", err)
	}
	if !sink.fenceTripped() {
		t.Error("sink did not record the fence trip on the session write")
	}
	if !killed {
		t.Error("sink did not kill the agent after being fenced out")
	}
	if stub.unfencedSessions != 0 {
		t.Errorf("refused session write retried through the unfenced door %d times", stub.unfencedSessions)
	}
	// The coordinate on the row is whatever the owner put there — here the
	// fixture's seeded id, standing in for the successor's.
	var stored string
	if err := database.QueryRow(`SELECT COALESCE(sdk_session_id, '') FROM conversations WHERE id = ?`, conversationID).Scan(&stored); err != nil {
		t.Fatalf("read sdk_session_id: %v", err)
	}
	if stored != "sess-fence-session" {
		t.Errorf("sdk_session_id = %q, want the owner's sess-fence-session — a fenced-out engagement's session must not land", stored)
	}
}

// TestConversationSink_SessionWithoutAClaim: a manual run that never claimed keeps its
// pre-fence door, so the paths with no engagement to name are unaffected.
func TestConversationSink_SessionWithoutAClaim(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-session-noclaim")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	sink := newConversationSink(s, runmode.LocalDefaultOrgID, conversationID, "", "event", "")
	if err := sink.OnSession("sess-plain"); err != nil {
		t.Fatalf("OnSession: %v", err)
	}
	if stub.sessions != 0 {
		t.Errorf("claimless sink took the fenced path %d times", stub.sessions)
	}
	var stored string
	if err := database.QueryRow(`SELECT COALESCE(sdk_session_id, '') FROM conversations WHERE id = ?`, conversationID).Scan(&stored); err != nil {
		t.Fatalf("read sdk_session_id: %v", err)
	}
	if stored != "sess-plain" {
		t.Errorf("sdk_session_id = %q, want sess-plain", stored)
	}
}

// TestSetWorktreePath_RoutesByClaim: the workspace stamp picks its door the
// way updatePhase does — an engagement names its claim and meets the fence, a
// claimless writer (mint / enqueue, where no successor can exist yet) keeps the
// unfenced one.
func TestSetWorktreePath_RoutesByClaim(t *testing.T) {
	s, _, conversationID, _ := setupAdvanceFixture(t, "fence-worktree")
	stub := &worktreeFencedStore{ConversationStore: s.conversations}
	s.conversations = stub

	err := s.setWorktreePath(context.Background(), runmode.LocalDefaultOrgID, conversationID, "claim-1", "/tmp/triagefactory-runs/zombie")
	if !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("fenced stamp = %v, want ErrClaimReleased returned to the caller", err)
	}
	if stub.byClaim != 1 || stub.unfenced != 0 {
		t.Errorf("with a claim: byClaim=%d unfenced=%d, want 1/0", stub.byClaim, stub.unfenced)
	}

	if err := s.setWorktreePath(context.Background(), runmode.LocalDefaultOrgID, conversationID, "", "/tmp/triagefactory-runs/mint"); err != nil {
		t.Fatalf("claimless stamp: %v", err)
	}
	if stub.unfenced != 1 {
		t.Errorf("without a claim: unfenced=%d, want the claimless door", stub.unfenced)
	}
}

// TestEnsureWorkspace_RehydrateFenceTripLeavesTheSuccessorsPathAlone: a cold
// rehydrate that finishes after the claim moved on still returns its rebuilt
// path to the caller (the agent has to run somewhere), but must not stamp it —
// the row's path is the successor's, on whatever host it is running.
func TestEnsureWorkspace_RehydrateFenceTripLeavesTheSuccessorsPathAlone(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-rehydrate")
	stub := &worktreeFencedStore{ConversationStore: s.conversations}
	s.conversations = stub

	// The successor's stamp, standing where the fixture's seeded path was.
	if _, err := database.Exec(`UPDATE conversations SET worktree_path = ? WHERE id = ?`,
		"/tmp/triagefactory-runs/successors-host", conversationID); err != nil {
		t.Fatalf("seed successor path: %v", err)
	}

	// The zombie's rebuild lands somewhere else and tries to record it.
	err := s.setWorktreePath(context.Background(), runmode.LocalDefaultOrgID, conversationID, "claim-1", "/tmp/triagefactory-runs/zombies-host")
	if !errors.Is(err, db.ErrClaimReleased) {
		t.Fatalf("rehydrate stamp = %v, want ErrClaimReleased", err)
	}
	if stub.unfenced != 0 {
		t.Errorf("refused rehydrate stamp fell back to the unfenced door %d times", stub.unfenced)
	}

	var path string
	if err := database.QueryRow(`SELECT COALESCE(worktree_path, '') FROM conversations WHERE id = ?`, conversationID).Scan(&path); err != nil {
		t.Fatalf("read worktree_path: %v", err)
	}
	if path != "/tmp/triagefactory-runs/successors-host" {
		t.Errorf("worktree_path = %q, want the successor's host — a fenced-out rehydrate must not re-point the row", path)
	}
}

// worktreeFencedStore refuses the claim-keyed workspace stamp and counts which
// door each call came through.
type worktreeFencedStore struct {
	db.ConversationStore
	byClaim  int
	unfenced int
}

func (w *worktreeFencedStore) SetWorktreePathForClaimSystem(context.Context, string, string, string, string) (*domain.Conversation, error) {
	w.byClaim++
	return nil, db.ErrClaimReleased
}

func (w *worktreeFencedStore) SetWorktreePathSystem(context.Context, string, string, string) (*domain.Conversation, error) {
	w.unfenced++
	return nil, nil
}

// TestStampExecutor_RoutesByClaim: the go-live ownership stamp is an assertion
// about who is running the conversation, so an engagement makes it against its
// own claim. A refusal is logged and nothing else — the engagement carries on
// to a first transcript write that meets the same fence.
func TestStampExecutor_RoutesByClaim(t *testing.T) {
	s, _, conversationID, _ := setupAdvanceFixture(t, "fence-executor")
	stub := &executorFencedStore{ConversationStore: s.conversations}
	s.conversations = stub

	s.stampExecutor(runmode.LocalDefaultOrgID, conversationID, "claim-1")
	if stub.byClaim != 1 || stub.unfenced != 0 {
		t.Errorf("with a claim: byClaim=%d unfenced=%d, want 1/0", stub.byClaim, stub.unfenced)
	}

	s.stampExecutor(runmode.LocalDefaultOrgID, conversationID, "")
	if stub.unfenced != 1 {
		t.Errorf("without a claim: unfenced=%d, want the active-claim fallback", stub.unfenced)
	}
}

// executorFencedStore refuses the claim-keyed go-live stamp and counts doors.
type executorFencedStore struct {
	db.ConversationStore
	byClaim  int
	unfenced int
}

func (e *executorFencedStore) SetExecutorForClaimSystem(context.Context, string, string, string, string, int64) (*domain.ExecutorClaim, error) {
	e.byClaim++
	return nil, db.ErrClaimReleased
}

func (e *executorFencedStore) SetExecutorSystem(context.Context, string, string, string, int64) (*domain.ExecutorClaim, error) {
	e.unfenced++
	return nil, nil
}

// TestProcessCompletion_IdleParkFenceTripReportsFenced: a no-conclusion turn
// parks the run, and that park now carries the claim. When it is refused the
// disposition is fenced — the caller must not advance the blueprint behind a
// run somebody else is driving — while parked stays true so the workspace the
// successor may be running in is left alone.
func TestProcessCompletion_IdleParkFenceTripReportsFenced(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "fence-idle-park")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	parked, fenced := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID,
		"bpr-"+conversationID, "claim-1", loadTask(t, s, taskID),
		res("just thinking out loud, no envelope here"), t.TempDir(), nil, "", "event", "")
	if !fenced {
		t.Fatal("a refused idle park reported unfenced; the caller would react to a run it no longer owns")
	}
	if !parked {
		t.Error("processCompletion(idle park, fenced) = parked false; the workspace would be torn down under a successor")
	}
	if stub.cancels != 1 {
		t.Errorf("fenced idle park attempted %d times, want 1", stub.cancels)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run status = %q, want running (a refused park must leave the row alone)", status)
	}
}

// TestProcessCompletion_FenceTripRecordsNothing: the result of an engagement
// that lost its claim is not the run's disposition. The terminal is refused,
// and processCompletion reports both "fenced" (so the caller skips the
// blueprint reactor) and "parked" (so the workspace the successor may be
// using is left on disk).
func TestProcessCompletion_FenceTripRecordsNothing(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "fence-complete")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	parked, fenced := s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, conversationID,
		"bpr-"+conversationID, "claim-1", loadTask(t, s, taskID),
		res(`{"outcome":"finish","summary":"done"}`), t.TempDir(), nil, "", "event", "")
	if !fenced {
		t.Fatal("processCompletion did not report the fence trip; the caller would react to a run it no longer owns")
	}
	if !parked {
		t.Error("processCompletion(fenced) = parked false; the workspace would be torn down under a successor")
	}
	if stub.completes != 1 {
		t.Errorf("fenced terminal attempted %d times, want 1", stub.completes)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run status = %q, want running (a refused terminal must leave the row alone)", status)
	}
	var msgs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID).Scan(&msgs); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgs != 0 {
		t.Errorf("messages = %d, want none written after the fence trip", msgs)
	}
}

// TestFailConversation_FenceTripRecordsNothing: the same for the infra-failure
// terminal — a fenced-out engagement's crash is not the successor's failure.
func TestFailConversation_FenceTripRecordsNothing(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "fence-fail")
	s.conversations = &fencedConversationStore{ConversationStore: s.conversations}

	if fenced := s.failConversation(runmode.LocalDefaultOrgID, conversationID, taskID, "claim-1", "event", "", "boom", domain.ConversationFailureCrash); !fenced {
		t.Fatal("failConversation did not report the fence trip; the dispatcher would react to a run it no longer owns")
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run status = %q, want running (a refused failure must leave the row alone)", status)
	}
	var msgs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID).Scan(&msgs); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgs != 0 {
		t.Errorf("messages = %d, want no failure row on a conversation this engagement no longer owns", msgs)
	}
}

// TestUpdatePhase_RoutesByClaimAndSurvivesAFenceTrip: with a claim in scope
// the phase write is claim-keyed (so the store can refuse it); without one it
// falls back to the conversation's active claim. A refusal is reported to the
// caller and nothing else happens — the store has already kept the write off
// the owner's claim, so there is nothing to undo, and it is the caller that
// knows whether a runtime is about to be launched on the strength of it.
func TestUpdatePhase_RoutesByClaimAndReportsAFenceTrip(t *testing.T) {
	s, _, conversationID, _ := setupAdvanceFixture(t, "fence-phase")
	stub := &phaseFencedStore{ConversationStore: s.conversations}
	s.conversations = stub

	if fenced := s.updatePhase(context.Background(), runmode.LocalDefaultOrgID, conversationID, "claim-1", "cloning"); !fenced {
		t.Error("with a claim: the refused write was not reported as fenced")
	}
	if stub.byClaim != 1 || stub.byConversation != 0 {
		t.Errorf("with a claim: byClaim=%d byConversation=%d, want 1/0", stub.byClaim, stub.byConversation)
	}

	if fenced := s.updatePhase(context.Background(), runmode.LocalDefaultOrgID, conversationID, "", "cloning"); fenced {
		t.Error("without a claim: the unfenced door reported a fence trip")
	}
	if stub.byConversation != 1 {
		t.Errorf("without a claim: byConversation=%d, want the active-claim fallback", stub.byConversation)
	}
}

// phaseFencedStore refuses the claim-keyed phase write and counts which door
// each call came through.
type phaseFencedStore struct {
	db.ConversationStore
	byClaim        int
	byConversation int
}

func (p *phaseFencedStore) SetClaimPhaseSystem(context.Context, string, string, string, string) (*domain.ExecutorClaim, error) {
	p.byClaim++
	return nil, db.ErrClaimReleased
}

func (p *phaseFencedStore) SetActiveClaimPhaseSystem(context.Context, string, string, string) (*domain.ExecutorClaim, error) {
	p.byConversation++
	return nil, nil
}

// TestParkConversationOpen_CancelFenceTripRecordsNothing: the path a partition
// self-fence trip drives every live run down. Killing the sandboxes cancels
// each run's ctx, and each goroutine then arrives here to record its own
// cancellation — so if the self-fence fired late, this is the write that
// would bury a successor's live conversation. It must be refused, and
// nothing around it may happen either.
func TestParkConversationOpen_CancelFenceTripRecordsNothing(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-cancelled")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	// A worktree dir the unfenced path would delete on its way out.
	wt := t.TempDir()
	marker := filepath.Join(wt, "keep-me")
	if err := os.WriteFile(marker, []byte("successor's workspace"), 0o600); err != nil {
		t.Fatalf("seed worktree marker: %v", err)
	}

	fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claudeCwd:      wt,
		triggerType:    "event",
		claimID:        "claim-1",
		reason:         db.ParkStopped("user_cancelled", "Cancelled by user"),
	}, "")
	if !fenced {
		t.Fatal("parkConversationOpen did not report the fence trip; runAgent would fall through to the unfenced disposition")
	}
	if stub.cancels != 1 {
		t.Errorf("fenced cancellation park attempted %d times, want 1", stub.cancels)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run status = %q, want running (a refused cancellation must leave the row alone)", status)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("worktree removed after a fence trip (%v); it may be the workspace the successor is running in", err)
	}
}

// TestParkConversationOpen_ResumeCancelFenceTripRecordsNothing: the resume path's own
// self-cancel, same contract — and the workspace snapshot the successor
// resumes from survives. It is the same park as every other stop now; what
// this pins is that routing it through the fence (claimID set) still refuses.
func TestParkConversationOpen_ResumeCancelFenceTripRecordsNothing(t *testing.T) {
	s, database, conversationID, _ := setupAdvanceFixture(t, "fence-resume-cancel")
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	fenced := s.markConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		triggerType:    "manual",
		claimID:        "claim-1",
		reason:         db.ParkStopped("user_cancelled", "Run cancelled by user"),
	})
	if !fenced {
		t.Fatal("the resume cancel did not report the fence trip; the blueprint would be finalized off a successor's run")
	}
	if stub.cancels != 1 {
		t.Errorf("fenced cancellation attempted %d times, want 1", stub.cancels)
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run status = %q, want running (a refused cancellation must leave the row alone)", status)
	}
}
