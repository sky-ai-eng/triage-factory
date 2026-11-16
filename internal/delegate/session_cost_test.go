package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeSDKSession is the running total the SDK keeps in a session and restores
// on --resume, which the fake process below reports as each result's
// total_cost_usd.
type fakeSDKSession struct {
	id       string
	totalUSD float64
}

// endTurnSpending is endTurn for a process that spends spendUSD on its turn.
// It reports what the SDK reports, the session's running total with this
// turn's spend added, to the sink and as the result's SessionCostUSD, and
// keeps the process's own spend as CostUSD the way agentproc's reader does:
// that total less the baseline the launch was handed.
func (f *interactiveFixture) endTurnSpending(t *testing.T, session *fakeSDKSession, reply string, spendUSD float64) *liveRunSpec {
	t.Helper()
	spec := f.endTurn(t, session.id, reply)
	inner := f.s.liveRunOverride
	f.s.liveRunOverride = func(ctx context.Context, launched liveRunSpec) liveOutcome {
		out := inner(ctx, launched)
		session.totalUSD += spendUSD
		launched.sink.OnSessionCost(session.totalUSD)
		out.result.SessionCostUSD = session.totalUSD
		out.result.CostUSD = max(session.totalUSD-launched.opts.SessionCostBaselineUSD, 0)
		out.costUSD = out.result.CostUSD
		return out
	}
	return spec
}

// TestDispatch_ResumedSDKTurnRecordsOnlyItsOwnSpend: a chat's second turn
// resumes the first turn's session, whose process reports the session's whole
// running total. The second launch is handed the total the first recorded, so
// the two engagements' recorded spends add up to the second process's total
// rather than counting the first turn twice.
func TestDispatch_ResumedSDKTurnRecordsOnlyItsOwnSpend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newInteractiveFixture(t, "1477")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	session := &fakeSDKSession{id: "sess-1477"}

	first := f.endTurnSpending(t, session, "the build is red because a test is flaky", 0.25)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	if first.opts.SessionID != "" {
		t.Fatalf("the first turn resumed session %q, want a fresh one", first.opts.SessionID)
	}
	f.stampModel(t)

	second := f.endTurnSpending(t, session, "rerunning it", 0.5)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "can you rerun it?"), time.Now())
	if second.opts.SessionID != session.id {
		t.Fatalf("the second turn launched session %q, want %q resumed", second.opts.SessionID, session.id)
	}
	if second.opts.SessionCostBaselineUSD != 0.25 {
		t.Errorf("the second turn's baseline = %v, want the 0.25 the first turn's process reported", second.opts.SessionCostBaselineUSD)
	}

	conv, err := f.stores.Conversations.GetSystem(ctx, org, f.conv.ID)
	if err != nil || conv == nil {
		t.Fatalf("GetSystem = (%v, %v)", conv, err)
	}
	if conv.TotalCostUSD == nil {
		t.Fatal("recorded spend = none, want 0.75, the second process's total")
	}
	if *conv.TotalCostUSD != 0.75 {
		t.Errorf("recorded spend = %v, want 0.75, the second process's total", *conv.TotalCostUSD)
	}
	if conv.SessionCostUSD != 0.75 {
		t.Errorf("stored session total = %v, want 0.75", conv.SessionCostUSD)
	}
}

// TestDispatch_ReclaimedSDKTurnRecordsOnlyItsOwnSpend: an engagement that
// took a person's message and ended before its reply leaves the turn cut off,
// and the next claim resumes the session on the step path rather than the
// follow-up one. That launch is handed the total the session's last process
// reported too.
func TestDispatch_ReclaimedSDKTurnRecordsOnlyItsOwnSpend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newInteractiveFixture(t, "1477-reclaim")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	session := &fakeSDKSession{id: "sess-1477-reclaim"}

	f.endTurnSpending(t, session, "the build is red because a test is flaky", 0.25)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	f.stampModel(t)

	// The engagement that took the next message ended before its reply, so
	// the claim below finds no message waiting and a session to continue.
	if err := f.s.SendMessage(ctx, org, f.conv.ID, runmode.LocalDefaultUserID, "can you rerun it?", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, _, ok, err := f.stores.ConversationPendingInput.Consume(ctx, org, f.conv.ID); err != nil || !ok {
		t.Fatalf("take the message off the queue = (%v, %v)", ok, err)
	}
	claimed, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil || claimed.ID != f.conv.ID {
		t.Fatalf("ClaimNextConversation = (%v, %v), want the conversation", claimed, err)
	}
	claimed.OrgID = org

	reclaimed := f.endTurnSpending(t, session, "rerunning it", 0.5)
	f.s.dispatchClaimedConversation(ctx, claimed, time.Now())
	if reclaimed.opts.SessionID != session.id {
		t.Fatalf("the re-claim launched session %q, want %q resumed", reclaimed.opts.SessionID, session.id)
	}
	if reclaimed.opts.SessionCostBaselineUSD != 0.25 {
		t.Errorf("the re-claim's baseline = %v, want the 0.25 the first turn's process reported", reclaimed.opts.SessionCostBaselineUSD)
	}

	conv, err := f.stores.Conversations.GetSystem(ctx, org, f.conv.ID)
	if err != nil || conv == nil {
		t.Fatalf("GetSystem = (%v, %v)", conv, err)
	}
	if conv.TotalCostUSD == nil {
		t.Fatal("recorded spend = none, want 0.75, the re-claimed process's total")
	}
	if *conv.TotalCostUSD != 0.75 {
		t.Errorf("recorded spend = %v, want 0.75, the re-claimed process's total", *conv.TotalCostUSD)
	}
}
