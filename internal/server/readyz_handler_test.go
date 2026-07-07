package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

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
	// runs_origin_requires_parents CHECK, which otherwise demands a
	// blueprint_run_id/task_id/prompt_id trio — this test only cares about
	// the status column the COUNT filters on.
	for i, status := range []string{"queued", "running", "completed"} {
		if _, err := s.db.Exec(
			`INSERT INTO runs (id, status, origin) VALUES (?, ?, 'interactive')`,
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
