package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDashboardPRs_AppModeNoPAT_ReturnsPRs is the TFAC-396 regression: an
// App-mode org has no stored PAT, but the poller has populated entity snapshots
// via the App installation token and the user has a bound host-scoped GitHub
// identity. The dashboard must return that user's PRs from the snapshot store.
// The pre-fix handler early-returned on `creds.GitHubPAT == ""` and wrote an
// empty list, so the whole PRs page rendered blank for App-mode orgs.
func TestDashboardPRs_AppModeNoPAT_ReturnsPRs(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	userID := runmode.LocalDefaultUserID

	// Bind the user's identity under exactly the host the handler resolves from
	// org_settings. No PAT is ever stored — that's the App-mode condition the
	// gate used to mishandle.
	host := dashboardTestHost(t, s, orgID, userID)
	if err := s.users.UpsertGitHubIdentity(ctx, userID, host, "octocat", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}

	// One snapshot authored by octocat (must be returned) and one by someone
	// else (must be filtered out — the dashboard is the requesting user's view).
	seedDashboardSnapshot(t, s, domain.PRSnapshot{
		Number: 18, Repo: "acme/widget", Author: "octocat", State: "OPEN",
		Title: "Fix the thing", URL: "https://github.com/acme/widget/pull/18",
		CreatedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-10T00:00:00Z",
	})
	seedDashboardSnapshot(t, s, domain.PRSnapshot{
		Number: 19, Repo: "acme/widget", Author: "someone-else", State: "OPEN",
		Title: "Not mine", URL: "https://github.com/acme/widget/pull/19",
	})

	out := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{}))
	if len(out.Items) != 1 || out.Total() != 1 {
		t.Fatalf("got %d PRs / total %d, want 1/1 (only octocat's)", len(out.Items), out.Total())
	}
	if out.Items[0].Number != 18 || out.Items[0].Repo != "acme/widget" || out.Items[0].Author != "octocat" {
		t.Errorf("returned PR = %+v; want #18 acme/widget by octocat", out.Items[0])
	}
}

// TestDashboardStats_AppModeNoPAT_ReturnsStats is the stats-endpoint half of
// the TFAC-396 regression. handleDashboardStats shares handleDashboardPRs's
// host-from-org_settings + bound-identity gate, so it must likewise return real
// aggregates (not the gated empty {}) for an App-mode org with no PAT. One open,
// non-draft PR by the bound user must count as a single "awaiting".
func TestDashboardStats_AppModeNoPAT_ReturnsStats(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()
	orgID := runmode.LocalDefaultOrgID
	userID := runmode.LocalDefaultUserID

	host := dashboardTestHost(t, s, orgID, userID)
	if err := s.users.UpsertGitHubIdentity(ctx, userID, host, "octocat", "", "connect_oauth"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}

	seedDashboardSnapshot(t, s, domain.PRSnapshot{
		Number: 18, Repo: "acme/widget", Author: "octocat", State: "OPEN",
		Title: "Fix the thing", URL: "https://github.com/acme/widget/pull/18",
		CreatedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-10T00:00:00Z",
	})

	rec := doJSON(t, s, "GET", "/api/dashboard/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/dashboard/stats: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var stats domain.DashboardStats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	// The gated (empty-identity) path writes {} -> all-zero; a real read counts
	// the open non-draft PR. Asserting Awaiting==1 distinguishes the two.
	if stats.Awaiting != 1 {
		t.Errorf("stats.Awaiting = %d, want 1 (one open non-draft PR by octocat); body=%s", stats.Awaiting, rec.Body.String())
	}
}

// TestDashboardPRs_NoBoundIdentity_ReturnsEmpty pins the other precondition:
// with a resolvable host but no bound identity, the handler returns an empty
// list (not an error) — there's no "me" to filter snapshots by.
func TestDashboardPRs_NoBoundIdentity_ReturnsEmpty(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	seedDashboardSnapshot(t, s, domain.PRSnapshot{
		Number: 7, Repo: "acme/widget", Author: "octocat", State: "OPEN",
		Title: "Orphan", URL: "https://github.com/acme/widget/pull/7",
	})

	out := decodeList[domain.PRSummaryRow](t, doJSON(t, s, "POST", "/api/dashboard/prs/list", map[string]any{}))
	if len(out.Items) != 0 || out.Total() != 0 {
		t.Errorf("got %d PRs / total %d, want 0/0 (no bound identity)", len(out.Items), out.Total())
	}
}

// TestDashboardPRs_LoginLookupError_Returns5xx pins the review finding: a real
// DB failure resolving the user's GitHub login must surface as a 5xx, not a
// silently-empty dashboard. Only a missing row (-> "", nil) should degrade to
// the empty response; an actual error must propagate. handleDashboardStats
// shares the identical fix. Dropping user_github_identities forces the
// GetGitHubLogin SELECT to error while org-settings + host resolution still
// succeed, so the handler reaches (and must not swallow) the login lookup.
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

// seedDashboardSnapshot inserts a github entity carrying snap as snapshot_json
// — the production-shaped source the dashboard reads.
func seedDashboardSnapshot(t *testing.T, s *Server, snap domain.PRSnapshot) {
	t.Helper()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("%s#%d", snap.Repo, snap.Number)
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, last_polled_at)
		VALUES (?, ?, 'github', ?, 'pr', ?, ?, ?, ?, ?)
	`, "ent-"+sourceID, runmode.LocalDefaultOrgID, sourceID, snap.Title, snap.URL, string(blob), now, now); err != nil {
		t.Fatalf("seed entity %s: %v", sourceID, err)
	}
}
