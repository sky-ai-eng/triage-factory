package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// testLinearDeploymentApp is the deployment's Linear app in these tests.
var testLinearDeploymentApp = linear.OAuthApp{ClientID: "dep-client", ClientSecret: "dep-secret"}

// getLinearAppStatus reads GET …/linear/app, decoded and as its raw keys, so
// a test can tell an omitted field from an empty one.
func (r *linearInstallRig) getLinearAppStatus(t *testing.T) (linearAppStatusResponse, map[string]json.RawMessage) {
	t.Helper()
	rec := doJSON(t, r.s, http.MethodGet, linearAppPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET linear app: %d: %s", rec.Code, rec.Body.String())
	}
	var got linearAppStatusResponse
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	return got, raw
}

// TestLinearApp_StatusFollowsTheResolvedSource pins the app and access status
// shapes in each of the three states the precedence resolves to. The org's own
// app wins over the deployment app; the deployment app alone offers Install
// with nothing for the admin to create; with neither there is nothing to
// install and the card offers the pre-filled create link.
func TestLinearApp_StatusFollowsTheResolvedSource(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rig           func(t *testing.T) *linearInstallRig
		wantOwnApp    bool
		wantInstall   bool
		wantDeploy    bool
		wantCreateURL bool
	}{
		{
			name: "org override",
			rig: func(t *testing.T) *linearInstallRig {
				r := newLinearInstallRigWith(t, testLinearDeploymentApp)
				r.registerApp(t, "lin-client-1")
				return r
			},
			wantOwnApp: true, wantInstall: true, wantCreateURL: true,
		},
		{
			name:        "deployment default",
			rig:         func(t *testing.T) *linearInstallRig { return newLinearInstallRigWith(t, testLinearDeploymentApp) },
			wantInstall: true, wantDeploy: true,
		},
		{
			name:          "none",
			rig:           func(t *testing.T) *linearInstallRig { return newLinearInstallRigWith(t, linear.OAuthApp{}) },
			wantCreateURL: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.rig(t)
			got, raw := r.getLinearAppStatus(t)
			if (got.App != nil) != tc.wantOwnApp || got.InstallAvailable != tc.wantInstall || got.UsingDeploymentDefault != tc.wantDeploy {
				t.Errorf("app status = %+v, want own app %v, install %v, deployment default %v",
					got, tc.wantOwnApp, tc.wantInstall, tc.wantDeploy)
			}
			if _, present := raw["create_url"]; present != tc.wantCreateURL {
				t.Errorf("create_url present = %v, want %v (body keys %v)", present, tc.wantCreateURL, raw)
			}
			// An app of the org's own can be registered by hand in every
			// state, so the redirect URIs are always listed.
			if len(got.RedirectURIs) != 2 {
				t.Errorf("redirect_uris = %v, want both callbacks", got.RedirectURIs)
			}

			access := r.access(t)
			if access.ConnectAvailable != tc.wantInstall || access.UsingDeploymentDefault != tc.wantDeploy {
				t.Errorf("access = %+v, want connect available %v, deployment default %v",
					access, tc.wantInstall, tc.wantDeploy)
			}
		})
	}
}

// TestLinearInstall_DeploymentApp: an org with no app of its own installs
// through the deployment app, and the install records that app as the one its
// tokens belong to. Disconnecting revokes with it. With the deployment app
// gone, the same org's card falls back to registering its own.
func TestLinearInstall_DeploymentApp(t *testing.T) {
	r := newLinearInstallRigWith(t, testLinearDeploymentApp)
	r.install(t)

	inst := r.installRow(t)
	if inst == nil || !inst.Live() || inst.AppClientID != testLinearDeploymentApp.ClientID {
		t.Fatalf("install row = %+v, want live under the deployment app", inst)
	}
	env, err := linear.ParseInstallCredential(r.secret(t, integrations.KeyLinearAppInstall))
	if err != nil {
		t.Fatalf("install envelope: %v", err)
	}
	if env.ClientID != testLinearDeploymentApp.ClientID {
		t.Errorf("envelope client = %q, want the deployment app's", env.ClientID)
	}
	if got := r.access(t); !got.Connected || got.AuthMethod != "app_install" || !got.UsingDeploymentDefault {
		t.Errorf("access = %+v, want installed on the deployment app", got)
	}

	if rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	r.expectKick(t)
	if got := r.oauth.RevokedBy(); !slices.Equal(got, []string{testLinearDeploymentApp.ClientID}) {
		t.Errorf("revoked as %v, want the deployment app", got)
	}

	r.s.linearOAuthApps = linear.NewOAuthAppResolver(r.stores.LinearApps, r.stores.Secrets, linear.OAuthApp{})
	got, raw := r.getLinearAppStatus(t)
	if got.InstallAvailable || got.UsingDeploymentDefault || got.App != nil {
		t.Errorf("app status with no deployment app = %+v, want nothing to install", got)
	}
	if _, present := raw["create_url"]; !present {
		t.Errorf("create_url absent with no deployment app, want the pre-filled link (body keys %v)", raw)
	}
}

// TestLinearInstall_DeploymentAppWorkspaceTaken: two orgs on the one
// deployment app cannot both install the same workspace. The second is
// refused, and the pair it minted is revoked with the deployment app.
func TestLinearInstall_DeploymentAppWorkspaceTaken(t *testing.T) {
	r := newLinearInstallRigWith(t, testLinearDeploymentApp)
	const otherOrg = "22222222-2222-2222-2222-222222222222"
	if _, err := r.s.db.Exec(`INSERT INTO orgs (id, slug, name) VALUES (?, 'other', 'Other')`, otherOrg); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := r.stores.LinearInstalls.UpsertSystem(t.Context(), domain.OrgLinearInstall{
		OrgID: otherOrg, InstallID: "inst-other", WorkspaceID: "org-acme", WorkspaceURLKey: "acme", AppUserID: "lu-other",
		AppClientID: testLinearDeploymentApp.ClientID, InstalledAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed other org's install: %v", err)
	}

	cookie, state := r.start(t)
	rec := r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	expectRedirect(t, rec, "linear_error", "workspace_taken")
	r.assertNothingWritten(t)
	if got := r.oauth.RevokedBy(); !slices.Equal(got, []string{testLinearDeploymentApp.ClientID, testLinearDeploymentApp.ClientID}) {
		t.Errorf("revoked as %v, want the minted pair revoked with the deployment app", got)
	}
	r.expectNoKick(t)
}
