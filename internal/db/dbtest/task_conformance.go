package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskStoreFactory is what a per-backend test file hands to
// RunTaskStoreConformance. The factory returns:
//   - the wired TaskStore impl
//   - the orgID to pass to every method (sqlite returns
//     runmode.LocalDefaultOrg, postgres returns a fresh org UUID)
//   - the teamID — caller-supplied team_id for FindOrCreate
//     (SKY-295). SQLite returns runmode.LocalDefaultTeamID; Postgres
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
// exercise the SKY-295 fanout. Local-mode (SQLite) seeds a real
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

	t.Run("Queued_returns_unclaimed_queued_tasks", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		seed(t, "q1")
		seed(t, "q2")
		out, err := s.Queued(ctx, orgID, nil)
		if err != nil {
			t.Fatalf("Queued: %v", err)
		}
		if len(out) < 2 {
			t.Errorf("Queued returned %d, want >= 2", len(out))
		}
		for _, task := range out {
			if task.Status != "queued" {
				t.Errorf("task %s status=%q, want queued", task.ID, task.Status)
			}
			if task.ClaimedByAgentID != "" || task.ClaimedByUserID != "" {
				t.Errorf("task %s shouldn't appear in queued (has claim)", task.ID)
			}
		}
	})

	t.Run("ByStatus_with_done_excludes_active", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "bs-done")
		// Active task should not appear in done list.
		done, err := s.ByStatus(ctx, orgID, "done", nil)
		if err != nil {
			t.Fatalf("ByStatus done: %v", err)
		}
		for _, task := range done {
			if task.ID == taskID {
				t.Errorf("active task %s appeared under ByStatus(done)", taskID)
			}
		}
		// Now close it; should appear under done.
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		done, err = s.ByStatus(ctx, orgID, "done", nil)
		if err != nil {
			t.Fatalf("ByStatus done (post-close): %v", err)
		}
		found := false
		for _, task := range done {
			if task.ID == taskID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("closed task %s missing from ByStatus(done)", taskID)
		}
	})

	// SKY-330: the "claimed" derived branch in ByStatus filters
	// status='queued' so the Board's Claimed column doesn't
	// double-render a user-claimed task that's also in In Progress
	// or In Review. Distinct from the broader "any non-terminal
	// user-claimed task" set the pre-330 derivation produced.
	t.Run("ByStatus_claimed_excludes_in_progress_and_in_review", func(t *testing.T) {
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

		claimed, err := s.ByStatus(ctx, orgID, "claimed", nil)
		if err != nil {
			t.Fatalf("ByStatus claimed: %v", err)
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

	// SKY-330: bot-claimed status='queued' tasks (just-delegated, run
	// not yet advanced) also belong in the Claimed projection so the
	// board's Claimed column surfaces them and the delegate-spawn-
	// failure retry UI can render. Without this they'd disappear
	// between the delegate stamp and the first non-initializing
	// run-status transition.
	t.Run("ByStatus_claimed_includes_bot_claimed_queued", func(t *testing.T) {
		s, orgID, _, agentID, _, seed, _ := mk(t)
		_, _, taskID := seed(t, "bs-claimed-bot")
		if _, err := s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, ""); err != nil {
			t.Fatalf("stamp agent: %v", err)
		}

		claimed, err := s.ByStatus(ctx, orgID, "claimed", nil)
		if err != nil {
			t.Fatalf("ByStatus claimed: %v", err)
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
		if err := s.SetStatus(ctx, orgID, taskID, "snoozed"); err != nil {
			t.Fatalf("SetStatus snoozed: %v", err)
		}
		if err := s.Bump(ctx, orgID, taskID, eventID); err != nil {
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
		if err := s.Close(ctx, orgID, taskID, "test_close", ""); err != nil {
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

	t.Run("CloseAllForEntity_counts_and_closes", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		entityID, _, taskID := seed(t, "closeall")
		closed, err := s.CloseAllForEntity(ctx, orgID, entityID, "entity_closed")
		if err != nil {
			t.Fatalf("CloseAllForEntity: %v", err)
		}
		if closed < 1 {
			t.Errorf("closed count=%d, want >=1", closed)
		}
		got, _ := s.Get(ctx, orgID, taskID)
		if got.Status != "done" {
			t.Errorf("status=%q post-CloseAllForEntity, want done", got.Status)
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
		_ = s.Close(ctx, orgID, taskID, "test", "")
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
		if err := s.Close(ctx, orgID, taskB, "test", ""); err != nil {
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
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
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
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
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
		if err := s.Close(ctx, orgID, taskID, "test", ""); err != nil {
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

	// SKY-330: AdvanceStatusForUser — the manual user board transition.

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
			t.Error("AdvanceStatusForUser landed on bot-claimed task; should refuse (status owned by run lifecycle)")
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
		if err := s.Close(ctx, orgID, taskID, "test_close", ""); err != nil {
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

	// SKY-330: QueuedIncludingSnoozed — surfaces future-snoozed entries
	// for the board's "show snoozed" toggle while keeping the canonical
	// Queued() projection unchanged.

	t.Run("QueuedIncludingSnoozed_includes_snoozed_while_Queued_excludes", func(t *testing.T) {
		s, orgID, _, _, _, seed, _ := mk(t)
		_, _, liveID := seed(t, "qsnz-live")
		_, _, snoozedID := seed(t, "qsnz-snoozed")
		if err := s.SetStatus(ctx, orgID, snoozedID, "snoozed"); err != nil {
			t.Fatalf("SetStatus snoozed: %v", err)
		}

		live, err := s.Queued(ctx, orgID, nil)
		if err != nil {
			t.Fatalf("Queued: %v", err)
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
			t.Error("Queued() missing the unsnoozed task")
		}
		if sawSnoozedInLive {
			t.Error("Queued() returned a snoozed task; should exclude")
		}

		all, err := s.QueuedIncludingSnoozed(ctx, orgID, nil)
		if err != nil {
			t.Fatalf("QueuedIncludingSnoozed: %v", err)
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
			t.Error("QueuedIncludingSnoozed() missing the live task")
		}
		if !sawSnoozedAll {
			t.Error("QueuedIncludingSnoozed() missing the snoozed task")
		}
	})

	t.Run("QueuedIncludingSnoozed_excludes_claimed", func(t *testing.T) {
		s, orgID, _, _, userID, seed, _ := mk(t)
		_, _, taskID := seed(t, "qsnz-claimed")
		if ok, err := s.ClaimQueuedForUser(ctx, orgID, taskID, userID); err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		out, err := s.QueuedIncludingSnoozed(ctx, orgID, nil)
		if err != nil {
			t.Fatalf("QueuedIncludingSnoozed: %v", err)
		}
		for _, x := range out {
			if x.ID == taskID {
				t.Error("QueuedIncludingSnoozed surfaced a user-claimed task; SKY-261 invariant says only unclaimed rows belong in the queue projection")
			}
		}
	})

	// --- Empty-arg / ctx-cancel quick guards ---

	t.Run("CtxCancellation_fails_fast", func(t *testing.T) {
		s, orgID, _, _, _, _, _ := mk(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Queued(cancelled, orgID, nil); err == nil {
			t.Errorf("Queued with cancelled ctx: want error, got nil")
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
}
