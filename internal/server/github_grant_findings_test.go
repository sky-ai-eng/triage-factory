package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The two grant findings as the panel reads them. What is pinned: each is a
// paginated list with a real total, a PAT workspace is refused rather than
// answered with an empty finding, and opening the panel — the status read plus
// both findings — asks GitHub nothing.

const (
	reachWithoutPurposePath = "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/grant/reach-without-purpose/list"
	scopeDriftPath          = "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/grant/scope-drift/list"
)

// seedGrantFixture puts the local org on its own App with one selective
// installation on acme, a mirror that reaches acme/api and acme/secrets, and a
// team tracking acme/api and acme/legacy: one repository reached for no reason,
// one tracked and unreachable.
func seedGrantFixture(t *testing.T, s *Server, selection string) {
	t.Helper()
	ctx := context.Background()
	stores := sqlitestore.New(s.db)
	if _, err := stores.Orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, domain.GitHubCredentialClassBYOApp); err != nil {
		t.Fatalf("set credential class: %v", err)
	}
	if _, err := stores.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
		InstallationID:      "1",
		OrgID:               runmode.LocalDefaultOrgID,
		AccountType:         "Organization",
		AccountLogin:        "acme",
		RepositorySelection: selection,
	}); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	if err := stores.ReachableRepos.ReplaceForInstallationSystem(ctx, runmode.LocalDefaultOrgID, domain.GitHubCredentialClassBYOApp, "1", []domain.ReachableRepository{
		{OrgID: runmode.LocalDefaultOrgID, InstallationID: "1", Owner: "acme", Repo: "api", ExternalID: "10", HTMLURL: "https://github.com/acme/api"},
		{OrgID: runmode.LocalDefaultOrgID, InstallationID: "1", Owner: "acme", Repo: "secrets", ExternalID: "11", Private: true, HTMLURL: "https://github.com/acme/secrets"},
	}); err != nil {
		t.Fatalf("replace mirror: %v", err)
	}
	seedConfiguredRepo(t, s, "acme", "api")
	seedConfiguredRepo(t, s, "acme", "legacy")
}

type findingList[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"next_page_token"`
	TotalCount    int    `json:"total_count"`
}

func decodeFindingList[T any](t *testing.T, body string) findingList[T] {
	t.Helper()
	var out findingList[T]
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode list: %v\n%s", err, body)
	}
	return out
}

func TestGitHubGrantFindings_ListWithTotals(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedGrantFixture(t, s, domain.RepositorySelectionSelected)

	t.Run("reach without purpose", func(t *testing.T) {
		rec := doJSON(t, s, "POST", reachWithoutPurposePath, map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		got := decodeFindingList[reachWithoutPurposeItem](t, rec.Body.String())
		if got.TotalCount != 1 || len(got.Items) != 1 {
			t.Fatalf("items=%d total=%d, want 1 and 1: %s", len(got.Items), got.TotalCount, rec.Body.String())
		}
		item := got.Items[0]
		if item.Slug != "acme/secrets" || !item.Private || item.HTMLURL != "https://github.com/acme/secrets" {
			t.Errorf("item=%+v, want acme/secrets (private) with its html_url", item)
		}
		// The verb travels with the row: the installation whose grant carries
		// it, and the GitHub page where that grant is narrowed.
		if item.InstallationID != "1" || item.AccountLogin != "acme" {
			t.Errorf("installation=%q account=%q, want 1 / acme", item.InstallationID, item.AccountLogin)
		}
		if want := "https://github.com/organizations/acme/settings/installations/1"; item.SettingsURL != want {
			t.Errorf("settings_url=%q, want %q", item.SettingsURL, want)
		}
		if item.ObservedAt == "" {
			t.Error("observed_at is empty; the finding cannot say when it was true")
		}
	})

	t.Run("scope drift", func(t *testing.T) {
		rec := doJSON(t, s, "POST", scopeDriftPath, map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		got := decodeFindingList[scopeDriftItem](t, rec.Body.String())
		if got.TotalCount != 1 || len(got.Items) != 1 {
			t.Fatalf("items=%d total=%d, want 1 and 1: %s", len(got.Items), got.TotalCount, rec.Body.String())
		}
		item := got.Items[0]
		if item.Slug != "acme/legacy" {
			t.Errorf("slug=%q, want acme/legacy", item.Slug)
		}
		if item.InstallationID != "1" || item.AccountLogin != "acme" {
			t.Errorf("installation=%q account=%q, want the selective installation on acme", item.InstallationID, item.AccountLogin)
		}
		if want := "https://github.com/organizations/acme/settings/installations/1"; item.SettingsURL != want {
			t.Errorf("settings_url=%q, want %q", item.SettingsURL, want)
		}
	})

	t.Run("count-only read", func(t *testing.T) {
		rec := doJSON(t, s, "POST", scopeDriftPath, map[string]any{"page_size": 0})
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		got := decodeFindingList[scopeDriftItem](t, rec.Body.String())
		if len(got.Items) != 0 || got.TotalCount != 1 {
			t.Errorf("items=%d total=%d, want no rows and the total", len(got.Items), got.TotalCount)
		}
	})

	t.Run("out-of-range page size is refused, never clamped", func(t *testing.T) {
		rec := doJSON(t, s, "POST", reachWithoutPurposePath, map[string]any{"page_size": 500})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "OUT_OF_RANGE") {
			t.Errorf("body=%s, want OUT_OF_RANGE", rec.Body.String())
		}
	})

	t.Run("unknown body field is refused", func(t *testing.T) {
		rec := doJSON(t, s, "POST", scopeDriftPath, map[string]any{"team_id": "x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400 for an unknown field", rec.Code)
		}
	})
}

// A grant of every repository cannot be drifted out of, and one of unknown
// width cannot be said to have been. The store proves the rule across all
// three; the route has to carry it, and carry the width on the installation so
// the panel can say which of "nothing drifts" and "drift is impossible here"
// it means.
func TestGitHubGrantFindings_ScopeDriftFollowsTheGrantsWidth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selection string
		wantDrift int
		wantWire  any // repository_selection on the status payload
	}{
		{"all", domain.RepositorySelectionAll, 0, "all"},
		{"selected", domain.RepositorySelectionSelected, 1, "selected"},
		{"unknown", "", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			s := newTestServer(t)
			seedGrantFixture(t, s, tc.selection)

			rec := doJSON(t, s, "POST", scopeDriftPath, map[string]any{})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := decodeFindingList[scopeDriftItem](t, rec.Body.String()); got.TotalCount != tc.wantDrift {
				t.Errorf("scope drift total=%d, want %d", got.TotalCount, tc.wantDrift)
			}

			status := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
			if status.Code != http.StatusOK {
				t.Fatalf("status GET=%d body=%s", status.Code, status.Body.String())
			}
			var out struct {
				Installations []map[string]any `json:"installations"`
			}
			if err := json.Unmarshal(status.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if len(out.Installations) != 1 {
				t.Fatalf("installations=%d, want 1", len(out.Installations))
			}
			if got := out.Installations[0]["repository_selection"]; got != tc.wantWire {
				t.Errorf("repository_selection=%v, want %v", got, tc.wantWire)
			}
		})
	}
}

// A PAT workspace holds no grant: no finding is computed for it, and the
// answer is a refusal rather than an empty list that would read as "nothing
// to address".
func TestGitHubGrantFindings_PATWorkspaceHasNoGrant(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	for _, path := range []string{reachWithoutPurposePath, scopeDriftPath} {
		rec := doJSON(t, s, "POST", path, map[string]any{})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status=%d, want 404 for a PAT workspace", path, rec.Code)
		}
	}
}

// Opening the panel is three reads — the status and the two findings — and
// none of them may ask GitHub anything or reconcile the mirror. The fake counts
// both reconciles the App store offers; opening the panel must leave both at
// zero.
func TestGitHubGrantFindings_OpeningThePanelAsksGitHubNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedGrantFixture(t, s, domain.RepositorySelectionSelected)

	real := s.githubApps
	insts, err := real.ListInstallationsForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("list installations: %v", err)
	}
	fake := &fakeGitHubAppsStore{GitHubAppsStore: real, insts: insts}
	s.githubApps = fake

	if rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil); rec.Code != http.StatusOK {
		t.Fatalf("status GET=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{reachWithoutPurposePath, scopeDriftPath} {
		if rec := doJSON(t, s, "POST", path, map[string]any{}); rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if fake.backfillCalls != 0 || fake.managedCalls != 0 {
		t.Errorf("opening the panel reconciled the mirror: backfill=%d managed=%d, want 0 and 0", fake.backfillCalls, fake.managedCalls)
	}
}

// The status payload's per-installation additions: the width of the grant
// (null when unknown), the GitHub page where it is edited, and the deployment
// flag that decides whether the empty state offers Connect.
func TestNewGitHubAppStatusResponse_InstallationGrantFields(t *testing.T) {
	sel := domain.RepositorySelectionSelected
	resp := newGitHubAppStatusResponse(domain.GitHubCredentialClassBYOApp, nil, []domain.OrgGitHubAppInstallation{
		{InstallationID: "1", AccountType: "Organization", AccountLogin: "acme", GitHubHost: "https://github.com", RepositorySelection: sel},
		{InstallationID: "2", AccountType: "User", AccountLogin: "octocat", GitHubHost: "https://ghe.example.com/"},
	}, "", "", nil)
	if len(resp.Installations) != 2 {
		t.Fatalf("installations=%d, want 2", len(resp.Installations))
	}
	org, user := resp.Installations[0], resp.Installations[1]
	if org.RepositorySelection == nil || *org.RepositorySelection != sel {
		t.Errorf("org repository_selection=%v, want %q", org.RepositorySelection, sel)
	}
	if user.RepositorySelection != nil {
		t.Errorf("user repository_selection=%q, want null for a width nobody reported", *user.RepositorySelection)
	}
	if want := "https://github.com/organizations/acme/settings/installations/1"; org.SettingsURL != want {
		t.Errorf("org settings_url=%q, want %q", org.SettingsURL, want)
	}
	if want := "https://ghe.example.com/settings/installations/2"; user.SettingsURL != want {
		t.Errorf("user settings_url=%q, want %q", user.SettingsURL, want)
	}
	// Not the constructor's to answer: the server stamps it, and a bare
	// constructor call leaves it false.
	if resp.DeploymentAppAvailable {
		t.Error("deployment_app_available=true from the constructor alone")
	}
}

// In local mode there is no deployment App to bind, whatever the environment
// says, so the empty state is never offered Connect there.
func TestGitHubAppStatus_LocalMode_NoDeploymentApp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out githubAppStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DeploymentAppAvailable {
		t.Error("deployment_app_available=true in local mode")
	}
}

// The wire contract the frontend's list hook relies on: no bare arrays, every
// paging key present even on an empty finding.
func TestGitHubGrantFindings_EmptyFindingIsAnEnvelope(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()
	stores := sqlitestore.New(s.db)
	if _, err := stores.Orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, domain.GitHubCredentialClassBYOApp); err != nil {
		t.Fatalf("set credential class: %v", err)
	}
	rec := doJSON(t, s, "POST", reachWithoutPurposePath, map[string]any{"page_size": 10})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// next_page_token is omitted on the last page, which an empty finding is;
	// the client normalizes that to "". The other two keys are unconditional.
	for _, key := range []string{"items", "total_count"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("empty finding lacks %q: %s", key, rec.Body.String())
		}
	}
	if string(raw["items"]) != "[]" {
		t.Errorf("items=%s, want [] (never null)", raw["items"])
	}
}
