package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

func body(s string) io.Reader { return strings.NewReader(s) }

// fleetLicensed grants FeatureFleet at the deployment scope, standing in for a
// license-backed provider so the gate's entitlement half passes.
type fleetLicensed struct{}

func (fleetLicensed) Active() entitlements.Entitlements {
	return entitlements.New([]entitlements.Feature{entitlements.FeatureFleet}, nil)
}

func openMigratedStores(t *testing.T) db.Stores {
	t.Helper()
	stores, _ := openMigratedStoresWithConn(t)
	return stores
}

// openMigratedStoresWithConn also hands back the raw connection, for fixtures
// that stage rows no store method mints (a conversation, a claim in a chosen
// end state).
func openMigratedStoresWithConn(t *testing.T) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "fleet-test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlitestore.New(conn), conn
}

// withLocalClaims returns a context carrying local-mode claims — in local mode
// the gate short-circuits isOperator to true, so with FeatureFleet licensed the
// gate passes and we exercise the real read path end to end.
func withLocalClaims(t *testing.T) context.Context {
	t.Helper()
	if runmode.Current() != runmode.ModeLocal {
		t.Skip("integration path assumes local runmode (single implicit operator)")
	}
	return httpx.WithClaims(context.Background(), &verify.Claims{Subject: runmode.LocalDefaultUserID})
}

// TestInstancesEndpoint_Integration drives handleInstances against a real
// migrated SQLite DB with a registered instance, through the licensed+operator
// gate — the full gate → store → JSON path.
func TestInstancesEndpoint_Integration(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})

	stores := openMigratedStores(t)
	ctx := withLocalClaims(t)

	if _, err := stores.Instances.Register(ctx, "exec-1", domain.InstanceRoleExecutor, "v9", ""); err != nil {
		t.Fatalf("register instance: %v", err)
	}
	maxRuns, active := 8, 3
	if _, _, err := stores.Instances.Heartbeat(ctx, "exec-1", 1, domain.InstanceHeartbeat{MaxRuns: &maxRuns, ActiveRuns: &active}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	h := &handler{stores: stores}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/fleet/instances", nil).WithContext(ctx)
	h.handleInstances(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got instancesDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Instances) != 1 || got.Instances[0].ID != "exec-1" {
		t.Fatalf("expected exec-1, got %+v", got.Instances)
	}
	inst := got.Instances[0]
	if inst.MaxRuns == nil || *inst.MaxRuns != 8 || inst.ActiveRuns == nil || *inst.ActiveRuns != 3 {
		t.Fatalf("capacity snapshot not reflected: %+v", inst)
	}
	if inst.Role != domain.InstanceRoleExecutor || inst.Version != "v9" {
		t.Fatalf("registry fields wrong: %+v", inst)
	}
}

// TestOverviewEndpoint_Integration drives handleOverview end to end and asserts
// the totals + empty queue/runs/spend degrade cleanly (no rows → zeros).
func TestOverviewEndpoint_Integration(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})

	stores := openMigratedStores(t)
	ctx := withLocalClaims(t)
	if _, err := stores.Instances.Register(ctx, "exec-1", domain.InstanceRoleExecutor, "v9", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := &handler{stores: stores}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/fleet/overview?hours=24", nil).WithContext(ctx)
	h.handleOverview(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got overviewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Totals.Instances != 1 || got.Totals.Executors != 1 {
		t.Fatalf("totals wrong: %+v", got.Totals)
	}
	if got.Queue.Depth != 0 || got.Runs.Total != 0 {
		t.Fatalf("empty deployment should report zero queue/runs: %+v / %+v", got.Queue, got.Runs)
	}
	if got.Spend == nil || got.Spend.TotalUSD != 0 {
		t.Fatalf("spend overlay should be present and zero: %+v", got.Spend)
	}
}

// TestDrainEndpoint_Integration flips the drain flag through the POST handler
// and confirms the registry row reflects it.
func TestDrainEndpoint_Integration(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})

	stores := openMigratedStores(t)
	ctx := withLocalClaims(t)
	if _, err := stores.Instances.Register(ctx, "exec-1", domain.InstanceRoleExecutor, "v9", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	h := &handler{stores: stores}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/fleet/instances/exec-1/drain", body(`{"draining":true}`)).WithContext(ctx)
	req.SetPathValue("id", "exec-1")
	h.handleDrain(rec, req)

	if rec.Code != 200 {
		t.Fatalf("drain expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	inst, err := stores.Instances.Get(ctx, "exec-1")
	if err != nil || inst == nil {
		t.Fatalf("get: %v", err)
	}
	if !inst.Draining {
		t.Fatalf("instance should be draining after the POST")
	}

	// Unknown id → 404.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/fleet/instances/ghost/drain", body(`{"draining":true}`)).WithContext(ctx)
	req2.SetPathValue("id", "ghost")
	h.handleDrain(rec2, req2)
	if rec2.Code != 404 {
		t.Fatalf("unknown instance drain should 404, got %d", rec2.Code)
	}
}

// --- per-sandbox breakdown ---

// seedSandboxClaim stages one conversation and one claims row against it, in
// whatever end state the case needs. Production accumulates these across a
// run's whole life (enqueue → claim → complete → teardown stamp), and no
// single store call reaches a chosen combination, so the fixture writes the
// claim directly.
func seedSandboxClaim(
	t *testing.T, conn *sql.DB,
	claimID, executorID, status, failureKind string,
	claimedAt time.Time, releasedAt *time.Time,
	peakMemMB *int, cpuUsec *int64,
) {
	t.Helper()
	conversationID := uuid.New().String()
	dbtest.SeedConversation(t, conn, domain.Conversation{
		ID: conversationID, Status: status, TriggerType: "event",
		FailureKind: domain.ConversationFailureKind(failureKind), StartedAt: claimedAt,
	})
	var released, peak, cpu any
	if releasedAt != nil {
		released = releasedAt.UTC()
	}
	if peakMemMB != nil {
		peak = *peakMemMB
	}
	if cpuUsec != nil {
		cpu = *cpuUsec
	}
	outcome := ""
	if releasedAt != nil {
		outcome = "completed"
	}
	if _, err := conn.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
		                    claimed_at, released_at, outcome, peak_mem_mb, cpu_usec)
		VALUES (?, ?, ?, ?, 1, ?, ?, NULLIF(?, ''), ?, ?)
	`, claimID, runmode.LocalDefaultOrgID, conversationID, executorID, claimedAt.UTC(),
		released, outcome, peak, cpu); err != nil {
		t.Fatalf("seed claim %s: %v", claimID, err)
	}
}

func getSandboxes(t *testing.T, h *handler, ctx context.Context, instID, query string) (*httptest.ResponseRecorder, sandboxesDTO) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/fleet/instances/"+instID+"/sandboxes"+query, nil).WithContext(ctx)
	req.SetPathValue("id", instID)
	h.handleInstanceSandboxes(rec, req)
	var got sandboxesDTO
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec, got
}

// TestInstanceSandboxesEndpoint_Integration drives the per-executor breakdown
// end to end: a released claim carrying measured actuals and a live one
// carrying none must BOTH appear, newest first, with the unmeasured columns
// absent rather than zeroed.
func TestInstanceSandboxesEndpoint_Integration(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})

	stores, conn := openMigratedStoresWithConn(t)
	ctx := withLocalClaims(t)
	if _, err := stores.Instances.Register(ctx, "exec-1", domain.InstanceRoleExecutor, "v9", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	measuredID := uuid.New().String()
	liveID := uuid.New().String()
	otherBoxID := uuid.New().String()
	released := base.Add(2 * time.Minute)
	peak, cpu := 2048, int64(42_000_000)
	seedSandboxClaim(t, conn, measuredID, "exec-1", "failed", "memory_limit", base, &released, &peak, &cpu)
	// Live: no release, nothing measured yet — the teardown that measures
	// hasn't run. Still real occupancy on the box.
	seedSandboxClaim(t, conn, liveID, "exec-1", "running", "", base.Add(5*time.Minute), nil, nil, nil)
	// A neighbour executor's claim must not leak into this box's breakdown.
	seedSandboxClaim(t, conn, otherBoxID, "exec-2", "completed", "", base.Add(9*time.Minute), &released, &peak, &cpu)

	h := &handler{stores: stores}
	rec, got := getSandboxes(t, h, ctx, "exec-1", "")
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got.InstanceID != "exec-1" || got.Limit != defaultSandboxLimit {
		t.Fatalf("envelope wrong: %+v", got)
	}
	if len(got.Sandboxes) != 2 {
		t.Fatalf("expected exec-1's 2 claims (and none of exec-2's), got %d: %+v", len(got.Sandboxes), got.Sandboxes)
	}

	// Newest first: the live claim was staged later.
	live, measured := got.Sandboxes[0], got.Sandboxes[1]
	if live.ID != liveID || measured.ID != measuredID {
		t.Fatalf("ordering wrong (want newest first): %+v", got.Sandboxes)
	}
	if !live.Live || live.ReleasedAt != nil {
		t.Errorf("unreleased claim must report live: %+v", live)
	}
	if live.PeakMemMB != nil || live.CPUUsec != nil {
		t.Errorf("unmeasured actuals must be absent, not zero: %+v", live)
	}
	if live.DurationS <= 0 {
		t.Errorf("a live claim's duration must run to now, got %v", live.DurationS)
	}

	if measured.Live || measured.ReleasedAt == nil {
		t.Errorf("released claim must not report live: %+v", measured)
	}
	if measured.PeakMemMB == nil || *measured.PeakMemMB != peak {
		t.Errorf("peak memory must match the claims row: %+v", measured)
	}
	if measured.CPUUsec == nil || *measured.CPUUsec != cpu {
		t.Errorf("cpu time must match the claims row: %+v", measured)
	}
	if measured.DurationS < 119 || measured.DurationS > 121 {
		t.Errorf("released claim duration = %v, want ~120s (claimed → released)", measured.DurationS)
	}
	if measured.Status != "failed" || measured.FailureKind != "memory_limit" {
		t.Errorf("conversation state not joined: %+v", measured)
	}
	if measured.OrgID == "" || measured.ConversationID == "" {
		t.Errorf("display identifiers must be present: %+v", measured)
	}
}

// TestInstanceSandboxesEndpoint_LimitAndNotFound pins the two input contracts:
// limit is validated and capped, and an unknown instance 404s in handleDrain's
// shape rather than reading as an idle executor.
func TestInstanceSandboxesEndpoint_LimitAndNotFound(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})

	stores, conn := openMigratedStoresWithConn(t)
	ctx := withLocalClaims(t)
	if _, err := stores.Instances.Register(ctx, "exec-1", domain.InstanceRoleExecutor, "v9", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		seedSandboxClaim(t, conn, uuid.New().String(), "exec-1", "completed", "",
			base.Add(time.Duration(i)*time.Minute), nil, nil, nil)
	}
	h := &handler{stores: stores}

	if rec, got := getSandboxes(t, h, ctx, "exec-1", "?limit=2"); rec.Code != 200 ||
		got.Limit != 2 || len(got.Sandboxes) != 2 {
		t.Fatalf("limit=2 => code %d, limit %d, %d rows", rec.Code, got.Limit, len(got.Sandboxes))
	}
	// Above the ceiling clamps rather than 400s — an operator asking for more
	// than we serve gets the most we serve.
	if rec, got := getSandboxes(t, h, ctx, "exec-1", "?limit=5000"); rec.Code != 200 || got.Limit != maxSandboxLimit {
		t.Fatalf("limit=5000 => code %d, limit %d, want capped to %d", rec.Code, got.Limit, maxSandboxLimit)
	}
	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=-3"} {
		if rec, _ := getSandboxes(t, h, ctx, "exec-1", q); rec.Code != 400 {
			t.Errorf("%s => %d, want 400 (validated, not silently defaulted)", q, rec.Code)
		}
	}

	// Unknown instance: same not-found shape as handleDrain.
	rec, _ := getSandboxes(t, h, ctx, "ghost", "")
	if rec.Code != 404 {
		t.Fatalf("unknown instance => %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "instance not found") {
		t.Errorf("not-found body = %s, want handleDrain's shape", rec.Body.String())
	}
}

// TestClaimSeriesEndpoint_Integration covers all three series shapes: a
// sampled claim renders its ordered cumulative series, a real-but-unsampled
// claim renders an EMPTY series (not an error — a sub-minute run or pre-sampler
// history), and an unknown claim 404s.
func TestClaimSeriesEndpoint_Integration(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)
	entitlements.RegisterDeploymentProvider(fleetLicensed{})

	stores, conn := openMigratedStoresWithConn(t)
	ctx := withLocalClaims(t)

	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sampledID := uuid.New().String()
	unsampledID := uuid.New().String()
	released := base.Add(10 * time.Minute)
	peak, cpu := 900, int64(30_000_000)
	seedSandboxClaim(t, conn, sampledID, "exec-1", "completed", "", base, &released, &peak, &cpu)
	seedSandboxClaim(t, conn, unsampledID, "exec-1", "completed", "", base, &released, nil, nil)

	// Three ticks with a DROPPED one between the second and third (a 2-minute
	// gap): the cumulative counter means the consumer's rate over the wide
	// interval stays correct, which is exactly what must survive the wire.
	if err := stores.SandboxStats.RecordBatch(ctx, []domain.SandboxStat{
		{ClaimID: sampledID, At: base, MemCurrentMB: ptrInt(120), CPUUsecCum: ptrInt64(1_000_000)},
		{ClaimID: sampledID, At: base.Add(time.Minute), MemCurrentMB: ptrInt(400), CPUUsecCum: ptrInt64(4_000_000)},
		{ClaimID: sampledID, At: base.Add(3 * time.Minute), MemCurrentMB: ptrInt(380), CPUUsecCum: ptrInt64(10_000_000)},
	}); err != nil {
		t.Fatalf("record series: %v", err)
	}

	h := &handler{stores: stores}
	series := func(t *testing.T, claimID string) (*httptest.ResponseRecorder, sandboxSeriesDTO) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/fleet/claims/"+claimID+"/series", nil).WithContext(ctx)
		req.SetPathValue("id", claimID)
		h.handleClaimSeries(rec, req)
		var got sandboxSeriesDTO
		if rec.Code == 200 {
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return rec, got
	}

	rec, got := series(t, sampledID)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got.ClaimID != sampledID || len(got.Samples) != 3 {
		t.Fatalf("series wrong: %+v", got)
	}
	for i, want := range []time.Time{base, base.Add(time.Minute), base.Add(3 * time.Minute)} {
		if !got.Samples[i].At.Equal(want) {
			t.Fatalf("sample %d at %v, want %v (oldest-first)", i, got.Samples[i].At, want)
		}
	}
	// Served cumulative, verbatim — the rate derivation is the consumer's.
	for i := 1; i < len(got.Samples); i++ {
		if *got.Samples[i].CPUUsecCum < *got.Samples[i-1].CPUUsecCum {
			t.Fatalf("cumulative cpu must not go backwards on the wire: %+v", got.Samples)
		}
	}

	// A real claim nobody sampled: an empty series, not a 404 and not a 500.
	rec, got = series(t, unsampledID)
	if rec.Code != 200 {
		t.Fatalf("unsampled claim => %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(got.Samples) != 0 {
		t.Fatalf("unsampled claim should carry no samples: %+v", got)
	}

	// Unknown claim: handleDrain's not-found shape.
	rec, _ = series(t, uuid.New().String())
	if rec.Code != 404 {
		t.Fatalf("unknown claim => %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "claim not found") {
		t.Errorf("not-found body = %s, want handleDrain's shape", rec.Body.String())
	}
}

// TestSandboxRoutesDenialParity proves the two new routes hide behind exactly
// the gate their siblings do: with the deployment unlicensed, every one of
// them 404s and none of them touches a store. The drain route is the
// comparison arm — the parity is the point, not any single route's code.
func TestSandboxRoutesDenialParity(t *testing.T) {
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)

	stores := openMigratedStores(t)
	ctx := withLocalClaims(t)
	if _, err := stores.Instances.Register(ctx, "exec-1", domain.InstanceRoleExecutor, "v9", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The gate runs before any store read, so an unlicensed caller must be
	// refused even holding a nil bundle — proving no read happens first.
	h := &handler{}

	cases := []struct {
		name   string
		method string
		path   string
		id     string
		body   io.Reader
		serve  func(*handler, *httptest.ResponseRecorder, *http.Request)
	}{
		{name: "drain (sibling)", method: "POST", path: "/api/fleet/instances/exec-1/drain", id: "exec-1",
			body:  body(`{"draining":true}`),
			serve: func(h *handler, w *httptest.ResponseRecorder, r *http.Request) { h.handleDrain(w, r) }},
		{name: "sandboxes", method: "GET", path: "/api/fleet/instances/exec-1/sandboxes", id: "exec-1",
			serve: func(h *handler, w *httptest.ResponseRecorder, r *http.Request) { h.handleInstanceSandboxes(w, r) }},
		{name: "series", method: "GET", path: "/api/fleet/claims/c1/series", id: "c1",
			serve: func(h *handler, w *httptest.ResponseRecorder, r *http.Request) { h.handleClaimSeries(w, r) }},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, tc.body).WithContext(ctx)
		req.SetPathValue("id", tc.id)
		tc.serve(h, rec, req)
		if rec.Code != 404 {
			t.Errorf("%s: unlicensed => %d, want 404 (non-disclosure parity)", tc.name, rec.Code)
		}
	}
}

func ptrInt(v int) *int       { return &v }
func ptrInt64(v int64) *int64 { return &v }
