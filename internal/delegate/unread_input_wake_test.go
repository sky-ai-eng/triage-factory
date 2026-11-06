package delegate

import (
	"context"
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
// undelivered (where the native loop leaves it, and where the SDK driver puts
// it back), and the engagement then records the stop's verdict.
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

// TestWakeForUnreadInput_CountsTheMessageAnAdvanceLeavesUnread is the window
// the wake cannot close: a message that lands after a non-final step's last
// model call, before its conclusion is recorded. The reactor advances, which
// ends the step's conversation, so the wake is refused and the message is
// counted under the rung that refused it.
func TestWakeForUnreadInput_CountsTheMessageAnAdvanceLeavesUnread(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	counted := unreadCounter(t)
	s, database, brID, _, convID := reactorFixture(t, "unread-advance", 2, "", "")
	markNative(t, database, convID)
	concludeWithUnreadMessage(t, s, convID, domain.ConversationOutcomeContinue)

	reactAsTheDispatchDoes(t, s, brID, convID)
	s.wakeForUnreadInput(context.Background(), org, convID)

	if br := mustGetRun(t, s, org, brID); br.CurrentStepIndex != 1 {
		t.Fatalf("current_step_index = %d, want 1 — the step advanced", br.CurrentStepIndex)
	}
	if st := storedStatus(t, database, convID); st != domain.StatusOpen {
		t.Errorf("stored status = %q, want open — an ended step is not woken", st)
	}
	got := counted()
	if got[ResumeBlockedEndedStepAdvanced] != 1 || len(got) != 1 {
		t.Errorf("unread-at-conclusion counts = %v, want one under %q", got, ResumeBlockedEndedStepAdvanced)
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

// TestWakeForUnreadInput_LeavesAStoppedFollowUpParked: a follow-up on concluded
// work that a person stops keeps its earlier conclusion stamp, and its
// message, which the stopped claim never delivered, is still on the queue.
// That is a stop being honoured, not a verdict with input behind it, so
// nothing wakes it.
func TestWakeForUnreadInput_LeavesAStoppedFollowUpParked(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, brID, _, convID := reactorFixture(t, "unread-stopped", 1, "", "")
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
	if flipped, err := s.conversations.ParkOpenForClaimSystem(ctx, org, convID, followUpClaim, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !flipped {
		t.Fatalf("stop the follow-up = (%v, %v), want (true, nil)", flipped, err)
	}

	s.wakeForUnreadInput(ctx, org, convID)

	if st := storedStatus(t, database, convID); st != domain.StatusOpen {
		t.Errorf("stored status = %q, want open — a stopped conversation stays stopped", st)
	}
	if got := pendingRows(t, s, convID); len(got) != 1 {
		t.Errorf("pending rows = %d, want the follow-up still queued for the next message", len(got))
	}
}

// TestWakeForUnreadInput_WakesAFollowUpThatParkedIdle: a follow-up on finished
// work that parks idle, a refused or unrecognized turn for instance, keeps the
// earlier conclusion stamp, so the claim queue does not take it up. A message
// that landed during that turn is woken for, as after a verdict.
func TestWakeForUnreadInput_WakesAFollowUpThatParkedIdle(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, brID, _, convID := reactorFixture(t, "unread-idle", 1, "", "")
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
	// The engagement drains the follow-up, a second message lands during its
	// turn, and the turn parks idle.
	if _, _, _, err := s.pendingInput.Consume(ctx, org, convID); err != nil {
		t.Fatalf("drain the follow-up: %v", err)
	}
	if _, err := s.conversations.InsertMessage(ctx, org, pendingUserInput(convID, runmode.LocalDefaultUserID, "and another")); err != nil {
		t.Fatalf("queue the second message: %v", err)
	}
	if flipped, err := s.conversations.ParkOpenForClaimSystem(ctx, org, convID, followUpClaim, db.ParkIdle()); err != nil || !flipped {
		t.Fatalf("park the follow-up idle = (%v, %v), want (true, nil)", flipped, err)
	}

	s.wakeForUnreadInput(ctx, org, convID)

	if st := storedStatus(t, database, convID); st != "" {
		t.Errorf("stored status = %q, want none — the second message wakes it", st)
	}
	if got := pendingRows(t, s, convID); len(got) != 1 || got[0].Content != "and another" {
		t.Errorf("pending rows = %+v, want the second message queued for the resume", got)
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

// stopOwedFixture is a live SDK engagement whose process took two steers, the
// first of them already answered, and owes a turn to the second.
func stopOwedFixture(t *testing.T) (*Spawner, *fakeLiveProc, liveParkContext) {
	t.Helper()
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-stop-owed", "sess-stop-owed", "/tmp/wt-stop-owed")
	claimID := markEngaged(t, database, "r-stop-owed")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.controller = &fakeController{}
	s.registerProc(org, "r-stop-owed", &agentproc.LiveRun{})
	for _, text := range []string{"answered", "thanks"} {
		if err := s.SendMessage(context.Background(), org, "r-stop-owed", runmode.LocalDefaultUserID, text); err != nil {
			t.Fatalf("steer %q: %v", text, err)
		}
	}
	proc := newFakeLiveProc("sess-stop-owed")
	proc.owe(1)
	return s, proc, liveParkContext{orgID: org, conversationID: "r-stop-owed", claimID: claimID}
}

func stopResult() *agentproc.Result {
	return &agentproc.Result{StopReason: "tool_use", Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeAbort, Reason: "r", Summary: "s"}}
}

// TestDriveLiveConversation_AStopWithAMessageOwedRequeuesIt is the SDK half. A
// person's message steered in while the turn ran is still owed a turn when a
// stop_blueprint ends it: the driver hands the stop back as the verdict,
// interrupts the owed turn rather than letting a close drain it, closes the
// process once the interrupt has settled it, and puts the message's row back
// on the queue. A message the process had already answered stays delivered.
func TestDriveLiveConversation_AStopWithAMessageOwedRequeuesIt(t *testing.T) {
	s, proc, park := stopOwedFixture(t)
	results := make(chan *agentproc.Result, 2)
	proc.onInterrupt = func() {
		proc.owe(0)
		results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	}
	stop := stopResult()
	results <- stop

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if out.result != stop || out.fenced || out.hibernated {
		t.Errorf("outcome = %+v, want the stop handed back as the verdict", out)
	}
	if proc.interrupts != 1 || !proc.wasClosed() || proc.wasKilled() {
		t.Errorf("interrupts = %d, closed = %v, killed = %v; want the owed turn interrupted, then a close", proc.interrupts, proc.wasClosed(), proc.wasKilled())
	}
	if got := pendingRows(t, s, "r-stop-owed"); len(got) != 1 || got[0].Content != "thanks" {
		t.Errorf("pending rows = %+v, want only the unanswered message back on the queue", got)
	}
}

// TestDriveLiveConversation_AnOwedTurnTheInterruptMissesIsKilled: when the
// interrupt settles nothing in time, the owed turn would run in a close's
// drain, so the process is killed, and the message still goes back on the
// queue.
func TestDriveLiveConversation_AnOwedTurnTheInterruptMissesIsKilled(t *testing.T) {
	s, proc, park := stopOwedFixture(t)
	s.owedTurnAbortWaitOverride = 10 * time.Millisecond
	results := make(chan *agentproc.Result, 1)
	results <- stopResult()

	s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if !proc.wasKilled() {
		t.Error("the process was not killed, want it killed once the interrupt settled nothing")
	}
	if got := pendingRows(t, s, "r-stop-owed"); len(got) != 1 || got[0].Content != "thanks" {
		t.Errorf("pending rows = %+v, want the unanswered message back on the queue", got)
	}
}

// TestDriveLiveConversation_ASteerAfterTheChecksKeepsTheTurnEndGoing: a
// message the process counts after the driver's first look at a finished turn,
// but before it lets the process go, is seen when it does: the turn it owes
// runs instead of being drained by a close.
func TestDriveLiveConversation_ASteerAfterTheChecksKeepsTheTurnEndGoing(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	reads := 0
	proc.queuedFn = func() int {
		reads++
		if reads == 2 {
			return 1 // the steer lands between the first check and the let-go
		}
		return 0
	}
	results := make(chan *agentproc.Result, 2)
	results <- &agentproc.Result{StopReason: "end_turn", Result: "Done, I think."}
	answered := &agentproc.Result{StopReason: "end_turn", Result: "Read your message."}
	results <- answered

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nil)

	if out.result != answered {
		t.Errorf("result = %+v, want the turn the late steer produced", out.result)
	}
}

// TestDriveLiveConversation_AStopWithNothingOwedCloses: with no turn owed a
// stop closes the process gracefully, with no interrupt and no kill.
func TestDriveLiveConversation_AStopWithNothingOwedCloses(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	results <- &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}

	s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nil)

	if !proc.wasClosed() || proc.wasKilled() || proc.interrupts != 0 {
		t.Errorf("closed = %v, killed = %v, interrupts = %d; want a graceful close", proc.wasClosed(), proc.wasKilled(), proc.interrupts)
	}
}

// TestSendLog_OwedSteersAreTheNewestSendsThatArePeoples: the turns a process
// owes are its newest sends, and only a person's steer names a row. A nudge
// among them names none, and a steer it answered before them is not owed.
func TestSendLog_OwedSteersAreTheNewestSendsThatArePeoples(t *testing.T) {
	cases := []struct {
		name string
		rows []int
		owed int
		want []int
	}{
		{"nothing owed", []int{11, 12}, 0, nil},
		{"the newest steer", []int{11, 12}, 1, []int{12}},
		{"a nudge sent after the steer is the owed send", []int{11, 0}, 1, nil},
		{"a nudge between two owed steers", []int{11, 0, 13}, 3, []int{11, 13}},
		{"owing more than the log holds takes the whole log", []int{11}, 2, []int{11}},
		{"a steer whose row did not record", []int{11, 0}, 2, []int{11}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &sendLog{rows: tc.rows}
			got := l.owedSteers(tc.owed)
			if len(got) != len(tc.want) {
				t.Fatalf("owedSteers = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("owedSteers = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
