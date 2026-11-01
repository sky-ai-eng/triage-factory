package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newTestPollerManager builds a *poller.Manager against the test server's
// own stores so a test can drive Health() (via the SetXForTest hooks)
// without running a real poll cycle. pub/resolver are nil — Health() and
// the test-only heartbeat/success setters never touch them.
func newTestPollerManager(t *testing.T, s *Server) *poller.Manager {
	t.Helper()
	return poller.NewManager(s.db, nil, s.allStores.Users, s.allStores.Tasks, s.allStores.Entities,
		s.allStores.Repos, s.allStores.Deliverables, s.allStores.ExternalActions, s.allStores.EventQueue, s.allStores.PRMergeability, s.allStores.Orgs, s.allStores.JiraStatusRules, s.allStores.JiraProjects, s.allStores.LinearTeamRules,
		s.allStores.TeamGitHubGroups, s.allStores.Secrets, s.allStores.GitHubApps, s.allStores.PollReadiness, nil, nil)
}

// TestHandleReadyz_Healthy: 200 with every check ok when every hard check
// passes and no soft signal is stale.
func TestHandleReadyz_Healthy(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	s.SetVersion("v1.2.3-test")

	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	pm.SetLinearHeartbeatForTest(now)
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
	for _, check := range []string{"db", "migrations", "poller_github", "poller_jira", "poller_linear"} {
		if got := resp.Checks[check]; got != "ok" {
			t.Errorf("checks[%s]: got %q want %q", check, got, "ok")
		}
	}
	// With no SetLeaseStatus call, as at role=all and local, `lease` is
	// omitted entirely.
	if resp.Lease != nil {
		t.Errorf("lease: got %+v, want nil (SetLeaseStatus was never called)", resp.Lease)
	}
}

// TestHandleReadyz_DBDown: a dead DB pool hard-fails with 503 and checks.db
// "failed".
func TestHandleReadyz_DBDown(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	pm.SetLinearHeartbeatForTest(now)
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

// TestHandleReadyz_HeartbeatStale: a poll loop whose heartbeat is older than
// 3x its base interval is a hard failure on the holder: 503, not_ready, that
// source's check failed and the others ok.
func TestHandleReadyz_HeartbeatStale(t *testing.T) {
	for _, stale := range []string{"poller_github", "poller_linear"} {
		t.Run(stale, func(t *testing.T) {
			s := newTestServer(t)
			s.SetMigrationsOK(true)

			pm := newTestPollerManager(t, s)
			now := time.Now()
			beat := func(check string) time.Time {
				if check == stale {
					return now.Add(-10 * time.Minute) // well past 90s
				}
				return now
			}
			pm.SetGitHubHeartbeatForTest(beat("poller_github"))
			pm.SetJiraHeartbeatForTest(beat("poller_jira"))
			pm.SetLinearHeartbeatForTest(beat("poller_linear"))
			s.SetPollerManager(pm.Health)

			rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
			}
			var resp readyzResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Status != "not_ready" {
				t.Errorf("status: got %q want %q", resp.Status, "not_ready")
			}
			for _, check := range []string{"poller_github", "poller_jira", "poller_linear"} {
				want := "ok"
				if check == stale {
					want = "failed"
				}
				if got := resp.Checks[check]; got != want {
					t.Errorf("checks[%s]: got %q want %q", check, got, want)
				}
			}
		})
	}
}

// TestHandleReadyz_StaleLastSuccessIsDegradedNot503: a stale last successful
// poll is a soft signal: 200, status "degraded", the age reported per org.
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
	pm.SetLinearHeartbeatForTest(now)
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

// TestHandleReadyz_ActiveRunsCount: on SQLite, active_runs counts waiting,
// working and parked-but-woken conversations, not idle parked or finished ones.
func TestHandleReadyz_ActiveRunsCount(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	pm.SetLinearHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)

	// origin='interactive' (anything other than 'blueprint') sidesteps the
	// origin-requires-parents CHECK, which otherwise demands a
	// blueprint_run_id/task_id/prompt_id trio.
	//
	// Both halves of the count are derived, so the fixture states them the
	// way the system does: a waiting conversation carries no stored status,
	// a working one carries no stored status AND an unreleased claim, a
	// parked one carries 'open', and a finished one carries its terminal.
	for _, seed := range []struct{ id, status string }{
		{"run_readyz_waiting", ""},
		{"run_readyz_working", ""},
		{"run_readyz_woken", "open"},
		{"run_readyz_parked", "open"},
		{"run_readyz_done", dbtest.SeedConcluded},
	} {
		var stored any
		status, concluded := dbtest.SeedStatus(seed.status)
		if status != "" {
			stored = status
		}
		if _, err := s.db.Exec(
			`INSERT INTO conversations (id, status, origin, completed_at) VALUES (?, ?, 'interactive', CASE WHEN ? THEN CURRENT_TIMESTAMP END)`,
			seed.id, stored, concluded,
		); err != nil {
			t.Fatalf("seed run %s: %v", seed.id, err)
		}
	}
	if _, err := s.db.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id, boot_epoch, lease_expires_at)
		VALUES ('claim_readyz_working', 'run_readyz_working', 'readyz-executor', 1, strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))
	`); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// A follow-up on the parked run wakes it: it is claimable and displays
	// as queued, so it is waiting work and must be counted. Its twin stays
	// parked with nothing queued and must not be. Nothing distinguishes them
	// but this row, which is the point.
	if _, err := s.db.Exec(`
		INSERT INTO messages (conversation_id, role, content, subtype, delivered, window_state)
		VALUES ('run_readyz_woken', 'user', 'keep going', '', false, 'active')
	`); err != nil {
		t.Fatalf("seed follow-up: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp readyzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ActiveRuns != 3 {
		t.Errorf("active_runs: got %d want 3 (waiting + working + parked-and-woken; not the idle parked one, not the finished one)", resp.ActiveRuns)
	}
}

// TestHandleReadyz_HolderLeaseFieldPresent: on the lease holder, `lease`
// carries the reported values and the poller hard checks run as they do on a
// Server with no lease wired.
func TestHandleReadyz_HolderLeaseFieldPresent(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	pm.SetLinearHeartbeatForTest(now)
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
	for _, check := range []string{"poller_github", "poller_jira", "poller_linear"} {
		if got := resp.Checks[check]; got != "ok" {
			t.Errorf("checks[%s]: got %q want %q (holder must hard-check exactly like today)", check, got, "ok")
		}
	}
}

// TestHandleReadyz_StandbyIgnoresDeadPollerHealth: a standby control pod
// answers 200 "ok" with a dead poller snapshot, because it hard-checks only db
// and migrations. Its poller checks read "standby", its sources are empty, and
// lease reports is_holder false.
func TestHandleReadyz_StandbyIgnoresDeadPollerHealth(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	// Heartbeats never set: Health() reports every source dead, which would
	// 503 a holder.
	s.SetPollerManager(pm.Health)
	s.SetLeaseStatus(func() (string, string, int64, bool) {
		return "background-brain", "pod-b", 3, false
	})

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on a standby even with a dead poller snapshot, got %d: %s", rec.Code, rec.Body.String())
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
	for _, check := range []string{"poller_github", "poller_jira", "poller_linear"} {
		if got := resp.Checks[check]; got != "standby" {
			t.Errorf("checks[%s]: got %q want %q", check, got, "standby")
		}
	}
	if len(resp.Sources.GitHub) != 0 || len(resp.Sources.Jira) != 0 || len(resp.Sources.Linear) != 0 {
		t.Errorf("sources: expected empty maps on a standby, got github=%v jira=%v linear=%v", resp.Sources.GitHub, resp.Sources.Jira, resp.Sources.Linear)
	}
}

// TestHandleReadyz_NoAuthRequired: /readyz answers 200 with no session
// context (doJSON never seeds one).
func TestHandleReadyz_NoAuthRequired(t *testing.T) {
	s := newTestServer(t)
	s.SetMigrationsOK(true)
	pm := newTestPollerManager(t, s)
	now := time.Now()
	pm.SetGitHubHeartbeatForTest(now)
	pm.SetJiraHeartbeatForTest(now)
	pm.SetLinearHeartbeatForTest(now)
	s.SetPollerManager(pm.Health)

	rec := doJSON(t, s, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with no session, got %d: %s", rec.Code, rec.Body.String())
	}
}
