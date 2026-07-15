package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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
	conn, err := db.OpenAt(filepath.Join(t.TempDir(), "fleet-test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlitestore.New(conn)
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
