package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newTestPollerManager builds a *poller.Manager against the test server's
// own stores so a test can drive Health() (via the SetXForTest hooks)
// without running a real poll cycle. pub/resolver are nil — Health() and
// the test-only heartbeat/success setters never touch them.
func newTestPollerManager(t *testing.T, s *Server) *poller.Manager {
	t.Helper()
	return poller.NewManager(s.db, nil, s.allStores.Users, s.allStores.Tasks, s.allStores.Entities,
		s.allStores.Repos, s.allStores.Orgs, s.allStores.JiraStatusRules, s.allStores.TeamGitHubGroups,
		s.allStores.Secrets, s.allStores.GitHubApps, nil)
}

// TestHandleReadyz_Healthy is the happy path (acceptance criterion 1):
// 200 + well-formed JSON when every hard check passes and no soft signal
// is stale.
func TestHandleReadyz_Healthy(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	s.SetVersion("v1.2.3-test")

	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: got %q want %q", resp.Status, "ok")
	}
	if resp.Version != "v1.2.3-test" {
		t.Errorf("version: got %q want %q", resp.Version, "v1.2.3-test")
	}
	if resp.CheckedAt == 0 {
		t.Error("checked_at: expected non-zero unix timestamp")
	}
	for _, check := range []string{"db", "migrations", "poller_github", "poller_jira"} {
		if got := resp.Checks[check]; got != "ok" {
			t.Errorf("checks[%s]: got %q want %q", check, got, "ok")
		}
	}
	// TFAC-583: a Server with no SetLeaseStatus call (every role=all /
	// local boot, and this bare test construction) must omit `lease`
	// entirely — the frozen single-process contract.
	if resp.Lease != nil {
		t.Errorf("lease: got %+v, want nil (SetLeaseStatus was never called)", resp.Lease)
	}
}

// TestHandleReadyz_DBDown covers acceptance criterion 2: a dead DB pool
// hard-fails with 503 and checks.db == "failed".
func TestHandleReadyz_DBDown(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)

	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Checks["db"] != "failed" {
		t.Errorf("checks[db]: got %q want %q", resp.Checks["db"], "failed")
	}
	if resp.Status != "not_ready" {
		t.Errorf("status: got %q want %q", resp.Status, "not_ready")
	}
}

// TestHandleReadyz_PollerHeartbeatStale covers acceptance criterion 3: a
// GitHub base-tick heartbeat aged past 3x basePollInterval hard-fails
// with 503, via the injectable test seam rather than a real 90s+ sleep.
func TestHandleReadyz_PollerHeartbeatStale(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)

	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now.Add(-10 * time.Minute)) // well past 90s
	pm.SetJiraHeartbeatForTest(now)                          // stays alive
	s.SetPollerManager(pm.Health)

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Checks["poller_github"] != "failed" {
		t.Errorf("checks[poller_github]: got %q want %q", resp.Checks["poller_github"], "failed")
	}
	if resp.Checks["poller_jira"] != "ok" {
		t.Errorf("checks[poller_jira]: got %q want %q", resp.Checks["poller_jira"], "ok")
	}
	if resp.Status != "not_ready" {
		t.Errorf("status: got %q want %q", resp.Status, "not_ready")
	}
}

// TestHandleReadyz_StaleLastSuccessIsDegradedNot503 covers acceptance
// criterion 4: a stale last_successful_poll is a soft signal — still 200,
// age reported, top-level status "degraded".
func TestHandleReadyz_StaleLastSuccessIsDegradedNot503(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	if err := s.orgs.CreateLocalTenant(t.Context()); err != nil {
		t.Fatalf("seed local tenant: %v", err)
	}

	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	// Default GitHubPollInterval is 5m (domain.DefaultOrgSettings); 1h ago
	// is well past readyzDegradedFactor(3) x 5m = 15m.
	pm.SetGitHubSuccessForTest(runmode.LocalDefaultOrgID, now.Add(-1*time.Hour))
	s.SetPollerManager(pm.Health)

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (soft signal only), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("status: got %q want %q", resp.Status, "degraded")
	}
	if resp.Checks["poller_github"] != "ok" {
		t.Errorf("checks[poller_github]: got %q want %q (hard check must still pass)", resp.Checks["poller_github"], "ok")
	}
	org, ok := resp.Sources.GitHub[runmode.LocalDefaultOrgID]
	if !ok {
		t.Fatalf("sources.github: expected entry for %s, got %+v", runmode.LocalDefaultOrgID, resp.Sources.GitHub)
	}
	if org.AgeSeconds < 3500 {
		t.Errorf("sources.github[org].age_seconds: got %d, want >= ~3600", org.AgeSeconds)
	}
	if org.IntervalSeconds != 300 {
		t.Errorf("sources.github[org].interval_seconds: got %d want 300", org.IntervalSeconds)
	}
}

// TestHandleReadyz_ActiveRunsCount covers acceptance criterion 5:
// active_runs reflects actual queued/running rows via the indexed count.
func TestHandleReadyz_ActiveRunsCount(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)

	// origin='interactive' (anything other than 'blueprint') sidesteps the
	// origin-requires-parents CHECK, which otherwise demands a
	// blueprint_run_id/task_id/prompt_id trio — this test only cares about
	// the status column the COUNT filters on.
	for i, status := range []string{"queued", "running", "completed"} {
		if _, err := s.db.Exec(
			`INSERT INTO conversations (id, status, origin) VALUES (?, ?, 'interactive')`,
			"run_readyz_"+status, status,
		); err != nil {
			t.Fatalf("seed run %d (%s): %v", i, status, err)
		}
	}

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ActiveRuns != 2 {
		t.Errorf("active_runs: got %d want 2 (queued+running, not completed)", resp.ActiveRuns)
	}
}

// TestHandleReadyz_RateLimitSurfaced covers the TFAC-573 follow-up: the
// poller's per-org GitHub rate-limit observation is mapped into
// rate_limit.github as a soft signal — reported, but never affecting the
// hard checks or the top-level status. Hand-builds the HealthSnapshot
// (bypassing poller.Manager) since poller.Health()'s own rate-limit
// plumbing is covered by internal/poller's tests — this one only pins
// the handler's JSON mapping.
func TestHandleReadyz_RateLimitSurfaced(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)

	now := time.Now()
	resetAt := now.Add(45 * time.Minute).Truncate(time.Second)
	s.SetPollerManager(func(ctx context.Context) poller.HealthSnapshot {
		return poller.HealthSnapshot{
			GitHub: poller.SourceHealth{Alive: true, LastTick: now, Orgs: map[string]poller.OrgPollHealth{}},
			Jira:   poller.SourceHealth{Alive: true, LastTick: now, Orgs: map[string]poller.OrgPollHealth{}},
			GitHubRateLimit: map[string]ghclient.RateLimitState{
				runmode.LocalDefaultOrgID: {Remaining: 123, Reset: resetAt, Used: 4877},
			},
		}
	})

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: got %q want %q (rate limit must never gate status)", resp.Status, "ok")
	}
	got, ok := resp.RateLimit.GitHub[runmode.LocalDefaultOrgID]
	if !ok {
		t.Fatalf("rate_limit.github: expected entry for %s, got %+v", runmode.LocalDefaultOrgID, resp.RateLimit.GitHub)
	}
	if got.Remaining != 123 {
		t.Errorf("remaining: got %d want 123", got.Remaining)
	}
	if got.Used != 4877 {
		t.Errorf("used: got %d want 4877", got.Used)
	}
	if got.ResetUnix != resetAt.Unix() {
		t.Errorf("reset_unix: got %d want %d", got.ResetUnix, resetAt.Unix())
	}
}

// TestHandleReadyz_HolderLeaseFieldPresent pins the holder-side contract
// (TFAC-583, spec §8.3): when SetLeaseStatus reports is_holder=true, the
// `lease` field appears with the reported values AND the poller hard
// checks behave exactly as an unwired (role=all) Server would — a holder
// is byte-identical to today plus the added lease field.
func TestHandleReadyz_HolderLeaseFieldPresent(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)
	s.SetLeaseStatus(func() (string, string, int64, bool) {
		return "background-brain", "pod-a", 7, true
	})

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Lease == nil {
		t.Fatal("lease: expected a non-nil field on a control pod with SetLeaseStatus wired")
	}
	if resp.Lease.Name != "background-brain" || resp.Lease.HolderID != "pod-a" || resp.Lease.Term != 7 || !resp.Lease.IsHolder {
		t.Errorf("lease: got %+v, want {background-brain pod-a true 7}", resp.Lease)
	}
	for _, check := range []string{"poller_github", "poller_jira"} {
		if got := resp.Checks[check]; got != "ok" {
			t.Errorf("checks[%s]: got %q want %q (holder must hard-check exactly like today)", check, got, "ok")
		}
	}
}

// TestHandleReadyz_StandbyIsAlwaysReady pins the standby contract (TFAC-573
// shipped, TFAC-583 conditionality): a standby control pod hard-checks only
// db + migrations, reports poller_github/poller_jira as the literal string
// "standby" (not ok/failed), and MUST return 200 even though it runs no
// pollers at all (SetPollerManager is never even called here) — an LB must
// keep every standby in rotation.
func TestHandleReadyz_StandbyIsAlwaysReady(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	s.SetLeaseStatus(func() (string, string, int64, bool) {
		return "background-brain", "pod-b", 7, false
	})

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on a standby regardless of poller state, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: got %q want %q", resp.Status, "ok")
	}
	if resp.Lease == nil || resp.Lease.IsHolder {
		t.Fatalf("lease: got %+v, want a non-nil field with is_holder=false", resp.Lease)
	}
	if resp.Lease.HolderID != "pod-b" {
		t.Errorf("lease.holder_id: got %q want %q", resp.Lease.HolderID, "pod-b")
	}
	for _, check := range []string{"poller_github", "poller_jira"} {
		if got := resp.Checks[check]; got != "standby" {
			t.Errorf("checks[%s]: got %q want %q", check, got, "standby")
		}
	}
	if len(resp.Sources.GitHub) != 0 || len(resp.Sources.Jira) != 0 {
		t.Errorf("sources: expected empty maps on a standby, got github=%v jira=%v", resp.Sources.GitHub, resp.Sources.Jira)
	}
}

// TestHandleReadyz_StandbyIgnoresDeadPollerHealth pins that a standby's
// 200 doesn't depend on pollerHealth being wired or healthy at all — the
// poller-alive check is skipped entirely, not "checked and happens to
// pass".
func TestHandleReadyz_StandbyIgnoresDeadPollerHealth(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	// Heartbeats never set — Health() reports both sources dead, which
	// would 503 a holder (see TestHandleReadyz_PollerHeartbeatStale).
	s.SetPollerManager(pm.Health)
	s.SetLeaseStatus(func() (string, string, int64, bool) {
		return "background-brain", "pod-b", 3, false
	})

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on a standby even with a dead poller snapshot, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleReadyz_NoAuthRequired covers acceptance criterion 6: reachable
// with no auth cookie/session context (doJSON never seeds one), and the
// response carries no tenant data beyond the opaque org ID key.
func TestHandleReadyz_NoAuthRequired(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with no session, got %d: %s", rec.Code, rec.Body.String())
	}
}
