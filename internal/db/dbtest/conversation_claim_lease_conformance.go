package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ClaimLeaseFixture is one subtest's world for the claim-lease suite. The
// store drives every mint, renewal and fenced write; the callbacks stage and
// read the column itself, which no store method exposes for writing.
type ClaimLeaseFixture struct {
	Stores db.Stores
	OrgID  string

	// StageStep stages one claimable step — a running blueprint_run whose
	// current_step_index names a conversation with no outcome — and returns
	// that conversation's id along with the task it hangs off, which the
	// ownership-predicate arm asks about by task.
	StageStep func(t *testing.T) (conversationID, taskID string)

	// SetLease rewrites a claim's lease_expires_at to database now plus `in`,
	// which may be negative. It is the only way to stage an expired lease:
	// the store never writes one into the past, which is the point.
	SetLease func(t *testing.T, claimID string, in time.Duration)

	// Lease reads a claim's lease_expires_at back on the BACKEND's own clock
	// and in the backend's own storage format, alongside database now, so the
	// suite compares two readings from one clock rather than trusting Go's
	// against the database's. ok is false for SQL NULL.
	Lease func(t *testing.T, claimID string) (expiry time.Time, now time.Time, ok bool)

	// LiveClaimsWithoutLease counts rows breaking the live-claim invariant:
	// unreleased, and carrying no lease. Postgres holds it as a CHECK; SQLite
	// cannot add one by ALTER TABLE, so on that dialect this assertion IS the
	// enforcement.
	LiveClaimsWithoutLease func(t *testing.T) int

	// StageStaleStopIntent raw-writes a stored status and a pending stop
	// intent onto a conversation together. It stages the one shape no store
	// method produces — a terminal row still carrying an intent — which the
	// dispatcher's settlement has to clear without touching the status.
	StageStaleStopIntent func(t *testing.T, conversationID, status, by string)
}

// ClaimLeaseFactory builds a fresh fixture per subtest.
type ClaimLeaseFactory func(t *testing.T) ClaimLeaseFixture

// claimLeaseExecutor / claimLeaseBootEpoch are the fixed claimant identity
// every claim in this suite mints under.
const (
	claimLeaseExecutor  = "claim-lease-exec"
	claimLeaseBootEpoch = int64(3)
	// testClaimLease is short enough that a backdated expiry and a fresh one
	// cannot be confused, and long enough that no subtest's own runtime can
	// lapse it.
	testClaimLease = 90 * time.Second
)

// RunClaimLeaseConformance is the shared assertion suite for the claim lease:
// a claim's authority is a timestamp on database time, its holder renews it,
// and every claim-fenced write presents it. Both backends run the same
// subtests.
func RunClaimLeaseConformance(t *testing.T, mk ClaimLeaseFactory) {
	t.Helper()
	ctx := context.Background()

	// Every fixture the suite builds is swept on the way out of the subtest
	// that built it: no claim may be unreleased with no lease. Wrapped here
	// rather than asserted per subtest so a case added later inherits it —
	// and on SQLite this sweep IS the enforcement, since ALTER TABLE cannot
	// add the CHECK Postgres carries.
	build := mk
	mk = func(t *testing.T) ClaimLeaseFixture {
		f := build(t)
		t.Cleanup(func() {
			if n := f.LiveClaimsWithoutLease(t); n != 0 {
				t.Errorf("%d live claims carry no lease at the end of this subtest", n)
			}
		})
		return f
	}

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

	t.Run("Mint_StampsTheLeaseFromDatabaseNow", func(t *testing.T) {
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)

		expiry, now, ok := f.Lease(t, conv.ClaimID)
		if !ok {
			t.Fatal("a freshly minted claim carries no lease; the live-claim invariant is broken at the mint")
		}
		// Within a second of now+lease on the backend's own clock: the stamp
		// is measured from database time, not from the caller's.
		if drift := expiry.Sub(now.Add(testClaimLease)); drift > time.Second || drift < -time.Second {
			t.Errorf("lease_expires_at = %s, want within 1s of database now + %s (drift %s)", expiry, testClaimLease, drift)
		}
	})

	t.Run("Renew_MeasuresFromDatabaseNowNotFromTheOldExpiry", func(t *testing.T) {
		// Backdate the expiry to a second out, renew, and the new expiry must
		// be now+lease rather than old+lease. A renewal that extended the old
		// timestamp would drift a claim's authority further from real time on
		// every pass.
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)
		f.SetLease(t, conv.ClaimID, time.Second)

		got, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease, 0, "")
		if err != nil {
			t.Fatalf("RenewClaimLeaseSystem: %v", err)
		}
		expiry, now, ok := f.Lease(t, conv.ClaimID)
		if !ok {
			t.Fatal("renewal cleared the lease")
		}
		if drift := expiry.Sub(now.Add(testClaimLease)); drift > time.Second || drift < -time.Second {
			t.Errorf("renewed lease_expires_at = %s, want within 1s of database now + %s (drift %s)", expiry, testClaimLease, drift)
		}
		if drift := got.ExpiresAt.Sub(expiry); drift > time.Second || drift < -time.Second {
			t.Errorf("RenewClaimLeaseSystem returned %s but the row carries %s", got.ExpiresAt, expiry)
		}
	})

	t.Run("Renew_RefusedWhenThisCallerIsNotTheOwner", func(t *testing.T) {
		// Released, expired, and naming the wrong conversation are one answer
		// — ErrClaimReleased — and none of them may move the column.
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)
		// The decoy is staged AFTER the claim: two steps staged back to back
		// can tie on started_at (SQLite's default resolves to the second) and
		// the tiebreak is a random id, so staging both first would decide by
		// coin flip which one the claim takes.
		otherID, _ := f.StageStep(t)

		t.Run("expired", func(t *testing.T) {
			f.SetLease(t, conv.ClaimID, -time.Second)
			before, _, _ := f.Lease(t, conv.ClaimID)
			if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease, 0, ""); !errors.Is(err, db.ErrClaimReleased) {
				t.Fatalf("renew of an expired lease = %v, want ErrClaimReleased", err)
			}
			after, _, _ := f.Lease(t, conv.ClaimID)
			if !after.Equal(before) {
				t.Errorf("a refused renewal moved lease_expires_at: %s -> %s", before, after)
			}
		})

		t.Run("wrong_conversation", func(t *testing.T) {
			// Back to live so the refusal is the conversation and nothing else.
			f.SetLease(t, conv.ClaimID, testClaimLease)
			before, _, _ := f.Lease(t, conv.ClaimID)
			if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, otherID, conv.ClaimID, testClaimLease, 0, ""); !errors.Is(err, db.ErrClaimReleased) {
				t.Fatalf("renew naming another conversation = %v, want ErrClaimReleased", err)
			}
			after, _, _ := f.Lease(t, conv.ClaimID)
			if !after.Equal(before) {
				t.Errorf("a refused renewal moved lease_expires_at: %s -> %s", before, after)
			}
		})

		t.Run("released", func(t *testing.T) {
			if _, err := f.Stores.ConversationQueue.RequeueConversation(ctx, f.OrgID, conversationID, "transient"); err != nil {
				t.Fatalf("RequeueConversation: %v", err)
			}
			before, _, ok := f.Lease(t, conv.ClaimID)
			if !ok {
				t.Fatal("release cleared lease_expires_at; the row must keep recording when the lease would have lapsed")
			}
			if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease, 0, ""); !errors.Is(err, db.ErrClaimReleased) {
				t.Fatalf("renew of a released claim = %v, want ErrClaimReleased", err)
			}
			after, _, _ := f.Lease(t, conv.ClaimID)
			if !after.Equal(before) {
				t.Errorf("a refused renewal moved lease_expires_at: %s -> %s", before, after)
			}
			if n := f.LiveClaimsWithoutLease(t); n != 0 {
				t.Errorf("%d live claims carry no lease after a release", n)
			}
		})
	})

	t.Run("FencedWrites_RefusedOnAnExpiredLeaseWithNoSuccessor", func(t *testing.T) {
		// Nobody has taken the conversation over — the claim is still
		// unreleased — and every fenced write is refused anyway. Expiry alone
		// ends authority; that is the whole property.
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)
		f.SetLease(t, conv.ClaimID, -time.Second)

		conversations := f.Stores.Conversations
		pending := false
		writes := []struct {
			name string
			call func() error
		}{
			{"InsertMessageForClaimSystem", func() error {
				_, err := conversations.InsertMessageForClaimSystem(ctx, f.OrgID, conv.ClaimID, &domain.Message{
					ConversationID: conversationID, Role: "assistant", Content: "zombie", Delivered: &pending,
				})
				return err
			}},
			{"SetClaimPhaseSystem", func() error {
				_, err := conversations.SetClaimPhaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, domain.ClaimPhaseCloning)
				return err
			}},
			{"ParkOpenForClaimSystem", func() error {
				_, err := conversations.ParkOpenForClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID, db.ParkIdle())
				return err
			}},
			{"CompleteForClaimSystem", func() error {
				_, err := conversations.CompleteForClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID,
					domain.StatusCompleted, 0, 0, 0, "done", string(domain.ConversationOutcomeFinish), "", "")
				return err
			}},
			{"MarkFailedIfActiveForClaimSystem", func() error {
				_, err := conversations.MarkFailedIfActiveForClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID, string(domain.ConversationFailureCrash))
				return err
			}},
			{"SetSessionForClaimSystem", func() error {
				_, err := conversations.SetSessionForClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID, "sess-zombie")
				return err
			}},
			{"SetWorktreePathForClaimSystem", func() error {
				_, err := conversations.SetWorktreePathForClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID, "/tmp/zombie")
				return err
			}},
		}
		for _, w := range writes {
			if err := w.call(); !errors.Is(err, db.ErrClaimReleased) {
				t.Errorf("%s on an expired lease = %v, want ErrClaimReleased", w.name, err)
			}
		}

		// And nothing landed behind any of those refusals.
		if msgs, err := conversations.MessagesForConversations(ctx, f.OrgID, []string{conversationID}); err != nil || len(msgs) != 0 {
			t.Errorf("MessagesForConversations = %v (err %v), want nothing written", msgs, err)
		}
		after, err := conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || after == nil {
			t.Fatalf("GetSystem: (%v, %v)", after, err)
		}
		if after.SessionID != "" {
			t.Errorf("sdk_session_id = %q, want untouched", after.SessionID)
		}
		if after.WorktreePath == "/tmp/zombie" {
			t.Error("worktree_path took the zombie's value")
		}

		// The sandbox stats write is deliberately outside the fence and stays
		// valid here: teardown measures a cell whose claim has already ended,
		// so a refusal would discard the only reading anyone gets.
		peak := 512
		if got, err := conversations.RecordClaimSandboxStatsSystem(ctx, f.OrgID, conv.ClaimID, &peak, nil); err != nil || got == nil {
			t.Fatalf("RecordClaimSandboxStatsSystem on an expired claim = (%v, %v), want the written row", got, err)
		}
	})

	t.Run("Renew_StampsActivityFromTheIdleItIsPassed", func(t *testing.T) {
		// The mint leaves both columns NULL; the renewal stamps
		// last_activity_at as database now minus the idle it is handed, and
		// current_op as the operation, "" clearing it.
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)
		q := f.Stores.ConversationQueue

		minted, err := q.ClaimByIDSystem(ctx, conv.ClaimID)
		if err != nil || minted == nil {
			t.Fatalf("ClaimByIDSystem = (%+v, %v)", minted, err)
		}
		if minted.LastActivityAt != nil || minted.CurrentOp != "" {
			t.Errorf("minted claim activity = (%v, %q), want both empty until the first renewal", minted.LastActivityAt, minted.CurrentOp)
		}

		const idle = 40 * time.Second
		if _, err := q.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease, idle, "tool:bash"); err != nil {
			t.Fatalf("RenewClaimLeaseSystem: %v", err)
		}
		_, now, _ := f.Lease(t, conv.ClaimID)
		got, err := q.ClaimByIDSystem(ctx, conv.ClaimID)
		if err != nil || got == nil {
			t.Fatalf("ClaimByIDSystem = (%+v, %v)", got, err)
		}
		if got.LastActivityAt == nil {
			t.Fatal("renewal left last_activity_at NULL")
		}
		if drift := got.LastActivityAt.Sub(now.Add(-idle)); drift > time.Second || drift < -time.Second {
			t.Errorf("last_activity_at = %s, want within 1s of database now - %s (drift %s)", got.LastActivityAt, idle, drift)
		}
		if got.CurrentOp != "tool:bash" {
			t.Errorf("current_op = %q, want tool:bash", got.CurrentOp)
		}
		// The conversation carries the live claim's stamps.
		cv, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || cv == nil {
			t.Fatalf("GetSystem = (%+v, %v)", cv, err)
		}
		if cv.ClaimLastActivityAt == nil || !cv.ClaimLastActivityAt.Equal(*got.LastActivityAt) || cv.ClaimCurrentOp != "tool:bash" {
			t.Errorf("conversation claim activity = (%v, %q), want (%v, tool:bash)", cv.ClaimLastActivityAt, cv.ClaimCurrentOp, got.LastActivityAt)
		}

		if _, err := q.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease, 0, ""); err != nil {
			t.Fatalf("RenewClaimLeaseSystem: %v", err)
		}
		_, now, _ = f.Lease(t, conv.ClaimID)
		got, _ = q.ClaimByIDSystem(ctx, conv.ClaimID)
		if got.CurrentOp != "" {
			t.Errorf("current_op after a renewal with none in flight = %q, want cleared", got.CurrentOp)
		}
		if got.LastActivityAt == nil || now.Sub(*got.LastActivityAt) > time.Second {
			t.Errorf("last_activity_at after an idle-0 renewal = %v, want within 1s of database now %s", got.LastActivityAt, now)
		}

		// Released, the conversation no longer reports a live claim's stamps.
		if _, err := q.RequeueConversation(ctx, f.OrgID, conversationID, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		cv, _ = f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if cv.ClaimLastActivityAt != nil || cv.ClaimCurrentOp != "" {
			t.Errorf("conversation claim activity after release = (%v, %q), want empty", cv.ClaimLastActivityAt, cv.ClaimCurrentOp)
		}
	})

	t.Run("OldestIdleClaimSystem_ReadsTheLongestIdleLiveLease", func(t *testing.T) {
		f := mk(t)
		q := f.Stores.ConversationQueue
		if d, err := q.OldestIdleClaimSystem(ctx); err != nil || d != 0 {
			t.Fatalf("OldestIdleClaimSystem with no claims = (%s, %v), want (0s, nil)", d, err)
		}

		first, _ := f.StageStep(t)
		a := claim(t, f, first)
		// A live claim that has not renewed carries no stamp and is not read.
		if d, err := q.OldestIdleClaimSystem(ctx); err != nil || d != 0 {
			t.Fatalf("OldestIdleClaimSystem before any renewal = (%s, %v), want (0s, nil)", d, err)
		}
		if _, err := q.RenewClaimLeaseSystem(ctx, f.OrgID, first, a.ClaimID, testClaimLease, 30*time.Second, ""); err != nil {
			t.Fatalf("RenewClaimLeaseSystem: %v", err)
		}
		second, _ := f.StageStep(t)
		b := claim(t, f, second)
		if _, err := q.RenewClaimLeaseSystem(ctx, f.OrgID, second, b.ClaimID, testClaimLease, 5*time.Second, "provider"); err != nil {
			t.Fatalf("RenewClaimLeaseSystem: %v", err)
		}
		d, err := q.OldestIdleClaimSystem(ctx)
		if err != nil {
			t.Fatalf("OldestIdleClaimSystem: %v", err)
		}
		if d < 29*time.Second || d > 35*time.Second {
			t.Errorf("oldest idle = %s, want about 30s (the longer of the two)", d)
		}

		// An expired lease is not a live engagement, whatever its stamp says.
		f.SetLease(t, a.ClaimID, -time.Second)
		d, err = q.OldestIdleClaimSystem(ctx)
		if err != nil {
			t.Fatalf("OldestIdleClaimSystem: %v", err)
		}
		if d < 4*time.Second || d > 10*time.Second {
			t.Errorf("oldest idle with the 30s claim expired = %s, want about 5s", d)
		}
		f.SetLease(t, a.ClaimID, testClaimLease)

		// Released claims are not read either.
		if _, err := q.RequeueConversation(ctx, f.OrgID, first, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		if _, err := q.RequeueConversation(ctx, f.OrgID, second, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		if d, err := q.OldestIdleClaimSystem(ctx); err != nil || d != 0 {
			t.Errorf("OldestIdleClaimSystem after release = (%s, %v), want (0s, nil)", d, err)
		}
	})

	t.Run("ExpiredClaimsSystem_CountsAndAges", func(t *testing.T) {
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)

		if n, age, err := f.Stores.ConversationQueue.ExpiredClaimsSystem(ctx); err != nil || n != 0 || age != 0 {
			t.Fatalf("ExpiredClaimsSystem with a live lease = (%d, %s, %v), want (0, 0s, nil)", n, age, err)
		}

		const past = 30 * time.Second
		f.SetLease(t, conv.ClaimID, -past)
		n, age, err := f.Stores.ConversationQueue.ExpiredClaimsSystem(ctx)
		if err != nil {
			t.Fatalf("ExpiredClaimsSystem: %v", err)
		}
		if n != 1 {
			t.Errorf("expired claims = %d, want 1", n)
		}
		// Whole-second resolution on SQLite, so the window is generous on the
		// upper side and exact on the lower: the age can never be less than
		// how long ago the lease lapsed.
		if age < past-2*time.Second || age > past+5*time.Second {
			t.Errorf("oldest past expiry = %s, want about %s", age, past)
		}

		if _, err := f.Stores.ConversationQueue.RequeueConversation(ctx, f.OrgID, conversationID, "transient"); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		if n, age, err := f.Stores.ConversationQueue.ExpiredClaimsSystem(ctx); err != nil || n != 0 || age != 0 {
			t.Errorf("ExpiredClaimsSystem after release = (%d, %s, %v), want (0, 0s, nil)", n, age, err)
		}
	})

	t.Run("OwnExpiredClaim_ListedAndReleasedOnlyOnceLapsed", func(t *testing.T) {
		// The executor that minted a claim is the one that may release it
		// once its lease lapses. The list is scoped to that executor boot, and
		// the release refuses a claim whose lease is still live, so a stale
		// list entry renewed in between is left alone.
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		conv := claim(t, f, conversationID)
		q := f.Stores.ConversationQueue

		if got, err := q.ExpiredClaimsOfExecutorSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch); err != nil || len(got) != 0 {
			t.Fatalf("ExpiredClaimsOfExecutorSystem with a live lease = (%v, %v), want none", got, err)
		}
		if released, err := q.ReleaseExpiredClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID); err != nil || released {
			t.Fatalf("ReleaseExpiredClaimSystem on a live lease = (%v, %v), want (false, nil)", released, err)
		}

		f.SetLease(t, conv.ClaimID, -time.Minute)
		if got, err := q.ExpiredClaimsOfExecutorSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch+1); err != nil || len(got) != 0 {
			t.Errorf("another boot's list = (%v, %v), want none — a claim is released only by the boot that minted it", got, err)
		}
		if got, err := q.ExpiredClaimsOfExecutorSystem(ctx, "another-exec", claimLeaseBootEpoch); err != nil || len(got) != 0 {
			t.Errorf("another executor's list = (%v, %v), want none", got, err)
		}
		got, err := q.ExpiredClaimsOfExecutorSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch)
		if err != nil {
			t.Fatalf("ExpiredClaimsOfExecutorSystem: %v", err)
		}
		want := db.ClaimRef{ClaimID: conv.ClaimID, OrgID: f.OrgID, ConversationID: conversationID}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("ExpiredClaimsOfExecutorSystem = %+v, want [%+v]", got, want)
		}

		released, err := q.ReleaseExpiredClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID)
		if err != nil || !released {
			t.Fatalf("ReleaseExpiredClaimSystem on a lapsed lease = (%v, %v), want (true, nil)", released, err)
		}
		if again, err := q.ReleaseExpiredClaimSystem(ctx, f.OrgID, conversationID, conv.ClaimID); err != nil || again {
			t.Errorf("a second release = (%v, %v), want (false, nil)", again, err)
		}
		// The release is the requeue: the conversation is claimable again.
		next, err := q.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil {
			t.Fatalf("ClaimNextConversation after the release: %v", err)
		}
		if next == nil || next.ID != conversationID {
			t.Errorf("claim after the release = %+v, want conversation %s back on the queue", next, conversationID)
		}
	})

	t.Run("ExpiredClaim_DisplaysQueuedAndStillHoldsItsConversation", func(t *testing.T) {
		// The display says nothing is driving it; the ownership predicates
		// still say the claim is there. Both are right, and they are
		// different questions: the one-active index would refuse a second
		// claim, so offering the conversation for one would be offering work
		// the insert then rejects.
		f := mk(t)
		conversationID, taskID := f.StageStep(t)
		conv := claim(t, f, conversationID)

		live, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || live == nil {
			t.Fatalf("GetSystem: (%v, %v)", live, err)
		}
		if live.Status != domain.StatusRunning {
			t.Fatalf("status under a live claim = %q, want running", live.Status)
		}

		f.SetLease(t, conv.ClaimID, -time.Second)
		expired, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || expired == nil {
			t.Fatalf("GetSystem after expiry: (%v, %v)", expired, err)
		}
		if expired.Status != domain.StatusQueued {
			t.Errorf("status under an expired claim = %q, want queued", expired.Status)
		}

		// A phase on the expired claim must not resurface it: the phase rung
		// of the ladder asks the same liveness question the running rung does.
		f.SetLease(t, conv.ClaimID, testClaimLease)
		if _, err := f.Stores.Conversations.SetClaimPhaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, domain.ClaimPhaseCloning); err != nil {
			t.Fatalf("SetClaimPhaseSystem: %v", err)
		}
		f.SetLease(t, conv.ClaimID, -time.Second)
		phased, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || phased == nil {
			t.Fatalf("GetSystem after expiry with a phase: (%v, %v)", phased, err)
		}
		if phased.Status != domain.StatusQueued {
			t.Errorf("status under an expired claim carrying a phase = %q, want queued", phased.Status)
		}

		if held, err := f.Stores.Conversations.HasActiveClaimForTaskSystem(ctx, f.OrgID, taskID); err != nil || !held {
			t.Errorf("HasActiveClaimForTaskSystem after expiry = (%v, %v), want true: ownership is released_at, not the lease", held, err)
		}

		// And nothing else may claim it: the one-active index still holds.
		if got, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, "successor", 9, db.ClaimPlacement{}, testClaimLease); err != nil || got != nil {
			t.Errorf("ClaimNextConversation over an expired-but-unreleased claim = (%+v, %v), want nothing claimable", got, err)
		}
	})
}
