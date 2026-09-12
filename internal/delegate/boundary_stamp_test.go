// The boundary stamps the executor writes: the step advance, the failure
// terminal, and the three blueprint dispositions that used to leave a claimed
// conversation neither failed nor ended.

package delegate

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// boundaryOf reads one conversation's stamp straight off the row, so a test
// asserts what was written rather than what a projection derives.
func boundaryOf(t *testing.T, database *sql.DB, conversationID string) (endedAt sql.NullString, reason string) {
	t.Helper()
	var r sql.NullString
	if err := database.QueryRow(
		`SELECT ended_at, ended_reason FROM conversations WHERE id = ?`, conversationID,
	).Scan(&endedAt, &r); err != nil {
		t.Fatalf("read boundary of %s: %v", conversationID, err)
	}
	return endedAt, r.String
}

func assertEnded(t *testing.T, database *sql.DB, conversationID string, want domain.EndedReason) {
	t.Helper()
	endedAt, reason := boundaryOf(t, database, conversationID)
	if !endedAt.Valid || reason != string(want) {
		t.Errorf("%s boundary = (ended_at valid=%v, reason=%q), want a stamp with reason %q",
			conversationID, endedAt.Valid, reason, want)
	}
}

func assertNotEnded(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	if endedAt, reason := boundaryOf(t, database, conversationID); endedAt.Valid || reason != "" {
		t.Errorf("%s boundary = (ended_at valid=%v, reason=%q), want none", conversationID, endedAt.Valid, reason)
	}
}

// TestReactor_AdvanceEndsTheStepItMovesPast: step N stops being the task's
// live conversation the moment N+1 is minted, so the advance stamps it
// `step_advanced` — and stamps it BEFORE the enqueue, which is the invariant
// that keeps a task from momentarily having two live conversations.
//
// The ordering is asserted the only way it is observable from outside: with
// the enqueue made to fail, the stamp is still there. A stamp written after
// the enqueue would be missing here.
func TestReactor_AdvanceEndsTheStepItMovesPast(t *testing.T) {
	org := runmode.LocalDefaultOrgID

	t.Run("the advance lands", func(t *testing.T) {
		s, database, brID, _, step0 := reactorFixture(t, "adv-boundary", 2, "completed", "continue")
		stepConversation := loadConversation(t, s, step0)
		stepConversation.TriggerType = "manual"
		stepConversation.CreatorUserID = runmode.LocalDefaultUserID
		s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

		if q := queuedStepConversations(t, database, brID); len(q) != 1 || q[0] != 1 {
			t.Fatalf("queued step runs = %v, want [1]", q)
		}
		assertEnded(t, database, step0, domain.EndedStepAdvanced)
	})

	t.Run("the enqueue fails", func(t *testing.T) {
		s, database, brID, _, step0 := reactorFixture(t, "adv-boundary-fail", 2, "completed", "continue")
		// Point the frozen plan's step 1 at a prompt that does not exist, so
		// enqueueBlueprintStep fails on the conversation's FK. Whatever the
		// reactor does after that, step 0 is already behind it.
		if _, err := database.Exec(
			`UPDATE blueprint_runs SET step_plan = replace(step_plan, ?, ?) WHERE id = ?`,
			`"prompt_id":"adv-boundary-fail-p1"`, `"prompt_id":"no-such-prompt"`, brID,
		); err != nil {
			t.Fatalf("re-point step 1 at a missing prompt: %v", err)
		}
		stepConversation := loadConversation(t, s, step0)
		stepConversation.TriggerType = "manual"
		stepConversation.CreatorUserID = runmode.LocalDefaultUserID
		s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

		if q := queuedStepConversations(t, database, brID); len(q) != 0 {
			t.Fatalf("queued step runs = %v, want none — the enqueue was meant to fail", q)
		}
		assertEnded(t, database, step0, domain.EndedStepAdvanced)
	})
}

// TestReactor_AdvancedStepRefusesAFollowUp: the rung a person actually meets.
// Past the advance, step 0's composer is disabled and its send is refused with
// the boundary that ended it — not with the blueprint rung, which is true of
// the row too but says the less useful of the two things.
func TestReactor_AdvancedStepRefusesAFollowUp(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, brID, _, step0 := reactorFixture(t, "adv-refusal", 2, "completed", "continue")
	giveConversationResumeState(t, database, step0)

	stepConversation := loadConversation(t, s, step0)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(ctx, org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if ok, reason := s.ResumabilityFor(ctx, org, loadConversation(t, s, step0)); ok || reason != ResumeBlockedEndedStepAdvanced {
		t.Errorf("ResumabilityFor = (%v, %q), want (false, %q)", ok, reason, ResumeBlockedEndedStepAdvanced)
	}
	if err := s.SendMessage(ctx, org, step0, runmode.LocalDefaultUserID, "one more thing"); !errors.Is(err, ErrConversationEnded) {
		t.Errorf("SendMessage = %v, want ErrConversationEnded", err)
	}
	if got := pendingRows(t, s, step0); len(got) != 0 {
		t.Errorf("a refused follow-up left %d queued rows behind; the gate runs before the write", len(got))
	}
}

// TestFailConversation_StampsTheFailedBoundary: a failure is a boundary, so
// the terminal and the stamp land together. Without it a failed conversation
// stays un-ended, which is the state that owes its memory to nobody.
func TestFailConversation_StampsTheFailedBoundary(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-fail-boundary"
	seedConversation(t, database, conversationID, "sess", t.TempDir())
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if fenced := s.failConversation(runmode.LocalDefaultOrgID, conversationID, "", "", "manual", runmode.LocalDefaultUserID,
		"the runtime died", domain.ConversationFailureUnclassified); fenced {
		t.Fatal("failConversation reported fenced; nothing holds a claim on this fixture")
	}
	if st := storedStatus(t, database, conversationID); st != "failed" {
		t.Fatalf("stored status = %q, want failed", st)
	}
	assertEnded(t, database, conversationID, domain.EndedFailed)
}

// TestFailedConversationRefusesAFollowUpAsEnded: `failed` was already refused
// by the not-steerable rung, which said nothing about why. It now answers with
// the boundary, which is the sentence a person can act on.
func TestFailedConversationRefusesAFollowUpAsEnded(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	database := newDelegateTestDB(t)
	const conversationID = "r-failed-ended"
	seedConversation(t, database, conversationID, "sess", t.TempDir())
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	if fenced := s.failConversation(org, conversationID, "", "", "manual", runmode.LocalDefaultUserID,
		"the runtime died", domain.ConversationFailureUnclassified); fenced {
		t.Fatal("failConversation reported fenced")
	}

	if ok, reason := s.ResumabilityFor(ctx, org, loadConversation(t, s, conversationID)); ok || reason != ResumeBlockedEndedFailed {
		t.Errorf("ResumabilityFor = (%v, %q), want (false, %q)", ok, reason, ResumeBlockedEndedFailed)
	}
	if err := s.SendMessage(ctx, org, conversationID, runmode.LocalDefaultUserID, "retry"); !errors.Is(err, ErrConversationEnded) {
		t.Errorf("SendMessage = %v, want ErrConversationEnded", err)
	}
}

// TestTerminalBlueprintFailures_MarkAndEndTheStep covers the three
// dispositions that failed a blueprint and left its step's row untouched:
// NULL-status with a released claim, which is neither failed nor ended. Such a
// row reads as live to the task and stays in the needs-driving set of a
// blueprint that is already `failed`, so the claim scan hands it back forever.
//
// Each arm asserts the whole fix: the terminal, the boundary, and the claim
// scan declining to pick the row up again.
func TestTerminalBlueprintFailures_MarkAndEndTheStep(t *testing.T) {
	org := runmode.LocalDefaultOrgID

	for _, tc := range []struct {
		name   string
		suffix string
		// dispose runs the path under test against a claimed, never-run step.
		dispose func(t *testing.T, s *Spawner, br *domain.BlueprintRun, conv domain.Conversation)
	}{
		{
			name:   "the task under the step is gone",
			suffix: "term-task",
			dispose: func(t *testing.T, s *Spawner, br *domain.BlueprintRun, conv domain.Conversation) {
				s.failClaimedConversation(org, &conv, "load task: gone")
				s.terminateBlueprint(org, br.ID, conv.TaskID, conv.TriggerType, conv.CreatorUserID, time.Now(),
					runConfig{orgID: org, teamID: conv.TeamID}, domain.BlueprintRunStatusFailed, "load task: gone", conv.BlueprintStepIndex, true)
			},
		},
		{
			name:   "a first engagement's setup budget is exhausted",
			suffix: "term-exhausted",
			dispose: func(t *testing.T, s *Spawner, br *domain.BlueprintRun, conv domain.Conversation) {
				if survived := s.disposeOfExhaustedConversation(org, br, conv, errors.New("workspace setup failed")); survived {
					t.Fatal("disposeOfExhaustedConversation reported survived; a first engagement is the poison pill")
				}
			},
		},
		{
			name:   "the model the step would run on left the team's set",
			suffix: "term-refusal",
			dispose: func(t *testing.T, s *Spawner, br *domain.BlueprintRun, conv domain.Conversation) {
				s.disposeOfModelRefusal(org, br, conv, errors.New("model not enabled for this team"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, database, brID, _, step0 := reactorFixture(t, tc.suffix, 2, "completed", "continue")
			// A step that has been claimed and has not run: no transcript, no
			// terminal. That is the shape all three paths are answering for.
			if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, step0); err != nil {
				t.Fatalf("force the step mid-flight: %v", err)
			}
			claimed, err := s.conversationQueue.ClaimNextConversation(context.Background(), "exec-boundary", 1, db.ClaimPlacement{})
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if claimed == nil || claimed.ID != step0 {
				t.Fatalf("claimed = %v, want the seeded step %s", claimed, step0)
			}

			tc.dispose(t, s, mustGetRun(t, s, org, brID), *claimed)

			if st := storedStatus(t, database, step0); st != "failed" {
				t.Errorf("stored status = %q, want failed", st)
			}
			assertEnded(t, database, step0, domain.EndedFailed)

			// The point of marking it at all: the scan is done with this row.
			again, err := s.conversationQueue.ClaimNextConversation(context.Background(), "exec-boundary", 1, db.ClaimPlacement{})
			if err != nil {
				t.Fatalf("re-claim: %v", err)
			}
			if again != nil {
				t.Errorf("claimed %q again — a failed, ended step under a failed blueprint must leave the needs-driving set", again.ID)
			}
		})
	}
}

// TestStopCause_NotesNameTheLifecycleEvent: the note is the transcript's whole
// explanation of why a run stopped, so the two causes split out of
// "dispositioned" have to say what actually happened — the task is still open
// in both, and it is the work that moved, not the task that went away.
func TestStopCause_NotesNameTheLifecycleEvent(t *testing.T) {
	for _, tc := range []struct {
		cause StopCause
		want  string
	}{
		{StopCauseTaskDelegated, "Run stopped: the task was handed to a new delegation."},
		{StopCauseTaskTakenOver, "Run stopped: a person took the task over."},
	} {
		if got := tc.cause.note(); got != tc.want {
			t.Errorf("%s.note() = %q, want %q", tc.cause, got, tc.want)
		}
		if got := tc.cause.note(); got == StopCauseTaskDispositioned.note() {
			t.Errorf("%s.note() still reads as a disposition; the task is open", tc.cause)
		}
	}
}

// TestEndedFollowUpBlock_CoversTheWholeVocabulary: the rung is derived from
// the stored reason by prefixing, so the declared constants are a second
// spelling of the same thing and could drift from the vocabulary silently — a
// reason with no rung would still refuse, but with a name no client has copy
// for. This walks the closed set so adding a reason fails here rather than in
// front of a person.
func TestEndedFollowUpBlock_CoversTheWholeVocabulary(t *testing.T) {
	declared := map[string]bool{
		ResumeBlockedEndedRequeued:     true,
		ResumeBlockedEndedDelegated:    true,
		ResumeBlockedEndedTakenOver:    true,
		ResumeBlockedEndedStepAdvanced: true,
		ResumeBlockedEndedTeamArchived: true,
		ResumeBlockedEndedFailed:       true,
	}
	if len(declared) != len(domain.AllEndedReasons()) {
		t.Fatalf("declared rungs = %d, ended reasons = %d", len(declared), len(domain.AllEndedReasons()))
	}
	endedAt := time.Now()
	for _, reason := range domain.AllEndedReasons() {
		block := endedFollowUpBlock(&domain.Conversation{EndedAt: &endedAt, EndedReason: reason})
		if !declared[block] {
			t.Errorf("reason %q derives rung %q, which no ResumeBlockedEnded* constant declares", reason, block)
		}
		if !errors.Is(blockedFollowUpError(block), ErrConversationEnded) {
			t.Errorf("rung %q maps to %v, want ErrConversationEnded", block, blockedFollowUpError(block))
		}
	}
	// A live conversation has no rung, whatever else is on the row.
	if block := endedFollowUpBlock(&domain.Conversation{Status: "completed"}); block != "" {
		t.Errorf("endedFollowUpBlock on a live conversation = %q, want none", block)
	}
}
