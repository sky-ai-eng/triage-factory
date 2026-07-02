package slack

// Internal (package slack) pgtest-backed handler suite. Deliberately NOT the
// full black-box HTTP rig ee/sso uses (which stands up a real GoTrue login
// flow purely to mint a session cookie) — Slack's handlers need only an org
// context + verified claims in the request, so tests build those directly
// via httpx.WithOrgID/WithClaims and call the handler methods, mirroring
// internal/server's team_archive_handler_test.go pattern. That also lets
// tests reach the unexported workspacesHandler + slackAPIBase test seam
// directly.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ---------- fake Slack API (auth.test + apps.connections.open) ----------

type fakeSlackServer struct {
	srv *httptest.Server

	mu           sync.Mutex
	authTest     map[string]map[string]any // bot token -> {"ok":true, "team_id":...} or {"ok":false,"error":...}
	openConn     map[string]map[string]any // app token -> {"ok":true} or {"ok":false,"error":...}
	authTestHits int
	openConnHits int
}

func newFakeSlackServer(t *testing.T) *fakeSlackServer {
	t.Helper()
	f := &fakeSlackServer{authTest: map[string]map[string]any{}, openConn: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authTestHits++
		resp, ok := f.authTest[bearerToken(r)]
		f.mu.Unlock()
		if !ok {
			resp = map[string]any{"ok": false, "error": "invalid_auth"}
		}
		writeSlackFakeJSON(w, resp)
	})
	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.openConnHits++
		resp, ok := f.openConn[bearerToken(r)]
		f.mu.Unlock()
		if !ok {
			resp = map[string]any{"ok": true, "url": "wss://fake.slack.example/link/1"}
		}
		writeSlackFakeJSON(w, resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	orig := slackAPIBase
	slackAPIBase = f.srv.URL
	t.Cleanup(func() { slackAPIBase = orig })
	return f
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func writeSlackFakeJSON(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// stageAuthTestOK stages a successful auth.test response for token.
func (f *fakeSlackServer) stageAuthTestOK(token, teamID, teamName, botUserID, enterpriseID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := map[string]any{"ok": true, "team_id": teamID, "team": teamName, "user_id": botUserID}
	if enterpriseID != "" {
		resp["enterprise_id"] = enterpriseID
	}
	f.authTest[token] = resp
}

// stageOpenConnFail stages a failing apps.connections.open response for token.
func (f *fakeSlackServer) stageOpenConnFail(token, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openConn[token] = map[string]any{"ok": false, "error": reason}
}

// ---------- rig ----------

type slackWorkspaceRig struct {
	t      *testing.T
	h      *pgtest.Harness
	stores db.Stores
	hdl    *workspacesHandler
	fake   *fakeSlackServer
}

func newSlackWorkspaceRig(t *testing.T, publicURL string) *slackWorkspaceRig {
	t.Helper()
	// memberGate 404s unconditionally in local mode (before it even reads
	// the entitlement) — the rig always runs the multi-mode path so its
	// tests actually reach the gate's entitlement/admin logic under test.
	// The local-mode 404 itself is covered separately (no DB needed) in
	// TestHandlers_LocalMode404.
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	az := authz.New(h.AdminDB, stores.Tx)
	fake := newFakeSlackServer(t)

	hdl := &workspacesHandler{
		tx:        stores.Tx,
		az:        az,
		client:    slackHTTPClient,
		publicURL: func() string { return publicURL },
	}
	return &slackWorkspaceRig{t: t, h: h, stores: stores, hdl: hdl, fake: fake}
}

// req builds a request as callerID with the org + claims context set
// directly (no cookie-session dance) and, for path-valued routes, the
// {workspace_id} path value.
func (r *slackWorkspaceRig) req(method, path, callerID, orgID, workspaceID string, body any) *http.Request {
	r.t.Helper()
	var rdr io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if workspaceID != "" {
		req.SetPathValue("workspace_id", workspaceID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, orgID)
	return req.WithContext(ctx)
}

func (r *slackWorkspaceRig) secretValue(orgID, key string) string {
	r.t.Helper()
	var v string
	if err := r.stores.Tx.WithTx(r.t.Context(), orgID, orgID, func(tx db.TxStores) error {
		var e error
		v, e = tx.Secrets.Get(r.t.Context(), orgID, key)
		return e
	}); err != nil {
		r.t.Fatalf("read secret %s: %v", key, err)
	}
	return v
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---------- entitlement gate ----------

// TestHandleList_EntitlementGate: a Static provider without FeatureSlack
// refuses every route with 404; licensing it (the rig's default) allows it.
func TestHandleList_EntitlementGate(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-gate")

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/workspaces", owner, orgID, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("licensed org: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	entitlements.RegisterProvider(entitlements.Static())
	rec2 := httptest.NewRecorder()
	r.hdl.handleList(rec2, r.req(http.MethodGet, "/api/slack/workspaces", owner, orgID, "", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unlicensed org: status = %d, want 404", rec2.Code)
	}
}

// TestHandleConnect_NonAdmin_404: a plain member can't connect a workspace —
// 404 (non-disclosure), no row written.
func TestHandleConnect_NonAdmin_404(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-nonadmin")
	member := pgtest.SeedUser(t, r.h, "slack-nonadmin-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")
	r.fake.stageAuthTestOK("xoxb-nonadmin", "T0NONADMIN", "Acme", "U0BOT", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-nonadmin", "app_token": "xapp-1"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", member, orgID, "", body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (non-admin); body=%s", rec.Code, rec.Body.String())
	}
	if r.fake.authTestHits != 0 {
		t.Errorf("auth.test called %d times; want 0 (gated before any Slack call)", r.fake.authTestHits)
	}
}

// ---------- connect: transport inference + validation, end to end ----------

// TestHandleConnect_AppTokenOnly_Socket: the happy path for Socket Mode —
// app.token supplied, no signing secret, transport inferred as socket,
// apps.connections.open validated, secrets + row persisted.
func TestHandleConnect_AppTokenOnly_Socket(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-socket")
	r.fake.stageAuthTestOK("xoxb-socket", "T0SOCKET1", "Acme", "U0BOT1", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-socket", "app_token": "xapp-socket-1"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if view.WorkspaceID != "T0SOCKET1" || view.Transport != transportSocket || view.BotUserID != "U0BOT1" {
		t.Errorf("view = %+v; want workspace_id/transport/bot_user_id from auth.test", view)
	}
	if r.fake.openConnHits != 1 {
		t.Errorf("apps.connections.open called %d times; want 1 (socket transport validates the app token)", r.fake.openConnHits)
	}
	if got := r.secretValue(orgID, "slack_ws_T0SOCKET1_bot_token"); got != "xoxb-socket" {
		t.Errorf("stored bot token = %q; want xoxb-socket", got)
	}
	if got := r.secretValue(orgID, "slack_ws_T0SOCKET1_app_token"); got != "xapp-socket-1" {
		t.Errorf("stored app token = %q; want xapp-socket-1", got)
	}
}

// TestHandleConnect_SigningSecretOnly_EventsAPI: signing secret supplied, no
// app token -> transport inferred as events_api, apps.connections.open
// never called.
func TestHandleConnect_SigningSecretOnly_EventsAPI(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-events")
	r.fake.stageAuthTestOK("xoxb-events", "T0EVENTS1", "Acme", "U0BOT2", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-events", "signing_secret": "shh-secret"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if view.Transport != transportEventsAPI {
		t.Errorf("transport = %q; want events_api", view.Transport)
	}
	if r.fake.openConnHits != 0 {
		t.Errorf("apps.connections.open called %d times; want 0 (events_api never validates an app token)", r.fake.openConnHits)
	}
}

// TestHandleConnect_BothCredentials_NoExplicitTransport_400 and its sibling
// with neither credential both hit the same 400 path, proving the pure
// inferTransport rules (transport_test.go) are actually wired into the
// handler.
func TestHandleConnect_BothCredentials_NoExplicitTransport_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-ambiguous")
	r.fake.stageAuthTestOK("xoxb-ambiguous", "T0AMBIG01", "Acme", "U0BOT3", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-ambiguous", "signing_secret": "shh", "app_token": "xapp-1"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleConnect_NeitherCredential_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-neither")
	r.fake.stageAuthTestOK("xoxb-neither", "T0NEITHER", "Acme", "U0BOT4", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-neither"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleConnect_BothCredentials_ExplicitChoiceWins: an explicit
// transport picks between two valid credentials.
func TestHandleConnect_BothCredentials_ExplicitChoiceWins(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-explicit")
	r.fake.stageAuthTestOK("xoxb-explicit", "T0EXPLICIT", "Acme", "U0BOT5", "")

	rec := httptest.NewRecorder()
	body := map[string]string{
		"bot_token": "xoxb-explicit", "signing_secret": "shh", "app_token": "xapp-1", "transport": "events_api",
	}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if view.Transport != transportEventsAPI {
		t.Errorf("transport = %q; want events_api (the explicit choice)", view.Transport)
	}
}

// TestHandleConnect_AuthTestFailure_400: Slack rejecting the bot token
// surfaces as a 400, and nothing is persisted.
func TestHandleConnect_AuthTestFailure_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-badtoken")
	// No stageAuthTestOK call -> the fake answers {"ok":false,"error":"invalid_auth"}.

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-bad", "app_token": "xapp-1"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	var list []map[string]any
	rec2 := httptest.NewRecorder()
	r.hdl.handleList(rec2, r.req(http.MethodGet, "/api/slack/workspaces", owner, orgID, "", nil))
	_ = json.Unmarshal(rec2.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Errorf("workspaces after a failed auth.test = %v; want none persisted", list)
	}
}

// TestHandleConnect_AppTokenValidationFailure_400: apps.connections.open
// rejecting the app token also surfaces as a 400, and nothing is persisted.
func TestHandleConnect_AppTokenValidationFailure_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-badapp")
	r.fake.stageAuthTestOK("xoxb-badapp", "T0BADAPP1", "Acme", "U0BOT6", "")
	r.fake.stageOpenConnFail("xapp-badapp", "invalid_auth")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-badapp", "app_token": "xapp-badapp"}
	r.hdl.handleConnect(rec, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleConnect_CrossOrgConflict_409Generic: a second org connecting a
// workspace_id already bound to the first org gets a generic 409 that never
// names the owning org.
func TestHandleConnect_CrossOrgConflict_409Generic(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgA, ownerA, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-xorg-a")
	orgB, ownerB, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-xorg-b")
	// Two different bot tokens (plausible: two different Slack apps
	// installed to the same workspace) resolving to the SAME team_id.
	r.fake.stageAuthTestOK("xoxb-a", "T0SHARED9", "Acme", "U0BOTA", "")
	r.fake.stageAuthTestOK("xoxb-b", "T0SHARED9", "Acme", "U0BOTB", "")

	recA := httptest.NewRecorder()
	r.hdl.handleConnect(recA, r.req(http.MethodPost, "/api/slack/workspaces", ownerA, orgA, "",
		map[string]string{"bot_token": "xoxb-a", "app_token": "xapp-a"}))
	if recA.Code != http.StatusCreated {
		t.Fatalf("org A connect status = %d, want 201 (body=%s)", recA.Code, recA.Body.String())
	}

	recB := httptest.NewRecorder()
	r.hdl.handleConnect(recB, r.req(http.MethodPost, "/api/slack/workspaces", ownerB, orgB, "",
		map[string]string{"bot_token": "xoxb-b", "app_token": "xapp-b"}))
	if recB.Code != http.StatusConflict {
		t.Fatalf("org B connect status = %d, want 409 (body=%s)", recB.Code, recB.Body.String())
	}
	if strings.Contains(strings.ToLower(recB.Body.String()), "slack-xorg-a") {
		t.Errorf("409 body leaks the owning org: %s", recB.Body.String())
	}
}

// TestHandleConnect_LeaveBlankKeepsExisting: a re-submit that leaves the
// signing secret blank preserves the previously stored value rather than
// clearing it — the "leave blank to keep current" token-field convention.
func TestHandleConnect_LeaveBlankKeepsExisting(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-keepblank")
	r.fake.stageAuthTestOK("xoxb-keep", "T0KEEPBLANK", "Acme", "U0BOT7", "")

	first := httptest.NewRecorder()
	r.hdl.handleConnect(first, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
		map[string]string{"bot_token": "xoxb-keep", "signing_secret": "first-secret"}))
	if first.Code != http.StatusCreated {
		t.Fatalf("first connect status = %d (body=%s)", first.Code, first.Body.String())
	}

	// Re-submit: same bot token, signing_secret left blank.
	second := httptest.NewRecorder()
	r.hdl.handleConnect(second, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
		map[string]string{"bot_token": "xoxb-keep"}))
	if second.Code != http.StatusOK {
		t.Fatalf("re-submit status = %d, want 200 (idempotent update; body=%s)", second.Code, second.Body.String())
	}
	view := decodeBody[workspaceView](t, second)
	if view.Transport != transportEventsAPI {
		t.Errorf("transport after blank re-submit = %q; want events_api (kept)", view.Transport)
	}
	if got := r.secretValue(orgID, "slack_ws_T0KEEPBLANK_signing_secret"); got != "first-secret" {
		t.Errorf("signing secret after blank re-submit = %q; want first-secret (kept, unchanged)", got)
	}
}

// ---------- delete ----------

// TestHandleDelete_RemovesRowAndSecretKeyset: disconnecting a workspace
// deletes the row and sweeps every secret in its keyset, present or not.
func TestHandleDelete_RemovesRowAndSecretKeyset(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delete")
	r.fake.stageAuthTestOK("xoxb-delete", "T0DELETE1", "Acme", "U0BOT8", "")

	connect := httptest.NewRecorder()
	r.hdl.handleConnect(connect, r.req(http.MethodPost, "/api/slack/workspaces", owner, orgID, "",
		map[string]string{"bot_token": "xoxb-delete", "app_token": "xapp-delete"}))
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect status = %d (body=%s)", connect.Code, connect.Body.String())
	}

	del := httptest.NewRecorder()
	r.hdl.handleDelete(del, r.req(http.MethodDelete, "/api/slack/workspaces/T0DELETE1", owner, orgID, "T0DELETE1", nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body=%s)", del.Code, del.Body.String())
	}

	for _, key := range []string{"slack_ws_T0DELETE1_bot_token", "slack_ws_T0DELETE1_app_token", "slack_ws_T0DELETE1_signing_secret"} {
		if got := r.secretValue(orgID, key); got != "" {
			t.Errorf("secret %s = %q after delete; want swept (\"\")", key, got)
		}
	}

	list := httptest.NewRecorder()
	r.hdl.handleList(list, r.req(http.MethodGet, "/api/slack/workspaces", owner, orgID, "", nil))
	var got []workspaceView
	_ = json.Unmarshal(list.Body.Bytes(), &got)
	if len(got) != 0 {
		t.Errorf("workspaces after delete = %v; want none", got)
	}
}

// TestHandleDelete_Missing_404: deleting a workspace_id the org never
// connected is a 404.
func TestHandleDelete_Missing_404(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delete-missing")

	rec := httptest.NewRecorder()
	r.hdl.handleDelete(rec, r.req(http.MethodDelete, "/api/slack/workspaces/T0NOTHERE", owner, orgID, "T0NOTHERE", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ---------- manifest ----------

// TestHandleManifest_EventsAPI_NoPublicURL_409: the manifest endpoint
// refuses to advertise a request_url built from an empty deployment base.
func TestHandleManifest_EventsAPI_NoPublicURL_409(t *testing.T) {
	r := newSlackWorkspaceRig(t, "")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-manifest-nourl")

	rec := httptest.NewRecorder()
	r.hdl.handleManifest(rec, r.req(http.MethodGet, "/api/slack/manifest?transport=events_api", owner, orgID, "", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleManifest_Socket_OK: socket manifests never need a public URL.
func TestHandleManifest_Socket_OK(t *testing.T) {
	r := newSlackWorkspaceRig(t, "")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-manifest-socket")

	rec := httptest.NewRecorder()
	r.hdl.handleManifest(rec, r.req(http.MethodGet, "/api/slack/manifest?transport=socket", owner, orgID, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var m slackManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Settings.SocketModeEnabled == nil || !*m.Settings.SocketModeEnabled {
		t.Errorf("socket_mode_enabled = %v, want true", m.Settings.SocketModeEnabled)
	}
}

// TestHandleManifest_EventsAPI_OK: with a public URL configured, the
// request_url carries the org id in its path.
func TestHandleManifest_EventsAPI_OK(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-manifest-events")

	rec := httptest.NewRecorder()
	r.hdl.handleManifest(rec, r.req(http.MethodGet, "/api/slack/manifest?transport=events_api", owner, orgID, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var m slackManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	want := "https://tf.example/api/webhooks/slack/" + orgID
	if m.Settings.EventSubscriptions.RequestURL != want {
		t.Errorf("request_url = %q, want %q", m.Settings.EventSubscriptions.RequestURL, want)
	}
}

// TestHandleManifest_BadTransport_400: an unrecognized transport query value is a 400.
func TestHandleManifest_BadTransport_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-manifest-bad")

	rec := httptest.NewRecorder()
	r.hdl.handleManifest(rec, r.req(http.MethodGet, "/api/slack/manifest?transport=carrier-pigeon", owner, orgID, "", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
