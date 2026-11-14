package delegate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// These run a real Claude Code process under the driver, wired as
// runLiveAndDrive wires it (the OnResult channel, the PostToolBatch hook
// answered by notesAtToolBatch, the TF tools, the activity sink over the
// conversation sink), against an in-memory store, and send input the way the
// message endpoint and the event router do. Gated like agentproc's live
// smokes.

// sdkLiveCheck is one live SDK engagement under test.
type sdkLiveCheck struct {
	t       *testing.T
	s       *Spawner
	park    liveParkContext
	lr      *agentproc.LiveRun
	ctx     context.Context
	uses    chan string            // Bash tool_use ids, as each call streams
	batches chan string            // the text each tool batch handed the model
	results chan *agentproc.Result // every result the process produced
	out     chan liveOutcome
}

// startSDKLiveCheck seeds a claimed conversation, starts a live process on
// message for it, registers the process, and runs the driver on it in the
// background. The driver's outcome arrives on out.
func startSDKLiveCheck(t *testing.T, convID, message string) *sdkLiveCheck {
	t.Helper()
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live SDK checks")
	}
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	cwd := t.TempDir()
	seedConversation(t, database, convID, "", cwd)
	claimID := markEngaged(t, database, convID)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	park := liveParkContext{
		orgID: org, conversationID: convID, namespace: "seedbpr-" + convID, claudeCwd: cwd,
		claimID: claimID, reason: db.ParkIdle(), runtime: domain.ConversationRuntimeSDK,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	e := &sdkLiveCheck{
		t: t, s: s, park: park, ctx: ctx,
		uses: make(chan string, 16), batches: make(chan string, 16),
		results: make(chan *agentproc.Result, 16), out: make(chan liveOutcome, 1),
	}
	driverResults := make(chan *agentproc.Result, resultsBufferDepth)
	opts := agentproc.RunOptions{
		Cwd:          cwd,
		Message:      message,
		Model:        "haiku",
		AllowedTools: "Bash(sleep *),Bash(echo *)",
		TraceID:      "live-input-" + convID,
		OnResult: func(r *agentproc.Result) {
			select {
			case e.results <- r:
			default:
			}
			select {
			case driverResults <- r:
			default:
			}
		},
		OnToolBatch: func() agentproc.ToolBatchNotes {
			n := s.notesAtToolBatch(org, convID, claimID)
			select {
			case e.batches <- n.Text:
			default:
			}
			return n
		},
		TFTools: sdkTFTools(true),
	}
	inner := newConversationSink(s, org, convID, claimID, "manual", runmode.LocalDefaultUserID)
	sink := &toolStartSink{
		activitySink: newActivitySink(inner, nil, s.activityFor(convID), s.resolvedActivityTimings()),
		uses:         e.uses,
	}
	lr, err := agentproc.RunInteractive(ctx, opts, sink, nil)
	if err != nil {
		cancel()
		t.Fatalf("RunInteractive: %v", err)
	}
	e.lr = lr
	s.registerProc(org, convID, lr)
	go func() { e.out <- s.driveLiveConversation(ctx, park, lr, driverResults, nil) }()
	t.Cleanup(func() {
		s.deregisterProc(convID)
		_ = lr.Close()
		cancel()
	})
	return e
}

// toolStartSink reports each Bash call as it streams, which the activity sink
// keeps to itself, so a test can time input into the call.
type toolStartSink struct {
	*activitySink
	uses chan string
}

func (k *toolStartSink) OnToolUse(id, name string) {
	k.activitySink.OnToolUse(id, name)
	if name == "Bash" {
		select {
		case k.uses <- id:
		default:
		}
	}
}

func (e *sdkLiveCheck) firstBash(within time.Duration) {
	e.t.Helper()
	select {
	case <-e.uses:
	case <-time.After(within):
		e.t.Fatalf("no Bash call within %s", within)
	}
}

func (e *sdkLiveCheck) firstBatch(within time.Duration) string {
	e.t.Helper()
	select {
	case text := <-e.batches:
		return text
	case <-time.After(within):
		e.t.Fatalf("no tool batch within %s", within)
		return ""
	}
}

func (e *sdkLiveCheck) send(text string) error {
	return e.s.SendMessage(e.ctx, e.park.orgID, e.park.conversationID, runmode.LocalDefaultUserID, text)
}

func (e *sdkLiveCheck) outcome(within time.Duration) liveOutcome {
	e.t.Helper()
	select {
	case out := <-e.out:
		return out
	case <-time.After(within):
		e.t.Fatalf("the driver did not return within %s", within)
		return liveOutcome{}
	}
}

// drainResults returns every result the process produced so far.
func (e *sdkLiveCheck) drainResults() []*agentproc.Result {
	var got []*agentproc.Result
	for {
		select {
		case r := <-e.results:
			got = append(got, r)
		default:
			return got
		}
	}
}

func (e *sdkLiveCheck) drainBatches() []string {
	var got []string
	for {
		select {
		case text := <-e.batches:
			got = append(got, text)
		default:
			return got
		}
	}
}

// row returns the user row whose content contains text, and whether one exists.
func (e *sdkLiveCheck) row(text string) (domain.Message, bool) {
	e.t.Helper()
	rows, err := e.s.conversations.ListForAssemblySystem(context.Background(), e.park.orgID, e.park.conversationID)
	if err != nil {
		e.t.Fatalf("list for assembly: %v", err)
	}
	for _, r := range rows {
		if r.Role == "user" && strings.Contains(r.Content, text) {
			return r, true
		}
	}
	return domain.Message{}, false
}

func delivered(r domain.Message) bool { return r.Delivered == nil || *r.Delivered }

// logTranscript logs each session-transcript line that mentions text: how
// Claude Code recorded the input the model read.
func (e *sdkLiveCheck) logTranscript(text string) {
	e.t.Helper()
	sid := e.lr.SessionID()
	home, _ := os.UserHomeDir()
	var found string
	_ = filepath.WalkDir(filepath.Join(home, ".claude", "projects"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && found == "" && d.Name() == sid+".jsonl" {
			found = path
		}
		return nil
	})
	if found == "" {
		e.t.Logf("no transcript for session %s", sid)
		return
	}
	raw, err := os.ReadFile(found) //nolint:gosec // a path found by name above
	if err != nil {
		e.t.Logf("read transcript: %v", err)
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for i := 0; sc.Scan(); i++ {
		line := sc.Text()
		if strings.Contains(line, text) {
			if len(line) > 900 {
				line = line[:900] + "…"
			}
			e.t.Logf("transcript line %d: %s", i, line)
		}
	}
}

// TestLiveInput_AMessageDuringAToolCallRidesTheBatch: a person's message sent
// while a tool call runs is queued for the process and handed to the model at
// that call's tool batch, in the steer envelope. The model acts on it in the
// same turn, the turn's one result is the engagement's, and the row is
// delivered.
func TestLiveInput_AMessageDuringAToolCallRidesTheBatch(t *testing.T) {
	e := startSDKLiveCheck(t, "li-batch",
		"Use the Bash tool to run `sleep 15`. After you see its output, use the Bash tool to run `echo second` "+
			"as a separate, later tool call (do not run both in one message). Then reply with the word DONE.")
	e.firstBash(90 * time.Second)
	time.Sleep(2 * time.Second)
	const msg = "Also include the word PINEAPPLE in your final reply."
	if err := e.send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	out := e.outcome(150 * time.Second)
	if out.err != nil || out.result == nil {
		t.Fatalf("outcome = %+v, want the turn's result", out)
	}
	batches := e.drainBatches()
	results := e.drainResults()
	for i, b := range batches {
		t.Logf("batch %d handed over %q", i+1, b)
	}
	for i, r := range results {
		t.Logf("result %d: stop=%s text=%q", i+1, r.StopReason, r.Result)
	}
	took := false
	for _, b := range batches {
		if strings.Contains(b, domain.SteerOpen+msg+domain.SteerClose) {
			took = true
		}
	}
	if !took {
		t.Errorf("no tool batch handed the message over in the steer envelope")
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1: the message rode the batch and started no turn of its own", len(results))
	}
	if !strings.Contains(out.result.Result, "PINEAPPLE") {
		t.Errorf("final reply %q does not act on the message", out.result.Result)
	}
	if r, ok := e.row(msg); !ok || !delivered(r) {
		t.Errorf("message row = %+v (found %v), want it delivered", r, ok)
	}
	e.logTranscript("PINEAPPLE")
}

// TestLiveInput_AMessageDuringAReplyIsTheNextTurn: a person's message sent
// while the model writes a reply with no tool call has no tool batch to ride,
// so the driver sends it as the next turn when the turn ends. A message that
// arrives once the driver has let the process go is refused, and no row is
// written for it.
func TestLiveInput_AMessageDuringAReplyIsTheNextTurn(t *testing.T) {
	e := startSDKLiveCheck(t, "li-reply", "Count from 1 to 60, one number per line, with no other text.")
	time.Sleep(1500 * time.Millisecond)
	const msg = "Now reply with exactly the word PONG and nothing else."
	if err := e.send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	out := e.outcome(150 * time.Second)
	if out.err != nil || out.result == nil {
		t.Fatalf("outcome = %+v, want the result of the turn the message started", out)
	}
	results := e.drainResults()
	for i, r := range results {
		t.Logf("result %d: stop=%s text=%.80q", i+1, r.StopReason, r.Result)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2: the counting turn, then the message's own", len(results))
	}
	if !strings.Contains(out.result.Result, "PONG") {
		t.Errorf("the driver handed back %q, want the reply to the message", out.result.Result)
	}
	if r, ok := e.row(msg); !ok || !delivered(r) {
		t.Errorf("message row = %+v (found %v), want it delivered", r, ok)
	}

	const late = "This message arrives after the driver let go."
	err := e.send(late)
	if !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("a message after the let-go = %v, want ErrNoLiveProcess", err)
	}
	if r, ok := e.row(late); ok {
		t.Errorf("a refused message left a row: %+v", r)
	}
}

// TestLiveInput_APauseWithAMessageWaitingRunsIt: a person's message queued
// during a tool call, then a pause before the call returns. The pause ends the
// turn, and the message, which no batch delivered (or whose batch was given
// up), is the next turn rather than a park.
func TestLiveInput_APauseWithAMessageWaitingRunsIt(t *testing.T) {
	e := startSDKLiveCheck(t, "li-pause",
		"Use the Bash tool to run `sleep 15`. After you see its output, reply with the word DONE.")
	e.firstBash(90 * time.Second)
	time.Sleep(2 * time.Second)
	const msg = "Reply with exactly the word PONG and nothing else."
	if err := e.send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := e.s.Interrupt(e.ctx, e.park.conversationID); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	out := e.outcome(150 * time.Second)
	batches := e.drainBatches()
	results := e.drainResults()
	for i, b := range batches {
		t.Logf("batch %d handed over %q", i+1, b)
	}
	for i, r := range results {
		t.Logf("result %d: stop=%s interrupted=%v is_error=%v text=%.80q", i+1, r.StopReason, r.Interrupted, r.IsError, r.Result)
	}
	t.Logf("outcome: hibernated=%v err=%v", out.hibernated, out.err)
	if out.hibernated || out.result == nil {
		t.Fatalf("outcome = %+v, want the result of the turn the message started, not a park", out)
	}
	if len(results) != 2 || !results[0].Interrupted {
		t.Errorf("results = %d (first interrupted: %v), want the paused turn then the message's", len(results), len(results) > 0 && results[0].Interrupted)
	}
	if !strings.Contains(out.result.Result, "PONG") {
		t.Errorf("the driver handed back %q, want the reply to the message", out.result.Result)
	}
	if r, ok := e.row(msg); !ok || !delivered(r) {
		t.Errorf("message row = %+v (found %v), want it delivered", r, ok)
	}
}

// TestLiveInput_AStopLeavesAMessageWaiting: a person's message that arrives
// after the last tool batch that could carry it, ahead of a stop_blueprint
// call, is not sent: the stop is the verdict, and the message stays pending
// for the wake that follows the blueprint's reaction.
func TestLiveInput_AStopLeavesAMessageWaiting(t *testing.T) {
	e := startSDKLiveCheck(t, "li-stop",
		"Use the Bash tool to run `sleep 3`. After you see its output, call the stop_blueprint tool with type "+
			"\"abort\", reason \"testing\" and summary \"testing\". Write no other text.")
	first := e.firstBatch(90 * time.Second)
	t.Logf("first batch handed over %q", first)
	const msg = "Also include the word PINEAPPLE in your final reply."
	if err := e.send(msg); err != nil {
		if errors.Is(err, ErrNoLiveProcess) {
			t.Skipf("the stop landed before the message: %v", err)
		}
		t.Fatalf("send: %v", err)
	}

	out := e.outcome(150 * time.Second)
	results := e.drainResults()
	for i, r := range results {
		t.Logf("result %d: stop=%s terminal=%+v text=%.80q", i+1, r.StopReason, r.Terminal, r.Result)
	}
	for i, b := range e.drainBatches() {
		t.Logf("later batch %d handed over %q", i+1, b)
	}
	if out.result == nil || out.result.Terminal == nil {
		t.Fatalf("outcome = %+v, want the stop's verdict", out)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1: the stop's turn, and none for the message", len(results))
	}
	if r, ok := e.row(msg); !ok || delivered(r) {
		t.Errorf("message row = %+v (found %v), want it still pending", r, ok)
	}
}

// TestLiveInput_ANewEventDuringAReplyIsTheNextTurn: a new-event injection
// staged while the model writes a reply with no tool call keeps the finishing
// turn going, as it does a native loop: the driver sends it as the next turn.
func TestLiveInput_ANewEventDuringAReplyIsTheNextTurn(t *testing.T) {
	e := startSDKLiveCheck(t, "li-event", "Count from 1 to 60, one number per line, with no other text.")
	time.Sleep(1500 * time.Millisecond)
	const body = "New commits were pushed to the pull request. Acknowledge this note by replying with exactly the word MANGO."
	if _, ok := e.s.stageNoteAtToolBoundary(e.ctx, e.park.orgID, e.park.conversationID, domain.MessageSubtypeInjectionEvent,
		domain.StagedInjectionProducerPRNewCommits, body, domain.NoteProvenance{}); !ok {
		t.Fatalf("stage failed")
	}

	out := e.outcome(150 * time.Second)
	if out.err != nil || out.result == nil {
		t.Fatalf("outcome = %+v, want the result of the turn the note started", out)
	}
	results := e.drainResults()
	for i, r := range results {
		t.Logf("result %d: stop=%s text=%.80q", i+1, r.StopReason, r.Result)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2: the counting turn, then the note's", len(results))
	}
	if r, ok := e.row("New commits were pushed"); !ok || !delivered(r) {
		t.Errorf("note row = %+v (found %v), want it delivered", r, ok)
	}
	e.logTranscript("MANGO")
}
