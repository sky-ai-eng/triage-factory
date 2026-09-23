package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stopTestUser is the actor a user stop names. stop_requested_by is text with
// no foreign key, so any id stands in for a person.
const stopTestUser = "user-who-stopped"

// RunStopIntentConformance is the shared assertion suite for the stop intent:
// a request path records it and never writes status, every holder or
// dispatcher status write clears it, the park derives its reason from it, the
// claim gate refuses a conversation carrying it, the renewal reads it back, and
// the dispatcher settles it for a conversation no live claim holds. It runs on
// the claim-lease fixture, whose staged steps are claimable delegations under
// a running blueprint.
func RunStopIntentConformance(t *testing.T, mk ClaimLeaseFactory) {
	t.Helper()
	ctx := context.Background()

	claim := func(t *testing.T, f ClaimLeaseFixture, conversationID string) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil {
			t.Fatalf("ClaimNextConversation: %v", err)
		}
		if got == nil || got.ID != conversationID {
			t.Fatalf("ClaimNextConversation = %+v, want conversation %s", got, conversationID)
		}
		return got
	}
	get := func(t *testing.T, f ClaimLeaseFixture, conversationID string) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = (%+v, %v)", conversationID, got, err)
		}
		return got
	}
	request := func(t *testing.T, f ClaimLeaseFixture, conversationID, by string) {
		t.Helper()
		ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, conversationID, by, "")
		if err != nil || !ok {
			t.Fatalf("RequestStopSystem(%s, by=%q) = (%v, %v), want (true, nil)", conversationID, by, ok, err)
		}
	}
	assertNoIntent := func(t *testing.T, f ClaimLeaseFixture, conversationID, after string) {
		t.Helper()
		if got := get(t, f, conversationID); got.StopRequestedAt != nil || got.StopRequestedBy != "" {
			t.Errorf("after %s: stop intent = (%v, %q), want cleared", after, got.StopRequestedAt, got.StopRequestedBy)
		}
	}
	claimOutcome := func(t *testing.T, f ClaimLeaseFixture, claimID string) (released bool, outcome string) {
		t.Helper()
		c, err := f.Stores.ConversationQueue.ClaimByIDSystem(ctx, claimID)
		if err != nil || c == nil {
			t.Fatalf("ClaimByIDSystem(%s) = (%+v, %v)", claimID, c, err)
		}
		return c.ReleasedAt != nil, c.Outcome
	}
	settledFor := func(t *testing.T, f ClaimLeaseFixture, conversationID string) (db.SettledStop, bool) {
		t.Helper()
		settled, err := f.Stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx)
		if err != nil {
			t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
		}
		for _, st := range settled {
			if st.ConversationID == conversationID {
				return st, true
			}
		}
		return db.SettledStop{}, false
	}
	runOf := func(t *testing.T, f ClaimLeaseFixture, conversationID string) *domain.BlueprintRun {
		t.Helper()
		br, err := f.Stores.Blueprints.GetRunSystem(ctx, f.OrgID, get(t, f, conversationID).BlueprintRunID)
		if err != nil || br == nil {
			t.Fatalf("GetRunSystem for %s = (%+v, %v)", conversationID, br, err)
		}
		return br
	}

	t.Run("Request_SetsTheIntentAndLeavesStatusAlone", func(t *testing.T) {
		f := mk(t)
		queued, _ := f.StageStep(t)
		request(t, f, queued, stopTestUser)
		got := get(t, f, queued)
		if got.StopRequestedAt == nil || got.StopRequestedBy != stopTestUser {
			t.Errorf("mid-flight intent = (%v, %q), want set by %s", got.StopRequestedAt, got.StopRequestedBy, stopTestUser)
		}
		if got.Status != domain.StatusQueued {
			t.Errorf("status = %q, want queued (a request writes no status)", got.Status)
		}

		// An `open` row takes the intent too: a follow-up could re-arm it, and
		// the stop has to be able to reach it first.
		parked, _ := f.StageStep(t)
		conv := claim(t, f, parked)
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, parked, conv.ClaimID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		request(t, f, parked, "")
		if got := get(t, f, parked); got.StopRequestedAt == nil || got.StopRequestedBy != "" || got.Status != domain.StatusOpen {
			t.Errorf("open row after a system request = (status %q, intent %v, by %q), want (open, set, system)", got.Status, got.StopRequestedAt, got.StopRequestedBy)
		}
	})

	t.Run("Request_IsIdempotentAndKeepsTheFirstActor", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		request(t, f, id, stopTestUser)
		first := get(t, f, id)
		request(t, f, id, "")
		second := get(t, f, id)
		if second.StopRequestedBy != stopTestUser {
			t.Errorf("actor after a second request = %q, want the first's %q", second.StopRequestedBy, stopTestUser)
		}
		if first.StopRequestedAt == nil || second.StopRequestedAt == nil || !second.StopRequestedAt.Equal(*first.StopRequestedAt) {
			t.Errorf("time after a second request = %v, want the first's %v", second.StopRequestedAt, first.StopRequestedAt)
		}

		// A system stop records no actor, and a user request after it must
		// not supply one: the park reason is derived from the actor, so the
		// stop would read as the user's.
		sys, _ := f.StageStep(t)
		request(t, f, sys, "")
		request(t, f, sys, stopTestUser)
		if got := get(t, f, sys); got.StopRequestedAt == nil || got.StopRequestedBy != "" {
			t.Errorf("system-first intent after a user request = (%v, %q), want set with no actor", got.StopRequestedAt, got.StopRequestedBy)
		}
	})

	t.Run("Request_RefusesATerminalRow", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		if _, err := f.Stores.Conversations.CompleteForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("complete: %v", err)
		}
		ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, id, stopTestUser, "")
		if err != nil || ok {
			t.Errorf("RequestStopSystem on a terminal row = (%v, %v), want (false, nil)", ok, err)
		}
		assertNoIntent(t, f, id, "a refused request")
	})

	t.Run("Park_DerivesTheReasonFromThePendingIntent", func(t *testing.T) {
		for _, tc := range []struct {
			name, by string
			park     db.Park
			want     domain.ParkReason
			outcome  string
		}{
			{"user stop over an idle park", stopTestUser, db.ParkIdle(), domain.ParkReasonUserCancelled, "parked"},
			{"system stop over a deliberate park", "", db.ParkStopped(domain.ParkReasonBlueprintCancelled, ""), domain.ParkReasonSystemCancelled, "cancelled"},
			{"user stop over a deliberate park", stopTestUser, db.ParkStopped(domain.ParkReasonUserCancelled, ""), domain.ParkReasonUserCancelled, "cancelled"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := mk(t)
				id, _ := f.StageStep(t)
				conv := claim(t, f, id)
				request(t, f, id, tc.by)
				if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, tc.park); err != nil || !ok {
					t.Fatalf("park: (%v, %v)", ok, err)
				}
				got := get(t, f, id)
				if got.Status != domain.StatusOpen || got.ParkReason != tc.want {
					t.Errorf("after the park = (%q, %q), want (open, %q)", got.Status, got.ParkReason, tc.want)
				}
				assertNoIntent(t, f, id, "the park")
				if released, outcome := claimOutcome(t, f, conv.ClaimID); !released || outcome != tc.outcome {
					t.Errorf("claim after the park = (released %v, %q), want (true, %q)", released, outcome, tc.outcome)
				}
			})
		}

		// No intent: the caller's reason stands.
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, db.ParkStopped(domain.ParkReasonLaunchFailed, "")); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		if got := get(t, f, id); got.ParkReason != domain.ParkReasonLaunchFailed {
			t.Errorf("park_reason with no intent = %q, want the caller's launch_failed", got.ParkReason)
		}
	})

	t.Run("HolderTerminals_ClearTheIntent", func(t *testing.T) {
		f := mk(t)
		done, _ := f.StageStep(t)
		conv := claim(t, f, done)
		request(t, f, done, stopTestUser)
		if _, err := f.Stores.Conversations.CompleteForClaimSystem(ctx, f.OrgID, done, conv.ClaimID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("complete: %v", err)
		}
		assertNoIntent(t, f, done, "CompleteForClaimSystem")

		failed, _ := f.StageStep(t)
		conv = claim(t, f, failed)
		request(t, f, failed, stopTestUser)
		if ok, err := f.Stores.Conversations.MarkFailedIfActiveForClaimSystem(ctx, f.OrgID, failed, conv.ClaimID, string(domain.ConversationFailureCrash)); err != nil || !ok {
			t.Fatalf("mark failed: (%v, %v)", ok, err)
		}
		assertNoIntent(t, f, failed, "MarkFailedIfActiveForClaimSystem")
	})

	t.Run("Resume_ClearsTheIntent", func(t *testing.T) {
		// A follow-up re-arms: the stop was asked for, and the message after
		// it is the user asking for the work again.
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		request(t, f, id, stopTestUser)
		if ok, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, id); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume: (%v, %v)", ok, err)
		}
		assertNoIntent(t, f, id, "MarkQueuedForResume")
	})

	t.Run("ClaimGate_RefusesAStopRequestedConversation", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		request(t, f, id, stopTestUser)
		got, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || got != nil {
			t.Fatalf("ClaimNextConversation with the only candidate stop-requested = (%+v, %v), want (nil, nil)", got, err)
		}

		// Settled and re-armed, it is claimable again.
		if _, ok := settledFor(t, f, id); !ok {
			t.Fatal("the settlement did not take the stop-requested conversation")
		}
		if ok, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, id); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume: (%v, %v)", ok, err)
		}
		claim(t, f, id)
	})

	t.Run("Renewal_ReadsThePendingStopBack", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		r, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, id, conv.ClaimID, testClaimLease)
		if err != nil || r.StopRequested || r.StopRequestedBy != "" {
			t.Fatalf("renewal with no stop = (%+v, %v), want no stop", r, err)
		}
		request(t, f, id, stopTestUser)
		r, err = f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, id, conv.ClaimID, testClaimLease)
		if err != nil || !r.StopRequested || r.StopRequestedBy != stopTestUser {
			t.Errorf("renewal after a user stop = (%+v, %v), want StopRequested by %s", r, err, stopTestUser)
		}
	})

	t.Run("Settle_ParksAQueuedRowAndLeavesAPlainStoppedRunRunning", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		request(t, f, id, stopTestUser)
		st, ok := settledFor(t, f, id)
		if !ok {
			t.Fatal("the settlement did not take an unclaimed stop-requested conversation")
		}
		if st.OrgID != f.OrgID || st.BlueprintRunID != "" {
			t.Errorf("settled = %+v, want this org and no run cancelled (a plain stop)", st)
		}
		got := get(t, f, id)
		if got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("after settlement = (%q, %q), want (open, user_cancelled)", got.Status, got.ParkReason)
		}
		assertNoIntent(t, f, id, "the settlement")
		if br := runOf(t, f, id); br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("run after a plain stop = %q, want running (what keeps the step resumable)", br.Status)
		}
		// Settled once: a second pass matches nothing.
		if _, again := settledFor(t, f, id); again {
			t.Error("a second settlement pass took a row the first already settled")
		}
	})

	t.Run("Settle_ReParksAnOpenRowWithTheSystemReason", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		request(t, f, id, "")
		if _, ok := settledFor(t, f, id); !ok {
			t.Fatal("the settlement did not take an open stop-requested conversation")
		}
		if got := get(t, f, id); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonSystemCancelled {
			t.Errorf("after settlement = (%q, %q), want (open, system_cancelled)", got.Status, got.ParkReason)
		}
		assertNoIntent(t, f, id, "the settlement")
	})

	t.Run("Settle_ClearsAStaleIntentOnATerminalRowOnly", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		if _, err := f.Stores.Conversations.CompleteForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("complete: %v", err)
		}
		br := runOf(t, f, id)
		if _, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, br.ID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}
		f.StageStaleStopIntent(t, id, "completed", stopTestUser)
		st, ok := settledFor(t, f, id)
		if !ok {
			t.Fatal("the settlement did not take a terminal row carrying a stale intent")
		}
		if st.BlueprintRunID != "" {
			t.Errorf("settled = %+v, want no run cancelled (the conversation concluded before the stop)", st)
		}
		got := get(t, f, id)
		if got.Status != "completed" || got.ParkReason != "" {
			t.Errorf("after settlement = (%q, %q), want (completed, none)", got.Status, got.ParkReason)
		}
		assertNoIntent(t, f, id, "the settlement")
		if br := runOf(t, f, id); br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("run = %q, want running (not this settlement's to cancel)", br.Status)
		}
	})

	t.Run("Settle_CancelsACancelRequestedRun", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		br := runOf(t, f, id)
		if _, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, br.ID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}
		request(t, f, id, stopTestUser)
		st, ok := settledFor(t, f, id)
		if !ok {
			t.Fatal("the settlement did not take the stop-requested step")
		}
		if st.BlueprintRunID != br.ID || st.StepIndex == nil || *st.StepIndex != 0 {
			t.Errorf("settled = %+v, want run %s cancelled at step 0", st, br.ID)
		}
		after := runOf(t, f, id)
		if after.Status != domain.BlueprintRunStatusCancelled || after.AbortReason != "user_cancelled" ||
			after.AbortedAtStep == nil || *after.AbortedAtStep != 0 || after.CompletedAt == nil {
			t.Errorf("run after settlement = (%q, %q, step %v, completed %v), want (cancelled, user_cancelled, 0, stamped)",
				after.Status, after.AbortReason, after.AbortedAtStep, after.CompletedAt)
		}
		if got := get(t, f, id); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("step after settlement = (%q, %q), want (open, user_cancelled)", got.Status, got.ParkReason)
		}
	})

	t.Run("Settle_SkipsARowALiveClaimHolds", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		request(t, f, id, stopTestUser)
		if _, ok := settledFor(t, f, id); ok {
			t.Fatal("the settlement took a row its holder is still driving")
		}
		got := get(t, f, id)
		if got.StopRequestedAt == nil {
			t.Error("the intent was cleared on a held row; the holder settles it")
		}
		if released, _ := claimOutcome(t, f, conv.ClaimID); released {
			t.Error("the settlement released a live claim")
		}
	})
}
