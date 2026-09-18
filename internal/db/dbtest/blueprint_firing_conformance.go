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

// BlueprintFiringFactory is what a per-backend test file hands to
// RunBlueprintFiringConformance: the wired BlueprintStore plus a scaffold that
// stages firings and reads back what they committed. Each call must hand back a
// clean database — the suite counts rows per task and asserts exact totals.
type BlueprintFiringFactory func(t *testing.T) (db.BlueprintStore, BlueprintFiringScaffold)

// BlueprintFiringScaffold stages one backend's firings and reads their
// aftermath. Every write below is a real store call or the backend's own SQL;
// nothing here reimplements the method under test.
type BlueprintFiringScaffold struct {
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
	// FirstStep composes the step-0 conversation a firing commits with its run.
	FirstStep func(br domain.BlueprintRun) domain.Conversation
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
	// ConversationTeam reads a conversation's team_id. Nil on a backend whose
	// conversations do not derive their team from the task (local mode's
	// single-team sentinel), where the assertion would test nothing.
	ConversationTeam func(t *testing.T, convID string) string
}

// RunBlueprintFiringConformance is the shared suite for the one door a
// delegation is fired through.
//
// Its subject is indivisibility. Four writes — the task's owner, the
// blueprint_run, the task's agent claim, the first step's conversation — are
// each implied by the one before it, and the shapes a partial commit leaves
// behind are all silent: a run with no step is a 'running' parent nothing
// drives and no recovery arm can see, and a firing under a stale owner opens
// its conversation on the wrong team's board. So the suite fires, and then
// fires things that fail, and asserts the database only ever holds all of it
// or none of it.
func RunBlueprintFiringConformance(t *testing.T, mk BlueprintFiringFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Commits_the_owner_the_run_the_claim_and_the_step", func(t *testing.T) {
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br := sc.Firing(t, taskID)

		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, sc.FirstStep(br))
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
	})

	t.Run("A_failing_step_insert_rolls_the_whole_firing_back", func(t *testing.T) {
		// The last statement in the transaction, made to fail by pointing the
		// step at a conversation id that already exists. Everything committed
		// before it in the same transaction must go with it.
		store, sc := mk(t)
		taken := sc.NewTask(t)
		takenBr := sc.Firing(t, taken)
		_, _, occupied, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, takenBr, db.AgentClaimStamp{}, "", sc.FirstStep(takenBr))
		if err != nil || occupied == nil {
			t.Fatalf("seed a committed conversation: (%v, %v)", occupied, err)
		}

		taskID := sc.NewTask(t)
		ownerBefore := sc.TaskOwnerTeam(t, taskID)
		br := sc.Firing(t, taskID)
		step := sc.FirstStep(br)
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
		_, _, occupied, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, takenBr, db.AgentClaimStamp{}, "", sc.FirstStep(takenBr))
		if err != nil || occupied == nil {
			t.Fatalf("seed a committed conversation: (%v, %v)", occupied, err)
		}

		taskID := sc.NewTask(t)
		br := sc.Firing(t, taskID)
		br.ID = uuid.New().String()
		doomed := sc.FirstStep(br)
		doomed.ID = occupied.ID
		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{AgentID: sc.AgentID}, "", doomed); err == nil {
			t.Fatal("the doomed firing was expected to fail")
		}

		// Same (triggering_event_id, trigger_id), fresh run id: the replay.
		retry := br
		retry.ID = uuid.New().String()
		inserted, _, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, retry, db.AgentClaimStamp{AgentID: sc.AgentID}, "", sc.FirstStep(retry))
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
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		br := sc.Firing(t, taskID)
		br.ID = uuid.New().String()
		if inserted, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{AgentID: sc.AgentID}, "", sc.FirstStep(br)); err != nil || !inserted {
			t.Fatalf("first fire: inserted=%v err=%v", inserted, err)
		}

		replay := br
		replay.ID = uuid.New().String()
		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, replay, db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, sc.FirstStep(replay))
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
	})

	t.Run("A_refused_claim_still_commits_the_run_and_its_step", func(t *testing.T) {
		// The claim race has a winner either way; the firing must still stand,
		// and the owner it consolidated with it.
		store, sc := mk(t)
		taskID := sc.NewTask(t)
		sc.ClaimTaskForUser(t, taskID)
		br := sc.Firing(t, taskID)

		inserted, claimed, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br,
			db.AgentClaimStamp{AgentID: sc.AgentID}, sc.ConsolidateTeamID, sc.FirstStep(br))
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

		inserted, _, conv, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{}, "", sc.FirstStep(br))
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
		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, second, db.AgentClaimStamp{}, "", sc.FirstStep(second)); err == nil {
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
		_, _, occupied, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, takenBr, db.AgentClaimStamp{}, "", sc.FirstStep(takenBr))
		if err != nil || occupied == nil {
			t.Fatalf("seed a committed conversation: (%v, %v)", occupied, err)
		}

		taskID := sc.NewTask(t)
		br := sc.ManualFiring(t, taskID)
		br.ID = uuid.New().String()
		step := sc.FirstStep(br)
		step.ID = occupied.ID
		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{}, "", step); err == nil {
			t.Fatal("a duplicate conversation id must fail the manual firing too")
		}
		if n := sc.RunCount(t, taskID); n != 0 {
			t.Errorf("blueprint_runs on the task = %d, want 0 — the manual arm committed a run without its step", n)
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

		if _, _, _, err := store.CreateRunWithFirstStepSystem(ctx, sc.OrgID, br, db.AgentClaimStamp{AgentID: sc.AgentID}, sc.BadTeamID, sc.FirstStep(br)); err == nil {
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
}
