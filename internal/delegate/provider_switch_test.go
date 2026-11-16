package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProviderSwitch_HandsTheClaimBackAtOnce: an engagement a person's
// message moved onto another provider's model goes back to the queue
// mid-flight, released requeued_provider_switch, with no wait and no budget
// spent, so the next claim seals that provider and runs at once.
func TestProviderSwitch_HandsTheClaimBackAtOnce(t *testing.T) {
	f := newStallFixture(t, "r-provider-switch", activityTimings{idle: time.Hour})
	result := agentloop.Result{Kind: agentloop.ResultProviderSwitch, Model: "us.anthropic.claude-sonnet-5-5"}

	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", runmode.LocalDefaultUserID, time.Now(), result, nil)
	if !disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want handed back and unfenced", disp)
	}

	var status, parkReason string
	var waits bool
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, ''), next_attempt_at IS NOT NULL FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&status, &parkReason, &waits); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "" || parkReason != "" {
		t.Errorf("conversation = (%q, %q), want mid-flight with no park", status, parkReason)
	}
	if waits {
		t.Error("the hand-back set next_attempt_at; the next claim may start at once")
	}
	if got := claimOutcome(t, f, f.claimID); got != db.HandBackProviderSwitch {
		t.Errorf("claim outcome = %q, want %s", got, db.HandBackProviderSwitch)
	}
	if p, ok := db.HandBackPolicyFor(db.HandBackProviderSwitch); !ok || p.Budget != db.BudgetNone || p.Delay(3) != 0 {
		t.Errorf("policy = (%+v, %v), want no budget and no wait", p, ok)
	}

	next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute)
	if err != nil || next == nil || next.ID != f.conversationID {
		t.Fatalf("successor claim = (%+v, %v), want conversation %s at once", next, err, f.conversationID)
	}
	if next.JailLosses != 0 || next.LostEngagements != 0 || next.SetupFailures != 0 || next.UpstreamHandBacks != 0 {
		t.Errorf("successor's budgets = (jail %d, lost %d, setup %d, upstream %d), want none spent",
			next.JailLosses, next.LostEngagements, next.SetupFailures, next.UpstreamHandBacks)
	}
}

// TestWarmResume_KeepsTheEngagementForASwitchableModel: a warm engagement
// takes a message on a model its credentials can call, which its engine
// moves to at the next call, and lets the claim go for a model another
// provider serves, for a cold claim to seal.
func TestWarmResume_KeepsTheEngagementForASwitchableModel(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	const (
		engagement = "claude-sonnet-5-5"
		sameProv   = "claude-fable-5-2"
		otherProv  = "us.anthropic.claude-sonnet-5-5"
	)
	for _, tc := range []struct {
		name  string
		model string
		keeps bool
	}{
		{"its own model", engagement, true},
		{"a model on its provider", sameProv, true},
		{"a model on another provider", otherProv, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			connectProviders(t, database, true, true)
			seedConversation(t, database, "r-warm-switch", "", "")
			brID := blueprintRunIDForConversation(t, database, "r-warm-switch")
			finishBlueprint(t, database, brID, "completed", 0)
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, engagement)
			if _, err := s.conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID,
				pendingUserInput("r-warm-switch", runmode.LocalDefaultUserID, "next", domain.TurnSettings{Model: tc.model})); err != nil {
				t.Fatalf("insert person row: %v", err)
			}
			n := nativeTail{
				orgID:          runmode.LocalDefaultOrgID,
				conversationID: "r-warm-switch",
				cfg:            runConfig{orgID: runmode.LocalDefaultOrgID, teamID: runmode.LocalDefaultTeamID, blueprintRunID: brID},
				params: agentloop.Params{
					Model:            engagement,
					SwitchableModels: s.switchableModels(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, engagement),
				},
			}
			if got := s.warmResumeModelHolds(context.Background(), n) != nil; got != tc.keeps {
				t.Errorf("warm resume kept the engagement = %v, want %v (switchable %v)", got, tc.keeps, n.params.SwitchableModels)
			}
		})
	}
}
