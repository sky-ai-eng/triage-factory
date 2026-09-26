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
		ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, conversationID, by, "", "")
		if err != nil || !ok {
			t.Fatalf("RequestStopSystem(%s, by=%q) = (%v, %v), want (true, nil)", conversationID, by, ok, err)
		}
	}
	requestStall := func(t *testing.T, f ClaimLeaseFixture, conversationID string) {
		t.Helper()
		ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, conversationID, "", "", domain.ParkReasonStalled)
		if err != nil || !ok {
			t.Fatalf("RequestStopSystem(%s, stalled) = (%v, %v), want (true, nil)", conversationID, ok, err)
		}
	}
	assertNoIntent := func(t *testing.T, f ClaimLeaseFixture, conversationID, after string) {
		t.Helper()
		if got := get(t, f, conversationID); got.StopRequestedAt != nil || got.StopRequestedBy != "" || got.StopRequestedReason != "" {
			t.Errorf("after %s: stop intent = (%v, %q, reason %q), want cleared", after, got.StopRequestedAt, got.StopRequestedBy, got.StopRequestedReason)
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
		ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, id, stopTestUser, "", "")
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
		r, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, id, conv.ClaimID, testClaimLease, db.ClaimActivity{})
		if err != nil || r.StopRequested || r.StopRequestedBy != "" {
			t.Fatalf("renewal with no stop = (%+v, %v), want no stop", r, err)
		}
		request(t, f, id, stopTestUser)
		r, err = f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, id, conv.ClaimID, testClaimLease, db.ClaimActivity{})
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

	t.Run("Request_StoresAReasonAndTheFirstStands", func(t *testing.T) {
		f := mk(t)
		stalled, _ := f.StageStep(t)
		requestStall(t, f, stalled)
		if got := get(t, f, stalled); got.StopRequestedAt == nil || got.StopRequestedReason != string(domain.ParkReasonStalled) || got.StopRequestedBy != "" {
			t.Errorf("stall intent = (%v, by %q, reason %q), want set, no actor, stalled", got.StopRequestedAt, got.StopRequestedBy, got.StopRequestedReason)
		}
		// A user stop after the stall keeps the stall's reason and actor.
		request(t, f, stalled, stopTestUser)
		if got := get(t, f, stalled); got.StopRequestedReason != string(domain.ParkReasonStalled) || got.StopRequestedBy != "" {
			t.Errorf("stall intent after a user request = (by %q, reason %q), want the stall's", got.StopRequestedBy, got.StopRequestedReason)
		}

		// A user stop first: the stall after it records no reason.
		user, _ := f.StageStep(t)
		request(t, f, user, stopTestUser)
		requestStall(t, f, user)
		if got := get(t, f, user); got.StopRequestedReason != "" || got.StopRequestedBy != stopTestUser {
			t.Errorf("user intent after a stall request = (by %q, reason %q), want the user's with no reason", got.StopRequestedBy, got.StopRequestedReason)
		}
	})

	t.Run("Park_DerivesTheStoredReasonFirst", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		conv := claim(t, f, id)
		requestStall(t, f, id)
		// The caller passes user_cancelled, as a holder that did not see the
		// stall's cause would; the stored reason wins.
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, id, conv.ClaimID, db.ParkStopped(domain.ParkReasonUserCancelled, "")); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		if got := get(t, f, id); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonStalled {
			t.Errorf("after the park = (%q, %q), want (open, stalled)", got.Status, got.ParkReason)
		}
		assertNoIntent(t, f, id, "the park")
		if released, outcome := claimOutcome(t, f, conv.ClaimID); !released || outcome != "cancelled" {
			t.Errorf("claim after the stall park = (released %v, %q), want (true, cancelled)", released, outcome)
		}

		// No intent: a holder that read the cause itself passes stalled, and
		// it lands as passed.
		g := mk(t)
		other, _ := g.StageStep(t)
		c := claim(t, g, other)
		if ok, err := g.Stores.Conversations.ParkOpenForClaimSystem(ctx, g.OrgID, other, c.ClaimID, db.ParkStopped(domain.ParkReasonStalled, "")); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		if got := get(t, g, other); got.ParkReason != domain.ParkReasonStalled {
			t.Errorf("park_reason with no intent = %q, want the caller's stalled", got.ParkReason)
		}
	})

	t.Run("Settle_DerivesTheStoredReasonForTheStepAndTheRun", func(t *testing.T) {
		f := mk(t)
		plain, _ := f.StageStep(t)
		requestStall(t, f, plain)
		if _, ok := settledFor(t, f, plain); !ok {
			t.Fatal("the settlement did not take the stalled conversation")
		}
		if got := get(t, f, plain); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonStalled {
			t.Errorf("plain stall after settlement = (%q, %q), want (open, stalled)", got.Status, got.ParkReason)
		}
		assertNoIntent(t, f, plain, "the settlement")
		if br := runOf(t, f, plain); br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("run behind a plain stall = %q, want running", br.Status)
		}

		cancelled, _ := f.StageStep(t)
		br := runOf(t, f, cancelled)
		if _, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, br.ID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}
		requestStall(t, f, cancelled)
		if st, ok := settledFor(t, f, cancelled); !ok || st.BlueprintRunID != br.ID {
			t.Fatalf("settled = (%+v, %v), want run %s cancelled", st, ok, br.ID)
		}
		if got := get(t, f, cancelled); got.ParkReason != domain.ParkReasonStalled {
			t.Errorf("stalled step under a cancel-requested run = %q, want stalled", got.ParkReason)
		}
		if after := runOf(t, f, cancelled); after.Status != domain.BlueprintRunStatusCancelled || after.AbortReason != string(domain.ParkReasonStalled) {
			t.Errorf("run after settlement = (%q, %q), want (cancelled, stalled)", after.Status, after.AbortReason)
		}
	})

	t.Run("StatusWrites_ClearTheReason", func(t *testing.T) {
		f := mk(t)
		done, _ := f.StageStep(t)
		conv := claim(t, f, done)
		requestStall(t, f, done)
		if _, err := f.Stores.Conversations.CompleteForClaimSystem(ctx, f.OrgID, done, conv.ClaimID, "completed", 0, 0, 0, "", "finish", "", ""); err != nil {
			t.Fatalf("complete: %v", err)
		}
		assertNoIntent(t, f, done, "CompleteForClaimSystem")

		failed, _ := f.StageStep(t)
		conv = claim(t, f, failed)
		requestStall(t, f, failed)
		if ok, err := f.Stores.Conversations.MarkFailedIfActiveForClaimSystem(ctx, f.OrgID, failed, conv.ClaimID, string(domain.ConversationFailureCrash)); err != nil || !ok {
			t.Fatalf("mark failed: (%v, %v)", ok, err)
		}
		assertNoIntent(t, f, failed, "MarkFailedIfActiveForClaimSystem")

		resumed, _ := f.StageStep(t)
		conv = claim(t, f, resumed)
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, resumed, conv.ClaimID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("park: (%v, %v)", ok, err)
		}
		requestStall(t, f, resumed)
		if ok, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, resumed); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume: (%v, %v)", ok, err)
		}
		assertNoIntent(t, f, resumed, "MarkQueuedForResume")

	})

	t.Run("Settle_ParksAReleasedStallAsStalled", func(t *testing.T) {
		// The holder wrote the stall's intent and then lost the claim before
		// its own park: the release writes no status and leaves the intent
		// whole, and the settlement parks the row with the stored reason.
		f := mk(t)
		id, _ := f.StageStep(t)
		claim(t, f, id)
		requestStall(t, f, id)
		if _, err := f.Stores.ConversationQueue.RequeueConversation(ctx, f.OrgID, id, db.RequeueSetupFailure, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		if got := get(t, f, id); got.StopRequestedAt == nil || got.StopRequestedReason != string(domain.ParkReasonStalled) {
			t.Fatalf("intent after the release = (%v, reason %q), want the stall's intact", got.StopRequestedAt, got.StopRequestedReason)
		}
		if _, ok := settledFor(t, f, id); !ok {
			t.Fatal("the settlement did not take the released stall")
		}
		if got := get(t, f, id); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonStalled {
			t.Errorf("after settlement = (%q, %q), want (open, stalled)", got.Status, got.ParkReason)
		}
		assertNoIntent(t, f, id, "the settlement")
	})

	t.Run("SettleForTask_SettlesThatTaskAlone", func(t *testing.T) {
		f := mk(t)
		mine, taskID := f.StageStep(t)
		other, otherTask := f.StageStep(t)
		if otherTask == taskID {
			t.Fatal("the fixture staged both steps on one task; the scope is untested")
		}
		request(t, f, mine, stopTestUser)
		request(t, f, other, stopTestUser)

		settled, err := f.Stores.ConversationQueue.SettleUnclaimedStopsForTaskSystem(ctx, f.OrgID, taskID)
		if err != nil {
			t.Fatalf("SettleUnclaimedStopsForTaskSystem: %v", err)
		}
		if len(settled) != 1 || settled[0].ConversationID != mine {
			t.Fatalf("settled = %+v, want only %s", settled, mine)
		}
		if got := get(t, f, mine); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("the task's step = (%q, %q), want (open, user_cancelled)", got.Status, got.ParkReason)
		}
		if got := get(t, f, other); got.StopRequestedAt == nil || got.Status != domain.StatusQueued {
			t.Errorf("another task's step = (status %q, intent %v), want untouched", got.Status, got.StopRequestedAt)
		}
	})
}
