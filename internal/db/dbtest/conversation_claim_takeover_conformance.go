package dbtest

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// takeoverOtherExecutor is the identity of "some other dispatcher" in this
// suite: any executor that is not the one calling the takeover.
const takeoverOtherExecutor = "takeover-other-exec"

// RunClaimTakeoverConformance is the shared assertion suite for the recovery
// passes every dispatcher runs: the takeover of expired claims, the
// clean-shutdown release, the boot reset, the settlement of a step a run's
// cancel never reached, and the stranded-run read. It runs on the claim-lease
// fixture, whose staged steps are claimable delegations under a running
// blueprint.
func RunClaimTakeoverConformance(t *testing.T, mk ClaimLeaseFactory) {
	t.Helper()
	ctx := context.Background()

	// stageClaimed stages one step and claims it under the given identity.
	// Staging and claiming one at a time keeps the claim scan unambiguous:
	// the step just staged is the only claimable one.
	stageClaimed := func(t *testing.T, f ClaimLeaseFixture, executorID string, bootEpoch int64) *domain.Conversation {
		t.Helper()
		id, _ := f.StageStep(t)
		got, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, executorID, bootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil {
			t.Fatalf("ClaimNextConversation(%s/%d): %v", executorID, bootEpoch, err)
		}
		if got == nil || got.ID != id {
			t.Fatalf("ClaimNextConversation(%s/%d) = %+v, want conversation %s", executorID, bootEpoch, got, id)
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
	get := func(t *testing.T, f ClaimLeaseFixture, conversationID string) *domain.Conversation {
		t.Helper()
		got, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || got == nil {
			t.Fatalf("GetSystem(%s) = (%+v, %v)", conversationID, got, err)
		}
		return got
	}
	runOf := func(t *testing.T, f ClaimLeaseFixture, conversationID string) *domain.BlueprintRun {
		t.Helper()
		br, err := f.Stores.Blueprints.GetRunSystem(ctx, f.OrgID, get(t, f, conversationID).BlueprintRunID)
		if err != nil || br == nil {
			t.Fatalf("GetRunSystem for %s = (%+v, %v)", conversationID, br, err)
		}
		return br
	}
	takeOver := func(t *testing.T, f ClaimLeaseFixture, limit int) []string {
		t.Helper()
		refs, err := f.Stores.ConversationQueue.TakeOverExpiredClaimsSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch, limit)
		if err != nil {
			t.Fatalf("TakeOverExpiredClaimsSystem: %v", err)
		}
		ids := make([]string, 0, len(refs))
		for _, r := range refs {
			if r.OrgID != f.OrgID {
				t.Errorf("taken-over claim %s reports org %q, want %q", r.ClaimID, r.OrgID, f.OrgID)
			}
			ids = append(ids, r.ClaimID)
		}
		sort.Strings(ids)
		return ids
	}

	t.Run("Takeover_ReleasesOnlyExpiredClaimsThisBootDidNotMint", func(t *testing.T) {
		f := mk(t)
		other := stageClaimed(t, f, takeoverOtherExecutor, 1)
		priorBoot := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch-1)
		thisBoot := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
		live := stageClaimed(t, f, takeoverOtherExecutor, 1)
		for _, c := range []*domain.Conversation{other, priorBoot, thisBoot} {
			f.SetLease(t, c.ClaimID, -time.Minute)
		}

		got := takeOver(t, f, 100)
		want := []string{other.ClaimID, priorBoot.ClaimID}
		sort.Strings(want)
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("taken over = %v, want %v (another executor's and a prior boot's, never this boot's or a live lease)", got, want)
		}
		for _, c := range []*domain.Conversation{other, priorBoot} {
			if released, outcome := claimState(t, f, c.ClaimID); !released || outcome != "reaped" {
				t.Errorf("claim %s after the takeover = (released %v, %q), want (true, reaped)", c.ClaimID, released, outcome)
			}
		}
		if released, _ := claimState(t, f, thisBoot.ClaimID); released {
			t.Error("the takeover released this boot's own claim; only the process that minted it may")
		}
		if released, _ := claimState(t, f, live.ClaimID); released {
			t.Error("the takeover released a claim whose lease is still live")
		}
		if again := takeOver(t, f, 100); len(again) != 0 {
			t.Errorf("a second takeover = %v, want nothing — the first released everything it may", again)
		}
	})

	t.Run("Takeover_RespectsTheLimit", func(t *testing.T) {
		f := mk(t)
		var claims []*domain.Conversation
		for i := 0; i < 3; i++ {
			c := stageClaimed(t, f, takeoverOtherExecutor, 1)
			f.SetLease(t, c.ClaimID, -time.Minute)
			claims = append(claims, c)
		}
		if got := takeOver(t, f, 2); len(got) != 2 {
			t.Fatalf("takeover with limit 2 released %d claims, want 2", len(got))
		}
		if got := takeOver(t, f, 2); len(got) != 1 {
			t.Fatalf("the next pass released %d claims, want the 1 left", len(got))
		}
		for _, c := range claims {
			if released, _ := claimState(t, f, c.ClaimID); !released {
				t.Errorf("claim %s still live after two passes", c.ClaimID)
			}
		}
	})

	t.Run("Takeover_RefusesTheHoldersLateRenewal", func(t *testing.T) {
		// The renewal's guard and the takeover's are complementary over one
		// row: once the takeover has released the claim, the holder that
		// comes back finds it gone.
		f := mk(t)
		c := stageClaimed(t, f, takeoverOtherExecutor, 1)
		f.SetLease(t, c.ClaimID, -time.Second)
		if got := takeOver(t, f, 100); len(got) != 1 {
			t.Fatalf("taken over = %v, want the one expired claim", got)
		}
		if _, err := f.Stores.ConversationQueue.RenewClaimLeaseSystem(ctx, f.OrgID, c.ID, c.ClaimID, testClaimLease); !errors.Is(err, db.ErrClaimReleased) {
			t.Errorf("renewal after the takeover = %v, want ErrClaimReleased", err)
		}
	})

	t.Run("Takeover_ReturnsTheConversationToTheQueueAsALoss", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f, takeoverOtherExecutor, 1)
		f.SetLease(t, c.ClaimID, -time.Second)
		takeOver(t, f, 100)
		next, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || next == nil || next.ID != c.ID {
			t.Fatalf("claim after the takeover = (%+v, %v), want conversation %s back on the queue", next, err, c.ID)
		}
		if next.LostEngagements != 1 || next.SetupFailures != 0 {
			t.Errorf("claim budgets after one takeover = (lost %d, setup %d), want (1, 0)", next.LostEngagements, next.SetupFailures)
		}
	})

	t.Run("ShutdownRelease_ReleasesOnlyTheListedOwnClaims", func(t *testing.T) {
		f := mk(t)
		listed := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
		unlisted := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
		others := stageClaimed(t, f, takeoverOtherExecutor, 1)
		q := f.Stores.ConversationQueue

		live, err := q.LiveClaimsOfExecutorSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch)
		if err != nil {
			t.Fatalf("LiveClaimsOfExecutorSystem: %v", err)
		}
		gotLive := map[string]bool{}
		for _, c := range live {
			gotLive[c.ClaimID] = true
		}
		if len(live) != 2 || !gotLive[listed.ClaimID] || !gotLive[unlisted.ClaimID] {
			t.Fatalf("LiveClaimsOfExecutorSystem = %+v, want exactly this boot's two claims", live)
		}

		n, err := q.ReleaseOwnClaimsOnShutdownSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch, []string{listed.ID, others.ID})
		if err != nil {
			t.Fatalf("ReleaseOwnClaimsOnShutdownSystem: %v", err)
		}
		if n != 1 {
			t.Fatalf("released %d claims, want 1 — another executor's claim is never this boot's to release", n)
		}
		if released, outcome := claimState(t, f, listed.ClaimID); !released || outcome != "requeued_shutdown" {
			t.Errorf("listed claim = (released %v, %q), want (true, requeued_shutdown)", released, outcome)
		}
		if released, _ := claimState(t, f, unlisted.ClaimID); released {
			t.Error("an unlisted claim was released; its engagement may still be running")
		}
		if released, _ := claimState(t, f, others.ClaimID); released {
			t.Error("another executor's claim was released")
		}
		if n, err := q.ReleaseOwnClaimsOnShutdownSystem(ctx, claimLeaseExecutor, claimLeaseBootEpoch+1, []string{unlisted.ID}); err != nil || n != 0 {
			t.Errorf("a later boot's shutdown release = (%d, %v), want (0, nil)", n, err)
		}

		// Claimable at once, and charged to neither budget.
		next, err := q.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, db.ClaimPlacement{}, testClaimLease)
		if err != nil || next == nil || next.ID != listed.ID {
			t.Fatalf("claim after the shutdown release = (%+v, %v), want conversation %s", next, err, listed.ID)
		}
		if next.LostEngagements != 0 || next.SetupFailures != 0 || next.Attempts != 2 {
			t.Errorf("claim after a shutdown release = (lost %d, setup %d, attempts %d), want (0, 0, 2)", next.LostEngagements, next.SetupFailures, next.Attempts)
		}
	})

	t.Run("BootReset_ReleasesAPriorBootsClaimWhateverTheRowsState", func(t *testing.T) {
		f := mk(t)
		open := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch-1)
		f.SetStoredStatus(t, open.ID, domain.StatusOpen)
		done := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch-1)
		f.SetStoredStatus(t, done.ID, "completed")
		midFlight := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch-1)
		current := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)

		n, err := f.Stores.ConversationQueue.ResetProcessingConversations(ctx, claimLeaseExecutor, claimLeaseBootEpoch)
		if err != nil {
			t.Fatalf("ResetProcessingConversations: %v", err)
		}
		if n != 3 {
			t.Fatalf("the boot reset released %d claims, want 3 (every prior-boot claim, parked and terminal rows included)", n)
		}
		for _, c := range []*domain.Conversation{open, done, midFlight} {
			if released, outcome := claimState(t, f, c.ClaimID); !released || outcome != "reaped" {
				t.Errorf("prior-boot claim on %s = (released %v, %q), want (true, reaped)", c.ID, released, outcome)
			}
		}
		if released, _ := claimState(t, f, current.ClaimID); released {
			t.Error("the boot reset released a claim of the current boot")
		}
		if got := get(t, f, open.ID); got.Status != domain.StatusOpen {
			t.Errorf("parked row after the reset = %q, want open — the reset writes claims, never a status", got.Status)
		}
	})

	t.Run("Settle_ParksAQueuedStepACancelNeverReachedAndCancelsItsRun", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		br := runOf(t, f, id)
		if _, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, br.ID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}
		settled, err := f.Stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx)
		if err != nil {
			t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
		}
		if len(settled) != 1 || settled[0].ConversationID != id || settled[0].BlueprintRunID != br.ID {
			t.Fatalf("settled = %+v, want %s with run %s cancelled", settled, id, br.ID)
		}
		if got := get(t, f, id); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonBlueprintCancelled {
			t.Errorf("step after settlement = (%q, %q), want (open, blueprint_cancelled)", got.Status, got.ParkReason)
		}
		after := runOf(t, f, id)
		if after.Status != domain.BlueprintRunStatusCancelled || after.AbortReason != "cancelled" ||
			after.AbortedAtStep == nil || *after.AbortedAtStep != 0 || after.CompletedAt == nil {
			t.Errorf("run after settlement = (%q, %q, step %v, completed %v), want (cancelled, cancelled, 0, stamped)",
				after.Status, after.AbortReason, after.AbortedAtStep, after.CompletedAt)
		}
	})

	t.Run("Settle_ReParksAnOpenStepACancelNeverReached", func(t *testing.T) {
		f := mk(t)
		id, _ := f.StageStep(t)
		f.SetStoredStatus(t, id, domain.StatusOpen)
		br := runOf(t, f, id)
		if _, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, br.ID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}
		if _, err := f.Stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx); err != nil {
			t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
		}
		if got := get(t, f, id); got.Status != domain.StatusOpen || got.ParkReason != domain.ParkReasonBlueprintCancelled {
			t.Errorf("step after settlement = (%q, %q), want (open, blueprint_cancelled)", got.Status, got.ParkReason)
		}
		if after := runOf(t, f, id); after.Status != domain.BlueprintRunStatusCancelled {
			t.Errorf("run after settlement = %q, want cancelled", after.Status)
		}
	})

	t.Run("Settle_LeavesAClaimedStepUnderACancelRequestedRunToItsHolder", func(t *testing.T) {
		f := mk(t)
		c := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
		br := runOf(t, f, c.ID)
		if _, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, br.ID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}
		settled, err := f.Stores.ConversationQueue.SettleUnclaimedStopsSystem(ctx)
		if err != nil {
			t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
		}
		if len(settled) != 0 {
			t.Fatalf("settled = %+v, want nothing — the holder settles its own step", settled)
		}
		if released, _ := claimState(t, f, c.ClaimID); released {
			t.Error("the settlement released a live claim")
		}
		if after := runOf(t, f, c.ID); after.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("run = %q, want running", after.Status)
		}
	})

	t.Run("Stranded_FindsAConcludedCurrentStepPastTheGraceOnly", func(t *testing.T) {
		f := mk(t)
		const grace = time.Minute
		conclude := func(t *testing.T, status string) *domain.Conversation {
			t.Helper()
			c := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
			if _, err := HolderComplete(f.Stores.Conversations, ctx, f.OrgID, c.ID, status, 0, 0, 0, "", "", "", ""); err != nil {
				t.Fatalf("HolderComplete(%s): %v", status, err)
			}
			return c
		}
		stranded := conclude(t, "completed")
		f.BackdateConclusion(t, stranded.ID, 2*grace)
		strandedFailed := conclude(t, "failed")
		f.BackdateConclusion(t, strandedFailed.ID, 2*grace)
		fresh := conclude(t, "completed")

		parked := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
		if ok, err := HolderPark(f.Stores.Conversations, ctx, f.OrgID, parked.ID, db.ParkIdle()); err != nil || !ok {
			t.Fatalf("HolderPark = (%v, %v)", ok, err)
		}
		f.BackdateConclusion(t, parked.ID, 2*grace)

		// A terminal row still holding a live claim: its engagement is not
		// done with it, whatever the status says.
		held := stageClaimed(t, f, claimLeaseExecutor, claimLeaseBootEpoch)
		f.SetStoredStatus(t, held.ID, "completed")
		f.BackdateConclusion(t, held.ID, 2*grace)

		got, err := f.Stores.ConversationQueue.StrandedBlueprintRunsSystem(ctx, grace, 100)
		if err != nil {
			t.Fatalf("StrandedBlueprintRunsSystem: %v", err)
		}
		found := map[string]db.StrandedRun{}
		for _, r := range got {
			found[r.ConversationID] = r
		}
		for _, c := range []*domain.Conversation{stranded, strandedFailed} {
			r, ok := found[c.ID]
			if !ok {
				t.Errorf("stranded run of %s not found", c.ID)
				continue
			}
			if r.OrgID != f.OrgID || r.BlueprintRunID != runOf(t, f, c.ID).ID {
				t.Errorf("stranded run of %s = %+v, want org %s and its own run", c.ID, r, f.OrgID)
			}
		}
		for name, c := range map[string]*domain.Conversation{"inside the grace": fresh, "open": parked, "claimed": held} {
			if _, ok := found[c.ID]; ok {
				t.Errorf("a %s step's run was reported stranded", name)
			}
		}
		if len(got) != 2 {
			t.Errorf("StrandedBlueprintRunsSystem returned %d runs, want 2", len(got))
		}
		if limited, err := f.Stores.ConversationQueue.StrandedBlueprintRunsSystem(ctx, grace, 1); err != nil || len(limited) != 1 {
			t.Errorf("StrandedBlueprintRunsSystem with limit 1 = (%d runs, %v), want 1", len(limited), err)
		}
	})
}
