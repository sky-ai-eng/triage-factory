package fleet

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
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
	timings := []domain.RunTiming{
		{Status: "completed", StartedAt: now.Add(-10 * time.Minute), ClaimedAt: tp(now.Add(-9 * time.Minute)), DurationMS: ip(60000)},
		{Status: "failed", FailureKind: "memory_limit", StartedAt: now.Add(-8 * time.Minute), ClaimedAt: tp(now.Add(-8 * time.Minute)), DurationMS: ip(30000)},
		{Status: "running", StartedAt: now.Add(-2 * time.Minute), ClaimedAt: tp(now.Add(-2 * time.Minute))},
		{Status: "queued", StartedAt: now.Add(-1 * time.Minute)}, // not claimed → no wait sample
	}
	rs := summarizeRuns(timings, 24)
	if rs.Total != 4 || rs.Completed != 1 || rs.Failed != 1 || rs.Active != 1 {
		t.Fatalf("run counts wrong: %+v", rs)
	}
	if rs.DurationP50MS == nil {
		t.Fatalf("expected duration percentile")
	}
	if len(rs.FailureKinds) != 1 || rs.FailureKinds[0].Kind != "memory_limit" {
		t.Fatalf("failure kinds wrong: %+v", rs.FailureKinds)
	}

	queued := []domain.QueuedRun{
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
		t.Fatalf("expected wait percentile from 3 claimed runs")
	}

	shares := byOrgShares(queued, now)
	if len(shares) != 2 || shares[0].OrgID != "a" || shares[0].Count != 2 {
		t.Fatalf("org shares wrong: %+v", shares)
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
