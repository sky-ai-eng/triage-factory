package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeController records control-seam calls so the routing in SendMessage and
// Interrupt can be asserted without spawning a live subprocess. It satisfies
// RunController; the real inProcessController is exercised separately.
type fakeController struct {
	mu                      sync.Mutex
	steerCalls              int
	steerConversationID     string
	steerText               string
	interruptCalls          int
	interruptConversationID string
}

func (f *fakeController) Interrupt(_ context.Context, conversationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptCalls++
	f.interruptConversationID = conversationID
	return nil
}

func (f *fakeController) Steer(_ context.Context, conversationID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerCalls++
	f.steerConversationID = conversationID
	f.steerText = text
	return nil
}

func (f *fakeController) Cancel(string) bool { return false }

// TestSendMessage_LiveProcessRoutesToSteer: a conversation with a registered
// live process is steered in place through the control seam — asserted via a
// fake controller, and with no DB wired at all, which is the point of the fast
// path.
//
// It is also the test that pins the fast path's precondition. The registry is
// written only by the SDK live driver, so a handle being present is proof the
// conversation is an SDK one; that is why routing here without reading the
// runtime is safe.
func TestSendMessage_LiveProcessRoutesToSteer(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc
	// The handle's LiveRun is never touched — the fake controller stands in for
	// the real Steer.
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "run-live", runmode.LocalDefaultUserID, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 1 || fc.steerConversationID != "run-live" || fc.steerText != "hello" {
		t.Errorf("steer = {calls:%d conversationID:%q text:%q}, want {1 run-live hello}", fc.steerCalls, fc.steerConversationID, fc.steerText)
	}
}

// TestSendMessage_ParkedQueuesAndWakes is the follow-up path's core contract,
// under both runtimes: a parked conversation gets the message written as an
// undelivered row and the row flipped back to mid-flight so an executor picks
// it up. Delivery is a separate, later concern
// (TestDispatchResumeClaim_DeliversRecordedInput for the SDK; the native loop's
// own drain otherwise).
func TestSendMessage_ParkedQueuesAndWakes(t *testing.T) {
	for _, runtime := range []string{"sdk", "native"} {
		t.Run(runtime, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedConversation(t, database, "r-open", "sess-open", t.TempDir())
			if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-open'`); err != nil {
				t.Fatalf("open: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
			fc := &fakeController{}
			s.controller = fc
			if runtime == "native" {
				markNative(t, database, "r-open")
			}

			if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-open", runmode.LocalDefaultUserID, "go on"); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}

			if st := storedStatus(t, database, "r-open"); st != "" {
				t.Errorf("stored status = %q, want none — a parked conversation woken by input goes back to mid-flight, which is what makes it claimable again", st)
			}
			if fc.steerCalls != 0 {
				t.Errorf("controller steer calls = %d, want 0 — nothing is live to steer", fc.steerCalls)
			}
			pending := pendingRows(t, s, "r-open")
			if len(pending) != 1 || pending[0].Content != "go on" || pending[0].Role != "user" {
				t.Fatalf("the message must be queued as one undelivered user row: %+v", pending)
			}
		})
	}
}

// TestSendMessage_RunningRemoteRoutesToController pins the multi-mode fix: a
// running run with NO process in THIS pod's registry — the shape a control pod
// always sees, since a running run's process lives on an executor — must route
// through the controller (which delivers over conversation_signals to the owner), NOT be
// misread as unsteerable because the LOCAL registry missed. Before the fix this
// fell through to the resumable check (running isn't resumable) and 409'd.
func TestSendMessage_RunningRemoteRoutesToController(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-run", "sess-run", "/tmp/wt-run")
	if _, err := database.Exec(`UPDATE conversations SET status='running' WHERE id='r-run'`); err != nil {
		t.Fatalf("running: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc
	// Deliberately do NOT registerProc: getProc stays nil, exactly as on a control
	// pod whose running run's process lives on an executor. The controller (real
	// crossPodController in production) is what resolves the remote owner; here the
	// fake stands in to assert the routing decision, not the delivery.

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-run", runmode.LocalDefaultUserID, "steer me"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 1 || fc.steerConversationID != "r-run" || fc.steerText != "steer me" {
		t.Errorf("steer = {calls:%d conversationID:%q text:%q}, want {1 r-run \"steer me\"} — a running-but-remote run must route to the controller, not 409", fc.steerCalls, fc.steerConversationID, fc.steerText)
	}
}

// TestSendMessage_QueuedSDKNotSteerable pins the one place `queued` answers
// differently per runtime, and the reason: an SDK conversation waiting for its
// FIRST claim has no session to resume, and its undelivered rows are read by
// exactly one thing — the resume dispatch — so staging a row against it would
// route that claim down the resume path instead of running the step's mission
// at all. Native's queued arm is the opposite (see
// TestSendMessage_NativeQueuedAppendsWithoutWaking) because its loop drains the
// transcript itself.
func TestSendMessage_QueuedSDKNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-q", "sess-q", "/tmp/wt-q")
	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id='r-q'`); err != nil {
		t.Fatalf("queued: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-q", runmode.LocalDefaultUserID, "hi")
	if !errors.Is(err, ErrConversationNotSteerable) {
		t.Errorf("err = %v, want ErrConversationNotSteerable for a queued SDK conversation", err)
	}
	if fc.steerCalls != 0 {
		t.Errorf("steer calls = %d, want 0 — a queued run has no owner to steer", fc.steerCalls)
	}
	if got := pendingRows(t, s, "r-q"); len(got) != 0 {
		t.Errorf("a refused message must not leave a row behind: %+v", got)
	}
}

// TestResumableState pins the wake gate the routing and the
// MarkQueuedForResume CAS both key on: every state a conversation comes to
// rest on is resumable except `failed`, and outcome does not discriminate.
func TestResumableState(t *testing.T) {
	cases := []struct {
		status, outcome string
		want            bool
	}{
		{"open", "", true},
		{"completed", "abort", true},
		{"completed", "finish", true}, // concluded work is followed up on
		{"completed", "continue", true},
		{"completed", "", true},
		{"running", "", false},
		{"queued", "", false},
		{"failed", "", false},      // the one rest state with no workspace left
		{"failed", "abort", false}, // outcome never rescues a status
	}
	for _, tc := range cases {
		if got := resumableState(tc.status, tc.outcome); got != tc.want {
			t.Errorf("resumableState(%q, %q) = %v, want %v", tc.status, tc.outcome, got, tc.want)
		}
	}
}

// TestDrainsUndeliveredInput pins the second surviving runtime-conditional:
// which conversations read a queued row without being woken for it. Only the
// native loop drains, so only native says yes — and only while something is
// driving it or about to.
//
// The SDK rows are all false by construction, and two of them for different
// reasons: `running` never reaches the question (SendMessage sends it to
// Steer), and `queued` reaches it and must still refuse, because a step waiting
// for its first claim has no session for the resume dispatch to resume.
func TestDrainsUndeliveredInput(t *testing.T) {
	cases := []struct {
		runtime, status string
		want            bool
	}{
		{domain.ConversationRuntimeNative, domain.StatusRunning, true},
		{domain.ConversationRuntimeNative, domain.ClaimPhaseCloning, true},
		{domain.ConversationRuntimeNative, domain.StatusQueued, true},
		{domain.ConversationRuntimeNative, domain.StatusOpen, false},      // parked: needs a wake
		{domain.ConversationRuntimeNative, domain.StatusCompleted, false}, // ditto
		{domain.ConversationRuntimeNative, domain.StatusFailed, false},

		{domain.ConversationRuntimeSDK, domain.StatusRunning, false},
		{domain.ConversationRuntimeSDK, domain.StatusQueued, false},
		{domain.ConversationRuntimeSDK, domain.StatusOpen, false},
		{domain.ConversationRuntimeSDK, domain.StatusFailed, false},
	}
	for _, tc := range cases {
		got := drainsUndeliveredInput(domain.Conversation{Runtime: tc.runtime, Status: tc.status})
		if got != tc.want {
			t.Errorf("drainsUndeliveredInput(%s/%s) = %v, want %v", tc.runtime, tc.status, got, tc.want)
		}
	}
}

// TestBlueprintDrivableForClaim walks the gate the claim scan applies in SQL
// and the dispatcher + resume pre-check apply in Go. One rule under every
// status: the blueprint drives the step its current_step_index names, and no
// other. An earlier step is refused while the blueprint is still RUNNING for
// the sharpest reason of all — a later step is executing in the shared worktree
// right now, so driving this one puts two agents in one tree and lets a stale
// terminal rewrite the sequence out from under the live step.
//
// The refusal holds for every terminal too, `aborted` included. An aborted
// blueprint's own step resumes and re-opens it, but an earlier one does not —
// the re-open is conditioned on the conversation's completed+abort terminal, so
// waking an earlier step leaves the blueprint terminal with current_step_index
// pointing past the row, and nothing ever claims it.
func TestBlueprintDrivableForClaim(t *testing.T) {
	idx := func(i int) *int { return &i }
	cases := []struct {
		name      string
		status    domain.BlueprintRunStatus
		cancelReq bool
		stepIndex *int // the conversation's; the blueprint is on step 2
		want      bool
	}{
		{"running drives its current step", domain.BlueprintRunStatusRunning, false, idx(2), true},
		{"running refuses an earlier step", domain.BlueprintRunStatusRunning, false, idx(0), false},
		{"running refuses a nil step", domain.BlueprintRunStatusRunning, false, nil, false},
		{"cancel_requested drives nothing", domain.BlueprintRunStatusRunning, true, idx(2), false},
		{"cancelled terminal drives nothing", domain.BlueprintRunStatusCancelled, false, idx(2), false},

		{"completed, final step", domain.BlueprintRunStatusCompleted, false, idx(2), true},
		{"completed, earlier step", domain.BlueprintRunStatusCompleted, false, idx(0), false},
		{"aborted, final step", domain.BlueprintRunStatusAborted, false, idx(2), true},
		{"aborted, earlier step", domain.BlueprintRunStatusAborted, false, idx(0), false},
		{"failed, final step", domain.BlueprintRunStatusFailed, false, idx(2), true},
		{"failed, earlier step", domain.BlueprintRunStatusFailed, false, idx(0), false},

		// A conversation that never recorded its position cannot prove it is
		// the one holding the workspace — the Go spelling of the SQL's NULL
		// comparison.
		{"terminal, no step index", domain.BlueprintRunStatusCompleted, false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			br := &domain.BlueprintRun{Status: tc.status, CancelRequested: tc.cancelReq, CurrentStepIndex: 2}
			if got := blueprintDrivableForClaim(br, tc.stepIndex); got != tc.want {
				t.Errorf("blueprintDrivableForClaim = %v, want %v", got, tc.want)
			}
		})
	}

	// No blueprint parent (interactive, reserved) is not this
	// gate's business — it mirrors the SQL's LEFT JOIN, not an inner one.
	if !blueprintDrivableForClaim(nil, nil) {
		t.Error("a nil blueprint must be drivable")
	}
}

// TestInjectionWillFlush pins the narrower predicate the staged-injection
// producers use, and the gap between it and resumableState: a conversation
// that FINISHED can be woken by a person but has nothing coming on its own, so
// an automated injection staged against it would wait on a follow-up that may
// never arrive.
func TestInjectionWillFlush(t *testing.T) {
	cases := []struct {
		status, outcome string
		want            bool
	}{
		{"open", "", true},
		{"completed", "abort", true},
		{"completed", "finish", false},
		{"completed", "", false},
		{"failed", "", false},
	}
	for _, tc := range cases {
		if got := injectionWillFlush(tc.status, tc.outcome); got != tc.want {
			t.Errorf("injectionWillFlush(%q, %q) = %v, want %v", tc.status, tc.outcome, got, tc.want)
		}
		if injectionWillFlush(tc.status, tc.outcome) && !resumableState(tc.status, tc.outcome) {
			t.Errorf("injectionWillFlush(%q, %q) is true where resumableState is false; it must stay the narrower of the two",
				tc.status, tc.outcome)
		}
	}
}

// TestSendMessage_CompletedAbortIsResumable: a completed+abort run passes the
// steerable gate and routes to a resume (the agent's voluntary stop can be
// picked back up). This is also the path a terminal run with an unresolved
// artifact resumes through — approval is never a parked status.
func TestSendMessage_CompletedAbortIsResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-ab", "sess-ab", "/tmp/does-not-exist-ab")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort' WHERE id='r-ab'`); err != nil {
		t.Fatalf("completed+abort: %v", err)
	}
	// The blueprint an abort terminates, as the reactor leaves it — without
	// this the fixture is staging the hand-off window, not a stopped run.
	settleConversationBlueprint(t, database, "r-ab", "aborted")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-ab", runmode.LocalDefaultUserID, "pick it back up")
	if errors.Is(err, ErrConversationNotSteerable) {
		t.Errorf("completed+abort run rejected at the steerable gate: %v", err)
	}
	if st := storedStatus(t, database, "r-ab"); st != "" {
		t.Errorf("stored status = %q, want none (resume-by-enqueue's un-terminal write)", st)
	}
}

// TestSendMessage_CompletedFinishIsResumable: the case the epic exists for. A
// blueprint whose final step finished cleanly takes a follow-up — the message
// is recorded, the row goes back to mid-flight, and the blueprint is left
// exactly as it was.
func TestSendMessage_CompletedFinishIsResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-fin", "sess-fin", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-fin'`); err != nil {
		t.Fatalf("completed+finish: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET status='completed', current_step_index=0 WHERE id='seedbpr-r-fin'`); err != nil {
		t.Fatalf("finish blueprint: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-fin", runmode.LocalDefaultUserID, "more please"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if st := storedStatus(t, database, "r-fin"); st != "" {
		t.Errorf("stored status = %q, want none (resume-by-enqueue's un-terminal write)", st)
	}
	var bpStatus string
	var bpStep int
	if err := database.QueryRow(`SELECT status, current_step_index FROM blueprint_runs WHERE id='seedbpr-r-fin'`).Scan(&bpStatus, &bpStep); err != nil {
		t.Fatalf("read blueprint: %v", err)
	}
	if bpStatus != "completed" || bpStep != 0 {
		t.Errorf("blueprint moved to (%q, %d); finished machinery never restarts", bpStatus, bpStep)
	}
}

// TestSendMessage_MissingConversationNotFound: an unknown run id (no process, no row)
// answers "not found" rather than "not steerable" — the two sentinels ask for
// different client reactions, and the endpoint maps this one to 404 so a run
// deleted between its visibility read and this routing read doesn't read as a
// conflict worth re-reading.
func TestSendMessage_MissingConversationNotFound(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "ghost", runmode.LocalDefaultUserID, "hi")
	if !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("err = %v, want ErrConversationNotFound", err)
	}
}

// TestSendMessage_SteerRecordsAfterDelivery: the live-steer path writes the
// transcript row the steer itself never does — one delivered user row,
// attributed to the sender — and writes it only once the process has taken
// the text. A live steer injects into the process without touching the queue,
// so without this write the steered turn would be missing from the transcript
// entirely.
func TestSendMessage_SteerRecordsAfterDelivery(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-rec", "sess-rec", "/tmp/wt-rec")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc
	s.registerProc(runmode.LocalDefaultOrgID, "r-rec", &agentproc.LiveRun{})

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-rec", runmode.LocalDefaultUserID, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 1 {
		t.Fatalf("steer calls = %d, want 1", fc.steerCalls)
	}

	var role, userID, content string
	var delivered bool
	if err := database.QueryRow(`SELECT role, delivered, COALESCE(user_id, ''), content FROM messages WHERE conversation_id='r-rec'`).Scan(&role, &delivered, &userID, &content); err != nil {
		t.Fatalf("read recorded message: %v", err)
	}
	if role != "user" || !delivered || userID != runmode.LocalDefaultUserID || content != "hello" {
		t.Errorf("recorded message = {role:%q delivered:%v user:%q content:%q}, want one delivered user row attributed to the sender",
			role, delivered, userID, content)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id='r-rec'`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 1 {
		t.Errorf("messages recorded = %d, want exactly 1", n)
	}
}

// TestSendMessage_EmptyInputsRefusedBeforeSteer: the validation gate sits
// ahead of the routing, so the live-steer arm refuses an empty message and an
// empty user id exactly as the queued follow-up path does — nothing reaches
// the process and nothing is recorded.
func TestSendMessage_EmptyInputsRefusedBeforeSteer(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-blank", "sess-blank", "/tmp/wt-blank")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc
	s.registerProc(runmode.LocalDefaultOrgID, "r-blank", &agentproc.LiveRun{})

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-blank", runmode.LocalDefaultUserID, ""); err == nil {
		t.Error("expected an error for an empty message")
	}
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-blank", "", "hello"); err == nil {
		t.Error("expected an error for an empty user id")
	}
	if fc.steerCalls != 0 {
		t.Errorf("steer calls = %d, want 0 — a refused input must never reach the process", fc.steerCalls)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id='r-blank'`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 0 {
		t.Errorf("messages recorded = %d, want 0", n)
	}
}

// TestSendMessage_SteerRecordSurvivesRequestCancellation: the bookkeeping
// insert runs detached from the request's cancellation. A client that
// disconnects (or a handler that times out) right after the controller accepts
// the steer must not cost the transcript its only record of a turn the agent
// is already acting on.
func TestSendMessage_SteerRecordSurvivesRequestCancellation(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-gone", "sess-gone", "/tmp/wt-gone")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc
	s.registerProc(runmode.LocalDefaultOrgID, "r-gone", &agentproc.LiveRun{})

	// A ctx already canceled when SendMessage runs is the post-steer
	// disconnect at its sharpest: the fake controller (like a real steer whose
	// delivery beat the disconnect) still accepts, and everything after it
	// sees only the canceled ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SendMessage(ctx, runmode.LocalDefaultOrgID, "r-gone", runmode.LocalDefaultUserID, "keep going"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var content string
	if err := database.QueryRow(`SELECT content FROM messages WHERE conversation_id='r-gone'`).Scan(&content); err != nil {
		t.Fatalf("read recorded message: %v", err)
	}
	if content != "keep going" {
		t.Errorf("recorded content = %q, want %q", content, "keep going")
	}
}

// TestSendMessage_RefusedSteerRecordsNothing: a running SDK run with no live
// process anywhere — the orphaned window a restart leaves behind — refuses the
// send with ErrNoLiveProcess and writes no transcript row. This is the window
// where the message endpoint's old record-first ordering showed a user their
// words in the transcript alongside the error toast for the same send.
func TestSendMessage_RefusedSteerRecordsNothing(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-orphan", "sess-orphan", "/tmp/wt-orphan")
	// Status stays `running` (seedConversation's default) and no process is registered:
	// the default inProcessController answers the steer with ErrNoLiveProcess.
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-orphan", runmode.LocalDefaultUserID, "hello?")
	if !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("err = %v, want ErrNoLiveProcess", err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id='r-orphan'`).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 0 {
		t.Errorf("messages recorded = %d, want 0 — a refused steer must leave no row behind", n)
	}
}

// TestInterrupt_LiveRoutesToController: Spawner.Interrupt drives the live
// process through the control seam — asserted via a fake controller.
func TestInterrupt_LiveRoutesToController(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc

	if err := s.Interrupt(context.Background(), "run-live"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if fc.interruptCalls != 1 || fc.interruptConversationID != "run-live" {
		t.Errorf("interrupt = {calls:%d conversationID:%q}, want {1 run-live}", fc.interruptCalls, fc.interruptConversationID)
	}
}

// TestSteerSendError_ClosingReadsAsNoLiveProcess: a process the driver is
// closing still sits in the registry until Close returns, and refuses the
// send. The steer reports that as ErrNoLiveProcess — the 409 that tells the
// client to re-read the conversation, which by then has parked and takes the
// message through the queue — and leaves every other send error as it is.
func TestSteerSendError_ClosingReadsAsNoLiveProcess(t *testing.T) {
	if err := steerSendError("run-closing", agentproc.ErrRunClosing); !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("closing process: err = %v, want ErrNoLiveProcess", err)
	}
	if err := steerSendError("run-live", nil); err != nil {
		t.Errorf("accepted send: err = %v, want nil", err)
	}
	other := errors.New("write: broken pipe")
	if err := steerSendError("run-live", other); !errors.Is(err, other) || errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("write fault: err = %v, want the fault itself", err)
	}
}
