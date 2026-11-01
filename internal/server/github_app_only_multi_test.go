package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// A multi org's GitHub access is an App: the routes that bind an org token do
// not exist, the one that removes it does, and an imported App must bring the
// client secret members link their accounts through.

// sendOrg sends method to one of the org's own routes from the admin's
// signed-in browser, with body as JSON when it is non-nil.
func (r *bindRig) sendOrg(t *testing.T, method, route string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/api/orgs/"+r.orgID.String()+route, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: r.sid})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// seedPAT stores an org token directly, the state a development database
// carried over from before multi orgs were App-only would be in.
func (r *bindRig) seedPAT(t *testing.T, pat string) {
	t.Helper()
	ctx := t.Context()
	if err := r.srv.tx.WithTx(ctx, r.orgID.String(), r.userID.String(), func(tx db.TxStores) error {
		return integrations.Save(ctx, tx.Secrets, r.orgID.String(), auth.Credentials{GitHubPAT: pat})
	}); err != nil {
		t.Fatalf("seed pat: %v", err)
	}
}

func TestOrgTokenRoutes_MultiModeIs404(t *testing.T) {
	rig := newBindRig(t, newFakeGitHub())
	rig.seedPAT(t, "ghp_old")
	calls := rig.gh.callCount()

	for _, c := range []struct{ method, route string }{
		{http.MethodPut, "/github/pat"},
		{http.MethodPost, "/github/pat/switch-to"},
		{http.MethodPost, "/github/pat/preflight"},
	} {
		rec := rig.sendOrg(t, c.method, c.route, map[string]string{"pat": "ghp_new"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s in multi mode = %d, want 404 (body: %s)", c.method, c.route, rec.Code, rec.Body.String())
			continue
		}
		assertFirstError(t, rec, httpx.ReasonNotFound, "")
	}
	if rig.gh.callCount() != calls {
		t.Error("a token was checked against GitHub on a route that does not exist")
	}
	if pat := rig.storedPAT(t); pat != "ghp_old" {
		t.Errorf("stored token = %q after the refused binds, want the seeded one", pat)
	}

	// The unbind stays, so an org that holds a token can remove it.
	if rec := rig.sendOrg(t, http.MethodDelete, "/github/pat", nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE /github/pat in multi mode = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if pat := rig.storedPAT(t); pat != "" {
		t.Error("DELETE /github/pat left the token stored")
	}
}

func TestGitHubAppImport_MultiModeRequiresClientSecret(t *testing.T) {
	importRig := func(t *testing.T, tokenStatus int) *bindRig {
		t.Helper()
		rig := newBindRig(t, newFakeGitHub())
		stub := newImportStub(t, importStubCfg{appID: 11, slug: "acme-bot", clientID: "Iv1.x", tokenStatus: tokenStatus})
		rig.setGitHubHost(t, stub.URL)
		return rig
	}

	for _, c := range []struct {
		name  string
		extra map[string]any
	}{
		{"absent", nil},
		{"empty", map[string]any{"client_secret": ""}},
		{"blank", map[string]any{"client_secret": "   "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := importRig(t, 0)
			rec := rig.sendOrg(t, http.MethodPost, "/github/app/import", importBody("11", testRSAPEM(t), c.extra))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("import without a client secret = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			items := errorItems(t, rec)
			if len(items) != 1 || items[0].Reason != httpx.ReasonMissingField || items[0].Field != "client_secret" {
				t.Fatalf("errors = %+v, want one MISSING_FIELD on client_secret", items)
			}
			if !strings.Contains(items[0].Message, "sign in with GitHub") {
				t.Errorf("message = %q, want it to say the secret is how members sign in with GitHub", items[0].Message)
			}
			if rig.hasAppRow(t) {
				t.Error("an import refused for a missing client secret wrote an App row")
			}
		})
	}

	t.Run("every_missing_field_at_once", func(t *testing.T) {
		rig := importRig(t, 0)
		rec := rig.sendOrg(t, http.MethodPost, "/github/app/import", map[string]any{})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty import = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		var fields []string
		for _, it := range errorItems(t, rec) {
			if it.Reason != httpx.ReasonMissingField {
				t.Errorf("reason = %q for %q, want MISSING_FIELD", it.Reason, it.Field)
			}
			fields = append(fields, it.Field)
		}
		if got := strings.Join(fields, ","); got != "app_id,pem,client_secret" {
			t.Errorf("missing fields = %s, want app_id,pem,client_secret", got)
		}
	})

	t.Run("rejected_secret_is_422", func(t *testing.T) {
		rig := importRig(t, http.StatusUnauthorized)
		rec := rig.sendOrg(t, http.MethodPost, "/github/app/import",
			importBody("11", testRSAPEM(t), map[string]any{"client_secret": "wrong"}))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("import with a rejected client secret = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonUpstreamRejected, "client_secret")
		if rig.hasAppRow(t) {
			t.Error("an import with a rejected client secret wrote an App row")
		}
	})

	t.Run("with_a_secret_it_imports", func(t *testing.T) {
		rig := importRig(t, 0)
		rec := rig.sendOrg(t, http.MethodPost, "/github/app/import",
			importBody("11", testRSAPEM(t), map[string]any{"client_secret": "cs"}))
		if rec.Code != http.StatusOK {
			t.Fatalf("import with a client secret = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var out githubAppImportResponse
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !out.ClientSecretStored || !out.ClientSecretValidated {
			t.Errorf("client secret stored=%v validated=%v, want both true", out.ClientSecretStored, out.ClientSecretValidated)
		}
	})
}
