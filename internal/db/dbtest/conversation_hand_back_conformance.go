package dbtest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// handBackWait is the wait the suite's deferred hand-backs set: far longer
// than any subtest runs, so a conversation it defers cannot become claimable
// by the clock while the subtest is still asserting that it is not.
const handBackWait = time.Hour

// RunHandBackConformance is the shared assertion suite for the hand-back an
// engagement makes of its own claim (HandBackClaimSystem) and the wait it can
// set: the fence, the next_attempt_at it stamps, the claim scan and every
// queue read the wait keeps the conversation out of, the writes that clear
// it, and the upstream budget the claim counts. It runs on the claim-lease
// fixture, whose staged steps are claimable delegations under a running
// blueprint.
func RunHandBackConformance(t *testing.T, mk ClaimLeaseFactory) {
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
	// stageClaimed stages one step and claims it. One at a time keeps the
	// claim scan unambiguous.
	stageClaimed := func(t *testing.T, f ClaimLeaseFixture) *domain.Conversation {
		t.Helper()
		id, _ := f.StageStep(t)
		return mustClaim(t, f, id)
	}
	handBack := func(t *testing.T, f ClaimLeaseFixture, c *domain.Conversation, outcome string, delay time.Duration) {
		t.Helper()
		if err := f.Stores.ConversationQueue.HandBackClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, outcome, delay, ""); err != nil {
			t.Fatalf("HandBackClaimSystem(%s, %v): %v", outcome, delay, err)
		}
	}
	claimState := func(t *testing.T, f ClaimLeaseFixture, claimID string) (released bool, outcome string) {
		t.Helper()
		c, err := f.Stores.ConversationQueue.ClaimByIDSystem(ctx, claimID)
		if err != nil || c == nil {
			t.Fatalf("ClaimByIDSystem(%s) = (%+v, %v)", claimID, c, err)
		}
		return c.ReleasedAt != nil, c.Outcome
	}
	get := func(t *testing.T, f ClaimLeaseFixture, id string) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, id)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = (%+v, %v)", id, got, err)
		}
		return got
	}
	// waitIs asserts next_attempt_at sits `want` past database now, read on
	// the backend's own clock. The tolerance covers the statements between
	// the stamp and the read, not clock skew: both readings are the
	// database's.
	waitIs := func(t *testing.T, f ClaimLeaseFixture, id string, want time.Duration) {
		t.Helper()
		at, now, ok := f.NextAttempt(t, id)
		if !ok {
			t.Fatalf("next_attempt_at on %s is NULL, want about now + %v", id, want)
		}
		if got := at.Sub(now); got > want || got < want-10*time.Second {
			t.Errorf("next_attempt_at on %s is now + %v, want about now + %v", id, got, want)
		}
	}
	position := func(p *int) string {
		if p == nil {
			return "none"
		}
		return strconv.Itoa(*p)
	}
	noWait := func(t *testing.T, f ClaimLeaseFixture, id string) {
		t.Helper()
		if at, _, ok := f.NextAttempt(t, id); ok {
			t.Errorf("next_attempt_at on %s = %v, want NULL", id, at)
		}
	}

	t.Run("HandBack_ReleasesWithTheOutcomeAndStampsTheWait", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		q := f.Stores.ConversationQueue

		if err := q.HandBackClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, db.HandBackUpstream, 30*time.Second, "provider returned 503"); err != nil {
			t.Fatalf("HandBackClaimSystem: %v", err)
		}
		if released, outcome := claimState(t, f, c.ClaimID); !released || outcome != db.HandBackUpstream {
			t.Errorf("handed-back claim = (released %v, %q), want (true, %s)", released, outcome, db.HandBackUpstream)
		}
		waitIs(t, f, c.ID, 30*time.Second)

		got := get(t, f, c.ID)
		if got.Status != domain.StatusQueued {
			t.Errorf("display status of a deferred conversation = %q, want %q — the vocabulary does not grow", got.Status, domain.StatusQueued)
		}
		if got.NextAttemptAt == nil {
			t.Error("the conversation read carries no next_attempt_at for a deferred conversation")
		}
		if got.ResultSummary != "provider returned 503" {
			t.Errorf("result_summary = %q, want the hand-back's last error", got.ResultSummary)
		}

		// The fence: a second hand-back, or one naming another engagement's
		// claim, writes nothing and says why.
		if err := q.HandBackClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, db.HandBackUpstream, time.Minute, "again"); !errors.Is(err, db.ErrClaimReleased) {
			t.Errorf("repeat hand-back = %v, want ErrClaimReleased", err)
		}
		other := stageClaimed(t, f)
		if err := q.HandBackClaimSystem(ctx, f.OrgID, c.ID, other.ClaimID, db.HandBackUpstream, time.Minute, "theirs"); !errors.Is(err, db.ErrClaimReleased) {
			t.Errorf("hand-back naming another conversation's claim = %v, want ErrClaimReleased", err)
		}
		if released, _ := claimState(t, f, other.ClaimID); released {
			t.Error("a mismatched hand-back released the claim it named")
		}
		waitIs(t, f, c.ID, 30*time.Second)
	})

	t.Run("HandBack_RefusesAnOutcomeThatIsNotAHandBackAndWritesNothing", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		for _, outcome := range []string{"", "completed", "parked", "cancelled", "requeued_elsewhere"} {
			err := f.Stores.ConversationQueue.HandBackClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, outcome, time.Minute, "boom")
			if !errors.Is(err, db.ErrInvalidRequeueOutcome) {
				t.Errorf("HandBackClaimSystem(%q) = %v, want ErrInvalidRequeueOutcome", outcome, err)
			}
		}
		if released, _ := claimState(t, f, c.ClaimID); released {
			t.Error("a refused hand-back released the claim")
		}
		noWait(t, f, c.ID)
	})

	t.Run("HandBack_WithNoDelayIsClaimableAtOnce", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		handBack(t, f, c, db.HandBackShutdown, 0)
		noWait(t, f, c.ID)
		mustClaim(t, f, c.ID)
	})

	t.Run("Claim_SkipsADeferredConversationUntilItsTimeAndClearsTheWait", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		handBack(t, f, c, db.HandBackUpstream, handBackWait)
		if got := claim(t, f); got != nil {
			t.Fatalf("ClaimNextConversation = %s, want nothing claimable while the wait is ahead", got.ID)
		}

		// The wait is over: backdated rather than waited out.
		f.SetNextAttempt(t, c.ID, -time.Second)
		got := mustClaim(t, f, c.ID)
		if got.UpstreamHandBacks != 1 {
			t.Errorf("UpstreamHandBacks = %d, want 1", got.UpstreamHandBacks)
		}
		noWait(t, f, c.ID)
		if after := get(t, f, c.ID); after.NextAttemptAt != nil {
			t.Errorf("the claimed conversation still reads next_attempt_at %v", after.NextAttemptAt)
		}
	})

	t.Run("QueueReads_LeaveADeferredConversationOut", func(t *testing.T) {
		f := mk(t)
		q := f.Stores.ConversationQueue
		deferred := stageClaimed(t, f)
		waitingID, waitingTask := f.StageStep(t)
		handBack(t, f, deferred, db.HandBackUpstream, handBackWait)

		if n, err := q.CountQueuedSystem(ctx); err != nil || n != 1 {
			t.Errorf("CountQueuedSystem = (%d, %v), want (1, nil) — a deferred conversation waits for time, not capacity", n, err)
		}
		shares, err := q.FleetQueueShares(ctx)
		if err != nil {
			t.Fatalf("FleetQueueShares: %v", err)
		}
		queued := 0
		for _, s := range shares {
			if s.OrgID == f.OrgID {
				queued = s.Queued
			}
		}
		if queued != 1 {
			t.Errorf("FleetQueueShares queued for the org = %d, want 1", queued)
		}
		if ages, err := q.QueuedConversationAgesSystem(ctx); err != nil || len(ages) != 1 {
			t.Errorf("QueuedConversationAgesSystem = (%d rows, %v), want (1, nil)", len(ages), err)
		}
		if ages, err := q.QueuedConversationAgesForOrgSystem(ctx, f.OrgID); err != nil || len(ages) != 1 {
			t.Errorf("QueuedConversationAgesForOrgSystem = (%d rows, %v), want (1, nil)", len(ages), err)
		}
		if byOrg, err := q.CountDeferredSystem(ctx); err != nil || byOrg[f.OrgID] != 1 || len(byOrg) != 1 {
			t.Errorf("CountDeferredSystem = (%v, %v), want {%s: 1}", byOrg, err, f.OrgID)
		}

		// Queue position: the deferred conversation displays queued with no
		// place in line, and the one behind it is first.
		convs, _, err := f.Stores.Conversations.List(ctx, f.OrgID,
			db.ConversationListFilter{TaskIDs: []string{get(t, f, deferred.ID).TaskID, waitingTask}}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		seen := 0
		for _, c := range convs {
			switch c.ID {
			case deferred.ID:
				seen++
				if c.Status != domain.StatusQueued || c.QueuePosition != nil {
					t.Errorf("deferred conversation lists as (%q, position %s), want (queued, none)", c.Status, position(c.QueuePosition))
				}
				if c.NextAttemptAt == nil {
					t.Error("the list read carries no next_attempt_at for the deferred conversation")
				}
			case waitingID:
				seen++
				if c.QueuePosition == nil || *c.QueuePosition != 1 {
					t.Errorf("the conversation behind the deferred one lists at position %s, want 1", position(c.QueuePosition))
				}
			}
		}
		if seen != 2 {
			t.Fatalf("List returned %d of the two staged conversations", seen)
		}

		// Its wait over, it is back in every count.
		f.SetNextAttempt(t, deferred.ID, -time.Second)
		if n, err := q.CountQueuedSystem(ctx); err != nil || n != 2 {
			t.Errorf("CountQueuedSystem after the wait = (%d, %v), want (2, nil)", n, err)
		}
		if byOrg, err := q.CountDeferredSystem(ctx); err != nil || len(byOrg) != 0 {
			t.Errorf("CountDeferredSystem after the wait = (%v, %v), want none", byOrg, err)
		}
	})

	t.Run("UpstreamHandBacks_CountsTheEpisodeAndSpendsNoOtherBudget", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		if c.UpstreamHandBacks != 0 {
			t.Fatalf("first claim UpstreamHandBacks = %d, want 0", c.UpstreamHandBacks)
		}
		for want := 1; want <= 2; want++ {
			handBack(t, f, c, db.HandBackUpstream, 0)
			c = mustClaim(t, f, c.ID)
			if c.UpstreamHandBacks != want || c.SetupFailures != 0 || c.LostEngagements != 0 || c.Attempts != want+1 {
				t.Errorf("claim after %d upstream hand-backs = (upstream %d, setup %d, lost %d, attempts %d), want (%d, 0, 0, %d)",
					want, c.UpstreamHandBacks, c.SetupFailures, c.LostEngagements, c.Attempts, want, want+1)
			}
		}

		// The other budgets' hand-backs are not upstream ones.
		if _, err := f.Stores.ConversationQueue.RequeueConversation(ctx, f.OrgID, c.ID, db.RequeueSetupFailure, ""); err != nil {
			t.Fatalf("RequeueConversation: %v", err)
		}
		c = mustClaim(t, f, c.ID)
		if c.UpstreamHandBacks != 2 || c.SetupFailures != 1 {
			t.Errorf("claim after a setup failure = (upstream %d, setup %d), want (2, 1)", c.UpstreamHandBacks, c.SetupFailures)
		}

		// An engagement that got somewhere ends the episode: a park, then a
		// resume, starts the next claim at zero.
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("ParkOpenForClaimSystem = (%v, %v)", ok, err)
		}
		if ok, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, c.ID); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume = (%v, %v)", ok, err)
		}
		c = mustClaim(t, f, c.ID)
		if c.UpstreamHandBacks != 0 || c.SetupFailures != 0 {
			t.Errorf("claim after a concluded engagement = (upstream %d, setup %d), want (0, 0)", c.UpstreamHandBacks, c.SetupFailures)
		}
	})

	t.Run("ClearNextAttempt_DropsTheWaitAndReturnsTheRow", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		handBack(t, f, c, db.HandBackUpstream, handBackWait)

		cleared, err := f.Stores.Conversations.ClearNextAttempt(ctx, f.OrgID, c.ID)
		if err != nil {
			t.Fatalf("ClearNextAttempt: %v", err)
		}
		if cleared.NextAttemptAt != nil {
			t.Errorf("ClearNextAttempt returned next_attempt_at %v, want none", cleared.NextAttemptAt)
		}
		noWait(t, f, c.ID)
		AssertWriteReturnedStoredRow(t, "ClearNextAttempt", *cleared, func() (*domain.Conversation, error) {
			return f.Stores.Conversations.Get(ctx, f.OrgID, c.ID)
		})
		mustClaim(t, f, c.ID)

		// A conversation with no wait is written unchanged, and one that
		// does not exist is a miss.
		if _, err := f.Stores.Conversations.ClearNextAttempt(ctx, f.OrgID, c.ID); err != nil {
			t.Errorf("ClearNextAttempt with no wait set = %v, want nil", err)
		}
		if _, err := f.Stores.Conversations.ClearNextAttempt(ctx, f.OrgID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, db.ErrNoSuchConversation) {
			t.Errorf("ClearNextAttempt on no conversation = %v, want ErrNoSuchConversation", err)
		}
	})

	t.Run("StopSettlement_ClearsTheWait", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		handBack(t, f, c, db.HandBackUpstream, handBackWait)
		if ok, err := f.Stores.Conversations.RequestStopSystem(ctx, f.OrgID, c.ID, "", "", ""); err != nil || !ok {
			t.Fatalf("RequestStopSystem = (%v, %v)", ok, err)
		}
		if _, err := f.Stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx); err != nil {
			t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
		}
		if got := get(t, f, c.ID); got.Status != domain.StatusOpen {
			t.Fatalf("status after the settlement = %q, want open", got.Status)
		}
		noWait(t, f, c.ID)
	})

	t.Run("MarkQueuedForResume_ClearsTheWait", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f)
		if ok, err := f.Stores.Conversations.ParkOpenForClaimSystem(ctx, f.OrgID, c.ID, c.ClaimID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("ParkOpenForClaimSystem = (%v, %v)", ok, err)
		}
		// A stale wait on a parked row, which no writer leaves but a resume
		// must not carry: it would hold the woken conversation back.
		f.SetNextAttempt(t, c.ID, handBackWait)
		if ok, err := f.Stores.Conversations.MarkQueuedForResume(ctx, f.OrgID, c.ID); err != nil || !ok {
			t.Fatalf("MarkQueuedForResume = (%v, %v)", ok, err)
		}
		noWait(t, f, c.ID)
		mustClaim(t, f, c.ID)
	})
}
