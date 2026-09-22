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
		if n := f.LiveClaimsWithoutLease(t); n != 0 {
			t.Errorf("%d live claims carry no lease after a mint", n)
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

		got, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease)
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
		if drift := got.Sub(expiry); drift > time.Second || drift < -time.Second {
			t.Errorf("RenewClaimLeaseSystem returned %s but the row carries %s", got, expiry)
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
			if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease); !errors.Is(err, db.ErrClaimReleased) {
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
			if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, otherID, conv.ClaimID, testClaimLease); !errors.Is(err, db.ErrClaimReleased) {
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
			if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, conversationID, conv.ClaimID, testClaimLease); !errors.Is(err, db.ErrClaimReleased) {
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
