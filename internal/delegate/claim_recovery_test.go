package delegate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// getRunSpy counts GetRunSystem calls. It is the first read the claim gates
// make, so a dispatch that returns without one never reached the gates, the
// bring-up or the runtime behind them.
type getRunSpy struct {
	db.BlueprintStore
	calls atomic.Int32
}

func (s *getRunSpy) GetRunSystem(ctx context.Context, orgID, id string) (*domain.BlueprintRun, error) {
	s.calls.Add(1)
	return s.BlueprintStore.GetRunSystem(ctx, orgID, id)
}

// TestDispatch_LossBudgetSpent_FailsTheConversationWithoutRunningIt: a claim
// whose conversation was lost as many times in a row as the budget allows is
// the claim that ends it. It fails the conversation executor_lost through its
// own fence, ends the run the same way, and never reaches the gates, let
// alone the runtime.
func TestDispatch_LossBudgetSpent_FailsTheConversationWithoutRunningIt(t *testing.T) {
	f := newLaunchFixture(t, "lost")
	org := runmode.LocalDefaultOrgID
	spy := &getRunSpy{BlueprintStore: f.stores.Blueprints}
	f.s.blueprints = spy
	waker := newFakeWaker()
	f.s.SetFiringWaker(waker)

	conv := f.conv
	conv.LostEngagements = DefaultMaxClaimLosses
	f.s.dispatchClaimedConversation(context.Background(), &conv, time.Now())

	if n := spy.calls.Load(); n != 0 {
		t.Errorf("the dispatch read the blueprint run %d times; a spent loss budget must return before the gates", n)
	}
	got, err := f.stores.Conversations.GetSystem(context.Background(), org, f.conv.ID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem = (%+v, %v)", got, err)
	}
	if got.Status != domain.StatusFailed || got.FailureKind != domain.ConversationFailureExecutorLost {
		t.Errorf("conversation = (%q, %q), want (failed, executor_lost)", got.Status, got.FailureKind)
	}
	if want := fmt.Sprintf("lost %d times in a row", DefaultMaxClaimLosses); !strings.Contains(got.ResultSummary, want) || !strings.Contains(got.ResultSummary, "TF_MAX_CLAIM_ATTEMPTS") {
		t.Errorf("result_summary = %q, want it to say the engagement was %s and name TF_MAX_CLAIM_ATTEMPTS", got.ResultSummary, want)
	}
	if got.EndedReason != domain.EndedFailed {
		t.Errorf("ended_reason = %q, want %q — a failure is a boundary", got.EndedReason, domain.EndedFailed)
	}
	if outcomes := f.claimOutcomes(t); len(outcomes) != 1 || outcomes[0] != "failed" {
		t.Errorf("claim outcomes = %v, want [failed] — released through the claim's own fence", outcomes)
	}
	br, err := f.stores.Blueprints.GetRunSystem(context.Background(), org, f.br.ID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem = (%+v, %v)", br, err)
	}
	if br.Status != domain.BlueprintRunStatusFailed || br.AbortReason != string(domain.ConversationFailureExecutorLost) {
		t.Errorf("run = (%q, %q), want (failed, executor_lost)", br.Status, br.AbortReason)
	}
	if waker.count() == 0 {
		t.Error("the firing worker was never woken; the task's queued firings wait on this conversation")
	}
}

// TestDispatch_LossBudgetFollowsTheSetting: the budget is TF_MAX_CLAIM_ATTEMPTS
// as the spawner was given it, not the default.
func TestDispatch_LossBudgetFollowsTheSetting(t *testing.T) {
	f := newLaunchFixture(t, "lost-setting")
	f.s.SetMaxClaimLosses(1)
	conv := f.conv
	conv.LostEngagements = 1
	f.s.dispatchClaimedConversation(context.Background(), &conv, time.Now())
	if got := f.storedStatus(t); got != domain.StatusFailed {
		t.Errorf("stored status with one loss against a budget of one = %q, want failed", got)
	}
}

// TestPreAgentFailure_CredentialsTimeoutSpendsNoBudget: a credential bundle
// that never arrived hands the claim back as requeued_credentials, which the
// next claim counts toward neither budget — even when the setup budget is
// already one failure from spent.
func TestPreAgentFailure_CredentialsTimeoutSpendsNoBudget(t *testing.T) {
	f := newLaunchFixture(t, "cred-timeout")
	conv := f.conv
	conv.SetupFailures = maxClaimAttempts - 1
	// Wrapped the way bring-up wraps it on its way to the dispatcher.
	cause := fmt.Errorf("bring up credential sidecar: %w",
		fmt.Errorf("agentproc: provision sidecar credentials: %w",
			fmt.Errorf("%w: conversation %s", errAwaitingCredentialsTimeout, conv.ID)))

	if survived := f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, conv, cause); !survived {
		t.Fatal("a credentials timeout ended the step")
	}
	if outcomes := f.claimOutcomes(t); len(outcomes) != 1 || outcomes[0] != string(db.RequeueAwaitingCredentials) {
		t.Fatalf("claim outcomes = %v, want [requeued_credentials]", outcomes)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}
	reclaimed, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), "lf-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || reclaimed == nil || reclaimed.ID != f.conv.ID {
		t.Fatalf("re-claim = (%+v, %v), want the same conversation", reclaimed, err)
	}
	if reclaimed.SetupFailures != 0 || reclaimed.LostEngagements != 0 {
		t.Errorf("re-claim budgets = (setup %d, lost %d), want (0, 0)", reclaimed.SetupFailures, reclaimed.LostEngagements)
	}
}

// noBundle is a credentials channel the brain never answers.
type noBundle struct{ db.ClaimCredentialsStore }

func (noBundle) Get(context.Context, string, string) (db.SealedBundle, bool, error) {
	return db.SealedBundle{}, false, nil
}

// TestSidecarProvision_TimeoutIsTheSentinel: the wait running out answers
// with errAwaitingCredentialsTimeout, which is what the dispatcher routes on.
func TestSidecarProvision_TimeoutIsTheSentinel(t *testing.T) {
	f := newLaunchFixture(t, "cred-sentinel")
	f.s.claimCredentials = noBundle{}
	f.s.SetExecutorID("lf-exec", 1)
	f.s.SetAwaitingCredentialsTimeout(20*time.Millisecond, 5*time.Millisecond)

	_, _, err := f.s.sidecarProvisionFor(runmode.LocalDefaultOrgID, f.conv.ID)(context.Background(), "pubkey")
	if !errors.Is(err, errAwaitingCredentialsTimeout) {
		t.Fatalf("provision after the wait ran out = %v, want errAwaitingCredentialsTimeout", err)
	}
}

// backdateConclusion moves a conversation's completion past the stranded-run
// grace, bound as a Go time the way the terminal write stamps it.
func backdateConclusion(t *testing.T, database *sql.DB, conversationID string, ago time.Duration) {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET completed_at = ? WHERE id = ?`, time.Now().UTC().Add(-ago), conversationID); err != nil {
		t.Fatalf("backdate completion of %s: %v", conversationID, err)
	}
}

// TestReplayStrandedRuns_AdvancesARunExactlyOnceWhenTwoPassesRace: the
// executor that drove step 0 died between the step's terminal and its
// reactor. Two dispatchers' passes find the run at once; the advance is
// compare-and-swap guarded, so exactly one step 1 is minted.
func TestReplayStrandedRuns_AdvancesARunExactlyOnceWhenTwoPassesRace(t *testing.T) {
	s, database, brID, _, step0 := reactorFixture(t, "stranded-race", 2, "completed", "continue")
	backdateConclusion(t, database, step0, 2*strandedRunGrace)
	other := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	var wg sync.WaitGroup
	for _, sp := range []*Spawner{s, other} {
		wg.Add(1)
		go func(sp *Spawner) {
			defer wg.Done()
			sp.replayStrandedRuns(context.Background())
		}(sp)
	}
	wg.Wait()

	if q := queuedStepConversations(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step conversations = %v, want exactly [1]", q)
	}
	br := mustGetRun(t, s, runmode.LocalDefaultOrgID, brID)
	if br.CurrentStepIndex != 1 || br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("run = (step %d, %q), want (1, running)", br.CurrentStepIndex, br.Status)
	}

	// Nothing is stranded any more: a third pass writes nothing.
	s.replayStrandedRuns(context.Background())
	if q := queuedStepConversations(t, database, brID); len(q) != 1 {
		t.Errorf("queued step conversations after another pass = %v, want still one", q)
	}
}

// TestReplayStrandedRuns_LeavesARunInsideTheGraceAndOneThisProcessDrives: a
// reactor that is merely running right now is not stranded, whether the grace
// says so or this process's own engagement registry does.
func TestReplayStrandedRuns_LeavesARunInsideTheGraceAndOneThisProcessDrives(t *testing.T) {
	s, database, brID, _, step0 := reactorFixture(t, "stranded-fresh", 2, "completed", "continue")
	backdateConclusion(t, database, step0, time.Second)
	s.replayStrandedRuns(context.Background())
	if q := queuedStepConversations(t, database, brID); len(q) != 0 {
		t.Fatalf("a run inside the grace was replayed: queued = %v", q)
	}

	backdateConclusion(t, database, step0, 2*strandedRunGrace)
	s.mu.Lock()
	s.engagements[step0] = &engagement{}
	s.mu.Unlock()
	s.replayStrandedRuns(context.Background())
	if q := queuedStepConversations(t, database, brID); len(q) != 0 {
		t.Fatalf("a run this process's own engagement is still reacting to was replayed: queued = %v", q)
	}
}

// TestTerminateBlueprint_AnAlreadyTerminalRunRunsNoSideEffects: the terminal
// write that did not change the row did not end the run, so its task close,
// cleanup and firing wake belong to the write that did.
func TestTerminateBlueprint_AnAlreadyTerminalRunRunsNoSideEffects(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "term-unchanged")
	stampBotClaim(t, database, taskID)
	makeConversationBlueprintStep(t, database, conversationID, taskID)
	blueprintRunID := "bpr-" + conversationID
	setConversationStatus(t, database, conversationID, "completed")
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'aborted' WHERE id = ?`, blueprintRunID); err != nil {
		t.Fatalf("end the run first: %v", err)
	}
	waker := newFakeWaker()
	s.SetFiringWaker(waker)
	taskBefore := readTaskStatus(t, database, taskID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, blueprintRunID, taskID, "event", "",
		loadConversation(t, s, conversationID).StartedAt, runConfig{orgID: runmode.LocalDefaultOrgID},
		domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != taskBefore {
		t.Errorf("task.status = %q, want it untouched at %q — the completion did not end this run", got, taskBefore)
	}
	if n := waker.count(); n != 0 {
		t.Errorf("the firing worker was woken %d times by a terminal that changed nothing", n)
	}
	if st := mustGetRun(t, s, runmode.LocalDefaultOrgID, blueprintRunID).Status; st != domain.BlueprintRunStatusAborted {
		t.Errorf("run status = %q, want aborted — the first terminal stands", st)
	}
}

// TestReleaseOwnClaimsOnShutdown_HandsBackOnlyClaimsWhoseEngagementReturned:
// a clean shutdown releases the claims nothing in this process drives any
// more, as a deliberate stop, and leaves one whose engagement is still
// registered — it may yet write.
func TestReleaseOwnClaimsOnShutdown_HandsBackOnlyClaimsWhoseEngagementReturned(t *testing.T) {
	t.Run("returned", func(t *testing.T) {
		f := newLaunchFixture(t, "shutdown-idle")
		f.s.SetExecutorID("lf-exec", 1)
		f.s.ReleaseOwnClaimsOnShutdown()
		if outcomes := f.claimOutcomes(t); len(outcomes) != 1 || outcomes[0] != "requeued_shutdown" {
			t.Errorf("claim outcomes = %v, want [requeued_shutdown]", outcomes)
		}
	})
	t.Run("still engaged", func(t *testing.T) {
		f := newLaunchFixture(t, "shutdown-engaged")
		f.s.SetExecutorID("lf-exec", 1)
		f.s.mu.Lock()
		f.s.engagements[f.conv.ID] = &engagement{}
		f.s.mu.Unlock()
		f.s.ReleaseOwnClaimsOnShutdown()
		if outcomes := f.claimOutcomes(t); len(outcomes) != 1 || outcomes[0] != "" {
			t.Errorf("claim outcomes = %v, want the claim still live", outcomes)
		}
	})
	t.Run("claim loop still running", func(t *testing.T) {
		f := newLaunchFixture(t, "shutdown-loop")
		f.s.SetExecutorID("lf-exec", 1)
		f.s.dispatcherRunning.Store(true)
		f.s.ReleaseOwnClaimsOnShutdown()
		if outcomes := f.claimOutcomes(t); len(outcomes) != 1 || outcomes[0] != "" {
			t.Errorf("claim outcomes = %v, want the claim still live — a loop that may still mint claims has not stopped", outcomes)
		}
	})
}

// TestBootReset_RunsOnlyWhenThePreviousBootsCellsAreConfirmedGone: the boot
// reset releases an earlier boot's claims at once, which is only safe when
// nothing of that boot can still be running. Unconfirmed, the claims are left
// to lapse and be taken over.
func TestBootReset_RunsOnlyWhenThePreviousBootsCellsAreConfirmedGone(t *testing.T) {
	for _, clean := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed=%v", clean), func(t *testing.T) {
			f := newLaunchFixture(t, fmt.Sprintf("boot-reset-%v", clean))
			f.s.SetExecutorID("lf-exec", 2)
			f.s.SetCellsConfirmedClean(clean)
			f.s.reconcileConversationQueue(context.Background())
			outcomes := f.claimOutcomes(t)
			want := ""
			if clean {
				want = "reaped"
			}
			if len(outcomes) != 1 || outcomes[0] != want {
				t.Errorf("previous boot's claim outcome = %v, want [%q]", outcomes, want)
			}
		})
	}
}
