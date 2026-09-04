package delegate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// launchFixture is a claimed blueprint step with a worktree on disk — the
// shape every pre-agent failure acts on. The conversation is claimed for real
// (EnqueueConversation + ClaimNextConversation) so the claim id, the claim fence and the
// re-claim all behave as they do in production.
type launchFixture struct {
	s        *Spawner
	database *sql.DB
	stores   db.Stores
	br       *domain.BlueprintRun
	conv     domain.Conversation
	worktree string
}

func newLaunchFixture(t *testing.T, suffix string) *launchFixture {
	t.Helper()
	// A real directory, so "the worktree survives" is an assertion about the
	// filesystem rather than about a string.
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	return newLaunchFixtureWithWorktree(t, suffix, wt)
}

// newLaunchFixtureWithWorktree is newLaunchFixture with the run tree chosen by
// the caller. Empty means the blueprint has no worktree recorded yet, so the
// claim's bring-up takes the source setup's build-from-nothing arm — the fetch
// and the clone — rather than the rehydrate.
func newLaunchFixtureWithWorktree(t *testing.T, suffix, wt string) *launchFixture {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#"+suffix, "pr", "T", "https://x/"+suffix)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}

	bpID := "lfbp-" + suffix
	if _, err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	promptID := "lfp-" + suffix
	ensureTestPrompt(t, database, domain.Prompt{ID: promptID, Name: promptID, Body: "b", Source: "user"})
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, []string{promptID}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}

	created, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "lfbr-" + suffix, BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: wt,
		StepPlan: []domain.BlueprintPlanStep{{
			StepIndex: 0, PromptID: promptID, PromptName: promptID, PromptBody: "b", Source: "user",
		}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	brID := created.ID
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}

	step0 := 0
	if _, err := stores.ConversationQueue.EnqueueConversation(ctx, org, domain.Conversation{
		ID: "lfrun-" + suffix, TaskID: task.ID, PromptID: promptID, Model: "claude-sonnet-4-6",
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
		BlueprintRunID: brID, BlueprintStepIndex: &step0, WorktreePath: wt,
	}); err != nil {
		t.Fatalf("EnqueueConversation: %v", err)
	}
	claimed, err := stores.ConversationQueue.ClaimNextConversation(ctx, "lf-exec", 1, db.ClaimPlacement{})
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextConversation: (%v, %v)", claimed, err)
	}

	return &launchFixture{
		s:        NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6"),
		database: database,
		stores:   stores,
		br:       br,
		conv:     *claimed,
		worktree: wt,
	}
}

// speak puts a row on the transcript — what makes a conversation "work in
// progress" rather than a step that has never started.
func (f *launchFixture) speak(t *testing.T, role, content string) {
	t.Helper()
	delivered := true
	if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: f.conv.ID, Role: role, Content: content,
		Delivered: &delivered, WindowState: domain.MessageWindowActive,
	}); err != nil {
		t.Fatalf("insert transcript row: %v", err)
	}
}

func (f *launchFixture) storedStatus(t *testing.T) string {
	t.Helper()
	var status sql.NullString
	if err := f.database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, f.conv.ID).Scan(&status); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	return status.String
}

func (f *launchFixture) claimOutcomes(t *testing.T) []string {
	t.Helper()
	rows, err := f.database.Query(`SELECT COALESCE(outcome, '') FROM claims WHERE conversation_id = ? ORDER BY claimed_at`, f.conv.ID)
	if err != nil {
		t.Fatalf("read claim outcomes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatalf("scan outcome: %v", err)
		}
		out = append(out, o)
	}
	return out
}

func (f *launchFixture) blueprintStatus(t *testing.T) string {
	t.Helper()
	br, err := f.stores.Blueprints.GetRunSystem(context.Background(), runmode.LocalDefaultOrgID, f.br.ID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	return string(br.Status)
}

func (f *launchFixture) transcript(t *testing.T) []domain.Message {
	t.Helper()
	rows, err := f.stores.Conversations.ListForAssemblySystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil {
		t.Fatalf("ListForAssemblySystem: %v", err)
	}
	return rows
}

// TestPreAgentFailure_HandsTheClaimBackAndDestroysNothing is the ticket's
// headline: a runtime that would not start on a conversation mid-flight leaves
// every single thing it needs to try again exactly where it was, and the next
// scan re-claims it.
func TestPreAgentFailure_HandsTheClaimBackAndDestroysNothing(t *testing.T) {
	f := newLaunchFixture(t, "requeue")
	f.speak(t, "user", "have another look at the failing check")
	f.speak(t, "assistant", "on it")

	f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, f.conv,
		errors.New("connect to tool host: runsc: exit status 128"))

	if st := f.storedStatus(t); st != "" {
		t.Errorf("stored status = %q after a launch failure, want none — the conversation is still mid-flight", st)
	}
	if got := f.claimOutcomes(t); len(got) != 1 || got[0] != "requeued" {
		t.Errorf("claim outcomes = %v, want [requeued]", got)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running — a launch failure moves no blueprint", st)
	}
	if _, err := os.Stat(f.worktree); err != nil {
		t.Errorf("worktree gone after a launch failure: %v", err)
	}
	for _, m := range f.transcript(t) {
		if m.IsError {
			t.Errorf("a failure row reached the transcript (%q); nothing ran, so nothing failed", m.Content)
		}
	}

	reclaimed, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), "lf-exec", 1, db.ClaimPlacement{})
	if err != nil || reclaimed == nil || reclaimed.ID != f.conv.ID {
		t.Fatalf("re-claim after requeue = (%v, %v), want the same conversation", reclaimed, err)
	}
	if reclaimed.Attempts != 2 {
		t.Errorf("re-claim attempts = %d, want 2 — the hand-back counts toward the budget", reclaimed.Attempts)
	}
}

// TestPreAgentFailure_ExhaustedOnAConversationWithATranscript_Parks pins the
// disposition for work in progress: the budget runs out, and the conversation
// comes to rest with the reason on its transcript instead of being destroyed
// by a runtime that never started.
func TestPreAgentFailure_ExhaustedOnAConversationWithATranscript_Parks(t *testing.T) {
	f := newLaunchFixture(t, "parks")
	f.speak(t, "user", "have another look at the failing check")
	f.speak(t, "assistant", "on it")
	// The follow-up that woke it: undelivered, so it is also what would
	// re-claim the conversation the instant it parked.
	pending := false
	if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: f.conv.ID, Role: "user", Content: "and update the README",
		Delivered: &pending, WindowState: domain.MessageWindowActive,
	}); err != nil {
		t.Fatalf("queue the follow-up: %v", err)
	}

	spent := f.conv
	spent.Attempts = maxClaimAttempts
	f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, spent,
		errors.New("connect to tool host: runsc: exit status 128"))

	if st := f.storedStatus(t); st != domain.StatusOpen {
		t.Errorf("stored status = %q after an exhausted budget, want open", st)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running — the blueprint is untouched", st)
	}
	if _, err := os.Stat(f.worktree); err != nil {
		t.Errorf("worktree gone after an exhausted budget: %v", err)
	}

	rows := f.transcript(t)
	var note string
	for _, m := range rows {
		if m.Subtype == domain.MessageSubtypeStopNote {
			note = m.Content
		}
		if m.IsError {
			t.Errorf("a failure row reached the transcript (%q); the conversation parked, it did not fail", m.Content)
		}
	}
	if note == "" {
		t.Fatal("no stop-note on the transcript; the RunStation would show a park with no explanation")
	}
	for _, want := range []string{"5 attempts", "exit status 128", "Send a message to retry"} {
		if !strings.Contains(note, want) {
			t.Errorf("stop-note %q does not mention %q", note, want)
		}
	}

	// Nothing is left claimable: the queued follow-up was settled on the way
	// down, so `open` is a rest state rather than an immediate re-claim that
	// would make the budget buy nothing.
	if got, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), "lf-exec", 1, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextConversation after the park = (%v, %v), want nothing claimable", got, err)
	}

	// And the user's next message re-arms a whole fresh budget, which is what
	// makes "send a message to retry" a true statement.
	if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: f.conv.ID, Role: "user", Content: "try again",
		Delivered: &pending, WindowState: domain.MessageWindowActive,
	}); err != nil {
		t.Fatalf("send the follow-up: %v", err)
	}
	reclaimed, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), "lf-exec", 1, db.ClaimPlacement{})
	if err != nil || reclaimed == nil || reclaimed.ID != f.conv.ID {
		t.Fatalf("re-claim after the follow-up = (%v, %v), want the same conversation", reclaimed, err)
	}
	if reclaimed.Attempts != 1 {
		t.Errorf("re-claim attempts = %d, want 1 — the park ended the episode", reclaimed.Attempts)
	}
}

// TestPreAgentFailure_ExhaustedOnAFirstEngagement_StillPoisonPills pins the
// other half of the disposition. A step that has never spoken has nothing
// worth keeping, so an exhausted budget stays the loud failure it has always
// been — the blueprint fails, the task surfaces it, and the auto-delegation
// breaker keeps its signal.
func TestPreAgentFailure_ExhaustedOnAFirstEngagement_StillPoisonPills(t *testing.T) {
	f := newLaunchFixture(t, "poison")

	spent := f.conv
	spent.Attempts = maxClaimAttempts
	f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, spent,
		errors.New("workspace setup: clone: repository not found"))

	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusFailed) {
		t.Errorf("blueprint_run status = %q, want failed", st)
	}
	// No stop-note either: there is no transcript for one to explain
	// anything to, and the terminal is the record.
	for _, m := range f.transcript(t) {
		if m.Subtype == domain.MessageSubtypeStopNote {
			t.Errorf("a first engagement's poison pill wrote a park note: %q", m.Content)
		}
	}
}

// TestPreAgentFailure_WithNoBlueprintInScope_Parks covers the resume path,
// which has no blueprint in hand and needs none: a resume continues a
// conversation that has already been driven, so an exhausted budget can only
// mean park.
func TestPreAgentFailure_WithNoBlueprintInScope_Parks(t *testing.T) {
	f := newLaunchFixture(t, "resume")

	spent := f.conv
	spent.Attempts = maxClaimAttempts
	f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, nil, spent,
		errors.New("ensure workspace before resume failed: snapshot fetch: connection reset"))

	if st := f.storedStatus(t); st != domain.StatusOpen {
		t.Errorf("stored status = %q, want open", st)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}
}

// TestRunNativeAgent_ToolHostLaunchFailureRecordsNothing pins the native
// path's half of the contract at the site the ticket's live failure came from:
// a tool host that will not launch comes back as a launchErr for the
// dispatcher to retry, and the conversation is left exactly as it was found.
// Before this, the same two seconds of runsc trouble wrote a terminal.
func TestRunNativeAgent_ToolHostLaunchFailureRecordsNothing(t *testing.T) {
	f := newLaunchFixture(t, "native")
	f.speak(t, "user", "have another look")
	f.speak(t, "assistant", "on it")

	task, err := f.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.TaskID)
	if err != nil || task == nil {
		t.Fatalf("GetSystem task: (%v, %v)", task, err)
	}

	// No prebuilt run network, which is what a broker that could not build
	// one leaves behind — LaunchToolHost refuses on exactly that.
	disp := f.s.runNativeAgent(context.Background(), f.conv.ID, *task, "do the thing", runConfig{
		orgID:          runmode.LocalDefaultOrgID,
		teamID:         runmode.LocalDefaultTeamID,
		claimID:        f.conv.ClaimID,
		blueprintRunID: f.br.ID,
		wtPath:         f.worktree,
		runRoot:        f.worktree,
	}, time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID)

	if disp.launchErr == nil {
		t.Fatal("launch failure did not come back as a launchErr; the dispatcher would react to a terminal that was never written")
	}
	if disp.fenced {
		t.Error("launch failure reported fenced; nothing was refused")
	}
	if !strings.Contains(disp.launchErr.Error(), "launch tool host") {
		t.Errorf("launchErr = %q, want it to name the phase that failed", disp.launchErr)
	}
	if st := f.storedStatus(t); st != "" {
		t.Errorf("stored status = %q after a launch failure, want none", st)
	}
	for _, m := range f.transcript(t) {
		if m.IsError {
			t.Errorf("a failure row reached the transcript (%q); the agent never ran", m.Content)
		}
	}
	if got := f.claimOutcomes(t); len(got) != 1 || got[0] != "" {
		t.Errorf("claim outcomes = %v, want the claim still live — the dispatcher owns the hand-back", got)
	}
}

// TestParkAfterLaunchExhaustion_FencedClaimRecordsNothing pins the fence on
// the park: an engagement superseded while it was failing to launch owns none
// of this conversation, so neither the note nor the park is its to write.
//
// The refusal is injected rather than staged for real — local mode does not
// fence (single process, nothing to refuse), which is exactly why the reaction
// has to be exercised this way. See claim_fence_test.go.
func TestParkAfterLaunchExhaustion_FencedClaimRecordsNothing(t *testing.T) {
	f := newLaunchFixture(t, "fenced")
	f.speak(t, "user", "keep going")

	fenced := &fencedConversationStore{ConversationStore: f.s.conversations}
	f.s.conversations = fenced

	spent := f.conv
	spent.Attempts = maxClaimAttempts
	f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, spent, errors.New("launch tool host: no route to broker"))

	if fenced.inserts != 1 {
		t.Errorf("fenced note inserts = %d, want 1", fenced.inserts)
	}
	if fenced.cancels != 0 {
		t.Error("the park was attempted after the fence refused the note; a refusal means stop, not carry on")
	}
	if st := f.storedStatus(t); st != "" {
		t.Errorf("stored status = %q, want none — the successor is driving this conversation", st)
	}
}
