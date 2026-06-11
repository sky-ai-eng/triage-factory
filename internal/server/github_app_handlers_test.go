package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeGitHubAppsStore stands in for the real store so the refresh handler's
// branches can be driven directly: GetForOrgSystem decides the 404,
// BackfillInstallationsFromAPI decides the 502, and ListInstallationsForOrgSystem
// supplies the post-reconcile installations. The embedded interface is nil, so
// any method the handler doesn't call panics — which keeps the fake honest
// about the surface the handler actually depends on.
type fakeGitHubAppsStore struct {
	db.GitHubAppsStore
	app           *domain.OrgGitHubApp
	insts         []domain.OrgGitHubAppInstallation
	listErr       error
	backfillErr   error
	backfillCalls int
}

func (f *fakeGitHubAppsStore) GetForOrgSystem(context.Context, string) (*domain.OrgGitHubApp, error) {
	return f.app, nil
}

func (f *fakeGitHubAppsStore) BackfillInstallationsFromAPI(context.Context, string) error {
	f.backfillCalls++
	return f.backfillErr
}

func (f *fakeGitHubAppsStore) ListInstallationsForOrgSystem(context.Context, string) ([]domain.OrgGitHubAppInstallation, error) {
	return f.insts, f.listErr
}

// TestGitHubAppStatus_LocalMode_NoApp returns app:null + empty
// installations when nothing is registered.
func TestGitHubAppStatus_LocalMode_NoApp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out githubAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.App != nil {
		t.Errorf("app=%+v, want nil", out.App)
	}
	if len(out.Installations) != 0 {
		t.Errorf("installations=%v, want empty", out.Installations)
	}
	if out.UsingHostedDefault {
		t.Error("using_hosted_default=true, want false in local mode")
	}
}

// TestGitHubAppStatus_BadOrgID rejects a non-UUID path segment with 404.
func TestGitHubAppStatus_BadOrgID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", "/api/orgs/not-a-uuid/github-app", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rec.Code)
	}
}

// TestGitHubAppInstallURL_LocalMode_NoApp 404s when no App is registered
// (nothing to install).
func TestGitHubAppInstallURL_LocalMode_NoApp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/install-url", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

// TestGitHubAppStatus_MultiMode exercises the read endpoints against a
// Postgres-backed auth rig: org member sees registered App + install
// URL; a non-member is 404'd.
func TestGitHubAppStatus_MultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	respA, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(respA)

	bob := rig.seedUser()
	rig.seedOrg(bob, "bob-org")
	respB, _ := rig.driveCallback(bob)
	sidB := rig.sidFromResp(respB)

	// carol is a non-admin ('member') of orgA — the endpoints are
	// intentionally member-readable, so she must get 200, not 404.
	carol := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		carol.String(), orgA.String()); err != nil {
		t.Fatalf("seed carol membership: %v", err)
	}
	respC, _ := rig.driveCallback(carol)
	sidC := rig.sidFromResp(respC)

	// Seed an org App + one installation directly.
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_github_apps (org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref, registered_by_user_id)
		VALUES ($1, '999', 'acme-bot', 'Iv1.x', 'r1', 'r2', 'r3', $2)
	`, orgA.String(), alice.String()); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_login)
		VALUES ('inst-1', $1, 'Organization', 'acme-eng')
	`, orgA.String()); err != nil {
		t.Fatalf("seed installation: %v", err)
	}

	get := func(sid, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sid})
		rec := httptest.NewRecorder()
		rig.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("member_sees_app", func(t *testing.T) {
		rec := get(sidA, "/api/orgs/"+orgA.String()+"/github-app")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}
		var out githubAppStatusResponse
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.App == nil {
			t.Fatal("app=nil, want registered App")
		}
		if out.App.Slug != "acme-bot" {
			t.Errorf("slug=%q, want acme-bot", out.App.Slug)
		}
		if len(out.Installations) != 1 || out.Installations[0].AccountLogin != "acme-eng" {
			t.Errorf("installations=%+v, want one acme-eng row", out.Installations)
		}
	})

	t.Run("install_url", func(t *testing.T) {
		rec := get(sidA, "/api/orgs/"+orgA.String()+"/github-app/install-url")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}
		var out map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := "https://github.com/apps/acme-bot/installations/new"
		if out["url"] != want {
			t.Errorf("url=%q, want %q", out["url"], want)
		}
	})

	t.Run("non_admin_member_sees_app", func(t *testing.T) {
		rec := get(sidC, "/api/orgs/"+orgA.String()+"/github-app")
		if rec.Code != http.StatusOK {
			t.Errorf("non-admin member status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("non_member_404", func(t *testing.T) {
		rec := get(sidB, "/api/orgs/"+orgA.String()+"/github-app")
		if rec.Code != http.StatusNotFound {
			t.Errorf("non-member status=%d, want 404", rec.Code)
		}
	})
}

// TestGitHubAppInstallationsRefresh_NoApp 404s when the org has no registered
// App (nothing to reconcile) and never reaches the backfill.
func TestGitHubAppInstallationsRefresh_NoApp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	fake := &fakeGitHubAppsStore{app: nil}
	s.githubApps = fake

	rec := doJSON(t, s, "POST", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/installations/refresh", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	if fake.backfillCalls != 0 {
		t.Errorf("backfill called %d times, want 0 when no App is registered", fake.backfillCalls)
	}
}

// TestGitHubAppInstallationsRefresh_Success reconciles, then returns the fresh
// installations in the same githubAppStatusResponse shape the status GET uses.
func TestGitHubAppInstallationsRefresh_Success(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	fake := &fakeGitHubAppsStore{
		app: &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", Active: true},
		insts: []domain.OrgGitHubAppInstallation{
			{InstallationID: "inst-1", OrgID: runmode.LocalDefaultOrgID, AccountType: "Organization", AccountLogin: "acme-eng"},
		},
	}
	s.githubApps = fake

	rec := doJSON(t, s, "POST", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/installations/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if fake.backfillCalls != 1 {
		t.Errorf("backfill called %d times, want exactly 1", fake.backfillCalls)
	}
	var out githubAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.App == nil || out.App.Slug != "acme-bot" {
		t.Fatalf("app=%+v, want slug acme-bot", out.App)
	}
	if len(out.Installations) != 1 || out.Installations[0].AccountLogin != "acme-eng" {
		t.Errorf("installations=%+v, want one acme-eng row", out.Installations)
	}
}

// TestGitHubAppInstallationsRefresh_BackfillError surfaces a reconcile failure
// as 502 with the error in the body.
func TestGitHubAppInstallationsRefresh_BackfillError(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	fake := &fakeGitHubAppsStore{
		app:         &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", Active: true},
		backfillErr: errors.New("github unreachable"),
	}
	s.githubApps = fake

	rec := doJSON(t, s, "POST", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/installations/refresh", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
	if fake.backfillCalls != 1 {
		t.Errorf("backfill called %d times, want exactly 1", fake.backfillCalls)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(out["error"], "github unreachable") {
		t.Errorf("error=%q, want it to contain the backfill error", out["error"])
	}
}

// TestGitHubAppInstallationsRefresh_MultiMode_AdminGate verifies the refresh is
// admin-only against a Postgres-backed auth rig: a plain member and a
// non-member are both 404'd before any reconcile, while the org admin clears
// the gate. (Unlike the member-readable status GET, this endpoint mutates the
// mirror, so it gates on RequireOrgAdmin.)
func TestGitHubAppInstallationsRefresh_MultiMode_AdminGate(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org") // alice is the org owner → admin
	respA, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(respA)

	bob := rig.seedUser()
	rig.seedOrg(bob, "bob-org")
	respB, _ := rig.driveCallback(bob)
	sidB := rig.sidFromResp(respB)

	// carol is a plain 'member' (non-admin) of orgA.
	carol := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		carol.String(), orgA.String()); err != nil {
		t.Fatalf("seed carol membership: %v", err)
	}
	respC, _ := rig.driveCallback(carol)
	sidC := rig.sidFromResp(respC)

	// Register an App for orgA so the admin's request reaches the gate's far
	// side instead of short-circuiting on the no-App 404 — the point here is
	// to observe the gate, not the App-presence check.
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_github_apps (org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref, registered_by_user_id)
		VALUES ($1, '999', 'acme-bot', 'Iv1.x', 'r1', 'r2', 'r3', $2)
	`, orgA.String(), alice.String()); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}

	path := "/api/orgs/" + orgA.String() + "/github-app/installations/refresh"

	t.Run("non_admin_member_404", func(t *testing.T) {
		if resp := rig.requestWithSid("POST", path, sidC); resp.StatusCode != http.StatusNotFound {
			t.Errorf("member status=%d, want 404 (admin-only)", resp.StatusCode)
		}
	})

	t.Run("non_member_404", func(t *testing.T) {
		if resp := rig.requestWithSid("POST", path, sidB); resp.StatusCode != http.StatusNotFound {
			t.Errorf("non-member status=%d, want 404", resp.StatusCode)
		}
	})

	t.Run("admin_clears_gate", func(t *testing.T) {
		// alice is the org owner/admin, so the gate passes. The real backfill
		// then runs against a non-existent App PEM secret and fails fast (no
		// network) — so the response is a 502. The assertion is only that the
		// admin is NOT rejected at the gate (404).
		if resp := rig.requestWithSid("POST", path, sidA); resp.StatusCode == http.StatusNotFound {
			t.Errorf("admin status=%d, want the admin gate to pass (not a 404 rejection)", resp.StatusCode)
		}
	})
}
