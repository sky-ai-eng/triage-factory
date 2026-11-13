package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// queueOtherTask queues a delegation on a second task of the warm
// engagement's entity, queued long enough ago that every executor has had its
// chance at it, and returns the conversation.
func queueOtherTask(t *testing.T, f *warmEngagement) string {
	t.Helper()
	ctx := context.Background()
	var entityID, eventID, blueprintID string
	if err := f.h.AdminDB.QueryRow(`SELECT entity_id, primary_event_id FROM tasks WHERE id = $1`, f.taskID).Scan(&entityID, &eventID); err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if err := f.h.AdminDB.QueryRow(`SELECT blueprint_id FROM blueprint_runs WHERE id = $1`, f.brID).Scan(&blueprintID); err != nil {
		t.Fatalf("read the blueprint: %v", err)
	}
	taskID := uuid.New().String()
	pgtest.MustExec(t, f.h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'other', $7, 'queued', now())
	`, taskID, f.orgID, f.userID, f.teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID)
	conversationID, runID := uuid.New().String(), uuid.New().String()
	step0 := 0
	if _, _, _, _, err := f.stores.Blueprints.CreateRunWithFirstStepSystem(ctx, f.orgID, domain.BlueprintRun{
		ID: runID, BlueprintID: blueprintID, TaskID: taskID,
		TriggerType: domain.BlueprintTriggerManual, WorktreePath: "/tmp/wt-" + runID,
	}, db.AgentClaimStamp{}, "", domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: f.promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: f.userID, BlueprintRunID: runID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("queue a run on another task: %v", err)
	}
	pgtest.MustExec(t, f.h.AdminDB, `UPDATE conversations SET queued_at = now() - interval '1 minute' WHERE id = $1`, conversationID)
	return conversationID
}

// assertStillWarm checks the engagement is still waiting on its warm claim.
func assertStillWarm(t *testing.T, f *warmEngagement) {
	t.Helper()
	if phase := f.claimPhase(t); phase != domain.ClaimPhaseWarm {
		t.Fatalf("claim phase = %q, want %q", phase, domain.ClaimPhaseWarm)
	}
	if n := f.x.warmWaits.Len(); n != 1 {
		t.Fatalf("warm claims waiting = %d, want 1", n)
	}
}

// Claimable work queued on another task lets the warm claim go through the
// same let-go its window's end takes, and the work is then claimable.
func TestWarmPressure_ClaimableWorkLetsARealWarmClaimGo(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)
	ctx := context.Background()

	if f.x.letGoWarmForClaimableWork(ctx, DefaultDispatchScanInterval, "slot") {
		t.Fatal("let a warm claim go with nothing queued")
	}
	assertStillWarm(t, f)

	other := queueOtherTask(t, f)
	if !f.x.letGoWarmForClaimableWork(ctx, DefaultDispatchScanInterval, "slot") {
		t.Fatal("did not let the warm claim go with claimable work queued")
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	f.assertTurnEnded(t, "fixed it")
	id, epoch := f.x.executorIdentity()
	got, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, id, epoch, f.x.claimPlacement(), db.DefaultClaimLease)
	if err != nil || got == nil || got.ID != other {
		t.Fatalf("claim after the let-go = (%+v, %v), want conversation %s", got, err, other)
	}
}

// An org held at its cap by the warm claim lets the claim go for the work
// queued behind the cap.
func TestWarmPressure_OrgCapLetsARealWarmClaimGo(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)
	ctx := context.Background()
	pgtest.MustExec(t, f.h.AdminDB, `
		INSERT INTO org_settings (org_id, max_concurrent_runs) VALUES ($1, 1)
		ON CONFLICT (org_id) DO UPDATE SET max_concurrent_runs = EXCLUDED.max_concurrent_runs`, f.orgID)

	if f.x.letGoWarmBehindOrgCaps(ctx) {
		t.Fatal("let a warm claim go with nothing queued behind the cap")
	}
	assertStillWarm(t, f)

	other := queueOtherTask(t, f)
	id, epoch := f.x.executorIdentity()
	if got, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, id, epoch, f.x.claimPlacement(), db.DefaultClaimLease); err != nil || got != nil {
		t.Fatalf("claim behind the cap = (%+v, %v), want nothing", got, err)
	}
	if !f.x.letGoWarmBehindOrgCaps(ctx) {
		t.Fatal("did not let the warm claim go with work queued behind the org's cap")
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	got, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, id, epoch, f.x.claimPlacement(), db.DefaultClaimLease)
	if err != nil || got == nil || got.ID != other {
		t.Fatalf("claim after the let-go = (%+v, %v), want conversation %s", got, err, other)
	}
}
