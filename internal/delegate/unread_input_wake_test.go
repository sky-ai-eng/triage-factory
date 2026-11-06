package delegate

import (
	"context"
	"testing"
	"time"

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
			})
		}
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

// TestDriveLiveConversation_AStopWithAMessageOwedRequeuesIt is the SDK half. A
// person's message steered in while the turn ran is still owed a turn when a
// stop_blueprint ends it: the driver hands the stop back as the verdict, kills
// the process rather than letting it drain into the owed turn, and puts the
// message's row back on the queue. A message the process had already
// answered stays delivered.
func TestDriveLiveConversation_AStopWithAMessageOwedRequeuesIt(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-stop-owed", "sess-stop-owed", "/tmp/wt-stop-owed")
	claimID := markEngaged(t, database, "r-stop-owed")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.controller = &fakeController{}
	s.registerProc(org, "r-stop-owed", &agentproc.LiveRun{})
	for _, text := range []string{"answered", "thanks"} {
		if err := s.SendMessage(ctx, org, "r-stop-owed", runmode.LocalDefaultUserID, text); err != nil {
			t.Fatalf("steer %q: %v", text, err)
		}
	}

	proc := newFakeLiveProc("sess-stop-owed")
	proc.owe(1)
	results := make(chan *agentproc.Result, 1)
	stop := &agentproc.Result{StopReason: "tool_use", Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeAbort, Reason: "r", Summary: "s"}}
	results <- stop
	park := liveParkContext{orgID: org, conversationID: "r-stop-owed", claimID: claimID}

	out := s.driveLiveConversation(ctx, park, proc, results, nil)

	if out.result != stop || out.fenced || out.hibernated {
		t.Errorf("outcome = %+v, want the stop handed back as the verdict", out)
	}
	if !proc.wasKilled() {
		t.Error("the process was closed, want it killed so it cannot run the owed turn")
	}
	if got := pendingRows(t, s, "r-stop-owed"); len(got) != 1 || got[0].Content != "thanks" {
		t.Errorf("pending rows = %+v, want only the unanswered message back on the queue", got)
	}
}

// TestDriveLiveConversation_AStopWithNothingOwedCloses: with no turn owed the
// stop closes the process as before and touches no row.
func TestDriveLiveConversation_AStopWithNothingOwedCloses(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	results <- &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}

	s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nil)

	if !proc.wasClosed() || proc.wasKilled() {
		t.Errorf("closed = %v, killed = %v; want a graceful close", proc.wasClosed(), proc.wasKilled())
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
			proc := newFakeLiveProc("sess")
			proc.owe(tc.owed)
			got := l.owedSteers(proc)
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
