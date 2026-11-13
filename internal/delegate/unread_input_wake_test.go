package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// concludeWithUnreadMessage stages the moment a stop_blueprint leaves behind:
// the step's engagement holds a claim, a person's message is on the queue
// undelivered (where both runtimes leave it), and the engagement then records
// the stop's verdict.
func concludeWithUnreadMessage(t *testing.T, s *Spawner, convID string, outcome domain.ConversationOutcome) {
	t.Helper()
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	claimID := holderClaimFor(t, s, org, convID)
	if _, err := s.conversations.InsertMessage(ctx, org, pendingUserInput(convID, runmode.LocalDefaultUserID, "thanks")); err != nil {
		t.Fatalf("queue the message: %v", err)
	}
	if _, err := s.conversations.CompleteForClaimSystem(ctx, org, convID, claimID, domain.StatusOpen, 0, 0, 1,
		"looked into it", string(outcome), "needs a human", ""); err != nil {
		t.Fatalf("record the stop's verdict: %v", err)
	}
}

// reactAsTheDispatchDoes hands the step's stored ending to the reactor, with
// the attribution the dispatch carries onto it.
func reactAsTheDispatchDoes(t *testing.T, s *Spawner, brID, convID string) {
	t.Helper()
	org := runmode.LocalDefaultOrgID
	stepConversation := loadConversation(t, s, convID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())
}

// unreadCounter swaps the unread-at-conclusion counter for one backed by a
// manual reader and returns a read of every series, by reason.
func unreadCounter(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := unreadAtConclusion.Swap(newUnreadAtConclusionStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { unreadAtConclusion.Store(prev) })
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		got := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "conversations.unread_at_conclusion" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("conversations.unread_at_conclusion is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key("reason"))
					got[v.AsString()] = dp.Value
				}
			}
		}
		return got
	}
}

// TestWakeForUnreadInput_AStopStandsAndTheMessageResumesAfterIt: a message that
// arrived as the agent stopped its blueprint leaves the stop's verdict in
// place and then resumes the conversation, as one sent just after the stop
// would. The wake waits for the blueprint's reaction: before it, the step is
// handed off and nothing moves. After it, the conversation is queued with the
// message still to read, an aborted blueprint re-opens for the resume, and a
// finished one stays finished. The same for both runtimes.
func TestWakeForUnreadInput_AStopStandsAndTheMessageResumesAfterIt(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	cases := []struct {
		outcome   domain.ConversationOutcome
		wantAfter domain.BlueprintRunStatus
	}{
		{domain.ConversationOutcomeAbort, domain.BlueprintRunStatusRunning},
		{domain.ConversationOutcomeFinish, domain.BlueprintRunStatusCompleted},
	}
	for _, runtime := range []string{domain.ConversationRuntimeNative, domain.ConversationRuntimeSDK} {
		for _, tc := range cases {
			t.Run(runtime+"/"+string(tc.outcome), func(t *testing.T) {
				counted := unreadCounter(t)
				s, database, brID, _, convID := reactorFixture(t, "unread-"+runtime+"-"+string(tc.outcome), 2, "", "")
				if runtime == domain.ConversationRuntimeNative {
					markNative(t, database, convID)
				} else {
					giveConversationResumeState(t, database, convID)
				}
				concludeWithUnreadMessage(t, s, convID, tc.outcome)

				s.wakeForUnreadInput(context.Background(), org, convID)
				if !storedConcluded(t, database, convID) {
					t.Fatalf("stored status before the reactor = %q, want the verdict standing — the step is handed off", storedStatus(t, database, convID))
				}

				reactAsTheDispatchDoes(t, s, brID, convID)
				s.wakeForUnreadInput(context.Background(), org, convID)

				if st := storedStatus(t, database, convID); st != "" {
					t.Errorf("stored status after the reactor = %q, want none — the message wakes the conversation", st)
				}
				if got := pendingRows(t, s, convID); len(got) != 1 || got[0].Content != "thanks" {
					t.Errorf("pending rows = %+v, want the message still queued for the resume", got)
				}
				if br := mustGetRun(t, s, org, brID); br.Status != tc.wantAfter {
					t.Errorf("blueprint status = %q, want %q", br.Status, tc.wantAfter)
				}
				// Neither the handed-off wake nor the one that woke it left
				// the message behind.
				if got := counted(); len(got) != 0 {
					t.Errorf("unread-at-conclusion counts = %v, want none", got)
				}
			})
		}
	}
}

// advanceWithUnreadMessage concludes step 0 of a two-step native blueprint
// with `continue` and a person's message still unread on it, as a message
// landing after the step's last model call leaves it, and runs the reactor,
// which advances to step 1. It returns step 1's conversation id.
func advanceWithUnreadMessage(t *testing.T, suffix string) (*Spawner, *sql.DB, string, string) {
	t.Helper()
	s, database, brID, _, convID := reactorFixture(t, suffix, 2, "", "")
	markNative(t, database, convID)
	concludeWithUnreadMessage(t, s, convID, domain.ConversationOutcomeContinue)
	reactAsTheDispatchDoes(t, s, brID, convID)
	if br := mustGetRun(t, s, runmode.LocalDefaultOrgID, brID); br.CurrentStepIndex != 1 {
		t.Fatalf("current_step_index = %d, want 1 — the step advanced", br.CurrentStepIndex)
	}
	var nextID string
	if err := database.QueryRow(`SELECT id FROM conversations WHERE blueprint_run_id = ? AND blueprint_step_index = 1`, brID).Scan(&nextID); err != nil {
		t.Fatalf("read step 1: %v", err)
	}
	return s, database, convID, nextID
}

// TestWakeForUnreadInput_AnAdvancePassesTheMessageToTheNextStep: a message
// left unread on a step its blueprint advanced past goes to the step now
// running, under its sender's name, and leaves the ended step's transcript,
// so it appears once, where it is read. Nothing is counted.
func TestWakeForUnreadInput_AnAdvancePassesTheMessageToTheNextStep(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	counted := unreadCounter(t)
	s, database, convID, nextID := advanceWithUnreadMessage(t, "unread-advance")
	markNative(t, database, nextID)

	s.wakeForUnreadInput(context.Background(), org, convID)

	got := pendingRows(t, s, nextID)
	if len(got) != 1 || got[0].Content != "thanks" || got[0].UserID != runmode.LocalDefaultUserID {
		t.Errorf("step 1 pending rows = %+v, want the message, from its sender", got)
	}
	if left := pendingRows(t, s, convID); len(left) != 0 {
		t.Errorf("step 0 pending rows = %+v, want none — the message moved on", left)
	}
	msgs, err := s.conversations.MessagesForConversations(context.Background(), org, []string{convID})
	if err != nil {
		t.Fatalf("MessagesForConversations: %v", err)
	}
	for _, m := range msgs {
		if m.Content == "thanks" {
			t.Errorf("step 0's transcript still shows the forwarded message: %+v", m)
		}
	}
	if st := storedStatus(t, database, convID); st != domain.StatusOpen {
		t.Errorf("step 0 stored status = %q, want open — an ended step is not woken", st)
	}
	if c := counted(); len(c) != 0 {
		t.Errorf("unread-at-conclusion counts = %v, want none", c)
	}
}

// TestWakeForUnreadInput_CountsAMessageTheNextStepRefuses: when the step now
// running cannot take the message (here an SDK step waiting for its first
// claim, which takes no queued message), it stays on the ended step and the
// conversation is counted under the advance.
func TestWakeForUnreadInput_CountsAMessageTheNextStepRefuses(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	counted := unreadCounter(t)
	s, _, convID, nextID := advanceWithUnreadMessage(t, "unread-advance-refused")

	s.wakeForUnreadInput(context.Background(), org, convID)

	if got := pendingRows(t, s, nextID); len(got) != 0 {
		t.Errorf("step 1 pending rows = %+v, want none — it refused the message", got)
	}
	if left := pendingRows(t, s, convID); len(left) != 1 || left[0].Content != "thanks" {
		t.Errorf("step 0 pending rows = %+v, want the message left where it was", left)
	}
	if c := counted(); c[ResumeBlockedEndedStepAdvanced] != 1 || len(c) != 1 {
		t.Errorf("unread-at-conclusion counts = %v, want one under %q", c, ResumeBlockedEndedStepAdvanced)
	}
}

// Dispatcher startup creates every reason's series at zero, so the first
// conversation left with unread messages after a restart is a change
// increase() can see.
func TestOpenUnreadAtConclusionSeries_CreatesEachReasonAtZero(t *testing.T) {
	counted := unreadCounter(t)

	OpenUnreadAtConclusionSeries()

	got := counted()
	want := unreadAtConclusionReasons()
	if len(got) != len(want) {
		t.Errorf("series = %v, want exactly %v", got, want)
	}
	for _, reason := range want {
		if v, ok := got[reason]; !ok || v != 0 {
			t.Errorf("series %q = (%d, %v), want (0, true)", reason, v, ok)
		}
	}
	if _, ok := got[ResumeBlockedEndedStepAdvanced]; !ok {
		t.Errorf("series = %v, want the advance's rung among them", got)
	}
}

// TestWakeForUnreadInput_LeavesAFollowUpsParkToTheQueue: a follow-up on a
// finished blueprint parks with no verdict, since its resume cleared the
// earlier one, so this wake, which is a concluded conversation's, writes
// nothing to it. Its unread message makes it the queue's to drive, as for any
// conversation parked with no verdict, whether the turn parked idle with a
// message that landed during it or a person stopped it before the message was
// delivered.
func TestWakeForUnreadInput_LeavesAFollowUpsParkToTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name string
		park db.Park
		// deliverFirst is whether the engagement read the follow-up before a
		// second message landed and the turn parked.
		deliverFirst bool
		wantUnread   string
	}{
		{"parked idle with a message that landed during the turn", db.ParkIdle(), true, "and another"},
		{"stopped before the follow-up was delivered", db.ParkStopped(domain.ParkReasonUserCancelled, ""), false, "one more thing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := runmode.LocalDefaultOrgID
			ctx := context.Background()
			s, database, brID, _, convID := reactorFixture(t, "unread-followup-park", 1, "", "")
			markNative(t, database, convID)
			claimID := holderClaimFor(t, s, org, convID)
			if _, err := s.conversations.CompleteForClaimSystem(ctx, org, convID, claimID, domain.StatusOpen, 0, 0, 1, "done", "finish", "", ""); err != nil {
				t.Fatalf("conclude: %v", err)
			}
			reactAsTheDispatchDoes(t, s, brID, convID)
			if err := s.SendMessage(ctx, org, convID, runmode.LocalDefaultUserID, "one more thing"); err != nil {
				t.Fatalf("follow-up: %v", err)
			}
			followUpClaim := holderClaimFor(t, s, org, convID)
			if tc.deliverFirst {
				if _, _, _, err := s.pendingInput.Consume(ctx, org, convID); err != nil {
					t.Fatalf("drain the follow-up: %v", err)
				}
				if _, err := s.conversations.InsertMessage(ctx, org, pendingUserInput(convID, runmode.LocalDefaultUserID, "and another")); err != nil {
					t.Fatalf("queue the second message: %v", err)
				}
			}
			if flipped, err := s.conversations.ParkOpenForClaimSystem(ctx, org, convID, followUpClaim, tc.park); err != nil || !flipped {
				t.Fatalf("park the follow-up = (%v, %v), want (true, nil)", flipped, err)
			}
			if conv, err := s.conversations.GetSystem(ctx, org, convID); err != nil || conv == nil || conv.Concluded() {
				t.Fatalf("parked follow-up = (%+v, %v), want it parked with no verdict", conv, err)
			}

			s.wakeForUnreadInput(ctx, org, convID)

			if st := storedStatus(t, database, convID); st != domain.StatusOpen {
				t.Errorf("stored status = %q, want open: this wake is a concluded conversation's", st)
			}
			if got := pendingRows(t, s, convID); len(got) != 1 || got[0].Content != tc.wantUnread {
				t.Errorf("pending rows = %+v, want %q still queued", got, tc.wantUnread)
			}
			claimed, err := s.conversationQueue.ClaimNextConversation(ctx, "exec-followup-park", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
			if err != nil || claimed == nil || claimed.ID != convID {
				t.Errorf("ClaimNextConversation = (%+v, %v), want the queue to drive %s for its unread message", claimed, err, convID)
			}
		})
	}
}

// TestReplayStrandedRun_WakesForTheMessageTheStopLeftUnread: when the
// engagement that recorded a stop died before its reactor, the stranded-run
// replay runs the reactor in its place, and the wake that follows it.
func TestReplayStrandedRun_WakesForTheMessageTheStopLeftUnread(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	s, database, brID, _, convID := reactorFixture(t, "unread-stranded", 1, "", "")
	markNative(t, database, convID)
	concludeWithUnreadMessage(t, s, convID, domain.ConversationOutcomeAbort)

	s.replayStrandedRun(context.Background(), db.StrandedRun{OrgID: org, BlueprintRunID: brID, ConversationID: convID})

	if st := storedStatus(t, database, convID); st != "" {
		t.Errorf("stored status = %q, want none — the replayed reaction is followed by the wake", st)
	}
	if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running — the abort re-opens for the resume", br.Status)
	}
}

// liveInputFixture is a live SDK engagement: a conversation with an active
// claim and its process registered, and the identity the driver parks under.
// The driver runs against proc, a fake standing in for the registered one.
func liveInputFixture(t *testing.T, convID string) (*Spawner, *sql.DB, *fakeLiveProc, liveParkContext) {
	t.Helper()
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	seedConversation(t, database, convID, "sess-"+convID, "/tmp/wt-"+convID)
	claimID := markEngaged(t, database, convID)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.registerProc(org, convID, &agentproc.LiveRun{})
	park := liveParkContext{
		orgID: org, conversationID: convID, namespace: "seedbpr-" + convID, claudeCwd: "/tmp/wt-" + convID,
		claimID: claimID, reason: db.ParkIdle(), runtime: domain.ConversationRuntimeSDK,
	}
	return s, database, newFakeLiveProc("sess-" + convID), park
}

// sendWhileLive sends a person's message to the conversation's live process,
// as the message endpoint does.
func sendWhileLive(t *testing.T, s *Spawner, convID, text string) {
	t.Helper()
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, convID, runmode.LocalDefaultUserID, text); err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
}

func stopResult() *agentproc.Result {
	return &agentproc.Result{StopReason: "tool_use", Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeAbort, Reason: "r", Summary: "s"}}
}

// answerEachSend feeds result onto results for every message the process
// takes, the turn that message starts. A test that expects no send sets it
// too, so a send it did not expect ends in a wrong outcome rather than a
// driver waiting forever for a turn nothing answers.
func answerEachSend(proc *fakeLiveProc, results chan *agentproc.Result, result *agentproc.Result) {
	proc.onSend = func(int) { results <- result }
}

// TestDriveLiveConversation_AWaitingMessageIsTheNextTurn: a person's message
// sent while the turn ran, that no tool batch took, keeps a turn the model
// ended from concluding. The driver sends it as the next turn, marks it
// delivered, and reads the turn it starts, before the would-stop hook is
// asked: the model has not seen it.
func TestDriveLiveConversation_AWaitingMessageIsTheNextTurn(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-waiting")
	sendWhileLive(t, s, "r-waiting", "also check the tests")
	results := make(chan *agentproc.Result, 4)
	want := &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}
	answerEachSend(proc, results, want)
	results <- &agentproc.Result{StopReason: "end_turn", Result: "Done, I think."}
	asked := 0
	nudge := func(context.Context) string { asked++; return "did you mean to?" }

	out := s.driveLiveConversation(context.Background(), park, proc, results, nudge)

	if out.result != want {
		t.Errorf("result = %+v, want the conclusion of the turn the message started", out.result)
	}
	if got := proc.sentTexts(); len(got) != 1 || got[0] != "also check the tests" {
		t.Errorf("sent = %q, want the waiting message as the next turn", got)
	}
	if asked != 0 {
		t.Errorf("nudge asked %d times; the waiting message comes first", asked)
	}
	if got := pendingRows(t, s, "r-waiting"); len(got) != 0 {
		t.Errorf("pending rows = %+v, want the message delivered", got)
	}
}

// TestDriveLiveConversation_AWaitingMessageOutlivesThePause: an interrupt
// that lands with a person's message waiting ends this turn, and the message
// is the pause's next turn rather than a park.
func TestDriveLiveConversation_AWaitingMessageOutlivesThePause(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-waiting-pause")
	sendWhileLive(t, s, "r-waiting-pause", "do this instead")
	results := make(chan *agentproc.Result, 4)
	want := &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}
	answerEachSend(proc, results, want)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if out.result != want || out.hibernated {
		t.Errorf("outcome = %+v, want the conclusion of the turn the message started, not a park", out)
	}
	if got := proc.sentTexts(); len(got) != 1 || got[0] != "do this instead" {
		t.Errorf("sent = %q, want the waiting message", got)
	}
}

// TestDriveLiveConversation_APauseStandsOverANewEvent: a pause is a person
// stopping the turn, so a new-event injection waiting for the process does not
// start another one. The conversation parks, with the injection still staged
// for its next engagement.
func TestDriveLiveConversation_APauseStandsOverANewEvent(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-pause-event")
	if _, ok := s.stageNoteAtToolBoundary(context.Background(), park.orgID, "r-pause-event", domain.MessageSubtypeInjectionEvent, "p", "a new event", domain.NoteProvenance{}); !ok {
		t.Fatal("the injection was not staged")
	}
	results := make(chan *agentproc.Result, 2)
	answerEachSend(proc, results, &agentproc.Result{StopReason: "end_turn", Result: "answered"})
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if !out.hibernated {
		t.Errorf("outcome = %+v, want the pause to park", out)
	}
	if proc.sends() != 0 {
		t.Errorf("sent %q, want nothing after a pause with only an event waiting", proc.sentTexts())
	}
	if got := pendingRows(t, s, "r-pause-event"); len(got) != 1 || got[0].Subtype != domain.MessageSubtypeInjectionEvent {
		t.Errorf("pending rows = %+v, want the injection still staged", got)
	}
}

// TestDriveLiveConversation_ANewEventIsTheNextTurn: a new-event injection
// waiting for the process keeps a turn the model ended going, as it keeps a
// finishing native loop going, and rides the next turn with every other
// staged note, under the lead-in that says they landed while the agent was
// working.
func TestDriveLiveConversation_ANewEventIsTheNextTurn(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-event")
	ctx := context.Background()
	if _, ok := s.stageNoteAtToolBoundary(ctx, park.orgID, "r-event", domain.MessageSubtypeInjectionSystemNote, "p", "a pull request note", domain.NoteProvenance{}); !ok {
		t.Fatal("the note was not staged")
	}
	if _, ok := s.stageNoteAtToolBoundary(ctx, park.orgID, "r-event", domain.MessageSubtypeInjectionEvent, "p", "a new event", domain.NoteProvenance{}); !ok {
		t.Fatal("the injection was not staged")
	}
	results := make(chan *agentproc.Result, 4)
	want := &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}
	answerEachSend(proc, results, want)
	results <- &agentproc.Result{StopReason: "end_turn", Result: "Done."}

	out := s.driveLiveConversation(ctx, park, proc, results, nil)

	if out.result != want {
		t.Errorf("result = %+v, want the conclusion of the turn the injection started", out.result)
	}
	sent := proc.sentTexts()
	if len(sent) != 1 || !strings.Contains(sent[0], "a new event") || !strings.Contains(sent[0], "a pull request note") {
		t.Errorf("sent = %q, want one turn carrying the injection and the note", sent)
	} else if !strings.HasPrefix(sent[0], "<system-note>\nWhile you were working,") {
		t.Errorf("sent = %q, want the notes under the lead-in for input that landed while the agent worked", sent[0])
	}
	if got := pendingRows(t, s, "r-event"); len(got) != 0 {
		t.Errorf("pending rows = %+v, want everything the turn carried delivered", got)
	}
}

// TestDriveLiveConversation_ANoteAloneLetsTheTurnEnd: a staged note that is
// information, not work, does not keep a finishing turn going. The process is
// let go of with the note still staged for the next engagement.
func TestDriveLiveConversation_ANoteAloneLetsTheTurnEnd(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-note")
	if _, ok := s.stageNoteAtToolBoundary(context.Background(), park.orgID, "r-note", domain.MessageSubtypeInjectionSystemNote, "p", "a pull request note", domain.NoteProvenance{}); !ok {
		t.Fatal("the note was not staged")
	}
	results := make(chan *agentproc.Result, 2)
	answerEachSend(proc, results, &agentproc.Result{StopReason: "end_turn", Result: "answered"})
	turnEnd := &agentproc.Result{StopReason: "max_tokens", Result: "cut off"}
	results <- turnEnd

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if out.result != turnEnd || proc.sends() != 0 || !proc.wasClosed() {
		t.Errorf("outcome = %+v, sends = %d, closed = %v; want the turn handed back and the process closed", out, proc.sends(), proc.wasClosed())
	}
	if got := pendingRows(t, s, "r-note"); len(got) != 1 {
		t.Errorf("pending rows = %+v, want the note still staged", got)
	}
}

// TestDriveLiveConversation_AStopLeavesAWaitingMessageUnread is the SDK half
// of a stop with a message: the stop stands as the verdict, the process is
// closed without the message being sent, and the message stays on the queue
// for the wake that follows the blueprint's reaction. Once the driver has let
// go, a message sent afterwards is refused rather than queued for a process
// that will not read it.
func TestDriveLiveConversation_AStopLeavesAWaitingMessageUnread(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-stop")
	sendWhileLive(t, s, "r-stop", "thanks")
	results := make(chan *agentproc.Result, 2)
	answerEachSend(proc, results, &agentproc.Result{StopReason: "end_turn", Result: "answered"})
	stop := stopResult()
	results <- stop

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if out.result != stop || out.fenced || out.hibernated {
		t.Errorf("outcome = %+v, want the stop handed back as the verdict", out)
	}
	if proc.sends() != 0 || !proc.wasClosed() {
		t.Errorf("sends = %d, closed = %v; want the process closed with nothing sent", proc.sends(), proc.wasClosed())
	}
	if got := pendingRows(t, s, "r-stop"); len(got) != 1 || got[0].Content != "thanks" {
		t.Errorf("pending rows = %+v, want the message still queued", got)
	}
	err := s.SendMessage(context.Background(), park.orgID, "r-stop", runmode.LocalDefaultUserID, "one more")
	if !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("a send after the driver let go: err = %v, want ErrNoLiveProcess", err)
	}
}

// TestDriveLiveConversation_AMessageAfterTheFirstLookKeepsTheTurnGoing: a
// message queued after the driver's first look at a finished turn, but before
// it lets the process go, is seen when it does, and is the next turn.
func TestDriveLiveConversation_AMessageAfterTheFirstLookKeepsTheTurnGoing(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-late")
	results := make(chan *agentproc.Result, 4)
	answered := &agentproc.Result{StopReason: "end_turn", Result: "Read your message."}
	answerEachSend(proc, results, answered)
	results <- &agentproc.Result{StopReason: "end_turn", Result: "Done, I think."}
	// The would-stop hook runs between the first look and the let-go; the
	// message lands while it first does.
	sent := false
	nudge := func(context.Context) string {
		if !sent {
			sent = true
			sendWhileLive(t, s, "r-late", "wait, one more thing")
		}
		return ""
	}

	out := s.driveLiveConversation(context.Background(), park, proc, results, nudge)

	if out.result != answered {
		t.Errorf("result = %+v, want the turn the late message started", out.result)
	}
	if got := proc.sentTexts(); len(got) != 1 || got[0] != "wait, one more thing" {
		t.Errorf("sent = %q, want the late message", got)
	}
}

// TestDriveLiveConversation_AFailedSendPutsTheMessageBack: when the process
// will not take the turn, the message goes back on the queue and the turn
// ends as it would have without it.
func TestDriveLiveConversation_AFailedSendPutsTheMessageBack(t *testing.T) {
	s, _, proc, park := liveInputFixture(t, "r-refused")
	sendWhileLive(t, s, "r-refused", "are you there?")
	proc.sendErr = agentproc.ErrRunClosing
	results := make(chan *agentproc.Result, 1)
	turnEnd := &agentproc.Result{StopReason: "max_tokens", Result: "cut off"}
	results <- turnEnd

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if out.result != turnEnd {
		t.Errorf("result = %+v, want the turn handed back", out.result)
	}
	if got := pendingRows(t, s, "r-refused"); len(got) != 1 || got[0].Content != "are you there?" {
		t.Errorf("pending rows = %+v, want the message back on the queue", got)
	}
}

// TestToolBoundary_APersonsMessageRidesTheNextBatch: a person's message for a
// live process is handed to the agent at its next tool batch, in the steer
// envelope, and marked delivered under the engagement's claim. Undoing the
// batch, as the reader does when an interrupt gave it up first, puts the
// message back on the queue and re-arms the boundary.
func TestToolBoundary_APersonsMessageRidesTheNextBatch(t *testing.T) {
	s, _, _, park := liveInputFixture(t, "r-batch")
	s.getProc("r-batch").boundary.take() // armed from registration
	sendWhileLive(t, s, "r-batch", "also check the tests")

	batch := s.notesAtToolBatch(park.orgID, "r-batch", park.claimID)
	if batch.Text != domain.SteerOpen+"also check the tests"+domain.SteerClose || batch.Undo == nil {
		t.Fatalf("batch = %+v, want the message in the steer envelope with an undo", batch)
	}
	if got := pendingRows(t, s, "r-batch"); len(got) != 0 {
		t.Errorf("pending rows = %+v, want the message delivered", got)
	}
	if again := s.notesAtToolBatch(park.orgID, "r-batch", park.claimID).Text; again != "" {
		t.Errorf("the next batch handed over %q, want nothing", again)
	}

	batch.Undo()
	if got := pendingRows(t, s, "r-batch"); len(got) != 1 || got[0].Content != "also check the tests" {
		t.Errorf("pending rows after the undo = %+v, want the message back", got)
	}
	if again := s.notesAtToolBatch(park.orgID, "r-batch", park.claimID).Text; !strings.Contains(again, "also check the tests") {
		t.Errorf("the batch after the undo handed over %q, want the message again", again)
	}
}
