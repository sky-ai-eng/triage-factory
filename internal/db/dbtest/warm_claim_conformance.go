package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunWarmClaimConformance is the shared assertion suite for warm claims
// (domain.ClaimPhaseWarm): a claim kept after its engagement's turn ended and
// wrote its verdict. A warm claim is a claim fact only. The conversation reads
// as the turn left it, and the claim holds the conversation and the task's
// tree until it is resumed or let go.
func RunWarmClaimConformance(t *testing.T, mk ClaimLeaseFactory) {
	t.Helper()
	ctx := context.Background()

	// warmStep stages and claims one step, finishes its blueprint run, and
	// concludes its turn keeping the claim warm, as an engagement whose turn
	// opened a window does.
	warmStep := func(t *testing.T, f ClaimLeaseFixture) (conv *domain.Conversation, taskID string) {
		t.Helper()
		conversationID, taskID := f.StageStep(t)
		claimed, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || claimed == nil || claimed.ID != conversationID {
			t.Fatalf("ClaimNextConversation = (%+v, %v), want conversation %s", claimed, err, conversationID)
		}
		if _, err := f.Stores.Conversations.ConcludeKeepingClaimSystem(ctx, f.OrgID, conversationID, claimed.ClaimID, 1200, 3, "fixed it", string(domain.ConversationOutcomeContinue), ""); err != nil {
			t.Fatalf("ConcludeKeepingClaimSystem: %v", err)
		}
		// A warm claim's blueprint run is not running. Finished after the
		// verdict, because finishing a run parks a step still mid-flight.
		if _, err := f.Stores.Blueprints.MarkRunStatusSystem(ctx, f.OrgID, claimed.BlueprintRunID, domain.BlueprintRunStatusCompleted, "", nil); err != nil {
			t.Fatalf("MarkRunStatusSystem(completed): %v", err)
		}
		return claimed, taskID
	}
	get := func(t *testing.T, f ClaimLeaseFixture, conversationID string) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = (%+v, %v)", conversationID, got, err)
		}
		return got
	}
	claimState := func(t *testing.T, f ClaimLeaseFixture, claimID string) (released bool, outcome string) {
		t.Helper()
		c, err := f.Stores.ConversationQueue.ClaimByIDSystem(ctx, claimID)
		if err != nil || c == nil {
			t.Fatalf("ClaimByIDSystem(%s) = (%+v, %v)", claimID, c, err)
		}
		return c.ReleasedAt != nil, c.Outcome
	}
	// noSecondClaim checks that no executor can claim anything: the warm
	// claim holds its conversation whatever the conversation's status.
	noSecondClaim := func(t *testing.T, f ClaimLeaseFixture) {
		t.Helper()
		for _, exec := range []string{claimLeaseExecutor, takeoverOtherExecutor} {
			if next, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, exec, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease); err != nil || next != nil {
				t.Errorf("ClaimNextConversation(%s) = (%+v, %v), want none", exec, next, err)
			}
		}
	}
	// warm reports whether the task holds a warm claim: a live claim that no
	// engaged-claim read counts.
	warm := func(t *testing.T, f ClaimLeaseFixture, taskID string) bool {
		t.Helper()
		active, err := f.Stores.Conversations.HasActiveClaimForTaskSystem(ctx, f.OrgID, taskID)
		if err != nil {
			t.Fatalf("HasActiveClaimForTaskSystem: %v", err)
		}
		engaged, err := f.Stores.Conversations.HasEngagedClaimForTaskSystem(ctx, f.OrgID, taskID)
		if err != nil {
			t.Fatalf("HasEngagedClaimForTaskSystem: %v", err)
		}
		return active && !engaged
	}

	t.Run("ConcludeKeepingClaim_WritesTheVerdictAndKeepsTheClaim", func(t *testing.T) {
		f := mk(t)
		conversationID, taskID := f.StageStep(t)
		claimed, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimNextConversation = (%+v, %v)", claimed, err)
		}
		got, err := f.Stores.Conversations.ConcludeKeepingClaimSystem(ctx, f.OrgID, conversationID, claimed.ClaimID, 1200, 3, "fixed it", string(domain.ConversationOutcomeContinue), "")
		if err != nil {
			t.Fatalf("ConcludeKeepingClaimSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "ConcludeKeepingClaimSystem", *got, func() (*domain.Conversation, error) {
			return f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		})
		// The conversation reads exactly as a turn-end verdict leaves it.
		if got.Status != domain.StatusOpen || !got.Concluded() || got.Outcome != string(domain.ConversationOutcomeContinue) ||
			got.ResultSummary != "fixed it" || intOr0(got.DurationMs) != 1200 || intOr0(got.NumTurns) != 3 {
			t.Errorf("conversation = {status %q, concluded %v, outcome %q, summary %q, duration %d, turns %d}, want {open, true, continue, fixed it, 1200, 3}",
				got.Status, got.Concluded(), got.Outcome, got.ResultSummary, intOr0(got.DurationMs), intOr0(got.NumTurns))
		}
		if released, _ := claimState(t, f, claimed.ClaimID); released {
			t.Error("the claim was released; it should be kept warm")
		}
		if !warm(t, f, taskID) {
			t.Error("the claim is not warm")
		}
		noSecondClaim(t, f)
	})

	t.Run("ConcludeKeepingClaim_RefusedOnAReleasedClaim", func(t *testing.T) {
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		claimed, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimNextConversation = (%+v, %v)", claimed, err)
		}
		f.SetLease(t, claimed.ClaimID, -time.Second)
		if released, err := f.Stores.ConversationQueue.ReleaseExpiredClaimSystem(ctx, f.OrgID, conversationID, claimed.ClaimID); err != nil || !released {
			t.Fatalf("ReleaseExpiredClaimSystem = (%v, %v)", released, err)
		}
		if _, err := f.Stores.Conversations.ConcludeKeepingClaimSystem(ctx, f.OrgID, conversationID, claimed.ClaimID, 1, 1, "late", string(domain.ConversationOutcomeContinue), ""); !errors.Is(err, db.ErrClaimReleased) {
			t.Errorf("ConcludeKeepingClaimSystem on a released claim = %v, want ErrClaimReleased", err)
		}
		if got := get(t, f, conversationID); got.Concluded() {
			t.Error("a refused write concluded the conversation")
		}
	})

	t.Run("Resume_KeepsTheWarmClaimAndTheEngagementTakesItBack", func(t *testing.T) {
		f := mk(t)
		c, taskID := warmStep(t, f)
		flipped, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, c.ID)
		if err != nil || !flipped {
			t.Fatalf("MarkQueuedForResume = (%v, %v), want (true, nil)", flipped, err)
		}
		if released, _ := claimState(t, f, c.ClaimID); released {
			t.Fatal("the resume released the warm claim")
		}
		if got := get(t, f, c.ID); got.Status != domain.StatusQueued {
			t.Errorf("status after the resume = %q, want queued: resumed, and nothing running yet", got.Status)
		}
		noSecondClaim(t, f)
		// The claim's engagement is about to drive the conversation, so the
		// claim holds the task from the resume on.
		if warm(t, f, taskID) {
			t.Error("a warm claim whose conversation was resumed does not hold the task")
		}

		resumed, err := f.Stores.Conversations.ResumeWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID)
		if err != nil || !resumed {
			t.Fatalf("ResumeWarmClaimSystem = (%v, %v), want (true, nil)", resumed, err)
		}
		if got := get(t, f, c.ID); got.Status != domain.StatusRunning {
			t.Errorf("status once the engagement took it back = %q, want running", got.Status)
		}
		if warm(t, f, taskID) {
			t.Error("the claim is still warm after its engagement took the conversation back")
		}
	})

	t.Run("ResumeWarmClaim_ResumesAConversationNoFollowUpFlipped", func(t *testing.T) {
		f := mk(t)
		c, _ := warmStep(t, f)
		resumed, err := f.Stores.Conversations.ResumeWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID)
		if err != nil || !resumed {
			t.Fatalf("ResumeWarmClaimSystem = (%v, %v), want (true, nil)", resumed, err)
		}
		if got := get(t, f, c.ID); got.Status != domain.StatusRunning {
			t.Errorf("status = %q, want running", got.Status)
		}
	})

	t.Run("ResumeWarmClaim_RefusedOnAReleasedClaim", func(t *testing.T) {
		f := mk(t)
		c, _ := warmStep(t, f)
		if released, err := f.Stores.Conversations.ReleaseWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID); err != nil || !released {
			t.Fatalf("ReleaseWarmClaimSystem = (%v, %v)", released, err)
		}
		if _, err := f.Stores.Conversations.ResumeWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID); !errors.Is(err, db.ErrClaimReleased) {
			t.Errorf("ResumeWarmClaimSystem on a released claim = %v, want ErrClaimReleased", err)
		}
	})

	// The engagement resumes without the claim scan, so a conversation that
	// changed after the follow-up's flip is checked again: once ended, or
	// with a stop pending, it is not resumed and the claim stays warm for the
	// engagement to let go.
	for _, after := range []struct {
		name  string
		apply func(t *testing.T, f ClaimLeaseFixture, conversationID string)
	}{
		{"Ended", func(t *testing.T, f ClaimLeaseFixture, conversationID string) {
			if _, err := f.Stores.Conversations.EndConversationSystem(ctx, f.OrgID, conversationID, domain.EndedDelegated); err != nil {
				t.Fatalf("EndConversationSystem: %v", err)
			}
		}},
		{"StopPending", func(t *testing.T, f ClaimLeaseFixture, conversationID string) {
			if requested, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, conversationID, "", "", ""); err != nil || !requested {
				t.Fatalf("RequestStopSystem = (%v, %v), want (true, nil)", requested, err)
			}
		}},
	} {
		t.Run("ResumeWarmClaim_RefusedAfterTheFlip_"+after.name, func(t *testing.T) {
			f := mk(t)
			c, taskID := warmStep(t, f)
			if flipped, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, c.ID); err != nil || !flipped {
				t.Fatalf("MarkQueuedForResume = (%v, %v), want (true, nil)", flipped, err)
			}
			after.apply(t, f, c.ID)
			resumed, err := f.Stores.Conversations.ResumeWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID)
			if err != nil || resumed {
				t.Fatalf("ResumeWarmClaimSystem = (%v, %v), want (false, nil)", resumed, err)
			}
			active, err := f.Stores.Conversations.HasActiveClaimForTaskSystem(ctx, f.OrgID, taskID)
			if err != nil || !active {
				t.Fatalf("HasActiveClaimForTaskSystem = (%v, %v), want the claim kept", active, err)
			}
			if released, err := f.Stores.Conversations.ReleaseWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID); err != nil || !released {
				t.Errorf("ReleaseWarmClaimSystem after the refusal = (%v, %v), want the claim still warm to let go", released, err)
			}
		})
	}

	t.Run("ReleaseWarmClaim_ReleasesCompletedAndLeavesTheConversation", func(t *testing.T) {
		f := mk(t)
		c, taskID := warmStep(t, f)
		before := get(t, f, c.ID)
		released, err := f.Stores.Conversations.ReleaseWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID)
		if err != nil || !released {
			t.Fatalf("ReleaseWarmClaimSystem = (%v, %v), want (true, nil)", released, err)
		}
		if released, outcome := claimState(t, f, c.ClaimID); !released || outcome != "completed" {
			t.Errorf("claim = (released %v, %q), want (true, completed)", released, outcome)
		}
		after := get(t, f, c.ID)
		if after.Status != before.Status || after.Outcome != before.Outcome || after.ResultSummary != before.ResultSummary ||
			!after.CompletedAt.Equal(*before.CompletedAt) || intOr0(after.DurationMs) != intOr0(before.DurationMs) || intOr0(after.NumTurns) != intOr0(before.NumTurns) {
			t.Errorf("conversation moved on the release: before {%q %q %q %v %d %d}, after {%q %q %q %v %d %d}",
				before.Status, before.Outcome, before.ResultSummary, before.CompletedAt, intOr0(before.DurationMs), intOr0(before.NumTurns),
				after.Status, after.Outcome, after.ResultSummary, after.CompletedAt, intOr0(after.DurationMs), intOr0(after.NumTurns))
		}
		if active, err := f.Stores.Conversations.HasActiveClaimForTaskSystem(ctx, f.OrgID, taskID); err != nil || active {
			t.Errorf("HasActiveClaimForTaskSystem after the release = (%v, %v), want (false, nil)", active, err)
		}
		if again, err := f.Stores.Conversations.ReleaseWarmClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID); err != nil || again {
			t.Errorf("a second ReleaseWarmClaimSystem = (%v, %v), want (false, nil)", again, err)
		}
	})

	t.Run("ReleaseWarmClaim_LeavesAClaimThatIsNotWarm", func(t *testing.T) {
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		claimed, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimNextConversation = (%+v, %v)", claimed, err)
		}
		if released, err := f.Stores.Conversations.ReleaseWarmClaimSystem(ctx, f.OrgID, conversationID, claimed.ClaimID); err != nil || released {
			t.Errorf("ReleaseWarmClaimSystem on a working claim = (%v, %v), want (false, nil)", released, err)
		}
	})

	// Each path that releases a claim whose holder is gone releases a warm one
	// as completed, and leaves its conversation as the turn wrote it.
	for _, path := range []struct {
		name     string
		executor string
		epoch    int64
		release  func(t *testing.T, f ClaimLeaseFixture, c *domain.Conversation)
	}{
		{"Takeover", takeoverOtherExecutor, 1, func(t *testing.T, f ClaimLeaseFixture, c *domain.Conversation) {
			f.SetLease(t, c.ClaimID, -time.Second)
			if refs, err := f.Stores.ConversationQueue.TakeOverExpiredClaimsSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch, 100); err != nil || len(refs) != 1 {
				t.Fatalf("TakeOverExpiredClaimsSystem = (%+v, %v), want one", refs, err)
			}
		}},
		{"OwnRelease", claimLeaseExecutor, claimLeaseBootEpoch, func(t *testing.T, f ClaimLeaseFixture, c *domain.Conversation) {
			f.SetLease(t, c.ClaimID, -time.Second)
			if released, err := f.Stores.ConversationQueue.ReleaseExpiredClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID); err != nil || !released {
				t.Fatalf("ReleaseExpiredClaimSystem = (%v, %v), want (true, nil)", released, err)
			}
		}},
		{"BootReset", claimLeaseExecutor, claimLeaseBootEpoch - 1, func(t *testing.T, f ClaimLeaseFixture, c *domain.Conversation) {
			if n, err := f.Stores.ConversationQueue.ResetProcessingConversations(ctx, claimLeaseExecutor, claimLeaseBootEpoch); err != nil || n != 1 {
				t.Fatalf("ResetProcessingConversations = (%d, %v), want (1, nil)", n, err)
			}
		}},
	} {
		t.Run(path.name+"_ReleasesALapsedWarmClaimCompleted", func(t *testing.T) {
			f := mk(t)
			conversationID, _ := f.StageStep(t)
			claimed, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, path.executor, path.epoch, db.ClaimPlacement{}, testClaimLease)
			if err != nil || claimed == nil || claimed.ID != conversationID {
				t.Fatalf("ClaimNextConversation = (%+v, %v)", claimed, err)
			}
			if _, err := f.Stores.Conversations.ConcludeKeepingClaimSystem(ctx, f.OrgID, conversationID, claimed.ClaimID, 1200, 3, "fixed it", string(domain.ConversationOutcomeContinue), ""); err != nil {
				t.Fatalf("ConcludeKeepingClaimSystem: %v", err)
			}
			if _, err := f.Stores.Blueprints.MarkRunStatusSystem(ctx, f.OrgID, claimed.BlueprintRunID, domain.BlueprintRunStatusCompleted, "", nil); err != nil {
				t.Fatalf("MarkRunStatusSystem: %v", err)
			}
			before := get(t, f, conversationID)
			path.release(t, f, claimed)
			if released, outcome := claimState(t, f, claimed.ClaimID); !released || outcome != "completed" {
				t.Errorf("lapsed warm claim = (released %v, %q), want (true, completed): its turn had ended, so nothing was lost", released, outcome)
			}
			after := get(t, f, conversationID)
			if after.Status != before.Status || after.Outcome != before.Outcome || !after.CompletedAt.Equal(*before.CompletedAt) {
				t.Errorf("conversation after the release = {%q %q %v}, want it as the turn left it {%q %q %v}",
					after.Status, after.Outcome, after.CompletedAt, before.Status, before.Outcome, before.CompletedAt)
			}
			if next, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, takeoverOtherExecutor, 1, db.ClaimPlacement{}, testClaimLease); err != nil || next != nil {
				t.Errorf("ClaimNextConversation after the release = (%+v, %v), want none: a concluded turn is not re-driven", next, err)
			}
		})
	}

	t.Run("WarmWaitState_SeesAPersonsWaitingMessageAndTheEnd", func(t *testing.T) {
		f := mk(t)
		c, _ := warmStep(t, f)
		state := func(t *testing.T) db.WarmWaitState {
			t.Helper()
			st, err := f.Stores.Conversations.WarmWaitStateSystem(ctx, f.OrgID, c.ID)
			if err != nil {
				t.Fatalf("WarmWaitStateSystem: %v", err)
			}
			return st
		}
		if st := state(t); st != (db.WarmWaitState{}) {
			t.Fatalf("state after the turn = %+v, want nothing waiting and not ended", st)
		}

		person := get(t, f, c.ID).CreatorUserID
		pending := false
		insert := func(t *testing.T, subtype string) int64 {
			t.Helper()
			id, err := f.Stores.Conversations.InsertMessageSystem(ctx, f.OrgID, &domain.Message{
				ConversationID: c.ID, UserID: person, Role: "user", Content: "x", Subtype: subtype,
				Delivered: &pending, WindowState: domain.MessageWindowActive,
			})
			if err != nil {
				t.Fatalf("InsertMessageSystem(%q): %v", subtype, err)
			}
			return id
		}
		// A row TF wrote is not a person's message, whoever it names.
		insert(t, domain.MessageSubtypeInjectionEvent)
		if st := state(t); st.PersonMessageWaiting {
			t.Error("an event injection read as a person's message waiting")
		}
		id := insert(t, "")
		if st := state(t); !st.PersonMessageWaiting {
			t.Error("a person's undelivered message did not read as waiting")
		}
		if err := f.Stores.Conversations.MarkDeliveredForClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, []int{int(id)}, ""); err != nil {
			t.Fatalf("MarkDeliveredForClaimSystem: %v", err)
		}
		if st := state(t); st.PersonMessageWaiting {
			t.Error("a delivered message still read as waiting")
		}

		if _, err := f.Stores.Conversations.EndConversationSystem(ctx, f.OrgID, c.ID, domain.EndedDelegated); err != nil {
			t.Fatalf("EndConversationSystem: %v", err)
		}
		if st := state(t); !st.Ended {
			t.Error("an ended conversation did not read as ended")
		}
	})

	t.Run("WarmWaitState_NoSuchConversation", func(t *testing.T) {
		f := mk(t)
		if _, err := f.Stores.Conversations.WarmWaitStateSystem(ctx, f.OrgID, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, db.ErrNoSuchConversation) {
			t.Errorf("WarmWaitStateSystem for no conversation = %v, want ErrNoSuchConversation", err)
		}
	})

	t.Run("Gauges_SkipAWarmClaim", func(t *testing.T) {
		f := mk(t)
		q := f.Stores.ConversationQueue
		c, _ := warmStep(t, f)
		age := 30 * time.Second
		if _, err := q.RenewClaimLeaseSystem(ctx, f.OrgID, c.ID, c.ClaimID, testClaimLease, db.ClaimActivity{Idle: age, CheckpointAge: &age}); err != nil {
			t.Fatalf("RenewClaimLeaseSystem: %v", err)
		}
		if idle, err := q.OldestIdleClaimSystem(ctx); err != nil || idle != 0 {
			t.Errorf("OldestIdleClaimSystem = (%s, %v), want 0s: a warm claim waits by design", idle, err)
		}
		if ckpt, err := q.OldestCheckpointAgeSystem(ctx); err != nil || ckpt != 0 {
			t.Errorf("OldestCheckpointAgeSystem = (%s, %v), want 0s", ckpt, err)
		}
	})
}

// intOr0 reads an optional count, nil as 0.
func intOr0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
