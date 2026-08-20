package dbtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskStoreFactory is what a per-backend test file hands to
// RunTaskStoreConformance. The factory returns:
//   - the wired TaskStore impl
//   - the orgID to pass to every method (sqlite returns
//     runmode.LocalDefaultOrgID, postgres returns a fresh org UUID)
//   - the teamID — caller-supplied team_id for FindOrCreate.
//     SQLite returns runmode.LocalDefaultTeamID; Postgres
//     returns the seeded default team's UUID.
//   - the agentID + userID the backend test has seeded — claim
//     transitions need real FK-resolvable ids (auth.users / agents
//     rows already in place for the harness's chosen org)
//   - a TaskSeeder that creates the underlying entity + event +
//     task row the conformance asserts against. The seeder owns
//     backend-specific schema knowledge (sqlite has no org_id
//     column, postgres demands a creator_user_id, etc.) so the
//     harness stays schema-blind.
type TaskStoreFactory func(t *testing.T) (
	store db.TaskStore,
	orgID, teamID, agentID, userID string,
	seed TaskSeeder,
	seedTeam TeamSeeder,
)

// TaskSeeder produces a fresh (entity, event, task) chain for one
// assertion. Returns:
//   - entityID + eventID — usable as predicate inputs and re-use
//     across FindOrCreate dedup tests
//   - taskID — the pre-existing queued task the harness will mutate
//
// Each call must produce a distinct entity (so dedup index doesn't
// collapse independent assertions).
type TaskSeeder func(t *testing.T, suffix string) (entityID, eventID, taskID string)

// TeamSeeder creates a secondary team inside the harness's org and
// returns its ID, so the per-team dedup conformance subtest can
// exercise the per-team dedup fanout. Local-mode (SQLite) seeds a real
// teams row alongside the LocalDefaultTeamID baseline; Postgres
// seeds a fresh team UUID in the same org as the factory's primary
// teamID. The harness only calls this in the multi-team subtests
// so backends with stricter team-FK requirements (Postgres'
// memberships graph) can stay simple in the single-team path.
type TeamSeeder func(t *testing.T, suffix string) (teamID string)

// RunTaskStoreConformance is the shared assertion suite for any
// db.TaskStore implementation. Backend tests invoke it with their
// factory; both backends run the same subtests.
func RunTaskStoreConformance(t *testing.T, mk TaskStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Get_returns_nil_for_missing_id", func(t *testing.T) {
		s, orgID, _, _, _, _, _ := mk(t)
		task, err := s.Get(ctx, orgID, "00000000-0000-0000-0000-000000000bad")
		if err != nil {
			t.Fatalf("Get on missing id: %v", err)
		}
		if task != nil {
			t.Errorf("expected nil task for missing id, got %+v", task)
		}
	})

	t.Run("Get_returns_seeded_task_with_entity_join", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "get-happy")
		task, err := s.Get(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if task == nil {
			t.Fatal("Get returned nil for seeded task")
		}
		if task.Title == "" {
			t.Error("entity JOIN didn't populate Title")
		}
		if task.EntitySource == "" {
			t.Error("entity JOIN didn't populate EntitySource")
		}
	})

	t.Run("List_queue_projection_returns_unclaimed_queued_tasks", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		seed(t, "q1")
		seed(t, "q2")
		out, total, err := s.List(ctx, orgID, queueFilter(), db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(out) < 2 {
			t.Errorf("queue projection returned %d, want >= 2", len(out))
		}
		if total != len(out) {
			t.Errorf("total = %d, want %d (the page holds every match)", total, len(out))
		}
		for _, task := range out {
			if task.Status != "queued" {
				t.Errorf("task %s status=%q, want queued", task.ID, task.Status)
			}
			if task.ClaimedByAgentID != "" || task.ClaimedByUserID != "" {
				t.Errorf("task %s shouldn't appear in the queue projection (has claim)", task.ID)
			}
		}

		coRows, coTotal, err := s.List(ctx, orgID, queueFilter(), db.ListOpts{CountOnly: true})
		if err != nil {
			t.Fatalf("List count-only: %v", err)
		}
		AssertCountOnlyList(t, "tasks.List", len(coRows), coTotal, total)
	})

	t.Run("List_done_excludes_active", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "bs-done")
		doneFilter := db.TaskListFilter{Statuses: []string{"done"}, IncludeSnoozed: true}
		// Active task should not appear in the done lane.
		done, _, err := s.List(ctx, orgID, doneFilter, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List done: %v", err)
		}
		for _, task := range done {
			if task.ID == taskID {
				t.Errorf("active task %s appeared under the done filter", taskID)
			}
		}
		// Now close it; should appear under done.
		if _, err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		done, total, err := s.List(ctx, orgID, doneFilter, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List done (post-close): %v", err)
		}
		if total != len(done) {
			t.Errorf("total = %d, want %d", total, len(done))
		}
		found := false
		for _, task := range done {
			if task.ID == taskID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("closed task %s missing from the done filter", taskID)
		}
	})

	// The derived "claimed" member of the list vocabulary is scoped to
	// status='queued' so the Board's Claimed column doesn't double-render a
	// user-claimed task that's also in In Progress or In Review. Distinct
	// from the broader "any non-terminal user-claimed task" set.
	t.Run("List_claimed_excludes_in_progress_and_in_review", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, queuedID := seed(t, "bs-claimed-q")
		_, _, ipID := seed(t, "bs-claimed-ip")
		_, _, irID := seed(t, "bs-claimed-ir")

		// Claim all three.
		for _, id := range []string{queuedID, ipID, irID} {
			if ok, err := s.ClaimQueuedForUser(ctx, orgID, id, userID); err != nil || !ok {
				t.Fatalf("claim %s: ok=%v err=%v", id, ok, err)
			}
		}
		// Advance two of them.
		if ok, err := s.AdvanceStatusForUser(ctx, orgID, ipID, userID, "in_progress"); err != nil || !ok {
			t.Fatalf("advance ip: ok=%v err=%v", ok, err)
		}
		if ok, err := s.AdvanceStatusForUser(ctx, orgID, irID, userID, "in_review"); err != nil || !ok {
			t.Fatalf("advance ir: ok=%v err=%v", ok, err)
		}

		claimed, _, err := s.List(ctx, orgID, claimedFilter(), db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List claimed: %v", err)
		}
		seen := map[string]bool{}
		for _, x := range claimed {
			seen[x.ID] = true
		}
		if !seen[queuedID] {
			t.Errorf("Claimed projection missing the queued+claim task %s", queuedID)
		}
		if seen[ipID] {
			t.Errorf("Claimed projection contained an in_progress task %s; would double-render with In Progress column", ipID)
		}
		if seen[irID] {
			t.Errorf("Claimed projection contained an in_review task %s; would double-render with In Review column", irID)
		}
	})

	// Bot-claimed status='queued' tasks (just-delegated, conversation
	// not yet advanced) also belong in the Claimed projection so the
	// board's Claimed column surfaces them and the delegate-spawn-
	// failure retry UI can render. Without this they'd disappear
	// between the delegate stamp and the first non-initializing
	// conversation-status transition.
	t.Run("List_claimed_includes_bot_claimed_queued", func(t *testing.T) {
		s, orgID, _, agentID, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "bs-claimed-bot")
		if _, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, ""); err != nil {
			t.Fatalf("stamp agent: %v", err)
		}

		claimed, _, err := s.List(ctx, orgID, claimedFilter(), db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List claimed: %v", err)
		}
		var seen bool
		for _, x := range claimed {
			if x.ID == taskID {
				seen = true
				break
			}
		}
		if !seen {
			t.Errorf("Claimed projection missing bot-claimed queued task %s; delegate-failure retry UI would have nothing to render against", taskID)
		}
	})

	t.Run("FindOrCreate_idempotent_on_dedup_key", func(t *testing.T) {
		s, orgID, teamID, _, _, seed, _ := mk(t)
		entityID, eventID, _ := seed(t, "foc-dedup")
		// Re-call with the seed's eventType+dedupKey would collide on
		// the existing seeded task. Use a different eventType so the
		// first FindOrCreate creates a new row, and a second call with
		// the same args returns it idempotently.
		task1, created1, err := s.FindOrCreate(ctx, orgID, teamID, entityID, domain.EventGitHubPRCICheckPassed, "dedup-x", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate first call: %v", err)
		}
		if !created1 {
			t.Error("first FindOrCreate should return created=true")
		}
		task2, created2, err := s.FindOrCreate(ctx, orgID, teamID, entityID, domain.EventGitHubPRCICheckPassed, "dedup-x", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate second call: %v", err)
		}
		if created2 {
			t.Error("second FindOrCreate should return created=false (dedup)")
		}
		if task1.ID != task2.ID {
			t.Errorf("second call should return same task id; got %q vs %q", task1.ID, task2.ID)
		}
	})

	// --- One task per situation; team is visibility, not count ---

	t.Run("FindOrCreate_single_team_one_task", func(t *testing.T) {
		// Regression baseline: in a single-team scenario the dedup
		// index collapses repeat calls to one task.
		s, orgID, teamID, _, _, seed, _ := mk(t)
		entityID, eventID, _ := seed(t, "single-team")
		task, created, err := s.FindOrCreate(ctx, orgID, teamID, entityID, domain.EventGitHubPRCICheckPassed, "build", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if !created {
			t.Fatal("expected created=true on first call")
		}
		if task.ID == "" {
			t.Fatal("FindOrCreate returned task with empty ID")
		}
	})

	t.Run("FindOrCreate_cross_team_collapses_to_one", func(t *testing.T) {
		// Same (entity, event_type, dedup_key) in two teams → ONE
		// task. Identity dropped team_id: a situation already tasked
		// by another team's rule is returned (created=false), not
		// duplicated. The second team is recorded via the visibility
		// set instead (see SetVisibilityTeams).
		s, orgID, teamA, _, _, seed, seedTeam := mk(t)
		if seedTeam == nil {
			t.Skip("backend factory did not provide a TeamSeeder; multi-team test skipped")
		}
		teamB := seedTeam(t, "collapse")
		entityID, eventID, _ := seed(t, "cross-team-collapse")

		taskA, createdA, err := s.FindOrCreate(ctx, orgID, teamA, entityID, domain.EventGitHubPRCICheckPassed, "build", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate(teamA): %v", err)
		}
		if !createdA {
			t.Error("teamA FindOrCreate should create the task")
		}

		taskB, createdB, err := s.FindOrCreate(ctx, orgID, teamB, entityID, domain.EventGitHubPRCICheckPassed, "build", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate(teamB): %v", err)
		}
		if createdB {
			t.Error("teamB FindOrCreate should find the existing task (created=false), not create a second")
		}
		if taskA.ID != taskB.ID {
			t.Errorf("expected the SAME task id across teams; got %q vs %q", taskA.ID, taskB.ID)
		}
	})

	t.Run("FindOrCreate_same_team_dedup_collapses", func(t *testing.T) {
		// Within one team, repeat calls on the same key collapse to
		// one task — the basic dedup invariant.
		s, orgID, teamID, _, _, seed, _ := mk(t)
		entityID, eventID, _ := seed(t, "same-team-dedup")

		task1, created1, err := s.FindOrCreate(ctx, orgID, teamID, entityID, domain.EventGitHubPRCICheckPassed, "build", eventID, 0.5)
		if err != nil {
			t.Fatalf("first FindOrCreate: %v", err)
		}
		if !created1 {
			t.Error("first call should create")
		}
		task2, created2, err := s.FindOrCreate(ctx, orgID, teamID, entityID, domain.EventGitHubPRCICheckPassed, "build", eventID, 0.5)
		if err != nil {
			t.Fatalf("second FindOrCreate: %v", err)
		}
		if created2 {
			t.Error("second call within same team should dedup (created=false)")
		}
		if task1.ID != task2.ID {
			t.Errorf("same-team second call returned different ID %q (want %q)", task2.ID, task1.ID)
		}
	})

	t.Run("SetVisibilityTeams_records_and_reads_back", func(t *testing.T) {
		s, orgID, teamA, _, _, seed, seedTeam := mk(t)
		if seedTeam == nil {
			t.Skip("backend factory did not provide a TeamSeeder; multi-team test skipped")
		}
		teamB := seedTeam(t, "vis")
		entityID, eventID, _ := seed(t, "vis-set")
		task, _, err := s.FindOrCreate(ctx, orgID, teamA, entityID, domain.EventGitHubPRCICheckPassed, "vis", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		if err := s.SetVisibilityTeams(ctx, orgID, task.ID, []string{teamA, teamB}); err != nil {
			t.Fatalf("SetVisibilityTeams: %v", err)
		}
		// Idempotent: re-applying the same set must not error or
		// duplicate (composite PK + INSERT OR IGNORE / ON CONFLICT).
		if err := s.SetVisibilityTeams(ctx, orgID, task.ID, []string{teamA, teamB}); err != nil {
			t.Fatalf("SetVisibilityTeams (repeat): %v", err)
		}
		vis, err := s.VisibilityTeams(ctx, orgID, task.ID)
		if err != nil {
			t.Fatalf("VisibilityTeams: %v", err)
		}
		got := map[string]bool{}
		for _, v := range vis {
			got[v] = true
		}
		if len(vis) != 2 || !got[teamA] || !got[teamB] {
			t.Errorf("visibility set = %v, want both %q and %q", vis, teamA, teamB)
		}
	})

	t.Run("Bump_wakes_snoozed_task", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, eventID, taskID := seed(t, "bump-wake")
		// Force task into snoozed via SetStatus (sufficient for this
		// invariant — claim cols stay empty, satisfying the
		// "snoozed ↔ unclaimed" invariant trivially).
		if _, err := s.SetStatus(ctx, orgID, taskID, "snoozed"); err != nil {
			t.Fatalf("SetStatus snoozed: %v", err)
		}
		if _, err := s.Bump(ctx, orgID, taskID, eventID); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		got, err := s.Get(ctx, orgID, taskID)
		if err != nil || got == nil {
			t.Fatalf("Get post-bump: task=%v err=%v", got, err)
		}
		if got.Status != "queued" {
			t.Errorf("status=%q post-bump, want queued (snooze should clear)", got.Status)
		}
	})

	t.Run("Close_terminates_task", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "close")
		if _, err := s.Close(ctx, orgID, taskID, "test_close", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.Status != "done" {
			t.Errorf("status=%q post-close, want done", got.Status)
		}
		if got.ClosedAt == nil {
			t.Error("closed_at not set")
		}
	})

	t.Run("FindActiveByEntity_excludes_terminal", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		entityID, _, taskID := seed(t, "fab")
		active, err := s.FindActiveByEntity(ctx, orgID, entityID)
		if err != nil {
			t.Fatalf("FindActiveByEntity: %v", err)
		}
		if len(active) != 1 || active[0].ID != taskID {
			t.Fatalf("active list = %+v, want [%s]", active, taskID)
		}
		_, _ = s.Close(ctx, orgID, taskID, "test", "")
		active, _ = s.FindActiveByEntity(ctx, orgID, entityID)
		if len(active) != 0 {
			t.Errorf("active list post-close = %d rows, want 0", len(active))
		}
	})

	t.Run("ListActiveRefsForEntities_empty_input", func(t *testing.T) {
		s, orgID, _, _, _, _, _ := mk(t)
		refs, err := s.ListActiveRefsForEntities(ctx, orgID, nil, nil)
		if err != nil {
			t.Fatalf("ListActiveRefsForEntities(nil): %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %d refs, want 0 on empty input", len(refs))
		}
	})

	t.Run("ListActiveRefsForEntities_filters_terminal", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		entityA, _, taskA := seed(t, "ref-a")
		entityB, _, taskB := seed(t, "ref-b")
		// Close B; only A should remain.
		if _, err := s.Close(ctx, orgID, taskB, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		refs, err := s.ListActiveRefsForEntities(ctx, orgID, []string{entityA, entityB}, nil)
		if err != nil {
			t.Fatalf("ListActiveRefsForEntities: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("got %d refs, want 1 (only A active)", len(refs))
		}
		if refs[0].ID != taskA {
			t.Errorf("ref ID = %s, want %s", refs[0].ID, taskA)
		}
	})

	t.Run("EntityIDsWithActiveTasks_filters_by_source", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		entityID, _, _ := seed(t, "eida")
		ids, err := s.EntityIDsWithActiveTasks(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("EntityIDsWithActiveTasks: %v", err)
		}
		if _, ok := ids[entityID]; !ok {
			t.Errorf("seeded github entity %s missing from result", entityID)
		}
		// Wrong-source query: should not include the github entity.
		ids, err = s.EntityIDsWithActiveTasks(ctx, orgID, "jira")
		if err != nil {
			t.Fatalf("EntityIDsWithActiveTasks(jira): %v", err)
		}
		if _, ok := ids[entityID]; ok {
			t.Errorf("github entity %s leaked into jira-source query", entityID)
		}
	})

	// --- Claim invariants ---

	t.Run("ClaimQueuedForUser_lands_then_refuses_steal", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "cqfu")
		ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if !ok {
			t.Fatal("first claim returned ok=false on unclaimed task")
		}
		// Second claim attempt — even by the same user — should now be
		// refused because the task is no longer unclaimed.
		ok, err = s.ClaimQueuedForUser(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if ok {
			t.Error("second claim returned ok=true on already-claimed task; guard broken")
		}
		// Verify the original claim survived.
		got, _ := s.Get(ctx, orgID, taskID)
		if got.ClaimedByUserID != userID {
			t.Errorf("user claim was overwritten: got %q want %q", got.ClaimedByUserID, userID)
		}
	})

	t.Run("ClaimQueuedForUser_rejects_terminal_task", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "cqfu-term")
		if _, err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("claim on terminal: %v", err)
		}
		if ok {
			t.Error("ok=true claiming a closed task; status guard broken")
		}
	})

	t.Run("StampAgentClaimIfUnclaimed_lands_then_skips_same_agent", func(t *testing.T) {
		s, orgID, _, agentID, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "stamp")
		ok, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, "")
		if err != nil {
			t.Fatalf("first stamp: %v", err)
		}
		if !ok {
			t.Fatal("first stamp returned ok=false")
		}
		// Same agent again — should no-op (ok=false).
		ok, err = s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, "")
		if err != nil {
			t.Fatalf("second stamp: %v", err)
		}
		if ok {
			t.Error("second stamp returned ok=true; same-agent no-op guard broken")
		}
	})

	t.Run("StampAgentClaimIfUnclaimed_refuses_terminal", func(t *testing.T) {
		s, orgID, _, agentID, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "stamp-term")
		if _, err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, "")
		if err != nil {
			t.Fatalf("StampAgentClaimIfUnclaimed on terminal: %v", err)
		}
		if ok {
			t.Error("ok=true stamping a terminal task; status guard broken")
		}
	})

	// Folding an event into a live conversation is the bot committing to the
	// task, so the 'injected' mark and the claim are one durable step: either
	// both land or neither does, and the board can never show the task free
	// under the conversation the event was folded into.
	t.Run("MarkEventInjectedSystem_stamps_the_claim_with_the_mark", func(t *testing.T) {
		s, orgID, _, agentID, _, seed, _ := mk(t)
		_, eventID, taskID := seed(t, "inject-claim")
		if err := s.RecordEventSystem(ctx, orgID, taskID, eventID, "bumped"); err != nil {
			t.Fatalf("seed bumped row: %v", err)
		}
		claimed, err := s.MarkEventInjectedSystem(ctx, orgID, taskID, eventID, db.AgentClaimStamp{AgentID: agentID})
		if err != nil {
			t.Fatalf("MarkEventInjectedSystem with claim: %v", err)
		}
		if !claimed {
			t.Error("claimed=false stamping an unclaimed task")
		}
		task, err := s.Get(ctx, orgID, taskID)
		if err != nil || task == nil {
			t.Fatalf("Get after mark: (%v, %v)", task, err)
		}
		if task.ClaimedByAgentID != agentID {
			t.Errorf("claimed_by_agent_id = %q, want %q — the fold landed without its claim", task.ClaimedByAgentID, agentID)
		}
	})

	t.Run("MarkEventInjectedSystem_marks_even_when_the_stamp_is_refused", func(t *testing.T) {
		s, orgID, _, agentID, userID, seed, _ := mk(t)
		_, eventID, taskID := seed(t, "inject-refused")
		if err := s.RecordEventSystem(ctx, orgID, taskID, eventID, "bumped"); err != nil {
			t.Fatalf("seed bumped row: %v", err)
		}
		// A user owns the task: the stamp must refuse rather than steal, and
		// the fold must still be recorded — the injection already happened.
		if _, err := s.SetClaimedByUser(ctx, orgID, taskID, userID); err != nil {
			t.Fatalf("SetClaimedByUser: %v", err)
		}
		claimed, err := s.MarkEventInjectedSystem(ctx, orgID, taskID, eventID, db.AgentClaimStamp{AgentID: agentID})
		if err != nil {
			t.Fatalf("MarkEventInjectedSystem against a user-claimed task: %v", err)
		}
		if claimed {
			t.Error("claimed=true on a user-claimed task — the stamp stole the claim")
		}
		task, err := s.Get(ctx, orgID, taskID)
		if err != nil || task == nil {
			t.Fatalf("Get after mark: (%v, %v)", task, err)
		}
		if task.ClaimedByAgentID != "" || task.ClaimedByUserID != userID {
			t.Errorf("claim = (agent=%q, user=%q), want the user's claim untouched", task.ClaimedByAgentID, task.ClaimedByUserID)
		}
	})

	t.Run("HandoffAgentClaim_three_outcomes", func(t *testing.T) {
		s, orgID, _, agentID, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "handoff")
		// Unclaimed → bot: HandoffChanged.
		result, err := s.HandoffAgentClaim(ctx, orgID, taskID, agentID, userID)
		if err != nil {
			t.Fatalf("first handoff: %v", err)
		}
		if result != db.HandoffChanged {
			t.Errorf("first handoff result=%v, want HandoffChanged", result)
		}
		// Same-agent already-owns → HandoffNoOp.
		result, err = s.HandoffAgentClaim(ctx, orgID, taskID, agentID, userID)
		if err != nil {
			t.Fatalf("second handoff: %v", err)
		}
		if result != db.HandoffNoOp {
			t.Errorf("second handoff result=%v, want HandoffNoOp", result)
		}
		// Terminal task — HandoffRefused regardless of sticky claim.
		if _, err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		result, err = s.HandoffAgentClaim(ctx, orgID, taskID, agentID, userID)
		if err != nil {
			t.Fatalf("post-terminal handoff: %v", err)
		}
		if result != db.HandoffRefused {
			t.Errorf("post-terminal handoff result=%v, want HandoffRefused (terminal-status precedence)", result)
		}
	})

	t.Run("TakeoverClaimFromAgent_succeeds_on_bot_claim", func(t *testing.T) {
		s, orgID, _, agentID, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "takeover")
		// Set up bot claim first.
		if _, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, ""); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		ok, err := s.TakeoverClaimFromAgent(ctx, orgID, taskID, userID)
		if err != nil {
			t.Fatalf("Takeover: %v", err)
		}
		if !ok {
			t.Fatal("Takeover returned ok=false on bot-claimed task")
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.ClaimedByAgentID != "" {
			t.Errorf("ClaimedByAgentID=%q, want empty after takeover", got.ClaimedByAgentID)
		}
		if got.ClaimedByUserID != userID {
			t.Errorf("ClaimedByUserID=%q, want %q", got.ClaimedByUserID, userID)
		}
	})

	// AdvanceStatusForUser — the manual user board transition.

	t.Run("AdvanceStatusForUser_lands_for_user_claim", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "adv-happy")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		ok, err := s.AdvanceStatusForUser(ctx, orgID, taskID, userID, "in_progress")
		if err != nil {
			t.Fatalf("AdvanceStatusForUser: %v", err)
		}
		if !ok {
			t.Fatal("AdvanceStatusForUser returned false on a valid transition")
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.Status != "in_progress" {
			t.Errorf("status=%q, want in_progress", got.Status)
		}
		// Backward transition (in_progress → in_review then back to
		// in_progress) is also allowed — the guard only checks the
		// current status is in the active set, not that newStatus is
		// strictly forward.
		if ok2, err := s.AdvanceStatusForUser(ctx, orgID, taskID, userID, "in_review"); err != nil || !ok2 {
			t.Fatalf("Advance → in_review: ok=%v err=%v", ok2, err)
		}
		if ok2, err := s.AdvanceStatusForUser(ctx, orgID, taskID, userID, "in_progress"); err != nil || !ok2 {
			t.Fatalf("Advance back → in_progress: ok=%v err=%v", ok2, err)
		}
	})

	t.Run("AdvanceStatusForUser_refuses_bot_claim", func(t *testing.T) {
		s, orgID, _, agentID, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "adv-bot")
		if _, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, ""); err != nil {
			t.Fatalf("stamp agent: %v", err)
		}
		ok, err := s.AdvanceStatusForUser(ctx, orgID, taskID, userID, "in_progress")
		if err != nil {
			t.Fatalf("AdvanceStatusForUser: %v", err)
		}
		if ok {
			t.Error("AdvanceStatusForUser landed on bot-claimed task; should refuse (status owned by the conversation lifecycle)")
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.Status != "queued" {
			t.Errorf("status=%q, want queued (refusal must not transition)", got.Status)
		}
	})

	t.Run("AdvanceStatusForUser_refuses_different_user", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "adv-other")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		// A different user's id — must not be able to advance someone
		// else's task. Use a syntactic-only mismatch so the FK-shape
		// constant the seeder uses for userID still distinguishes.
		otherUserID := "00000000-0000-0000-0000-0000000000ff"
		ok, err := s.AdvanceStatusForUser(ctx, orgID, taskID, otherUserID, "in_progress")
		if err != nil {
			t.Fatalf("AdvanceStatusForUser: %v", err)
		}
		if ok {
			t.Error("AdvanceStatusForUser landed on someone else's task; should refuse")
		}
	})

	t.Run("AdvanceStatusForUser_refuses_terminal_task", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "adv-term")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		if _, err := s.Close(ctx, orgID, taskID, "test_close", ""); err != nil {
			t.Fatalf("close: %v", err)
		}
		ok, err := s.AdvanceStatusForUser(ctx, orgID, taskID, userID, "in_progress")
		if err != nil {
			t.Fatalf("AdvanceStatusForUser: %v", err)
		}
		if ok {
			t.Error("AdvanceStatusForUser landed on terminal task; should refuse")
		}
	})

	t.Run("AdvanceStatusForUser_rejects_invalid_target", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "adv-bad")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		for _, bad := range []string{"done", "dismissed", "queued", "snoozed", "wat"} {
			ok, err := s.AdvanceStatusForUser(ctx, orgID, taskID, userID, bad)
			if err != nil {
				t.Errorf("to=%q: unexpected err: %v", bad, err)
			}
			if ok {
				t.Errorf("to=%q: AdvanceStatusForUser returned ok=true; should only accept in_progress/in_review", bad)
			}
		}
	})

	// The snoozed lane — the board's "show snoozed" toggle asks for it by
	// widening the status set, and the canonical queue projection stays
	// unchanged underneath.

	t.Run("List_snoozed_lane_surfaces_what_the_queue_projection_excludes", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, liveID := seed(t, "qsnz-live")
		_, _, snoozedID := seed(t, "qsnz-snoozed")
		if _, err := s.SetStatus(ctx, orgID, snoozedID, "snoozed"); err != nil {
			t.Fatalf("SetStatus snoozed: %v", err)
		}

		live, _, err := s.List(ctx, orgID, queueFilter(), db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List queue: %v", err)
		}
		var sawLive, sawSnoozedInLive bool
		for _, x := range live {
			if x.ID == liveID {
				sawLive = true
			}
			if x.ID == snoozedID {
				sawSnoozedInLive = true
			}
		}
		if !sawLive {
			t.Error("queue projection missing the unsnoozed task")
		}
		if sawSnoozedInLive {
			t.Error("queue projection returned a snoozed task; should exclude")
		}

		all, _, err := s.List(ctx, orgID, queueWithSnoozedFilter(), db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List queue+snoozed: %v", err)
		}
		var sawLiveAll, sawSnoozedAll bool
		for _, x := range all {
			if x.ID == liveID {
				sawLiveAll = true
			}
			if x.ID == snoozedID {
				sawSnoozedAll = true
			}
		}
		if !sawLiveAll {
			t.Error("queue+snoozed projection missing the live task")
		}
		if !sawSnoozedAll {
			t.Error("queue+snoozed projection missing the snoozed task")
		}
	})

	t.Run("List_only_unclaimed_excludes_claimed", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "qsnz-claimed")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		out, _, err := s.List(ctx, orgID, queueWithSnoozedFilter(), db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List queue+snoozed: %v", err)
		}
		for _, x := range out {
			if x.ID == taskID {
				t.Error("only_unclaimed surfaced a user-claimed task; only unclaimed rows belong in the queue projection")
			}
		}
	})

	runTaskListConformance(ctx, t, mk)

	// --- Author-centric owner routing ---

	t.Run("FindOrCreate_empty_team_stores_null_owner", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		entityID, eventID, _ := seed(t, "null-owner")
		// Empty teamID = unresolved owner → team_id NULL, but the task is
		// still created (visible via task_teams elsewhere).
		task, created, err := s.FindOrCreate(ctx, orgID, "", entityID, domain.EventGitHubPRConflicts, "", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate(empty team): %v", err)
		}
		if !created {
			t.Error("expected a new task for the unresolved-owner situation")
		}
		if task.TeamID != nil {
			t.Errorf("team_id = %q, want nil (unresolved owner)", *task.TeamID)
		}
		// Re-read confirms the NULL persisted, not just the in-memory struct.
		got, err := s.Get(ctx, orgID, task.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read: %v", err)
		}
		if got.TeamID != nil {
			t.Errorf("persisted team_id = %q, want nil", *got.TeamID)
		}
	})

	t.Run("ClaimedRequiresTeam_check_rejects_unowned_claim", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		entityID, eventID, _ := seed(t, "claim-unowned")
		task, _, err := s.FindOrCreate(ctx, orgID, "", entityID, domain.EventGitHubPRConflicts, "", eventID, 0.5)
		if err != nil {
			t.Fatalf("FindOrCreate(empty team): %v", err)
		}
		// SetClaimedByUser is the no-guard primitive: it sets the claim
		// without consolidating an owner, so the claimed-⇒-owned CHECK must
		// reject claiming a NULL-team task. (Production claims go through
		// ClaimQueuedForUser, which sets the owning team atomically.)
		if _, err := s.SetClaimedByUser(ctx, orgID, task.ID, userID); err == nil {
			t.Error("expected the claimed-requires-team CHECK to reject claiming an unowned task, got nil error")
		}
	})

	t.Run("OwnerTeamForLatestTaskInTypes_latest_owned_excludes_null_and_out_of_set", func(t *testing.T) {
		s, orgID, teamID, _, _, seed, seedTeam := mk(t)
		entityID, eventID, _ := seed(t, "tier3")
		teamB := seedTeam(t, "tier3b")

		base := time.Now().UTC()
		// Owned, older.
		if _, _, err := s.FindOrCreateAt(ctx, orgID, teamID, entityID, domain.EventGitHubPRCICheckPassed, "a", eventID, 0.5, base); err != nil {
			t.Fatalf("seed owned task A: %v", err)
		}
		// Unowned (NULL), newer — must be ignored by the lookup.
		if _, _, err := s.FindOrCreateAt(ctx, orgID, "", entityID, domain.EventGitHubPRConflicts, "b", eventID, 0.5, base.Add(time.Second)); err != nil {
			t.Fatalf("seed unowned task B: %v", err)
		}

		// With only ci_check_passed (owned) and conflicts (NULL) in scope,
		// the NULL one is excluded → the owned team comes back.
		got, err := s.OwnerTeamForLatestTaskInTypesSystem(ctx, orgID, entityID, []string{domain.EventGitHubPRCICheckPassed, domain.EventGitHubPRConflicts})
		if err != nil {
			t.Fatalf("OwnerTeamForLatestTaskInTypes: %v", err)
		}
		if got != teamID {
			t.Errorf("owner = %q, want %q (NULL-owned newer task must be excluded)", got, teamID)
		}

		// A newer owned task on teamB wins "latest".
		if _, _, err := s.FindOrCreateAt(ctx, orgID, teamB, entityID, domain.EventGitHubPRLabelAdded, "c", eventID, 0.5, base.Add(2*time.Second)); err != nil {
			t.Fatalf("seed owned task C: %v", err)
		}
		got, err = s.OwnerTeamForLatestTaskInTypesSystem(ctx, orgID, entityID, []string{domain.EventGitHubPRCICheckPassed, domain.EventGitHubPRLabelAdded})
		if err != nil {
			t.Fatalf("OwnerTeamForLatestTaskInTypes (latest): %v", err)
		}
		if got != teamB {
			t.Errorf("owner = %q, want teamB %q (most recent owned task wins)", got, teamB)
		}

		// A type with no task → empty; an empty type set → empty.
		if got, err := s.OwnerTeamForLatestTaskInTypesSystem(ctx, orgID, entityID, []string{domain.EventGitHubPRNewCommits}); err != nil || got != "" {
			t.Errorf("no matching type: got (%q, %v), want (\"\", nil)", got, err)
		}
		if got, err := s.OwnerTeamForLatestTaskInTypesSystem(ctx, orgID, entityID, nil); err != nil || got != "" {
			t.Errorf("empty type set: got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	// --- Empty-arg / ctx-cancel quick guards ---

	t.Run("CtxCancellation_fails_fast", func(t *testing.T) {
		s, orgID, _, _, _, _, _ := mk(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := s.List(cancelled, orgID, queueFilter(), db.ListOpts{Limit: 50}); err == nil {
			t.Errorf("List with cancelled ctx: want error, got nil")
		}
	})

	t.Run("ListActiveRefs_empty_orgID_check_does_not_panic", func(t *testing.T) {
		s, orgID, _, _, _, _, _ := mk(t)
		refs, err := s.ListActiveRefsForEntities(ctx, orgID, []string{}, nil)
		if err != nil {
			t.Fatalf("empty slice: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("got %d refs, want 0", len(refs))
		}
	})

	// --- Returned-row standard ---

	t.Run("lifecycle_writes_return_the_stored_row", func(t *testing.T) {
		// The returned-row standard on TaskStore's ten converted writes:
		// Bump[System], Close[System], SetStatus[System], SetClaimedByAgent,
		// SetClaimedByUser, SetOwnerTeam[System]. Each returns tasks' OWN
		// columns, not the entity join Get reads alongside them — see the
		// shape note on db.TaskStore. bareRead mirrors that: a Get with the
		// join-populated display fields blanked, the same trick
		// TeamsStore.Role's conformance test uses for a per-user column no
		// write can see (settings_conformance.go).
		s, orgID, _, agentID, userID, seed, seedTeam := mk(t)
		bareRead := func(taskID string) func() (*domain.Task, error) {
			return func() (*domain.Task, error) {
				full, err := s.Get(ctx, orgID, taskID)
				if err != nil || full == nil {
					return full, err
				}
				bare := *full
				bare.Title, bare.SourceURL, bare.EntitySourceID = "", "", ""
				bare.EntitySource, bare.EntityKind = "", ""
				bare.OpenSubtaskCount, bare.SlackMessageCount = 0, 0
				return &bare, nil
			}
		}

		// Bump / BumpSystem: wakes a snoozed task.
		for _, sys := range []bool{false, true} {
			_, eventID, taskID := seed(t, fmt.Sprintf("rr-bump-%v", sys))
			if _, err := s.SetStatus(ctx, orgID, taskID, "snoozed"); err != nil {
				t.Fatalf("seed snoozed: %v", err)
			}
			what, got, err := "Tasks.Bump", domain.Task{}, error(nil)
			if sys {
				what = "Tasks.BumpSystem"
				got, err = s.BumpSystem(ctx, orgID, taskID, eventID)
			} else {
				got, err = s.Bump(ctx, orgID, taskID, eventID)
			}
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			AssertWriteReturnedStoredRow(t, what, got, bareRead(taskID))
			if got.Status != "queued" {
				t.Errorf("%s returned status=%q, want queued (snooze cleared)", what, got.Status)
			}
		}

		// Close / CloseSystem: the state guard's terminal side, not just the
		// happy path — a second close against the now-terminal row hits it.
		for _, sys := range []bool{false, true} {
			_, _, taskID := seed(t, fmt.Sprintf("rr-close-%v", sys))
			closeCall := func() (domain.Task, error) { return s.Close(ctx, orgID, taskID, "test_close", "") }
			what := "Tasks.Close"
			if sys {
				what = "Tasks.CloseSystem"
				closeCall = func() (domain.Task, error) { return s.CloseSystem(ctx, orgID, taskID, "test_close", "") }
			}
			got, err := closeCall()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			AssertWriteReturnedStoredRow(t, what, got, bareRead(taskID))
			if got.Status != "done" || got.ClosedAt == nil {
				t.Errorf("%s returned status=%q closed_at=%v, want done + a timestamp", what, got.Status, got.ClosedAt)
			}
			if _, err := closeCall(); !errors.Is(err, db.ErrNoSuchTask) {
				t.Errorf("%s on an already-terminal task = %v, want db.ErrNoSuchTask", what, err)
			}
		}

		// SetStatus / SetStatusSystem.
		for _, sys := range []bool{false, true} {
			_, _, taskID := seed(t, fmt.Sprintf("rr-status-%v", sys))
			what, got, err := "Tasks.SetStatus", domain.Task{}, error(nil)
			if sys {
				what = "Tasks.SetStatusSystem"
				got, err = s.SetStatusSystem(ctx, orgID, taskID, "in_review")
			} else {
				got, err = s.SetStatus(ctx, orgID, taskID, "in_review")
			}
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			AssertWriteReturnedStoredRow(t, what, got, bareRead(taskID))
			if got.Status != "in_review" {
				t.Errorf("%s returned status=%q, want in_review", what, got.Status)
			}
		}

		// SetClaimedByAgent.
		_, _, taskAgent := seed(t, "rr-claim-agent")
		gotAgent, err := s.SetClaimedByAgent(ctx, orgID, taskAgent, agentID)
		if err != nil {
			t.Fatalf("Tasks.SetClaimedByAgent: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Tasks.SetClaimedByAgent", gotAgent, bareRead(taskAgent))
		if gotAgent.ClaimedByAgentID != agentID {
			t.Errorf("SetClaimedByAgent returned claimed_by_agent_id=%q, want %q", gotAgent.ClaimedByAgentID, agentID)
		}

		// SetClaimedByUser.
		_, _, taskUser := seed(t, "rr-claim-user")
		gotUser, err := s.SetClaimedByUser(ctx, orgID, taskUser, userID)
		if err != nil {
			t.Fatalf("Tasks.SetClaimedByUser: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Tasks.SetClaimedByUser", gotUser, bareRead(taskUser))
		if gotUser.ClaimedByUserID != userID {
			t.Errorf("SetClaimedByUser returned claimed_by_user_id=%q, want %q", gotUser.ClaimedByUserID, userID)
		}

		if seedTeam == nil {
			t.Skip("backend factory did not provide a TeamSeeder; SetOwnerTeam subtests skipped")
		}
		otherTeam := seedTeam(t, "rr-owner")

		// SetOwnerTeam / SetOwnerTeamSystem.
		for _, sys := range []bool{false, true} {
			_, _, taskID := seed(t, fmt.Sprintf("rr-owner-%v", sys))
			what, got, err := "Tasks.SetOwnerTeam", domain.Task{}, error(nil)
			if sys {
				what = "Tasks.SetOwnerTeamSystem"
				got, err = s.SetOwnerTeamSystem(ctx, orgID, taskID, otherTeam)
			} else {
				got, err = s.SetOwnerTeam(ctx, orgID, taskID, otherTeam)
			}
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			AssertWriteReturnedStoredRow(t, what, got, bareRead(taskID))
			if got.TeamID == nil || *got.TeamID != otherTeam {
				t.Errorf("%s returned team_id=%v, want %q", what, got.TeamID, otherTeam)
			}
		}

		// SetOwnerTeam("") is a no-op on the column but still requires the id
		// to name a row — the write runs, just to a COALESCE that keeps the
		// stored value.
		_, _, taskNoop := seed(t, "rr-owner-noop")
		before, err := s.Get(ctx, orgID, taskNoop)
		if err != nil || before == nil {
			t.Fatalf("Get before no-op SetOwnerTeam: (%v, %v)", before, err)
		}
		noop, err := s.SetOwnerTeam(ctx, orgID, taskNoop, "")
		if err != nil {
			t.Fatalf("SetOwnerTeam (empty teamID): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Tasks.SetOwnerTeam (empty teamID)", noop, bareRead(taskNoop))
		if noop.TeamID == nil || before.TeamID == nil || *noop.TeamID != *before.TeamID {
			t.Errorf("SetOwnerTeam(\"\") changed team_id: before=%v after=%v", before.TeamID, noop.TeamID)
		}

		// Miss semantics: an id-keyed write against a task that never
		// existed reports it rather than succeeding silently.
		missingID := "00000000-0000-0000-0000-0000000000ba"
		misses := []struct {
			name string
			call func() error
		}{
			{"Tasks.Bump", func() error { _, e := s.Bump(ctx, orgID, missingID, "x"); return e }},
			{"Tasks.Close", func() error { _, e := s.Close(ctx, orgID, missingID, "x", ""); return e }},
			{"Tasks.SetStatus", func() error { _, e := s.SetStatus(ctx, orgID, missingID, "queued"); return e }},
			{"Tasks.SetClaimedByAgent", func() error { _, e := s.SetClaimedByAgent(ctx, orgID, missingID, agentID); return e }},
			{"Tasks.SetClaimedByUser", func() error { _, e := s.SetClaimedByUser(ctx, orgID, missingID, userID); return e }},
			{"Tasks.SetOwnerTeam", func() error { _, e := s.SetOwnerTeam(ctx, orgID, missingID, otherTeam); return e }},
		}
		for _, m := range misses {
			if err := m.call(); !errors.Is(err, db.ErrNoSuchTask) {
				t.Errorf("%s(missing id) = %v, want db.ErrNoSuchTask", m.name, err)
			}
		}
	})
}

// --- List: filters × paging × totals ---
//
// The projections below are the ones the product actually asks for, named
// once so a subtest reads as "the queue deck" rather than as a filter literal.
// They are constructed per call rather than shared as package vars because a
// TaskListFilter holds slices, and a subtest that appended to a shared one
// would rewrite every other subtest's query.

// queueFilter is the pickable-right-now projection: the triage deck and the
// board's Queued column. It is the exact filter set the former GET /api/queue
// hardcoded.
func queueFilter() db.TaskListFilter {
	return db.TaskListFilter{Statuses: []string{"queued"}, OnlyUnclaimed: true}
}

// queueWithSnoozedFilter widens queueFilter to the board's "show snoozed"
// toggle: the snoozed lane joins the queued one, and rows still inside their
// snooze window stay in.
func queueWithSnoozedFilter() db.TaskListFilter {
	return db.TaskListFilter{
		Statuses:       []string{"queued", "snoozed"},
		OnlyUnclaimed:  true,
		IncludeSnoozed: true,
	}
}

// claimedFilter is the board's Claimed column — the claim axis, not a
// lifecycle status.
func claimedFilter() db.TaskListFilter {
	return db.TaskListFilter{Statuses: []string{db.TaskListStatusClaimed}, IncludeSnoozed: true}
}

// runTaskListConformance covers what pagination adds on top of the filter
// semantics the subtests above pin: that a page is a *window* on a stable
// total order, so walking every page yields each matching row exactly once,
// and that total_count counts matches rather than the page.
func runTaskListConformance(ctx context.Context, t *testing.T, mk TaskStoreFactory) {
	t.Helper()

	// listIDs runs List and returns the ids in the order the store produced
	// them, plus the filtered total.
	listIDs := func(t *testing.T, s db.TaskStore, orgID string, f db.TaskListFilter, opts db.ListOpts) ([]string, int) {
		t.Helper()
		tasks, total, err := s.List(ctx, orgID, f, opts)
		if err != nil {
			t.Fatalf("List(%+v, %+v): %v", f, opts, err)
		}
		ids := make([]string, len(tasks))
		for i, task := range tasks {
			ids[i] = task.ID
		}
		return ids, total
	}

	t.Run("List_empty_result_has_zero_total", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		seed(t, "empty-a")
		// A lane nothing has reached yet: the seeder mints queued rows.
		ids, total := listIDs(t, s, orgID, db.TaskListFilter{Statuses: []string{"in_review"}}, db.ListOpts{Limit: 50})
		if len(ids) != 0 || total != 0 {
			t.Errorf("empty lane returned %d ids / total %d, want 0 / 0", len(ids), total)
		}
	})

	t.Run("List_pages_partition_the_result_set", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		const n = 5
		for i := range n {
			seed(t, fmt.Sprintf("page-%d", i))
		}
		want, wantTotal := listIDs(t, s, orgID, queueFilter(), db.ListOpts{Limit: 50})
		if wantTotal != n || len(want) != n {
			t.Fatalf("unpaged read returned %d ids / total %d, want %d / %d", len(want), wantTotal, n, n)
		}

		// Walk it two at a time: the concatenation must reproduce the
		// unpaged order exactly — no row dropped between pages, none
		// repeated across them — and every page must report the same total.
		var walked []string
		for offset := 0; offset < n; offset += 2 {
			ids, total := listIDs(t, s, orgID, queueFilter(), db.ListOpts{Limit: 2, Offset: offset})
			if total != n {
				t.Errorf("offset %d: total = %d, want %d (the filtered total, not the page length)", offset, total, n)
			}
			walked = append(walked, ids...)
		}
		if !slices.Equal(walked, want) {
			t.Errorf("paged walk = %v, want %v (pages must partition the total order)", walked, want)
		}

		// The page-size boundary: a window exactly the size of the result
		// set returns all of it, and the page after the last one is empty
		// while the total stays truthful.
		if ids, total := listIDs(t, s, orgID, queueFilter(), db.ListOpts{Limit: n}); len(ids) != n || total != n {
			t.Errorf("limit == result size returned %d ids / total %d, want %d / %d", len(ids), total, n, n)
		}
		if ids, total := listIDs(t, s, orgID, queueFilter(), db.ListOpts{Limit: 2, Offset: n}); len(ids) != 0 || total != n {
			t.Errorf("offset past the end returned %d ids / total %d, want 0 / %d", len(ids), total, n)
		}
	})

	t.Run("List_order_is_total_and_dialect_agnostic", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		// The seeder mints rows that tie on every ordering key except the
		// id, which is exactly the case offset paging breaks on without a
		// tiebreaker: the ids must come back ascending in both dialects
		// (Postgres compares uuid bytes, SQLite the canonical lowercase
		// text — the same order for a canonical uuid).
		for i := range 6 {
			seed(t, fmt.Sprintf("order-%d", i))
		}
		ids, _ := listIDs(t, s, orgID, queueFilter(), db.ListOpts{Limit: 50})
		if !slices.IsSorted(ids) {
			t.Errorf("ids = %v, want ascending (the id tiebreaker is what makes offset paging stable)", ids)
		}
	})

	t.Run("List_snoozed_sorts_behind_live_and_closed_behind_both", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, liveID := seed(t, "tail-live")
		_, _, snoozedID := seed(t, "tail-snoozed")
		_, _, doneID := seed(t, "tail-done")
		if _, err := s.SetStatus(ctx, orgID, snoozedID, "snoozed"); err != nil {
			t.Fatalf("SetStatus snoozed: %v", err)
		}
		if _, err := s.Close(ctx, orgID, doneID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}

		ids, total := listIDs(t, s, orgID, db.TaskListFilter{IncludeSnoozed: true}, db.ListOpts{Limit: 50})
		if total != 3 {
			t.Fatalf("total = %d, want 3 (an empty status set means every lane)", total)
		}
		if want := []string{liveID, doneID, snoozedID}; !slices.Equal(ids, want) {
			// live first, then the closed row, then the snoozed tail: a
			// deferred entry never outranks pickable work, and a closed one
			// never outranks either.
			t.Errorf("order = %v, want %v", ids, want)
		}
	})

	t.Run("List_closed_since_windows_terminal_rows_only", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, liveID := seed(t, "cs-live")
		_, _, doneID := seed(t, "cs-done")
		if _, err := s.Close(ctx, orgID, doneID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// A window that starts after the close hides the closed row and
		// leaves the open one alone — the filter is about closed_at, and an
		// open task has none.
		future := time.Now().UTC().Add(time.Hour)
		f := db.TaskListFilter{IncludeSnoozed: true, ClosedSince: &future}
		ids, total := listIDs(t, s, orgID, f, db.ListOpts{Limit: 50})
		if want := []string{liveID}; !slices.Equal(ids, want) || total != 1 {
			t.Errorf("future window = %v (total %d), want %v (total 1)", ids, total, want)
		}

		// A window that starts before it keeps both.
		past := time.Now().UTC().Add(-time.Hour)
		f.ClosedSince = &past
		ids, total = listIDs(t, s, orgID, f, db.ListOpts{Limit: 50})
		if len(ids) != 2 || total != 2 {
			t.Errorf("past window = %v (total %d), want both rows", ids, total)
		}
	})

	t.Run("List_filters_compose", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, freeID := seed(t, "compose-free")
		_, _, takenID := seed(t, "compose-taken")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, takenID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}

		// queued + only_unclaimed keeps the free one; the claimed lane keeps
		// the taken one; asking for both lanes at once keeps both, because
		// the status set is an OR and only_unclaimed is off.
		if ids, total := listIDs(t, s, orgID, queueFilter(), db.ListOpts{Limit: 50}); !slices.Equal(ids, []string{freeID}) || total != 1 {
			t.Errorf("queue projection = %v (total %d), want just the unclaimed task", ids, total)
		}
		if ids, total := listIDs(t, s, orgID, claimedFilter(), db.ListOpts{Limit: 50}); !slices.Equal(ids, []string{takenID}) || total != 1 {
			t.Errorf("claimed projection = %v (total %d), want just the claimed task", ids, total)
		}
		both := db.TaskListFilter{Statuses: []string{"queued", db.TaskListStatusClaimed}, IncludeSnoozed: true}
		if _, total := listIDs(t, s, orgID, both, db.ListOpts{Limit: 50}); total != 2 {
			t.Errorf("queued+claimed total = %d, want 2", total)
		}
		// Contradictory but well-formed: the claimed lane under
		// only_unclaimed is empty rather than an error.
		contradiction := claimedFilter()
		contradiction.OnlyUnclaimed = true
		if ids, total := listIDs(t, s, orgID, contradiction, db.ListOpts{Limit: 50}); len(ids) != 0 || total != 0 {
			t.Errorf("claimed+only_unclaimed = %v (total %d), want empty", ids, total)
		}
	})

	t.Run("List_unknown_status_matches_nothing", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		seed(t, "bogus-status")
		// The HTTP layer rejects a status outside the vocabulary; the store
		// is not the validator, and must answer an honest empty rather than
		// widening to everything.
		ids, total := listIDs(t, s, orgID, db.TaskListFilter{Statuses: []string{"not_a_status"}}, db.ListOpts{Limit: 50})
		if len(ids) != 0 || total != 0 {
			t.Errorf("unknown status returned %d ids / total %d, want 0 / 0", len(ids), total)
		}
	})
}
