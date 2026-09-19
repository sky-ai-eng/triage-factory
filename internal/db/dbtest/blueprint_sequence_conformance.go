package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// isTaskBusy is the one-active-run-per-task refusal, which both dialects
// translate to the same sentinel.
func isTaskBusy(err error) bool { return errors.Is(err, db.ErrTaskBusyActiveRun) }

// BlueprintSequenceFactory is what a per-backend test file hands to
// RunBlueprintSequenceConformance: the wired BlueprintStore plus a scaffold
// that stages firings and advances and reads back what they committed. Each
// call must hand back a clean database — the suite counts rows per task and
// asserts exact totals.
type BlueprintSequenceFactory func(t *testing.T) (db.BlueprintStore, BlueprintSequenceScaffold)

// BlueprintSequenceScaffold stages one backend's firings and advances and
// reads their aftermath. Every write below is a real store call or the
// backend's own SQL; nothing here reimplements the method under test.
type BlueprintSequenceScaffold struct {
	OrgID string
	// AgentID names a real agents row, so the claim stamp has something to write.
	AgentID string

	// ConsolidateTeamID is the team a firing asks the task's card to move to.
	// Multi-team backends pass a SECOND team, which is the case the
	// consolidation exists for; a single-team backend passes its only team,
	// where the write is real but the move is vacuous.
	ConsolidateTeamID string
	// BadTeamID is a team id the backend's schema will refuse, for the arm
	// that proves a failed consolidation takes the whole firing with it.
	// Empty skips that subtest — a backend with no FK on tasks.team_id has no
	// way to fail the write.
	BadTeamID string

	// NewTask mints a fresh task and returns its id.
	NewTask func(t *testing.T) string
	// Firing composes an event-triggered run for taskID, carrying its own
	// blueprint, trigger and triggering event so the replay fence is free
	// unless a subtest deliberately reuses one.
	Firing func(t *testing.T, taskID string) domain.BlueprintRun
	// ManualFiring composes the same thing for the unfenced manual arm.
	ManualFiring func(t *testing.T, taskID string) domain.BlueprintRun
	// Step composes the conversation for one step of br. Index 0 is the row a
	// firing commits with its run; a higher index is the row an advance mints.
	Step func(br domain.BlueprintRun, stepIndex int) domain.Conversation
	// ClaimTaskForUser hands the task to a human, so the firing's own stamp is
	// refused.
	ClaimTaskForUser func(t *testing.T, taskID string)

	// RunCount counts blueprint_runs on the task.
	RunCount func(t *testing.T, taskID string) int
	// ConversationCount counts conversations on the task.
	ConversationCount func(t *testing.T, taskID string) int
	// TaskOwnerTeam reads tasks.team_id.
	TaskOwnerTeam func(t *testing.T, taskID string) string
	// TaskAgentClaim reads tasks.claimed_by_agent_id ("" for NULL).
	TaskAgentClaim func(t *testing.T, taskID string) string
	// RunCurrentStep reads blueprint_runs.current_step_index — the pointer
	// the claim gate drives, and the one an advance moves.
	RunCurrentStep func(t *testing.T, blueprintRunID string) int
	// ConversationEnded reports whether a conversation carries an ended_at
	// stamp: the boundary an advance writes on the step it concludes.
	ConversationEnded func(t *testing.T, convID string) bool
	// ConversationTeam reads a conversation's team_id. Nil on a backend whose
	// conversations do not derive their team from the task (local mode's
	// single-team sentinel), where the assertion would test nothing.
	ConversationTeam func(t *testing.T, convID string) string
	// GetConversation point-reads a conversation through ConversationStore's
	// own projection, so the suite can hold each door's returned conversation
	// to the returned-row standard. These are the only two doors that mint
	// one, so this is where that standard is checked for the row shape.
	GetConversation func(t *testing.T, convID string) (*domain.Conversation, error)
}

// RunBlueprintSequenceConformance is the shared suite for the two doors that
// move a blueprint's sequence: the firing that mints step 0 with its run, and
// the advance that mints every step after with the pointer naming it.
//
// Its subject is indivisibility. The writes each door makes are implied by the
// one before them, and the shapes a partial commit leaves behind are all
// silent: a run with no step, or a pointer naming a step that was never
// minted, is a 'running' blueprint nothing drives and no recovery arm can
// reach, and a firing under a stale owner opens its conversation on the wrong
// team's board. So the suite fires and advances, then fires and advances
// things that fail, and asserts the database only ever holds all of it or none
// of it.
func RunBlueprintSequenceConformance(t *testing.T, mk BlueprintSequenceFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Commits_the_owner_the_run_the_claim_and_the_step", func(t *testing.T) {
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br := sc.Firing(t, taskID)

		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, sc.Step(br, 0))
		if err != nil {
			t.Fatalf("CreateRunWithFirstStepSystem: %v", err)
		}
		if !inserted || !claimed || conv == nil {
			t.Fatalf("(inserted=%v, claimed=%v, conv=%v), want (true, true, non-nil)", inserted, claimed, conv != nil)
		}
		if n := sc.RunCount(t, taskID); n != 1 {
			t.Errorf("blueprint_runs on the task = %d, want 1", n)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1 — a run without its step is an orphan nothing drives", n)
		}
		if got := sc.TaskOwnerTeam(t, taskID); got != sc.ConsolidateTeamID {
			t.Errorf("task owner team = %q, want %q", got, sc.ConsolidateTeamID)
		}
		if got := sc.TaskAgentClaim(t, taskID); got != sc.AgentID {
			t.Errorf("claimed_by_agent_id = %q, want %q", got, sc.AgentID)
		}
		if sc.ConversationTeam != nil {
			if got := sc.ConversationTeam(t, conv.ID); got != sc.ConsolidateTeamID {
				t.Errorf("conversation team_id = %q, want the consolidated owner %q — the step insert must read the consolidation, not the owner it replaced", got, sc.ConsolidateTeamID)
			}
		}
		AssertWriteReturnedStoredRow(t, "CreateRunWithFirstStepSystem", *conv,
			func() (*domain.Conversation, error) { return sc.GetConversation(t, conv.ID) })
	})

	t.Run("A_failing_step_insert_rolls_the_whole_firing_back", func(t *testing.T) {
		// The last statement in the transaction, made to fail by pointing the
		// step at a conversation id that already exists. Everything committed
		// before it in the same transaction must go with it.
		store, sc := mk(t)
		taken := sc.NewTask(t)
		takenBr := sc.Firing(t, taken)
		_, _, occupied, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, takenBr, db.AgentClaimStamp{}, "", sc.Step(takenBr, 0))
		if err != nil || occupied == nil {
			t.Fatalf("seed a committed conversation: (%v, %v)", occupied, err)
		}

		taskID := sc.NewTask(t)
		ownerBefore := sc.TaskOwnerTeam(t, taskID)
		br := sc.Firing(t, taskID)
		step := sc.Step(br, 0)
		step.ID = occupied.ID // primary-key collision

		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, step)
		if err == nil {
			t.Fatalf("a duplicate conversation id must fail the firing, got (inserted=%v, claimed=%v, conv=%v)", inserted, claimed, conv != nil)
		}
		if n := sc.RunCount(t, taskID); n != 0 {
			t.Errorf("blueprint_runs on the task = %d, want 0 — the run committed without its step", n)
		}
		if n := sc.ConversationCount(t, taskID); n != 0 {
			t.Errorf("conversations on the task = %d, want 0", n)
		}
		if got := sc.TaskAgentClaim(t, taskID); got != "" {
			t.Errorf("claimed_by_agent_id = %q, want empty — the claim committed under a firing that did not", got)
		}
		if got := sc.TaskOwnerTeam(t, taskID); got != ownerBefore {
			t.Errorf("task owner team = %q, want %q — the consolidation committed under a firing that did not", got, ownerBefore)
		}
	})

	t.Run("Replay_after_a_rolled_back_firing_fires_exactly_once", func(t *testing.T) {
		// The rollback above leaves no fence row, which is what makes the
		// event's replay the retry it is supposed to be rather than a
		// permanently satisfied no-op.
		store, sc := mk(t)
		taken := sc.NewTask(t)
		takenBr := sc.Firing(t, taken)
		_, _, occupied, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, takenBr, db.AgentClaimStamp{}, "", sc.Step(takenBr, 0))
		if err != nil || occupied == nil {
			t.Fatalf("seed a committed conversation: (%v, %v)", occupied, err)
		}

		taskID := sc.NewTask(t)
		br := sc.Firing(t, taskID)
		br.ID = uuid.New().String()
		doomed := sc.Step(br, 0)
		doomed.ID = occupied.ID
		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{AgentID: sc.AgentID}, "", doomed); err == nil {
			t.Fatal("the doomed firing was expected to fail")
		}

		// Same (triggering_event_id, trigger_id), fresh run id: the replay.
		retry := br
		retry.ID = uuid.New().String()
		inserted, _, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, retry, db.AgentClaimStamp{AgentID: sc.AgentID}, "", sc.Step(retry, 0))
		if err != nil {
			t.Fatalf("replay after a rolled-back firing: %v", err)
		}
		if !inserted || conv == nil {
			t.Fatalf("(inserted=%v, conv=%v), want (true, non-nil) — the rollback left a fence row behind", inserted, conv != nil)
		}
		if n := sc.RunCount(t, taskID); n != 1 {
			t.Errorf("blueprint_runs on the task = %d, want 1", n)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1", n)
		}
	})

	t.Run("Replay_after_a_committed_firing_commits_nothing", func(t *testing.T) {
		// "Nothing" is every write the firing makes, the owner included. The
		// replay below deliberately asks for a DIFFERENT owner than the
		// original firing settled — team-routing config can change between an
		// event and its redelivery — and must be refused the move: the run it
		// would have consolidated for already exists and belongs to the team
		// that fired it.
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br := sc.Firing(t, taskID)
		br.ID = uuid.New().String()
		if inserted, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{AgentID: sc.AgentID}, "", sc.Step(br, 0)); err != nil || !inserted {
			t.Fatalf("first fire: inserted=%v err=%v", inserted, err)
		}
		ownerAfterFirstFire := sc.TaskOwnerTeam(t, taskID)

		replay := br
		replay.ID = uuid.New().String()
		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, replay, db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, sc.Step(replay, 0))
		if err != nil {
			t.Fatalf("replay fire: %v", err)
		}
		if inserted || claimed || conv != nil {
			t.Errorf("replay = (inserted=%v, claimed=%v, conv=%v), want (false, false, nil)", inserted, claimed, conv != nil)
		}
		if n := sc.RunCount(t, taskID); n != 1 {
			t.Errorf("blueprint_runs on the task = %d, want 1 (the replay minted a second)", n)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1 (the replay minted a second step)", n)
		}
		if got := sc.TaskOwnerTeam(t, taskID); got != ownerAfterFirstFire {
			t.Errorf("task owner team = %q, want %q — a fenced replay reassigned the card it reported not firing", got, ownerAfterFirstFire)
		}
	})

	t.Run("A_refused_claim_still_commits_the_run_and_its_step", func(t *testing.T) {
		// The claim race has a winner either way; the firing must still stand,
		// and the owner it consolidated with it.
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		sc.ClaimTaskForUser(t, taskID)
		br := sc.Firing(t, taskID)

		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, sc.Step(br, 0))
		if err != nil {
			t.Fatalf("CreateRunWithFirstStepSystem against a user-claimed task: %v", err)
		}
		if !inserted || conv == nil {
			t.Fatalf("(inserted=%v, conv=%v), want (true, non-nil) — a refused stamp rolled the firing back", inserted, conv != nil)
		}
		if claimed {
			t.Error("claimed=true on a user-claimed task — the stamp stole the claim")
		}
		if got := sc.TaskAgentClaim(t, taskID); got != "" {
			t.Errorf("claimed_by_agent_id = %q, want empty — the user's claim was overwritten", got)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1", n)
		}
		if got := sc.TaskOwnerTeam(t, taskID); got != sc.ConsolidateTeamID {
			t.Errorf("task owner team = %q, want %q — consolidation rides the firing, not the stamp", got, sc.ConsolidateTeamID)
		}
		if sc.ConversationTeam != nil {
			if got := sc.ConversationTeam(t, conv.ID); got != sc.ConsolidateTeamID {
				t.Errorf("conversation team_id = %q, want %q", got, sc.ConsolidateTeamID)
			}
		}
	})

	t.Run("Manual_arm_commits_run_and_step_with_no_fence", func(t *testing.T) {
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br := sc.ManualFiring(t, taskID)
		br.ID = uuid.New().String()

		inserted, _, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{}, "", sc.Step(br, 0))
		if err != nil {
			t.Fatalf("manual firing: %v", err)
		}
		if !inserted || conv == nil {
			t.Fatalf("(inserted=%v, conv=%v), want (true, non-nil)", inserted, conv != nil)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1", n)
		}
		// An empty ownerTeamID still runs the consolidation statement (it is
		// the firing's lock order), so this is the assertion that it really is
		// a write of the stored value back to itself.
		if got := sc.TaskOwnerTeam(t, taskID); got == "" {
			t.Error("task owner team was cleared by a firing that asked for no consolidation")
		}

		// A second manual firing on the SAME task is refused by the
		// one-active-run index, not silently fenced away as a replay: the
		// intent is a deferral the caller must handle, not a satisfied one.
		second := sc.ManualFiring(t, taskID)
		second.ID = uuid.New().String()
		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, second, db.AgentClaimStamp{}, "", sc.Step(second, 0)); err == nil {
			t.Error("a second manual firing on a busy task should refuse")
		} else if !isTaskBusy(err) {
			t.Errorf("second manual firing = %v, want db.ErrTaskBusyActiveRun", err)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1 (the refused firing minted a step)", n)
		}
	})

	t.Run("Manual_arm_rolls_back_a_failing_step_insert", func(t *testing.T) {
		store, sc := mk(t)
		taken := sc.NewTask(t)
		takenBr := sc.ManualFiring(t, taken)
		takenBr.ID = uuid.New().String()
		_, _, occupied, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, takenBr, db.AgentClaimStamp{}, "", sc.Step(takenBr, 0))
		if err != nil || occupied == nil {
			t.Fatalf("seed a committed conversation: (%v, %v)", occupied, err)
		}

		taskID := sc.NewTask(t)
		ownerBefore := sc.TaskOwnerTeam(t, taskID)
		br := sc.ManualFiring(t, taskID)
		br.ID = uuid.New().String()
		step := sc.Step(br, 0)
		step.ID = occupied.ID
		// Everything the event arm's rollback asserts, on the manual arm too:
		// the two arms share one transaction body, and a regression that let a
		// claim or a consolidation outlive a rolled-back step would otherwise
		// only be caught on one of them.
		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, step); err == nil {
			t.Fatal("a duplicate conversation id must fail the manual firing too")
		}
		if n := sc.RunCount(t, taskID); n != 0 {
			t.Errorf("blueprint_runs on the task = %d, want 0 — the manual arm committed a run without its step", n)
		}
		if n := sc.ConversationCount(t, taskID); n != 0 {
			t.Errorf("conversations on the task = %d, want 0", n)
		}
		if got := sc.TaskAgentClaim(t, taskID); got != "" {
			t.Errorf("claimed_by_agent_id = %q, want empty — the claim outlived a rolled-back firing", got)
		}
		if got := sc.TaskOwnerTeam(t, taskID); got != ownerBefore {
			t.Errorf("task owner team = %q, want %q — the consolidation outlived a rolled-back firing", got, ownerBefore)
		}
	})

	t.Run("A_firing_against_a_task_that_does_not_exist_is_refused", func(t *testing.T) {
		// The first statement's job, and the only one that runs: a firing
		// naming no task writes nothing and says why, rather than failing
		// later on a foreign key with the run insert's error.
		store, sc := mk(t)
		real := sc.NewTask(t)
		br := sc.Firing(t, real)
		br.ID = uuid.New().String()
		br.TaskID = "00000000-0000-0000-0000-0000000000ba"
		step := sc.Step(br, 0)
		step.TaskID = br.TaskID

		_, _, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, step)
		if !errors.Is(err, db.ErrNoSuchTask) {
			t.Fatalf("CreateRunWithFirstStepSystem against a missing task = %v, want db.ErrNoSuchTask", err)
		}
		if conv != nil {
			t.Error("a refused firing returned a conversation")
		}
		if n := sc.RunCount(t, real); n != 0 {
			t.Errorf("blueprint_runs on the real task = %d, want 0", n)
		}
	})

	t.Run("A_failing_owner_consolidation_aborts_the_firing", func(t *testing.T) {
		store, sc := mk(t)
		if sc.BadTeamID == "" {
			t.Skip("backend has no refusable team id: tasks.team_id carries no foreign key here")
		}
		taskID := sc.NewTask(t)
		ownerBefore := sc.TaskOwnerTeam(t, taskID)
		br := sc.Firing(t, taskID)

		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{AgentID: sc.AgentID}, sc.BadTeamID, sc.Step(br, 0)); err == nil {
			t.Fatal("a refused consolidation must fail the firing, not proceed under the old owner")
		}
		if n := sc.RunCount(t, taskID); n != 0 {
			t.Errorf("blueprint_runs on the task = %d, want 0", n)
		}
		if n := sc.ConversationCount(t, taskID); n != 0 {
			t.Errorf("conversations on the task = %d, want 0", n)
		}
		if got := sc.TaskOwnerTeam(t, taskID); got != ownerBefore {
			t.Errorf("task owner team = %q, want %q (unchanged)", got, ownerBefore)
		}
	})

	// seedFiring stages a committed run and hands back its step-0 row — the
	// starting position for every advance below.
	seedFiring := func(t *testing.T, store db.BlueprintStore, sc BlueprintSequenceScaffold, taskID string) (domain.BlueprintRun, *domain.Conversation) {
		t.Helper()
		br := sc.Firing(t, taskID)
		_, _, step0, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{}, "", sc.Step(br, 0))
		if err != nil || step0 == nil {
			t.Fatalf("seed a committed firing: (%v, %v)", step0, err)
		}
		return br, step0
	}

	t.Run("Advance_commits_the_pointer_the_boundary_and_the_next_step", func(t *testing.T) {
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br, step0 := seedFiring(t, store, sc, taskID)

		advanced, ended, next, err := store.AdvanceRunToStepSystem(ctx, sc.OrgID, 0, step0.ID, sc.Step(br, 1))
		if err != nil {
			t.Fatalf("AdvanceRunToStepSystem: %v", err)
		}
		if !advanced || ended == nil || next == nil {
			t.Fatalf("(advanced=%v, ended=%v, conv=%v), want (true, non-nil, non-nil)", advanced, ended != nil, next != nil)
		}
		if got := sc.RunCurrentStep(t, br.ID); got != 1 {
			t.Errorf("current_step_index = %d, want 1", got)
		}
		if n := sc.ConversationCount(t, taskID); n != 2 {
			t.Errorf("conversations on the task = %d, want 2 — a pointer with no conversation at it is a run nothing can drive", n)
		}
		if !sc.ConversationEnded(t, step0.ID) {
			t.Error("the concluded step carries no ended_at — it stops being the task's live conversation the moment its successor is minted")
		}
		AssertWriteReturnedStoredRow(t, "AdvanceRunToStepSystem", *next,
			func() (*domain.Conversation, error) { return sc.GetConversation(t, next.ID) })
	})

	t.Run("A_failing_step_insert_rolls_the_advance_back", func(t *testing.T) {
		// The same collision the firing arm uses, one door over: the last
		// statement fails, so the pointer and the boundary go with it. A
		// pointer that survived would name a step nothing ever minted, which
		// is the shape the whole transaction exists to prevent.
		store, sc := mk(t)
		taken := sc.NewTask(t)
		_, occupied := seedFiring(t, store, sc, taken)

		taskID := sc.NewTask(t)
		br, step0 := seedFiring(t, store, sc, taskID)
		doomed := sc.Step(br, 1)
		doomed.ID = occupied.ID // primary-key collision

		advanced, _, _, err := store.AdvanceRunToStepSystem(ctx, sc.OrgID, 0, step0.ID, doomed)
		if err == nil {
			t.Fatalf("a duplicate conversation id must fail the advance, got advanced=%v", advanced)
		}
		if got := sc.RunCurrentStep(t, br.ID); got != 0 {
			t.Errorf("current_step_index = %d, want 0 — the pointer moved to a step that was never minted", got)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1", n)
		}
		if sc.ConversationEnded(t, step0.ID) {
			t.Error("the concluded step was ended under an advance that did not commit")
		}
	})

	t.Run("A_stale_pointer_advances_nothing", func(t *testing.T) {
		// A terminal arriving from a step the blueprint has already moved
		// past. The compare-and-swap on current_step_index is what refuses
		// it, so the second advance from step 0 mints no second successor and
		// writes no second boundary.
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br, step0 := seedFiring(t, store, sc, taskID)
		if _, _, _, err := store.AdvanceRunToStepSystem(ctx, sc.OrgID, 0, step0.ID, sc.Step(br, 1)); err != nil {
			t.Fatalf("first advance: %v", err)
		}

		advanced, ended, next, err := store.AdvanceRunToStepSystem(ctx, sc.OrgID, 0, step0.ID, sc.Step(br, 1))
		if err != nil {
			t.Fatalf("a stale advance is a refusal, not an error: %v", err)
		}
		if advanced || ended != nil || next != nil {
			t.Fatalf("(advanced=%v, ended=%v, conv=%v), want (false, nil, nil)", advanced, ended != nil, next != nil)
		}
		if got := sc.RunCurrentStep(t, br.ID); got != 1 {
			t.Errorf("current_step_index = %d, want 1 — the stale advance moved the sequence backwards", got)
		}
		if n := sc.ConversationCount(t, taskID); n != 2 {
			t.Errorf("conversations on the task = %d, want 2 — the stale advance minted a duplicate step", n)
		}
	})

	t.Run("A_terminal_run_advances_nothing", func(t *testing.T) {
		// The reactor refreshes the run before it decides, but a cancel can
		// land between that read and this write. The guard carries the status
		// as well as the pointer, so the advance is refused rather than
		// enqueuing a step under a blueprint that has already finished.
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br, step0 := seedFiring(t, store, sc, taskID)
		changed, err := store.MarkRunStatus(ctx, sc.OrgID, br.ID, domain.BlueprintRunStatusCancelled, "cancelled", nil)
		if err != nil || !changed {
			t.Fatalf("terminate the run: (changed=%v, %v)", changed, err)
		}

		advanced, _, _, err := store.AdvanceRunToStepSystem(ctx, sc.OrgID, 0, step0.ID, sc.Step(br, 1))
		if err != nil {
			t.Fatalf("advancing a terminal run is a refusal, not an error: %v", err)
		}
		if advanced {
			t.Error("advanced a run that is no longer running")
		}
		if got := sc.RunCurrentStep(t, br.ID); got != 0 {
			t.Errorf("current_step_index = %d, want 0", got)
		}
		if n := sc.ConversationCount(t, taskID); n != 1 {
			t.Errorf("conversations on the task = %d, want 1 — a step was minted under a finished blueprint", n)
		}
	})
}
