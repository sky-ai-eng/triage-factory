package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

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
