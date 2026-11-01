package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDashboardStats_AppModeNoPAT_ReturnsStats: an org with no stored PAT (App
// mode) still gets real aggregates, because the host comes from org_settings
// and the viewer from their bound login. One open non-draft PR by the bound
// user counts as one awaiting.
func TestDashboardStats_AppModeNoPAT_ReturnsStats(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	userID := runmode.LocalDefaultUserID

	host := dashboardTestHost(t, s, orgID, userID)
	if err := s.users.UpsertGitHubIdentity(ctx, userID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}

	seedCommissionedPR(t, s, domain.PRSnapshot{
		Number: 18, Repo: "acme/widget", Author: "octocat", State: "OPEN",
		Title: "Fix the thing", URL: "https://github.com/acme/widget/pull/18",
		CreatedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-10T00:00:00Z",
	}, "", "")

	rec := doJSON(t, s, "GET", "/api/dashboard/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/dashboard/stats: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var stats domain.DashboardStats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	// The no-identity path writes {} (all zero); a real read counts the open
	// non-draft PR, so Awaiting==1 tells the two apart.
	if stats.Awaiting != 1 {
		t.Errorf("stats.Awaiting = %d, want 1 (one open non-draft PR by octocat); body=%s", stats.Awaiting, rec.Body.String())
	}
}

// TestDashboardPRs_LoginLookupError_Returns5xx: a DB failure resolving the
// viewer's GitHub login is a 5xx, not an empty dashboard; only a missing row
// degrades to empty. Dropping user_github_identities fails that lookup while
// org settings and host resolution still succeed.
func TestDashboardPRs_LoginLookupError_Returns5xx(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	if _, err := s.db.ExecContext(context.Background(), `DROP TABLE user_github_identities`); err != nil {
		t.Fatalf("drop user_github_identities: %v", err)
	}

	rec := doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{})
	if rec.Code < 500 {
		t.Fatalf("got %d, want 5xx (a DB error must not degrade to an empty dashboard); body=%s", rec.Code, rec.Body.String())
	}
}

// dashboardTestHost returns the GitHub web host the dashboard handler resolves
// for the org, so a test can bind identity under the same (user, host) key the
// handler will look up.
func dashboardTestHost(t *testing.T, s *Server, orgID, userID string) string {
	t.Helper()
	var base string
	if err := s.tx.WithTx(context.Background(), orgID, userID, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(context.Background(), orgID)
		if err != nil {
			return err
		}
		base = set.GitHubBaseURL
		return nil
	}); err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	host, ok := resolveGitHubHost(base)
	if !ok {
		t.Fatalf("resolveGitHubHost(%q) returned !ok", base)
	}
	return host
}
