package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

func ip(v int) *int             { return &v }
func fp(v float64) *float64     { return &v }
func tp(v time.Time) *time.Time { return &v }

func TestPercentileMS(t *testing.T) {
	if got := percentileMS(nil, 50); got != nil {
		t.Fatalf("empty slice must be nil, got %v", *got)
	}
	vals := []int{10, 20, 30, 40, 100}
	if got := percentileMS(vals, 50); got == nil || *got != 30 {
		t.Fatalf("p50 = %v, want 30", got)
	}
	if got := percentileMS(vals, 95); got == nil || *got != 100 {
		t.Fatalf("p95 = %v, want 100", got)
	}
	// The source slice must not be mutated (sorted on a copy).
	unsorted := []int{5, 1, 3}
	_ = percentileMS(unsorted, 50)
	if unsorted[0] != 5 {
		t.Fatalf("percentileMS mutated the caller's slice: %v", unsorted)
	}
}

func TestSummarizeInstances(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	gated := true
	instances := []domain.Instance{
		// live executor: counts capacity + active
		{ID: "e1", Role: "executor", Version: "v2", LastHeartbeatAt: now.Add(-2 * time.Second), MaxRuns: ip(8), ActiveRuns: ip(3)},
		// draining + gated executor, still live
		{ID: "e2", Role: "executor", Version: "v2", LastHeartbeatAt: now.Add(-3 * time.Second), Draining: true, DispatchGated: &gated, MaxRuns: ip(4), ActiveRuns: ip(1)},
		// stale executor: excluded from live capacity/active
		{ID: "e3", Role: "executor", Version: "v1", LastHeartbeatAt: now.Add(-5 * time.Minute), MaxRuns: ip(16), ActiveRuns: ip(9)},
		// control pod
		{ID: "c1", Role: "control", Version: "v2", LastHeartbeatAt: now.Add(-1 * time.Second)},
	}
	tot := summarizeInstances(instances, now)
	if tot.Instances != 4 || tot.Executors != 3 || tot.Control != 1 {
		t.Fatalf("counts wrong: %+v", tot)
	}
	if tot.Draining != 1 || tot.Gated != 1 || tot.Stale != 1 {
		t.Fatalf("state counts wrong: %+v", tot)
	}
	// Only the two live executors' capacity counts (8+4), not the stale one's 16.
	if tot.CapacityMax != 12 || tot.ActiveRuns != 4 {
		t.Fatalf("live capacity/active wrong: cap=%d active=%d", tot.CapacityMax, tot.ActiveRuns)
	}
}

func TestVersionSkews(t *testing.T) {
	skews := versionSkews([]domain.Instance{
		{Version: "v2"}, {Version: "v2"}, {Version: "v1"},
	})
	if len(skews) != 2 || skews[0].Version != "v2" || skews[0].Count != 2 {
		t.Fatalf("version skew wrong: %+v", skews)
	}
}

func TestSummarizeRunsAndQueue(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	timings := []domain.ConversationTiming{
		{Status: "completed", StartedAt: now.Add(-10 * time.Minute), ClaimedAt: tp(now.Add(-9 * time.Minute)), DurationMS: ip(60000)},
		{Status: "failed", FailureKind: "memory_limit", StartedAt: now.Add(-8 * time.Minute), ClaimedAt: tp(now.Add(-8 * time.Minute)), DurationMS: ip(30000)},
		// An UNCLASSIFIED failure (empty failure_kind) — must still count as failed.
		{Status: "failed", StartedAt: now.Add(-7 * time.Minute), ClaimedAt: tp(now.Add(-7 * time.Minute)), DurationMS: ip(1000)},
		{Status: "running", StartedAt: now.Add(-2 * time.Minute), ClaimedAt: tp(now.Add(-2 * time.Minute))},
		// A parked "open" run — NOT executing, must NOT count as active.
		{Status: "open", StartedAt: now.Add(-90 * time.Second), ClaimedAt: tp(now.Add(-90 * time.Second))},
		{Status: "queued", StartedAt: now.Add(-1 * time.Minute)}, // not claimed → no wait sample
	}
	rs := summarizeRuns(timings, 24)
	if rs.Total != 6 || rs.Completed != 1 {
		t.Fatalf("run totals wrong: %+v", rs)
	}
	// Both failed runs count (classified + unclassified), but only the classified
	// one appears in the by-kind breakdown.
	if rs.Failed != 2 {
		t.Fatalf("Failed = %d, want 2 (unclassified failure must count)", rs.Failed)
	}
	if len(rs.FailureKinds) != 1 || rs.FailureKinds[0].Kind != "memory_limit" || rs.FailureKinds[0].Count != 1 {
		t.Fatalf("failure kinds wrong: %+v", rs.FailureKinds)
	}
	// Only the running run is active — queued and open are excluded.
	if rs.Active != 1 {
		t.Fatalf("Active = %d, want 1 (open + queued excluded)", rs.Active)
	}
	if rs.DurationP50MS == nil {
		t.Fatalf("expected duration percentile")
	}

	queued := []domain.QueuedConversation{
		{OrgID: "a", EnqueuedAt: now.Add(-5 * time.Minute)},
		{OrgID: "a", EnqueuedAt: now.Add(-4 * time.Minute)},
		{OrgID: "b", EnqueuedAt: now.Add(-3 * time.Minute)},
	}
	qs := summarizeQueue(queued, timings, now)
	if qs.Depth != 3 {
		t.Fatalf("queue depth = %d, want 3", qs.Depth)
	}
	if qs.OldestWaitS < 299 || qs.OldestWaitS > 301 {
		t.Fatalf("oldest wait ~300s, got %v", qs.OldestWaitS)
	}
	if qs.WaitP50MS == nil {
		t.Fatalf("expected wait percentile from the claimed runs")
	}

	// The backlog view's per-org breakdown (GET /api/fleet/backlog): busiest org
	// first, each carrying its own oldest wait.
	shares := backlogByOrg(queued, now)
	if len(shares) != 2 || shares[0].OrgID != "a" || shares[0].Count != 2 {
		t.Fatalf("backlog org shares wrong: %+v", shares)
	}
	if shares[0].OldestWaitS < 299 || shares[0].OldestWaitS > 301 {
		t.Fatalf("org a oldest wait ~300s, got %v", shares[0].OldestWaitS)
	}
}

// TestGateHidesWhenUnlicensed proves the 404-and-hide posture: with no fleet
// entitlement registered (the default), even an authenticated caller gets a 404
// and gate returns nil.
func TestGateHidesWhenUnlicensed(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)

	h := &handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/fleet/overview", nil)
	req = req.WithContext(httpx.WithClaims(context.Background(), &verify.Claims{Subject: "u1", Email: "op@example.com"}))

	if claims := h.gate(rec, req); claims != nil {
		t.Fatalf("gate must return nil when unlicensed")
	}
	if rec.Code != 404 {
		t.Fatalf("unlicensed gate must 404 (non-disclosure), got %d", rec.Code)
	}
}

func TestParseSandboxLimit(t *testing.T) {
	at := func(q string) (int, bool) {
		return parseSandboxLimit(httptest.NewRequest("GET", "/x"+q, nil))
	}
	if n, ok := at(""); !ok || n != defaultSandboxLimit {
		t.Errorf("absent limit = (%d, %v), want (%d, true)", n, ok, defaultSandboxLimit)
	}
	if n, ok := at("?limit=10"); !ok || n != 10 {
		t.Errorf("limit=10 = (%d, %v)", n, ok)
	}
	if n, ok := at("?limit=1000"); !ok || n != maxSandboxLimit {
		t.Errorf("limit=1000 = (%d, %v), want capped to %d", n, ok, maxSandboxLimit)
	}
	// Rejected rather than defaulted: a caller bug must surface as a 400, not
	// as plausible-looking data under a limit they never asked for.
	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=-1", "?limit=1.5"} {
		if n, ok := at(q); ok {
			t.Errorf("%s = (%d, true), want rejected", q, n)
		}
	}
}

func TestSandboxClaimDerivation(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	claimed := now.Add(-5 * time.Minute)

	// Released: the wall clock is claimed → released, not claimed → now.
	released := claimed.Add(90 * time.Second)
	got := sandboxClaim(domain.ExecutorClaim{
		ID: "c1", ClaimedAt: claimed, ReleasedAt: &released,
		PeakMemMB: ip(1024), Status: "completed",
	}, now)
	if got.Live {
		t.Errorf("released claim reported live: %+v", got)
	}
	if got.DurationS != 90 {
		t.Errorf("DurationS = %v, want 90 (claimed → released)", got.DurationS)
	}
	if got.CPUUsec != nil {
		t.Errorf("unmeasured cpu must stay absent: %+v", got)
	}

	// Live: the wall clock runs to now, and the row still renders.
	live := sandboxClaim(domain.ExecutorClaim{ID: "c2", ClaimedAt: claimed}, now)
	if !live.Live || live.DurationS != 300 {
		t.Errorf("live claim = (live %v, %v s), want (true, 300)", live.Live, live.DurationS)
	}

	// Clock skew between two pods can stamp a release before the claim. A
	// negative duration would poison every rate derived from it, so it floors
	// at zero rather than propagating.
	backwards := claimed.Add(-time.Minute)
	skewed := sandboxClaim(domain.ExecutorClaim{ID: "c3", ClaimedAt: claimed, ReleasedAt: &backwards}, now)
	if skewed.DurationS != 0 {
		t.Errorf("skewed release DurationS = %v, want 0", skewed.DurationS)
	}
}

// TestGateRefusesAPITokens pins the credential rule: the console's subject is
// the deployment, which no org-scoped token can name, so a token-authed caller
// who would otherwise be admitted gets 403 rather than the 404 the gate uses
// for callers who must not learn the console exists.
func TestGateRefusesAPITokens(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})
	// Local mode is a single implicit operator, so the operator half of the
	// gate passes and the token check is the only thing left to refuse.
	runmode.SetForTest(t, runmode.ModeLocal)

	h := &handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/fleet/overview", nil)
	ctx := httpx.WithClaims(context.Background(), &verify.Claims{Subject: "u1", Email: "op@example.com"})
	ctx = httpx.WithTokenAuth(ctx, &httpx.TokenAuth{TokenID: "tok-1", OrgID: "org-1"})
	req = req.WithContext(ctx)

	if claims := h.gate(rec, req); claims != nil {
		t.Fatalf("gate must return nil for a token-authed caller")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token-authed gate = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// The same caller, session-authed, is admitted — so the 403 above is about
	// the credential and nothing else.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/fleet/overview", nil).WithContext(
		httpx.WithClaims(context.Background(), &verify.Claims{Subject: "u1", Email: "op@example.com"}))
	if claims := h.gate(rec, req); claims == nil {
		t.Fatalf("session-authed operator must pass the gate, got %d: %s", rec.Code, rec.Body.String())
	}
}
