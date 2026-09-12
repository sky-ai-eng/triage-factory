package delegate

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// placeTaskInProgress is the whole of a delegation's board placement: one
// forward write when the blueprint run is minted. These tests pin that write
// and its three guards directly, then walk the paths a run passes through
// afterwards — a park, a step advance, a wake — to show none of them touches
// the column. (The fourth, a completion holding an unresolved artifact, is
// pinned next to the rest of terminateBlueprint in blueprint_advance_test.go.)
//
// setupAdvanceFixture seeds an entity + event + task + a 1-step blueprint_run
// whose single run starts 'running' (see seedConversation → seedConversationBlueprint).

func TestPlaceTaskInProgress_BotClaimedTaskMoves(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "ip")
	stampBotClaim(t, database, taskID)

	s.placeTaskInProgress(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (a minted delegation places its task)", got)
	}
}

// A run parked `open` and an unresolved draft PR say a human is owed
// something; neither is board placement. The card lands in_progress whatever
// the run's state, and the signal rides the card frame and the attention order.
func TestPlaceTaskInProgress_ParkedRunWithDraftPRStillInProgress(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "parked-draft")
	stampBotClaim(t, database, taskID)
	setConversationStatus(t, database, conversationID, "open")
	seedDraftPRArtifact(t, s, conversationID)

	s.placeTaskInProgress(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (a parked run with a draft PR is placed like any other)", got)
	}
}

// A user takeover flips the claim to the user, who owns the lifecycle from
// then on — a delegation must not move their card.
func TestPlaceTaskInProgress_UserClaimedTaskNeutral(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "user-claim")
	stampUserClaim(t, database, taskID)

	s.placeTaskInProgress(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (user-claimed task must not auto-move)", got)
	}
}

// Unclaimed task — the spawner mustn't place rows it doesn't own.
func TestPlaceTaskInProgress_UnclaimedTaskNeutral(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "unclaimed")

	s.placeTaskInProgress(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "queued" {
		t.Errorf("status = %q, want queued (unclaimed task must not move)", got)
	}
}

// Already-terminal task: a placement must not reopen a done/dismissed row.
func TestPlaceTaskInProgress_TerminalTaskNeutral(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "already-done")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'dismissed' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("dismiss task: %v", err)
	}

	s.placeTaskInProgress(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "dismissed" {
		t.Errorf("status = %q, want dismissed (terminal task must not flip)", got)
	}
}

// TestDelegate_PlacesTheTaskInProgress walks the one production call site: a
// minted blueprint run leaves its task in_progress, and the step it enqueued
// is queued rather than driven from here.
func TestDelegate_PlacesTheTaskInProgress(t *testing.T) {
	database := newDelegateTestDB(t)
	seedLocalBotAgent(t, database)
	task, bpID := delegatableFixture(t, database, "place")
	stampBotClaim(t, database, task.ID) // the manual handler claims before delegating

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	if _, err := s.Delegate(task, DelegateOpts{
		OrgID: runmode.LocalDefaultOrgID, ExplicitBlueprintID: bpID,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
	}); err != nil {
		t.Fatalf("Delegate: %v", err)
	}

	if got := readTaskStatus(t, database, task.ID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (mint places the task)", got)
	}
}

// TestReactor_StepAdvanceLeavesTheColumnAlone: the blueprint advancing to its
// next step is not a board event. The card was placed at mint and the reactor
// writes no column of its own.
func TestReactor_StepAdvanceLeavesTheColumnAlone(t *testing.T) {
	s, database, brID, taskID, step0ConversationID := reactorFixture(t, "noboard", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID
	seedLocalBotAgent(t, database)
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("place task: %v", err)
	}

	stepConversation, _ := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepConversations(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step runs = %v, want [1] — the advance itself must still happen", q)
	}
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (a step advance moves no column)", got)
	}
}

// TestMarkConversationOpen_ParkLeavesTheColumnAlone: parking a run mid-turn
// writes the run's status and nothing else — a park is not a board move.
func TestMarkConversationOpen_ParkLeavesTheColumnAlone(t *testing.T) {
	s, database, conversationID, taskID := setupAdvanceFixture(t, "park-noboard")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("place task: %v", err)
	}

	if fenced := s.markConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		triggerType:    "manual",
		reason:         db.ParkIdle(),
	}); fenced {
		t.Fatal("the park was fenced out; the fixture's run holds no successor claim")
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "open" {
		t.Fatalf("run status = %q, want open — the park itself must still land", status)
	}
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (a park moves no column)", got)
	}
}

// TestSendMessage_WakeLeavesTheColumnAlone: waking a parked run queues the
// message and the run; it does not bounce the task's column back.
func TestSendMessage_WakeLeavesTheColumnAlone(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, taskID := setupAdvanceFixture(t, "wake-noboard")
	stampBotClaim(t, database, taskID)
	setConversationStatus(t, database, conversationID, "open")
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_progress' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("place task: %v", err)
	}

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "carry on"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, conversationID); err != nil || !ok {
		t.Fatalf("the wake queued no input: ok=%v err=%v", ok, err)
	}
	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Errorf("task.status = %q, want in_progress (a wake moves no column)", got)
	}
}

// --- helpers ---

// setupAdvanceFixture creates a fresh spawner + seeded run+task pair (with its
// 1-step blueprint_run) and returns the run/task ids so each test can mutate run
// state or the claim independently.
func setupAdvanceFixture(t *testing.T, suffix string) (*Spawner, *sql.DB, string, string) {
	t.Helper()
	database := newDelegateTestDB(t)
	seedLocalBotAgent(t, database)
	conversationID := "r-adv-" + suffix
	seedConversation(t, database, conversationID, "sess-"+suffix, "/tmp/wt-adv-"+suffix)
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = ?`, conversationID).Scan(&taskID); err != nil {
		t.Fatalf("lookup task_id: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	return s, database, conversationID, taskID
}

// seedLocalBotAgent inserts the sentinel agent + team_agents rows a bot claim
// needs to satisfy the FK on claimed_by_agent_id. newDelegateTestDB seeds the
// tenant (orgs/teams) via BootstrapSchemaForTest but not the agent —
// production seeds that through the explicit provision action
// (BootstrapLocalOrg), which no test fixture runs.
func seedLocalBotAgent(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Triage Factory Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled) VALUES (?, ?, 1)`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed local team_agents: %v", err)
	}
}

func stampBotClaim(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE tasks SET claimed_by_agent_id = ?, claimed_by_user_id = NULL WHERE id = ?`,
		runmode.LocalDefaultAgentID, taskID,
	); err != nil {
		t.Fatalf("stamp bot claim: %v", err)
	}
}

func stampUserClaim(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE tasks SET claimed_by_user_id = ?, claimed_by_agent_id = NULL WHERE id = ?`,
		runmode.LocalDefaultUserID, taskID,
	); err != nil {
		t.Fatalf("stamp user claim: %v", err)
	}
}

func readTaskStatus(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func setConversationStatus(t *testing.T, database *sql.DB, conversationID, status string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, conversationID); err != nil {
		t.Fatalf("set run status: %v", err)
	}
}

func blueprintRunIDForConversation(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	var brID string
	if err := database.QueryRow(`SELECT blueprint_run_id FROM conversations WHERE id = ?`, conversationID).Scan(&brID); err != nil {
		t.Fatalf("read blueprint_run_id: %v", err)
	}
	return brID
}

// addStepConversation appends another step run to an existing blueprint_run, so
// a test can exercise a blueprint whose steps are more than one row.
func addStepConversation(t *testing.T, database *sql.DB, blueprintRunID, taskID, conversationID string, stepIndex int, status string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, trigger_type, team_id, visibility,
		                  creator_user_id, worktree_path, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, 'test-prompt', ?, 'manual', ?, 'team', ?, '/tmp/wt-step', ?, ?)
	`, conversationID, taskID, status, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, blueprintRunID, stepIndex); err != nil {
		t.Fatalf("add step run: %v", err)
	}
}
