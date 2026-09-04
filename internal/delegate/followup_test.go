package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// finishBlueprint settles a seeded blueprint_run on the step its conversation
// ran — the shape a blueprint leaves behind when its plan completes. Only
// SetRunCurrentStepSystem writes current_step_index and no terminal write touches it, so a
// blueprint that stopped at step N really does leave the column at N; the
// fixture writes both directly because the guarded store method refuses a
// non-running row.
func finishBlueprint(t *testing.T, database *sql.DB, blueprintRunID, status string, currentStep int) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE blueprint_runs SET status = ?, current_step_index = ? WHERE id = ?`,
		status, currentStep, blueprintRunID,
	); err != nil {
		t.Fatalf("finish blueprint: %v", err)
	}
}

// blueprintState reads back the two columns locked decision 2 says a follow-up
// must never move.
func blueprintState(t *testing.T, database *sql.DB, blueprintRunID string) (string, int) {
	t.Helper()
	var status string
	var step int
	if err := database.QueryRow(
		`SELECT status, current_step_index FROM blueprint_runs WHERE id = ?`, blueprintRunID,
	).Scan(&status, &step); err != nil {
		t.Fatalf("read blueprint state: %v", err)
	}
	return status, step
}

func taskStatus(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	return status
}

// TestFollowUpOnFinishedBlueprint_IsClaimedAndDriven is the acceptance test for
// the trap: a row that flips to claimable but is never CLAIMED fails silently
// and permanently, and it would pass any assertion that only checked the resume
// was accepted. So this claims for real, through the same ClaimNextConversation the
// dispatcher calls, and then drives the claim.
//
// The failure it guards is two-sided. Before the claim gate widened, the claim
// itself returned nothing. After it widened but before the dispatch guard did,
// the claim succeeded and dispatchClaimedConversation immediately parked the
// conversation as "Blueprint cancelled by user" — a worse outcome than the
// refusal, because it looks like it worked.
//
// The delivery here fails fast (no session transcript in a bare temp worktree)
// and lands the conversation terminal. That is the assertion: reaching a
// terminal at all means the claim went down the resume path rather than the
// raced-cancel park.
func TestFollowUpOnFinishedBlueprint_IsClaimedAndDriven(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-followup", "sess-followup", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-followup'`); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	bpr := blueprintRunIDForConversation(t, database, "r-followup")
	finishBlueprint(t, database, bpr, "completed", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-followup", runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage on a finished blueprint's final conversation: %v", err)
	}

	// Fatals if nothing is claimable — which is exactly the silent failure.
	claimAndDispatch(t, s, database)

	st := storedStatus(t, database, "r-followup")
	if st == "" || st == "open" {
		t.Fatalf("stored status = %q, want a terminal — the claim never reached delivery", st)
	}
	var stopReason sql.NullString
	if err := database.QueryRow(`SELECT park_reason FROM conversations WHERE id='r-followup'`).Scan(&stopReason); err != nil {
		t.Fatalf("read park_reason: %v", err)
	}
	if stopReason.String == "user_cancelled" {
		t.Error("the follow-up claim was parked as a cancelled blueprint step; it must route to the resume path")
	}
	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-followup"); err != nil || ok {
		t.Errorf("pending input survived the dispatch: ok=%v err=%v", ok, err)
	}
}

// TestFollowUpOnFinishedBlueprint_LeavesTheBlueprintAndTaskAlone pins locked
// decision 2 (finished machinery never restarts) and locked decision 1 (done
// tasks never leave the done column) across a whole follow-up. Both guards
// exist already and nothing else stops someone removing one.
func TestFollowUpOnFinishedBlueprint_LeavesTheBlueprintAndTaskAlone(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "followup-frozen")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(
		`UPDATE conversations SET status='completed', outcome='finish', worktree_path=? WHERE id=?`,
		t.TempDir(), conversationID,
	); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	if _, err := database.Exec(`UPDATE tasks SET status='done' WHERE id=?`, taskID); err != nil {
		t.Fatalf("close task: %v", err)
	}
	bpr := blueprintRunIDForConversation(t, database, conversationID)
	finishBlueprint(t, database, bpr, "completed", 0)

	wantStatus, wantStep := blueprintState(t, database, bpr)

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// The resume alone must not move anything.
	if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != wantStatus || gotStep != wantStep {
		t.Errorf("after the resume, blueprint = (%q, %d), want (%q, %d)", gotStatus, gotStep, wantStatus, wantStep)
	}
	if got := taskStatus(t, database, taskID); got != "done" {
		t.Errorf("after the resume, task status = %q, want done", got)
	}

	// Nor must driving it to its own conclusion.
	claimAndDispatch(t, s, database)
	if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != wantStatus || gotStep != wantStep {
		t.Errorf("after the follow-up concluded, blueprint = (%q, %d), want (%q, %d)", gotStatus, gotStep, wantStatus, wantStep)
	}
	if got := taskStatus(t, database, taskID); got != "done" {
		t.Errorf("after the follow-up concluded, task status = %q, want done", got)
	}
}

// TestFollowUpOnANonFinalStepIsRefused: the snapshot key is the blueprint run,
// one blob every step overwrites, so an earlier step has no tree of its own left
// to resume into. The refusal has to happen HERE, before the flip — the claim
// gate declining afterwards is not a refusal, it is a row stranded mid-flight
// that nothing will ever pick up.
//
// Every non-running terminal, and both runtimes. `aborted` is the one that
// nearly got away: an aborted blueprint re-opens on resume, but only for the
// conversation that carries the abort — the step it aborted ON, which is where
// current_step_index already points. An EARLIER step of the same blueprint
// concluded with `continue` and re-opens nothing, so waking it leaves the
// blueprint terminal and its index pointing past the row.
func TestFollowUpOnANonFinalStepIsRefused(t *testing.T) {
	for _, tc := range []struct{ blueprint, outcome string }{
		{"completed", "finish"},
		{"aborted", "continue"},
		{"failed", "continue"},
	} {
		for _, runtime := range []string{"sdk", "native"} {
			t.Run(tc.blueprint+"/"+runtime, func(t *testing.T) {
				paths.SetForTest(t, t.TempDir())
				database := newDelegateTestDB(t)
				seedConversation(t, database, "r-earlier", "sess-earlier", t.TempDir())
				if _, err := database.Exec(
					`UPDATE conversations SET status='completed', outcome=? WHERE id='r-earlier'`, tc.outcome,
				); err != nil {
					t.Fatalf("conclude conversation: %v", err)
				}
				bpr := blueprintRunIDForConversation(t, database, "r-earlier")
				// The blueprint came to rest on step 1; this conversation is step 0.
				finishBlueprint(t, database, bpr, tc.blueprint, 1)
				s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
				if runtime == "native" {
					markNative(t, database, "r-earlier")
				}

				err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-earlier", runmode.LocalDefaultUserID, "one more thing")
				if !errors.Is(err, ErrConversationConcluded) {
					t.Fatalf("err = %v, want ErrConversationConcluded — a wake here strands the row for good", err)
				}
				if st := storedStatus(t, database, "r-earlier"); st != "completed" {
					t.Errorf("stored status = %q, want completed — a refused wake writes nothing", st)
				}
				if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != tc.blueprint || gotStep != 1 {
					t.Errorf("blueprint = (%q, %d), want it untouched at (%q, 1)", gotStatus, gotStep, tc.blueprint)
				}
			})
		}
	}
}

// TestFollowUpOnTheAbortedStepItselfIsAccepted is the other half: the
// conversation an aborted blueprint actually stopped on is where
// current_step_index points, so it resumes — and re-opens its blueprint, which
// is what distinguishes it from a blueprint that finished.
func TestFollowUpOnTheAbortedStepItselfIsAccepted(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-aborted", "sess-aborted", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort' WHERE id='r-aborted'`); err != nil {
		t.Fatalf("abort conversation: %v", err)
	}
	bpr := blueprintRunIDForConversation(t, database, "r-aborted")
	finishBlueprint(t, database, bpr, "aborted", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-aborted", runmode.LocalDefaultUserID, "pick it back up"); err != nil {
		t.Fatalf("SendMessage on the step the blueprint aborted on: %v", err)
	}
	if gotStatus, _ := blueprintState(t, database, bpr); gotStatus != "running" {
		t.Errorf("blueprint status = %q, want running — an abort resumes its sequence", gotStatus)
	}
}

// TestModelForClaim_FollowUpDoesNotInheritTheStepModel pins locked decision 7.
// A blueprint's steps keep the model frozen onto them; a follow-up on the
// finished blueprint does not, because a review blueprint's last step is a
// cheap aggregator and answering someone's first follow-up with it is how they
// decide follow-ups don't work.
func TestModelForClaim_FollowUpDoesNotInheritTheStepModel(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "team-default-model")
	conv := domain.Conversation{Model: "cheap-aggregator", TeamID: runmode.LocalDefaultTeamID}
	ctx := context.Background()

	running := &domain.BlueprintRun{Status: domain.BlueprintRunStatusRunning}
	if got, err := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, running, conv); err != nil || got != "cheap-aggregator" {
		t.Errorf("step of a running blueprint = (%q, %v), want the step's own model (no mid-blueprint drift)", got, err)
	}

	for _, term := range []domain.BlueprintRunStatus{
		domain.BlueprintRunStatusCompleted,
		domain.BlueprintRunStatusAborted,
		domain.BlueprintRunStatusFailed,
	} {
		got, err := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: term}, conv)
		if err != nil || got != "team-default-model" {
			t.Errorf("follow-up under a %s blueprint = (%q, %v), want the configured default", term, got, err)
		}
	}

	// A resolver that answers no model at all must not blank it out from under
	// the turn — the step's is a worse answer than the default, never no answer.
	// A resolver that REFUSES is the other case, and it is not this one: that
	// refusal fails the claim by name rather than reaching for anything.
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return domain.NewTeamModels("", domain.ModelSet{}), nil
	})
	s.model = ""
	if got, err := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: domain.BlueprintRunStatusCompleted}, conv); err != nil || got != "cheap-aggregator" {
		t.Errorf("unresolvable default = (%q, %v), want a fall back to the step's model", got, err)
	}

	// A team whose default its own enable-set no longer includes refuses the
	// follow-up by name. Answering with the step's model would be the silent
	// substitution the whole selection design forbids, dressed as inheritance.
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return domain.NewTeamModels(domain.ModelOpus,
			domain.TeamModelSet([]string{domain.ModelHaiku}, orgUnrestricted())), nil
	})
	if got, err := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: domain.BlueprintRunStatusCompleted}, conv); !errors.Is(err, domain.ErrModelNotEnabled) || got != "" {
		t.Errorf("a disabled team default = (%q, %v), want a refusal naming the model", got, err)
	}

	// And the fallback itself is held to the set. A team with no default at all
	// whose step ran on a model the set has since dropped refuses too: reaching
	// the claim by inheritance is not a licence to run something nobody may
	// pick, and answering with it would be the same substitution wearing the
	// one costume the branch above does not check for.
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return domain.NewTeamModels("",
			domain.TeamModelSet([]string{domain.ModelHaiku}, orgUnrestricted())), nil
	})
	disabledStep := domain.Conversation{Model: domain.ModelOpus, TeamID: runmode.LocalDefaultTeamID}
	got, err := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: domain.BlueprintRunStatusCompleted}, disabledStep)
	if !errors.Is(err, domain.ErrModelNotEnabled) || got != "" {
		t.Fatalf("no default plus a disabled step model = (%q, %v), want a refusal", got, err)
	}
	for _, want := range []string{domain.ModelOpus, domain.ModelHaiku} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	// The same shape with the step's model still enabled is the case the
	// fallback exists for, and it still runs.
	enabledStep := domain.Conversation{Model: domain.ModelHaiku, TeamID: runmode.LocalDefaultTeamID}
	if got, err := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: domain.BlueprintRunStatusCompleted}, enabledStep); err != nil || got != domain.ModelHaiku {
		t.Errorf("no default plus an enabled step model = (%q, %v), want %q", got, err, domain.ModelHaiku)
	}
}

// TestFollowUpRecordsTaskEvent: the resume moves nothing a person can see, so
// the task_events row is the only trace the closed card keeps of work
// continuing after it closed.
func TestFollowUpRecordsTaskEvent(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-timeline", "sess-timeline", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-timeline'`); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	bpr := blueprintRunIDForConversation(t, database, "r-timeline")
	finishBlueprint(t, database, bpr, "completed", 0)
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id='r-timeline'`).Scan(&taskID); err != nil {
		t.Fatalf("read task id: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-timeline", runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var kind, metadata string
	if err := database.QueryRow(`
		SELECT te.kind, e.metadata_json
		FROM task_events te JOIN events e ON e.id = te.event_id
		WHERE te.task_id = ? AND e.event_type = ?
	`, taskID, domain.EventSystemConversationResumed).Scan(&kind, &metadata); err != nil {
		t.Fatalf("read the resume task_event: %v", err)
	}
	if kind != "resumed" {
		t.Errorf("task_events.kind = %q, want resumed", kind)
	}
	var meta events.SystemConversationResumedMetadata
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.ConversationID != "r-timeline" || meta.TaskID != taskID || meta.UserID != runmode.LocalDefaultUserID {
		t.Errorf("metadata = %+v, want it to name the conversation, task and actor", meta)
	}
	if meta.BlueprintRunID != bpr || meta.FromStatus != "completed" || meta.FromOutcome != "finish" {
		t.Errorf("metadata = %+v, want the pre-flip rest state and the blueprint it belonged to", meta)
	}
	if got := taskStatus(t, database, taskID); got == "done" {
		t.Error("fixture is not exercising the interesting case; the task was already done")
	}
}

// TestDispatchClaimedRun_FinishedBlueprintWithoutAMessageParks covers the one
// window the widened gate opens: an SDK claim on a finished blueprint whose
// staged message an earlier claim already consumed before dying. The step
// machinery would re-run the final step's mission from the top on concluded
// work, so the claim parks instead and leaves the workspace for the next
// follow-up.
func TestDispatchClaimedRun_FinishedBlueprintWithoutAMessageParks(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-nomsg", "sess-nomsg", t.TempDir())
	bpr := blueprintRunIDForConversation(t, database, "r-nomsg")
	finishBlueprint(t, database, bpr, "completed", 0)
	// Mid-flight with no staged message: what a crash between the resume
	// message's consume and the turn's conclusion leaves behind.
	if _, err := database.Exec(`UPDATE conversations SET status=NULL WHERE id='r-nomsg'`); err != nil {
		t.Fatalf("mid-flight: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	claimAndDispatch(t, s, database)

	if st := storedStatus(t, database, "r-nomsg"); st != "open" {
		t.Errorf("stored status = %q, want open — a message-less follow-up claim parks rather than re-running the step", st)
	}
	if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != "completed" || gotStep != 0 {
		t.Errorf("blueprint = (%q, %d), want it untouched at (completed, 0)", gotStatus, gotStep)
	}
}

// A team that may no longer run the model its concluded work would continue on
// is refused BEFORE the message is written, and the composer is told the same
// thing by the same walk.
//
// That is the whole point of putting the check on the refusal ladder rather
// than only at dispatch: the person is right there, having just typed, and the
// answer they need — pick a model — is one they can act on in the moment. A
// message accepted here would be queued against a claim that refuses it, and
// they would learn minutes later from a stop note.
func TestFollowUp_ModelNotEnabledIsRefusedBeforeTheWrite(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-disabled", "sess-disabled", t.TempDir())
	if _, err := database.Exec(
		`UPDATE conversations SET status='completed', outcome='finish', model=? WHERE id='r-disabled'`,
		domain.ModelOpus,
	); err != nil {
		t.Fatalf("conclude conversation: %v", err)
	}
	bpr := blueprintRunIDForConversation(t, database, "r-disabled")
	finishBlueprint(t, database, bpr, "completed", 0)

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	// The team's default is gone and its set no longer names what the step ran
	// on — the two halves of the refusal, together.
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return domain.NewTeamModels("",
			domain.TeamModelSet([]string{domain.ModelHaiku}, orgUnrestricted())), nil
	})

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-disabled")
	if err != nil || conv == nil {
		t.Fatalf("load conversation: %v", err)
	}
	ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, conv)
	if ok || reason != ResumeBlockedModelNotEnabled {
		t.Errorf("ResumabilityFor = (%v, %q), want the model rung — the composer has to say so before anyone types", ok, reason)
	}

	err = s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-disabled", runmode.LocalDefaultUserID, "one more thing")
	if !errors.Is(err, domain.ErrModelNotEnabled) {
		t.Fatalf("SendMessage err = %v, want a model refusal", err)
	}
	// Nothing was written: no queued row for a claim that would refuse it, and
	// the conversation is where it was.
	var queued int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE conversation_id='r-disabled' AND delivered=false`,
	).Scan(&queued); err != nil {
		t.Fatalf("count undelivered: %v", err)
	}
	if queued != 0 {
		t.Errorf("a refused send queued %d message(s)", queued)
	}
	if st := storedStatus(t, database, "r-disabled"); st != "completed" {
		t.Errorf("stored status = %q, want completed — a refused send writes nothing", st)
	}

	// Re-enabling is the whole fix, and it takes effect on the next read: the
	// ladder resolves the set fresh rather than caching a verdict onto the row.
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return domain.NewTeamModels("",
			domain.TeamModelSet([]string{domain.ModelHaiku, domain.ModelOpus}, orgUnrestricted())), nil
	})
	if ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, conv); !ok {
		t.Errorf("after re-enabling the model the composer is still blocked: %q", reason)
	}
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-disabled", runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Errorf("SendMessage after re-enabling: %v", err)
	}
}

// The dispatch-side backstop, and the reason it does not go through the retry
// ladder: an enable-set refusal is settled, so five attempts buy nothing and
// then report that the runtime failed to start — which is not what happened and
// points at the wrong fix.
//
// It is a backstop rather than the gate, because the set can narrow between the
// send the ladder allowed and the claim that picks it up. What lands is a park
// with the refusal itself on the transcript, on the first attempt.
func TestDisposeOfModelRefusal_ParksOnceWithTheRefusal(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-refused", "sess-refused", t.TempDir())
	if _, err := database.Exec(
		`INSERT INTO messages (org_id, conversation_id, role, content)
		 VALUES (?, 'r-refused', 'assistant', 'work happened here')`, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	claimID := markEngaged(t, database, "r-refused")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-refused")
	if err != nil || conv == nil {
		t.Fatalf("load conversation: %v", err)
	}
	// The claim path hands dispatch the conversation with its claim named;
	// the park below is fenced on it.
	conv.ClaimID = claimID
	cause := fmt.Errorf("%w: %s is not one of %s", domain.ErrModelNotEnabled, domain.ModelOpus, domain.ModelHaiku)
	s.disposeOfModelRefusal(runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: domain.BlueprintRunStatusCompleted}, *conv, cause)

	var status, reason string
	if err := database.QueryRow(
		`SELECT status, COALESCE(park_reason, '') FROM conversations WHERE id='r-refused'`,
	).Scan(&status, &reason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" || reason != string(domain.ParkReasonModelNotEnabled) {
		t.Errorf("parked as (%q, %q), want (open, %s)", status, reason, domain.ParkReasonModelNotEnabled)
	}

	var note string
	if err := database.QueryRow(
		`SELECT content FROM messages WHERE conversation_id='r-refused' AND subtype=?
		  ORDER BY id DESC LIMIT 1`, domain.MessageSubtypeStopNote,
	).Scan(&note); err != nil {
		t.Fatalf("read stop note: %v", err)
	}
	for _, want := range []string{domain.ModelOpus, domain.ModelHaiku} {
		if !strings.Contains(note, want) {
			t.Errorf("stop note %q does not name %s", note, want)
		}
	}
	// "Send a message to retry" is the launch-failure note's advice and would be
	// wrong here: a message wakes this straight back into the same refusal.
	if strings.Contains(note, "retry") {
		t.Errorf("stop note tells the reader to retry, which cannot work: %q", note)
	}
	if strings.Contains(note, "runtime failed to start") {
		t.Errorf("stop note blames the runtime for a settings refusal: %q", note)
	}
}
