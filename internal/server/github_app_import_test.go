package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// importStubCfg configures the fake GitHub the import handler talks to: GET /app
// (the validate-and-derive round trip) and POST /applications/{client_id}/token
// (the client-secret check). All fields are optional — a test sets only what it
// exercises.
type importStubCfg struct {
	appID       int64             // GET /app returns this id (the mismatch test makes it differ from the submitted app_id)
	slug        string            // GET /app slug
	clientID    string            // GET /app client_id
	ownerType   string            // GET /app owner.type ("Organization"/"User"); default "Organization"
	permissions map[string]string // GET /app permissions; nil → all granted (passes preflight)
	appStatus   int               // GET /app status; 0 → 200
	tokenStatus int               // POST /applications/{cid}/token status; 0 → 404 (valid secret)
	// botUserID drives GET /users/<slug>[bot] (the TFAC-474 best-effort fetch):
	// >0 → 200 {"id": botUserID}; 0 → 404, modeling the propagation-delay /
	// not-found case where the bot user id stays unknown (stored NULL).
	botUserID int64
}

// importFullPerms grants every permission the manifest requests, so a stub left
// at the default passes the preflight cleanly.
var importFullPerms = map[string]string{
	"issues":        "write",
	"pull_requests": "write",
	"contents":      "write",
	"metadata":      "read",
	"checks":        "read",
	"actions":       "read",
	"statuses":      "read",
	"members":       "read",
}

func newImportStub(t *testing.T, cfg importStubCfg) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/app", func(w http.ResponseWriter, r *http.Request) {
		if cfg.appStatus != 0 && cfg.appStatus != http.StatusOK {
			http.Error(w, `{"message":"unauthorized"}`, cfg.appStatus)
			return
		}
		perms := cfg.permissions
		if perms == nil {
			perms = importFullPerms
		}
		owner := cfg.ownerType
		if owner == "" {
			owner = "Organization"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          cfg.appID,
			"slug":        cfg.slug,
			"client_id":   cfg.clientID,
			"owner":       map[string]any{"login": "acme-eng", "type": owner},
			"permissions": perms,
			"events":      []string{"pull_request"},
		})
	})

	// POST /api/v3/applications/{client_id}/token — the check-token endpoint.
	mux.HandleFunc("/api/v3/applications/", func(w http.ResponseWriter, r *http.Request) {
		st := cfg.tokenStatus
		if st == 0 {
			st = http.StatusNotFound // GitHub's "creds good, token doesn't exist"
		}
		w.WriteHeader(st)
	})

	// GET /api/v3/users/{login} — the bot-user-id lookup (TFAC-474). Serves the
	// configured id when set, else 404 (bot account not found / not yet
	// propagated) so the best-effort fetch fails and the row stores NULL.
	mux.HandleFunc("/api/v3/users/", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.botUserID == 0 {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": cfg.botUserID})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// importBody is the JSON the frontend POSTs; a map keeps the test call sites
// readable and omits absent optional fields.
func importBody(appID, pem string, extra map[string]any) map[string]any {
	b := map[string]any{"app_id": appID, "pem": pem}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// TestGitHubAppImport_HappyPath_FreshSetup imports with no org PAT: the row
// lands active=true, owner_type comes from GET /app (Organization → org), the
// PEM + both secrets are vaulted, and the response carries the validated client
// secret + connect_callback_url.
func TestGitHubAppImport_HappyPath_FreshSetup(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	stub := newImportStub(t, importStubCfg{
		appID: 123, slug: "acme-bot", clientID: "Iv1.abc", ownerType: "Organization",
		tokenStatus: http.StatusNotFound, // valid client secret
	})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("123", testRSAPEM(t), map[string]any{"client_secret": "cs", "webhook_secret": "wh"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("no app row written by import")
	}
	if app.AppID != "123" || app.Slug != "acme-bot" || app.ClientID != "Iv1.abc" {
		t.Errorf("app = %+v, want app_id=123 slug=acme-bot client_id=Iv1.abc", app)
	}
	if app.OwnerType != "org" {
		t.Errorf("owner_type = %q, want org (mapped from Organization)", app.OwnerType)
	}
	if !app.Active {
		t.Error("Active = false, want true (no PAT → fresh setup, not staged)")
	}
	if app.ClientSecretRef == "" || app.WebhookSecretRef == "" || app.PEMRef == "" {
		t.Errorf("secret refs = %+v, want all set", app)
	}
	// Secrets actually vaulted.
	if v := getSecret(t, s, app.PEMRef); v == "" {
		t.Error("pem secret not stored")
	}
	if v := getSecret(t, s, app.ClientSecretRef); v != "cs" {
		t.Errorf("client secret = %q, want cs", v)
	}
	if v := getSecret(t, s, app.WebhookSecretRef); v != "wh" {
		t.Errorf("webhook secret = %q, want wh", v)
	}

	var out githubAppImportResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.ClientSecretStored || !out.ClientSecretValidated {
		t.Errorf("client secret stored=%v validated=%v, want both true", out.ClientSecretStored, out.ClientSecretValidated)
	}
	want := "http://localhost:3000/api/orgs/" + runmode.LocalDefaultOrgID + "/github/connect/callback"
	if out.ConnectCallbackURL != want {
		t.Errorf("connect_callback_url = %q, want %q", out.ConnectCallbackURL, want)
	}
	if len(out.Permissions) != len(importRequiredPermissions) {
		t.Errorf("permissions table has %d rows, want %d", len(out.Permissions), len(importRequiredPermissions))
	}
}

// TestGitHubAppImport_PersistsBotUserID pins TFAC-474's import-flow write: when
// GET /users/<slug>[bot] resolves, the numeric bot user id is persisted on the
// org_github_apps row so OrgIdentityFor can later build the numeric-id noreply
// commit email.
func TestGitHubAppImport_PersistsBotUserID(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{
		appID: 41, slug: "acme-bot", clientID: "Iv1.x", botUserID: 41898282,
	})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("41", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("no app row written")
	}
	if app.BotUserID != 41898282 {
		t.Errorf("BotUserID = %d, want 41898282 (fetched at import)", app.BotUserID)
	}
}

// TestGitHubAppImport_BotUserIDFetchFailure_StoresNull pins that a failed
// GET /users/<slug>[bot] (here a 404) is best-effort: the import still succeeds
// and the row is written with bot_user_id NULL (0), so the resolver falls back
// to the plain noreply form — never a worse state than before TFAC-474.
func TestGitHubAppImport_BotUserIDFetchFailure_StoresNull(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	// botUserID left 0 → the stub's /users handler 404s.
	stub := newImportStub(t, importStubCfg{appID: 42, slug: "acme-bot", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("42", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (bot-id fetch failure must not block import); body=%s", rec.Code, rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("no app row written — a best-effort bot-id fetch failure must not abort the import")
	}
	if app.BotUserID != 0 {
		t.Errorf("BotUserID = %d, want 0 (NULL on fetch failure)", app.BotUserID)
	}
}

// TestGitHubAppImport_StagedWhenPATLive imports while an org PAT is live: the
// staging rule writes active=false (the PAT stays the live credential until a
// cutover), exactly as the manifest callback does.
func TestGitHubAppImport_StagedWhenPATLive(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 7, slug: "acme-bot", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("7", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("no app row written")
	}
	if app.Active {
		t.Error("Active = true, want false (org PAT live → staged)")
	}
	// No secret provided → ref empty.
	if app.ClientSecretRef != "" {
		t.Errorf("ClientSecretRef = %q, want empty (no client secret provided)", app.ClientSecretRef)
	}
}

// TestGitHubAppImport_BaseURLFromSecret pins the resolver-precedence fix: an org
// with an empty org_settings.github_base_url but a stored github_url secret (a
// supported GHES / local-mode shape, where the host lives only in the keychain)
// resolves the validation host from the secret rather than defaulting to public
// github.com — so GET /app reaches the right host and the import succeeds. With
// the old settings-only derivation this would target github.com and fail.
func TestGitHubAppImport_BaseURLFromSecret(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 77, slug: "ghes-bot", clientID: "Iv1.x"})
	// Deliberately do NOT set org_settings.github_base_url; the host lives only
	// in the github_url secret — the keychain-first state BaseURLFor handles.
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubURL, stub.URL, ""); err != nil {
		t.Fatalf("seed github_url secret: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("77", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (base URL resolved from the github_url secret); body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("no app row written — import didn't reach the secret-derived host")
	}
}

// TestGitHubAppImport_AppIDMismatch rejects a PEM whose App reports a different
// id than the submitted app_id, and writes no row.
func TestGitHubAppImport_AppIDMismatch(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 999, slug: "other-bot", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("123", testRSAPEM(t), nil)) // submit 123, GitHub says 999
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("import = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app row written on mismatch: %+v", app)
	}
}

// TestGitHubAppImport_BadPEM 422s on an unparseable private key before any
// network call.
func TestGitHubAppImport_BadPEM(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newImportStub(t, importStubCfg{appID: 1})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("1", "not-a-pem", nil))
	// A PEM that isn't a PEM is a shape fault, not a semantic one: 400, in
	// line with the epic's 400-vs-422 rule.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGitHubAppImport_HardPermGap blocks with the full granted-vs-required table
// when a core permission is missing, regardless of accept_partial.
func TestGitHubAppImport_HardPermGap(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	// issues only read (needs write) — a hard gap.
	perms := map[string]string{
		"issues": "read", "pull_requests": "write", "contents": "write", "metadata": "read",
		"checks": "read", "actions": "read", "statuses": "read", "members": "read",
	}
	stub := newImportStub(t, importStubCfg{appID: 5, slug: "b", clientID: "Iv1.x", permissions: perms})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("5", testRSAPEM(t), map[string]any{"accept_partial": true})) // even with accept_partial
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("import = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var out githubAppImportErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Blocking {
		t.Error("blocking = false, want true (hard gap)")
	}
	// The table rides alongside the envelope, not instead of it: the shared
	// client parser has to find the message where it finds every other one.
	if len(out.Errors) != 1 || out.Errors[0].Reason != httpx.ReasonPermissionGap ||
		!strings.Contains(out.Errors[0].Message, "issues") {
		t.Errorf("errors = %+v, want one PERMISSION_GAP item naming issues", out.Errors)
	}
	if len(out.Permissions) != len(importRequiredPermissions) {
		t.Errorf("permissions table has %d rows, want the full %d", len(out.Permissions), len(importRequiredPermissions))
	}
	var issues githubAppPermissionRow
	for _, p := range out.Permissions {
		if p.Permission == "issues" {
			issues = p
		}
	}
	if issues.Satisfied {
		t.Error("issues row Satisfied = true, want false (read < write)")
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app row written despite hard gap: %+v", app)
	}
}

// TestGitHubAppImport_SoftPermGap blocks without accept_partial (non-blocking),
// then imports once it's set.
func TestGitHubAppImport_SoftPermGap(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	// members missing entirely — a soft gap (degrades team import).
	perms := map[string]string{
		"issues": "write", "pull_requests": "write", "contents": "write", "metadata": "read",
		"checks": "read", "actions": "read", "statuses": "read",
	}
	stub := newImportStub(t, importStubCfg{appID: 8, slug: "soft", clientID: "Iv1.x", permissions: perms})
	setOrgGitHubBase(t, s, stub.URL)
	path := "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/app/import"

	// Without accept_partial → 422, non-blocking, table present.
	rec := doJSON(t, s, http.MethodPost, path, importBody("8", testRSAPEM(t), nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("import without ack = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var out githubAppImportErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Blocking {
		t.Error("blocking = true, want false (soft gap is acknowledgeable)")
	}
	if len(out.Errors) != 1 || out.Errors[0].Reason != httpx.ReasonPermissionGap ||
		!strings.Contains(out.Errors[0].Message, "members") {
		t.Errorf("errors = %+v, want one PERMISSION_GAP item naming members", out.Errors)
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app != nil {
		t.Fatalf("app written before acknowledgment: %+v", app)
	}

	// With accept_partial → imports.
	rec = doJSON(t, s, http.MethodPost, path, importBody("8", testRSAPEM(t), map[string]any{"accept_partial": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("import with ack = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("no app row after acknowledged soft gap")
	}
}

// TestGitHubAppImport_BadClientSecret rejects a client secret GitHub's
// check-token endpoint 401s, and writes no row.
func TestGitHubAppImport_BadClientSecret(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{
		appID: 11, slug: "b", clientID: "Iv1.x",
		tokenStatus: http.StatusUnauthorized, // bad client credentials
	})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("11", testRSAPEM(t), map[string]any{"client_secret": "wrong"}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("import = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app written despite bad client secret: %+v", app)
	}
}

// TestGitHubAppImport_ClientSecretUnknown_StoresUnvalidated pins the
// inconclusive branch of the check-token contract: a status that's neither 401
// (bad) nor 404 (valid) stores the secret anyway and flags
// client_secret_validated=false — the only path that persists a potentially-bad
// credential, so its behavior is documented.
func TestGitHubAppImport_ClientSecretUnknown_StoresUnvalidated(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{
		appID: 21, slug: "b", clientID: "Iv1.x",
		tokenStatus: http.StatusInternalServerError, // neither 401 nor 404 → inconclusive
	})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("21", testRSAPEM(t), map[string]any{"client_secret": "maybe-good"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (inconclusive check stores unvalidated); body=%s", rec.Code, rec.Body.String())
	}
	var out githubAppImportResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.ClientSecretStored {
		t.Error("client_secret_stored = false, want true (the secret was stored)")
	}
	if out.ClientSecretValidated {
		t.Error("client_secret_validated = true, want false (inconclusive check)")
	}
	// The secret is persisted despite being unvalidated.
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil || app.ClientSecretRef == "" {
		t.Fatalf("app = %+v, want a stored client secret ref", app)
	}
	if v := getSecret(t, s, app.ClientSecretRef); v != "maybe-good" {
		t.Errorf("stored client secret = %q, want maybe-good", v)
	}
}

// TestGitHubAppImport_PersonalOwnerType covers mapAppOwnerType's non-org branch:
// GitHub's "User" account type maps to owner_type "user".
func TestGitHubAppImport_PersonalOwnerType(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 31, slug: "personal-bot", clientID: "Iv1.x", ownerType: "User"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("31", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("no app row written")
	}
	if app.OwnerType != "user" {
		t.Errorf("owner_type = %q, want user (mapped from GitHub's User)", app.OwnerType)
	}
}

// TestGitHubAppImport_OccupiedSlot 409s when the org already has an App (a
// staged one occupies the slot too).
func TestGitHubAppImport_OccupiedSlot(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true) // existing live app

	stub := newImportStub(t, importStubCfg{appID: 2, slug: "b", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("2", testRSAPEM(t), nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("import into occupied slot = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGitHubAppImport_NoSecret_ConnectUnavailable imports without a client
// secret: the ref is empty and the identity status then reports
// connect_available=false (the tightened §3 gate falls back to PAT capture).
func TestGitHubAppImport_NoSecret_ConnectUnavailable(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 3, slug: "b", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("3", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil || app.ClientSecretRef != "" {
		t.Fatalf("app = %+v, want ClientSecretRef empty", app)
	}

	rec = doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("identity status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var idStatus githubIdentityStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&idStatus); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if idStatus.ConnectAvailable {
		t.Error("connect_available = true, want false (App has client_id but no stored client secret)")
	}
}

// TestGitHubAppStatus_CarriesConnectCallbackURL pins §4: the status response
// carries connect_callback_url, built from the deploy config's public URL.
func TestGitHubAppStatus_CarriesConnectCallbackURL(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out githubAppStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "http://localhost:3000/api/orgs/" + runmode.LocalDefaultOrgID + "/github/connect/callback"
	if out.ConnectCallbackURL != want {
		t.Errorf("connect_callback_url = %q, want %q", out.ConnectCallbackURL, want)
	}
}
