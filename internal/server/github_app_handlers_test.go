package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
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

	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
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
	if out.UsingDeploymentDefault {
		t.Error("using_deployment_default=true, want false in local mode")
	}
}

// TestNewGitHubAppStatusResponse_CarriesActive pins the §4 contract the
// PAT↔App switching UX depends on: the status payload's `active` flag mirrors
// the registration's Active bit for both a staged (active=false, mid-switch)
// and a live (active=true) App, so the frontend can tell the staged window
// apart from a live App. A pure mapping test — no DB, no Docker — so it always
// runs.
func TestNewGitHubAppStatusResponse_CarriesActive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{"staged", false},
		{"live", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &domain.OrgGitHubApp{
				OrgID:  runmode.LocalDefaultOrgID,
				AppID:  "123",
				Slug:   "acme-bot",
				Active: tc.active,
			}
			resp := newGitHubAppStatusResponse(domain.GitHubCredentialClassBYOApp, app, nil, "", "", nil)
			if resp.App == nil {
				t.Fatal("App=nil, want the mapped registration")
			}
			if resp.App.Active != tc.active {
				t.Errorf("active=%v, want %v", resp.App.Active, tc.active)
			}
		})
	}

	// The status payload also carries each installation's suspension, so the
	// panel can distinguish an installation whose tokens GitHub refuses from a
	// working one. Nothing in the UI reads these yet — the surface is
	// deliberately inert beyond the DTO — but a field the front end cannot see
	// is a field it cannot adopt.
	t.Run("installation suspension", func(t *testing.T) {
		suspendedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
		resp := newGitHubAppStatusResponse(domain.GitHubCredentialClassBYOApp, nil, []domain.OrgGitHubAppInstallation{
			{InstallationID: "456", AccountType: "Organization", AccountLogin: "acme"},
			{InstallationID: "789", AccountType: "Organization", AccountLogin: "beta", SuspendedAt: suspendedAt, SuspendedBy: "octocat"},
		}, "", "", nil)
		if len(resp.Installations) != 2 {
			t.Fatalf("installations=%d, want 2", len(resp.Installations))
		}
		// A live installation reports "" rather than the zero instant
		// formatted, which would read as suspended since year one.
		if got := resp.Installations[0]; got.SuspendedAt != "" || got.SuspendedBy != "" {
			t.Errorf("live installation: suspended_at=%q suspended_by=%q, want both empty", got.SuspendedAt, got.SuspendedBy)
		}
		got := resp.Installations[1]
		if want := suspendedAt.Format(time.RFC3339); got.SuspendedAt != want {
			t.Errorf("suspended_at=%q, want %q", got.SuspendedAt, want)
		}
		if got.SuspendedBy != "octocat" {
			t.Errorf("suspended_by=%q, want %q", got.SuspendedBy, "octocat")
		}
	})

	// The payload also carries the App-webhook probe's answer, so the panel can
	// tell an App that receives deliveries from one that receives none. The
	// distinction the block has to preserve is between "no answer yet" and any
	// answer at all: a null must never be renderable as healthy.
	t.Run("webhook health", func(t *testing.T) {
		app := &domain.OrgGitHubApp{OrgID: runmode.LocalDefaultOrgID, AppID: "123", Slug: "acme-bot", Active: true}

		unprobed := newGitHubAppStatusResponse(domain.GitHubCredentialClassBYOApp, app, nil, "", "", nil)
		if unprobed.WebhookHealth != nil {
			t.Errorf("webhook_health=%+v with no probe answer, want null", unprobed.WebhookHealth)
		}

		health := &githubAppWebhookHealth{
			State:                  string(githubapp.WebhookStateRejected),
			HookHost:               "https://tf.example.org",
			SecretConfigured:       true,
			LastDeliveryAt:         "2026-08-15T12:00:00Z",
			LastDeliveryStatusCode: 401,
			CheckedAt:              "2026-08-15T12:01:00Z",
		}
		resp := newGitHubAppStatusResponse(domain.GitHubCredentialClassBYOApp, app, nil, "", "", health)
		if resp.WebhookHealth == nil {
			t.Fatal("webhook_health=null, want the probe answer")
		}
		if resp.WebhookHealth.State != string(githubapp.WebhookStateRejected) {
			t.Errorf("state=%q, want %q", resp.WebhookHealth.State, githubapp.WebhookStateRejected)
		}
		if resp.WebhookHealth.LastDeliveryStatusCode != 401 {
			t.Errorf("last_delivery_status_code=%d, want 401", resp.WebhookHealth.LastDeliveryStatusCode)
		}
		if resp.WebhookHealth.HookHost != "https://tf.example.org" {
			t.Errorf("hook_host=%q, want the configured origin", resp.WebhookHealth.HookHost)
		}

		// An unknown credential class renders no App, and must not describe the
		// webhooks of a registration it just declined to render.
		unknown := newGitHubAppStatusResponse(domain.GitHubCredentialClass("mystery"), app, nil, "", "", health)
		if unknown.WebhookHealth != nil {
			t.Errorf("webhook_health=%+v for an unknown credential class, want null", unknown.WebhookHealth)
		}
	})
}

// TestGitHubAppStatus_BadOrgID rejects a non-UUID path segment with 404.
func TestGitHubAppStatus_BadOrgID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", "/api/orgs/not-a-uuid/github/app", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rec.Code)
	}
}

// TestGitHubAppInstallURL_LocalMode_NoApp 404s when no App is registered
// (nothing to install).
func TestGitHubAppInstallURL_LocalMode_NoApp(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/install-url", nil)
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
	seedPGBYOAppCredentialClass(t, rig, orgA.String())
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
		rec := get(sidA, "/api/orgs/"+orgA.String()+"/github/app")
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
		// The seed INSERT omits owner_type, so the column default flows
		// through to the status response.
		if out.App.OwnerType != "user" {
			t.Errorf("owner_type=%q, want user (column default)", out.App.OwnerType)
		}
		// The seed INSERT omits `active`, so the column default (true) flows
		// through — a live App reports active=true (§4 over the real handler).
		if !out.App.Active {
			t.Errorf("active=%v, want true (column default → live App)", out.App.Active)
		}
		if len(out.Installations) != 1 || out.Installations[0].AccountLogin != "acme-eng" {
			t.Errorf("installations=%+v, want one acme-eng row", out.Installations)
		}
	})

	t.Run("install_url", func(t *testing.T) {
		rec := get(sidA, "/api/orgs/"+orgA.String()+"/github/app/install-url")
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
		rec := get(sidC, "/api/orgs/"+orgA.String()+"/github/app")
		if rec.Code != http.StatusOK {
			t.Errorf("non-admin member status=%d body=%s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("non_member_404", func(t *testing.T) {
		rec := get(sidB, "/api/orgs/"+orgA.String()+"/github/app")
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

	rec := doJSON(t, s, "POST", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/installations/refresh", nil)
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
	seedBYOAppCredentialClass(t, s, runmode.LocalDefaultOrgID)

	rec := doJSON(t, s, "POST", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/installations/refresh", nil)
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
	seedBYOAppCredentialClass(t, s, runmode.LocalDefaultOrgID)

	rec := doJSON(t, s, "POST", "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/installations/refresh", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
	if fake.backfillCalls != 1 {
		t.Errorf("backfill called %d times, want exactly 1", fake.backfillCalls)
	}
	if !strings.Contains(rec.Body.String(), "github unreachable") {
		t.Errorf("body=%s, want it to contain the backfill error", rec.Body.String())
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
	seedPGBYOAppCredentialClass(t, rig, orgA.String())

	path := "/api/orgs/" + orgA.String() + "/github/app/installations/refresh"

	// A member of the org can see the org, so the admin gate answers 403 and
	// names the role; only a non-member gets the non-disclosure 404.
	t.Run("non_admin_member_403", func(t *testing.T) {
		if resp := rig.requestWithSid("POST", path, sidC); resp.StatusCode != http.StatusForbidden {
			t.Errorf("member status=%d, want 403 (admin-only)", resp.StatusCode)
		}
	})

	t.Run("non_member_404", func(t *testing.T) {
		if resp := rig.requestWithSid("POST", path, sidB); resp.StatusCode != http.StatusNotFound {
			t.Errorf("non-member status=%d, want 404", resp.StatusCode)
		}
	})

	t.Run("admin_clears_gate", func(t *testing.T) {
		// alice is the org owner/admin, so the gate passes and the real backfill
		// runs. The App's PEM secret was never stored, so the reconcile fails
		// fast (no network — DiscoverAppInstallations reads the PEM first) and
		// the handler returns 502. Asserting exactly 502 confirms both that the
		// admin cleared the RequireOrgAdmin gate (not a 404) and that the
		// failure is the reconcile rather than authorization.
		if resp := rig.requestWithSid("POST", path, sidA); resp.StatusCode != http.StatusBadGateway {
			t.Errorf("admin status=%d, want 502 (gate passes, backfill fails on the missing PEM)", resp.StatusCode)
		}
	})
}

// seedPGBYOAppCredentialClass records the BYO-App credential class for a
// Postgres-rig org, which is what an App registration writes in the same
// transaction as the org_github_apps row. A fixture that inserts the row
// directly has to write this too — the handlers gate on the class before they
// look for a registration at all.
func seedPGBYOAppCredentialClass(t *testing.T, rig *authRig, orgID string) {
	t.Helper()
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_settings (org_id, github_credential_class)
		VALUES ($1, 'byo_app')
		ON CONFLICT (org_id) DO UPDATE SET github_credential_class = 'byo_app'
	`, orgID); err != nil {
		t.Fatalf("seed org_settings credential class: %v", err)
	}
}
