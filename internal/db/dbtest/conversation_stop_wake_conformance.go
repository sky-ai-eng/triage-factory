package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stopWakeNote is the stop note the suite writes, the text a person's stop
// writes onto the transcript before it records the request.
const stopWakeNote = "Run stopped by the user. It may be resumed later."

// RunStopWakeConformance is the shared assertion suite for the wake position:
// a stop that ends with no turn run keeps the person messages in front of it
// from waking the conversation, a message sent after the stop wakes it as any
// message does, a park after a turn ran leaves the message waiting behind
// that turn to run next, and the pending-input read hands a resume the
// stopped message, the stop note, and the new message in that order. It runs
// on the claim-lease fixture, whose staged steps are claimable delegations
// under a running blueprint.
func RunStopWakeConformance(t *testing.T, mk ClaimLeaseFactory) {
	t.Helper()
	ctx := context.Background()

	claim := func(t *testing.T, f ClaimLeaseFixture) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil {
			t.Fatalf("ClaimNextConversation: %v", err)
		}
		return got
	}
	mustClaim := func(t *testing.T, f ClaimLeaseFixture, id string) *domain.Conversation {
		t.Helper()
		got := claim(t, f)
		if got == nil || got.ID != id {
			t.Fatalf("ClaimNextConversation = %+v, want conversation %s", got, id)
		}
		return got
	}
	mustNotClaim := func(t *testing.T, f ClaimLeaseFixture, why string) {
		t.Helper()
		if got := claim(t, f); got != nil {
			t.Fatalf("ClaimNextConversation = %s, want nothing claimable: %s", got.ID, why)
		}
	}
	get := func(t *testing.T, f ClaimLeaseFixture, id string) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, id)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = (%+v, %v)", id, got, err)
		}
		return got
	}
	park := func(t *testing.T, f ClaimLeaseFixture, c *domain.Conversation, p db.Park) {
		t.Helper()
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, p); err != nil || !ok {
			t.Fatalf("ParkOpenForClaimSystem(%s) = (%v, %v), want (true, nil)", c.ID, ok, err)
		}
	}
	// parked stages one step parked open with nothing waiting: claimed, then
	// parked as a turn that ended, so only a person's message wakes it.
	parked := func(t *testing.T, f ClaimLeaseFixture) string {
		t.Helper()
		id, _ := f.StageStep(t)
		park(t, f, mustClaim(t, f, id), db.ParkTurnIncomplete())
		mustNotClaim(t, f, "a parked conversation with nothing waiting")
		return id
	}
	send := func(t *testing.T, f ClaimLeaseFixture, id, content string) {
		t.Helper()
		pending := false
		if _, err := f.Stores.Conversations.InsertMessageSystem(ctx, f.OrgID, &domain.Message{
			ConversationID: id, Role: "user", Content: content,
			Delivered: &pending, WindowState: domain.MessageWindowActive,
		}); err != nil {
			t.Fatalf("InsertMessageSystem(%q): %v", content, err)
		}
	}
	// stop is a person's stop as the stop verb writes it: the delivered stop
	// note first, then the request.
	stop := func(t *testing.T, f ClaimLeaseFixture, id string) {
		t.Helper()
		if _, err := f.Stores.Conversations.InsertMessageSystem(ctx, f.OrgID, &domain.Message{
			ConversationID: id, Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: stopWakeNote,
		}); err != nil {
			t.Fatalf("InsertMessageSystem(stop note): %v", err)
		}
		if ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, id, stopTestUser, "", ""); err != nil || !ok {
			t.Fatalf("RequestStopSystem(%s) = (%v, %v), want (true, nil)", id, ok, err)
		}
	}
	settle := func(t *testing.T, f ClaimLeaseFixture, id string) {
		t.Helper()
		settled, err := f.Stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx)
		if err != nil {
			t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
		}
		for _, st := range settled {
			if st.ConversationID == id {
				return
			}
		}
		t.Fatalf("SettleUnclaimedStopsSystem settled %+v, want conversation %s among them", settled, id)
	}
	peek := func(t *testing.T, f ClaimLeaseFixture, id string) string {
		t.Helper()
		msg, _, ok, err := f.Stores.ConversationPendingInput.Peek(ctx, f.OrgID, id)
		if err != nil || !ok {
			t.Fatalf("Peek(%s) = (%q, %v, %v), want queued input", id, msg, ok, err)
		}
		return msg
	}
	join := func(parts ...string) string {
		out := parts[0]
		for _, p := range parts[1:] {
			out += db.PendingInputSeparator + p
		}
		return out
	}

	t.Run("Settle_TheMessageInFrontOfTheStopNoLongerWakes", func(t *testing.T) {
		f := mk(t)
		id := parked(t, f)
		send(t, f, id, "fix the null check")
		stop(t, f, id)
		mustNotClaim(t, f, "a stop is pending")
		settle(t, f, id)
		mustNotClaim(t, f, "the only waiting message was in front of a stop that ended with no turn run")

		send(t, f, id, "actually, fix the test")
		mustClaim(t, f, id)
	})

	t.Run("Settle_PeekReadsTheStopWhereItFell", func(t *testing.T) {
		f := mk(t)
		id := parked(t, f)
		send(t, f, id, "fix the null check")
		stop(t, f, id)
		settle(t, f, id)
		send(t, f, id, "actually, fix the test")

		want := join("fix the null check", stopWakeNote, "actually, fix the test")
		if got := peek(t, f, id); got != want {
			t.Errorf("Peek = %q, want %q (the stopped message, the stop note, and the new message)", got, want)
		}
		msg, _, ok, err := f.Stores.ConversationPendingInput.Consume(ctx, f.OrgID, id)
		if err != nil || !ok || msg != want {
			t.Errorf("Consume = (%q, %v, %v), want %q, the text Peek returned", msg, ok, err, want)
		}
	})

	t.Run("Settle_AMessageSentBeforeTheParkStillWakes", func(t *testing.T) {
		f := mk(t)
		id := parked(t, f)
		send(t, f, id, "fix the null check")
		stop(t, f, id)
		send(t, f, id, "actually, fix the test")
		mustNotClaim(t, f, "a stop is pending")
		settle(t, f, id)
		mustClaim(t, f, id)
	})

	t.Run("Settle_AStopWithNothingWaitingMovesNothingAMessageNeeds", func(t *testing.T) {
		f := mk(t)
		id := parked(t, f)
		stop(t, f, id)
		settle(t, f, id)
		mustNotClaim(t, f, "nothing is waiting")
		send(t, f, id, "pick it back up")
		mustClaim(t, f, id)
	})

	t.Run("Park_BeforeAnyTurnRanKeepsTheMessageFromWaking", func(t *testing.T) {
		f := mk(t)
		id := parked(t, f)
		send(t, f, id, "fix the null check")
		c := mustClaim(t, f, id)
		stop(t, f, id)
		p := db.ParkStopped(domain.ParkReasonUserCancelled, "")
		p.NoTurnRan = true
		park(t, f, c, p)
		mustNotClaim(t, f, "the stop caught the engagement before its agent started")

		send(t, f, id, "actually, fix the test")
		mustClaim(t, f, id)
	})

	t.Run("Park_AfterATurnRanLeavesTheMessageBehindItToRunNext", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		c := mustClaim(t, f, id)
		send(t, f, id, "then fix the test")
		stop(t, f, id)
		park(t, f, c, db.ParkStopped(domain.ParkReasonUserCancelled, ""))
		mustClaim(t, f, id)
	})

	t.Run("Get_AConversationWhoseOnlyMessageIsInFrontOfTheStopReadsParked", func(t *testing.T) {
		f := mk(t)
		id := parked(t, f)
		send(t, f, id, "fix the null check")
		if got := get(t, f, id); got.Status != domain.StatusQueued {
			t.Fatalf("status with a message waiting = %q, want queued", got.Status)
		}
		stop(t, f, id)
		settle(t, f, id)
		got := get(t, f, id)
		if got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonUserCancelled {
			t.Errorf("after the stop settled = (status %q, park reason %q), want (open, %s)", got.Status, got.ParkReason, domain.ParkReasonUserCancelled)
		}
	})
}
