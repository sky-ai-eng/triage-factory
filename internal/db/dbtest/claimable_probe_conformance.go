package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ClaimableProbeFixture is the claim-lease fixture plus the three things the
// claimability probes need staged that it does not: where a conversation
// sits in the queue, and the org's cap.
type ClaimableProbeFixture struct {
	ClaimLeaseFixture

	// CapAndPlacement is true for the dialect whose claim applies the org
	// cap and placement (Postgres). SQLite has neither, so the shapes they
	// exclude are claimable there, and the probe has to say so too.
	CapAndPlacement bool

	// SetQueued rewrites a conversation's preferred executor ("" writing SQL
	// NULL, unowned) and moves its queued_at `ago` into the past, on the
	// clock and in the layout each dialect stamps it with.
	SetQueued func(t *testing.T, conversationID, preferredExecutorID string, ago time.Duration)

	// SetMaxConcurrentRuns sets the org's max_concurrent_runs.
	SetMaxConcurrentRuns func(t *testing.T, limit int)
}

// ClaimableProbeFactory builds a fresh fixture per subtest.
type ClaimableProbeFactory func(t *testing.T) ClaimableProbeFixture

// probeMinWait stands in for the dispatcher's scan interval, the wait the
// probe applies to work that is not the asking executor's own.
const probeMinWait = 2 * time.Second

// probeQueuedLongAgo is past probeMinWait and short of probePlacement's aging
// window, so a conversation queued that long ago is old enough for the probe
// and still young enough for placement to keep it for its preferred executor.
const probeQueuedLongAgo = 10 * time.Second

// probePlacement is the placement the probe and the claim are both asked
// under. Its aging window outlasts every subtest, so only a conversation's
// preferred executor decides its tier.
var probePlacement = db.ClaimPlacement{Enabled: true, AgingInterval: time.Hour, Liveness: time.Minute}

// RunClaimableProbeConformance is the shared suite for ClaimableExistsSystem
// and OrgCapBlockedWorkExistsSystem: each answers for the claim scan's own
// predicate, so on every shape the claim takes or refuses, the probe says the
// same thing.
func RunClaimableProbeConformance(t *testing.T, mk ClaimableProbeFactory) {
	t.Helper()
	ctx := context.Background()

	probe := func(t *testing.T, f ClaimableProbeFixture, minWait time.Duration) bool {
		t.Helper()
		got, err := f.Stores.ConversationQueue.ClaimableExistsSystem(ctx, claimLeaseExecutor, probePlacement, minWait)
		if err != nil {
			t.Fatalf("ClaimableExistsSystem: %v", err)
		}
		return got
	}
	claim := func(t *testing.T, f ClaimableProbeFixture) string {
		t.Helper()
		got, err := f.Stores.ConversationQueue.ClaimNextConversation(ctx, claimLeaseExecutor, claimLeaseBootEpoch, probePlacement, testClaimLease)
		if err != nil {
			t.Fatalf("ClaimNextConversation: %v", err)
		}
		if got == nil {
			return ""
		}
		return got.ID
	}
	// agrees asks the probe, then the claim, and checks both give want. The
	// probe goes first because the claim takes the conversation.
	agrees := func(t *testing.T, f ClaimableProbeFixture, conversationID string, want bool) {
		t.Helper()
		if got := probe(t, f, probeMinWait); got != want {
			t.Errorf("ClaimableExistsSystem = %v, want %v", got, want)
		}
		got := claim(t, f)
		if want && got != conversationID {
			t.Errorf("ClaimNextConversation took %q, want %s", got, conversationID)
		}
		if !want && got != "" {
			t.Errorf("ClaimNextConversation took %s, want nothing", got)
		}
	}
	capBlocked := func(t *testing.T, f ClaimableProbeFixture) bool {
		t.Helper()
		got, err := f.Stores.ConversationQueue.OrgCapBlockedWorkExistsSystem(ctx, f.OrgID)
		if err != nil {
			t.Fatalf("OrgCapBlockedWorkExistsSystem: %v", err)
		}
		return got
	}
	// queuedStep stages a claimable step that has waited past probeMinWait,
	// with no preferred executor.
	queuedStep := func(t *testing.T, f ClaimableProbeFixture) string {
		t.Helper()
		conversationID, _ := f.StageStep(t)
		f.SetQueued(t, conversationID, "", probeQueuedLongAgo)
		return conversationID
	}
	// holdOneClaim stages a step and claims it, so the org has one engagement.
	holdOneClaim := func(t *testing.T, f ClaimableProbeFixture) {
		t.Helper()
		conversationID := queuedStep(t, f)
		if got := claim(t, f); got != conversationID {
			t.Fatalf("ClaimNextConversation took %q, want %s", got, conversationID)
		}
	}

	t.Run("AgreesWithTheClaim_Claimable", func(t *testing.T) {
		f := mk(t)
		agrees(t, f, queuedStep(t, f), true)
	})

	t.Run("AgreesWithTheClaim_EmptyQueue", func(t *testing.T) {
		f := mk(t)
		agrees(t, f, "", false)
	})

	t.Run("AgreesWithTheClaim_NotDrivable", func(t *testing.T) {
		f := mk(t)
		conversationID := queuedStep(t, f)
		conv, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || conv == nil {
			t.Fatalf("GetSystem = (%+v, %v)", conv, err)
		}
		// A step of a run whose cancel was asked for is never claimed.
		if changed, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, conv.BlueprintRunID); err != nil || !changed {
			t.Fatalf("RequestRunCancelSystem = (%v, %v)", changed, err)
		}
		agrees(t, f, conversationID, false)
	})

	t.Run("AgreesWithTheClaim_OverTheOrgCap", func(t *testing.T) {
		f := mk(t)
		f.SetMaxConcurrentRuns(t, 1)
		holdOneClaim(t, f)
		agrees(t, f, queuedStep(t, f), !f.CapAndPlacement)
	})

	t.Run("AgreesWithTheClaim_PlacementExcluded", func(t *testing.T) {
		f := mk(t)
		// Preferred by another executor that is alive, and younger than the
		// aging window: placement keeps it for that executor.
		if _, err := f.Stores.Instances.Register(ctx, takeoverOtherExecutor, "executor", "test", ""); err != nil {
			t.Fatalf("Register(%s): %v", takeoverOtherExecutor, err)
		}
		conversationID, _ := f.StageStep(t)
		f.SetQueued(t, conversationID, takeoverOtherExecutor, probeQueuedLongAgo)
		agrees(t, f, conversationID, !f.CapAndPlacement)
	})

	t.Run("WaitsOutWorkThatIsNotItsOwn", func(t *testing.T) {
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		f.SetQueued(t, conversationID, "", 0)
		// Claimable by anyone, and only just queued: an executor with a free
		// slot gets the first chance at it.
		if probe(t, f, time.Hour) {
			t.Error("ClaimableExistsSystem counted unowned work queued inside the wait")
		}
		f.SetQueued(t, conversationID, "", probeQueuedLongAgo)
		if !probe(t, f, probeMinWait) {
			t.Error("ClaimableExistsSystem did not count unowned work queued past the wait")
		}
		if got := claim(t, f); got != conversationID {
			t.Errorf("ClaimNextConversation took %q, want %s; the work was claimable throughout", got, conversationID)
		}
	})

	t.Run("CountsItsOwnWorkAtOnce", func(t *testing.T) {
		f := mk(t)
		conversationID, _ := f.StageStep(t)
		f.SetQueued(t, conversationID, claimLeaseExecutor, 0)
		if !probe(t, f, time.Hour) {
			t.Error("ClaimableExistsSystem waited on work this executor is preferred for")
		}
	})

	t.Run("OrgCapBlocked_AtTheCapWithWorkQueued", func(t *testing.T) {
		f := mk(t)
		f.SetMaxConcurrentRuns(t, 1)
		holdOneClaim(t, f)
		queuedStep(t, f)
		if got := capBlocked(t, f); got != f.CapAndPlacement {
			t.Errorf("OrgCapBlockedWorkExistsSystem = %v, want %v", got, f.CapAndPlacement)
		}
	})

	t.Run("OrgCapBlocked_AtTheCapWithNothingQueued", func(t *testing.T) {
		f := mk(t)
		f.SetMaxConcurrentRuns(t, 1)
		holdOneClaim(t, f)
		if capBlocked(t, f) {
			t.Error("OrgCapBlockedWorkExistsSystem = true with nothing queued")
		}
	})

	t.Run("OrgCapBlocked_UnderTheCap", func(t *testing.T) {
		f := mk(t)
		f.SetMaxConcurrentRuns(t, 2)
		holdOneClaim(t, f)
		queuedStep(t, f)
		// The queued work is claimable now, so nothing waits on the cap.
		if capBlocked(t, f) {
			t.Error("OrgCapBlockedWorkExistsSystem = true under the cap")
		}
	})

	t.Run("OrgCapBlocked_QueuedWorkNotDrivable", func(t *testing.T) {
		f := mk(t)
		f.SetMaxConcurrentRuns(t, 1)
		holdOneClaim(t, f)
		conversationID := queuedStep(t, f)
		conv, err := f.Stores.Conversations.GetSystem(ctx, f.OrgID, conversationID)
		if err != nil || conv == nil {
			t.Fatalf("GetSystem = (%+v, %v)", conv, err)
		}
		if changed, err := f.Stores.Blueprints.RequestRunCancelSystem(ctx, f.OrgID, conv.BlueprintRunID); err != nil || !changed {
			t.Fatalf("RequestRunCancelSystem = (%v, %v)", changed, err)
		}
		// Letting a claim go would free a place nothing could take.
		if capBlocked(t, f) {
			t.Error("OrgCapBlockedWorkExistsSystem = true for work the claim would refuse under the cap too")
		}
	})
}
