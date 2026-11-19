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
	"net/url"
	"strings"
	"sync"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/ttlcache"
)

// ---------- fake Slack API (auth.test, bots.info, apps.connections.open, OAuth, and the channel calls) ----------

type fakeSlackServer struct {
	srv *httptest.Server

	mu           sync.Mutex
	authTest     map[string]map[string]any // bot token -> {"ok":true, "team_id":...} or {"ok":false,"error":...}
	botsInfo     map[string]map[string]any // bot id -> {"ok":true,"bot":{"app_id":...}} or {"ok":false,"error":...}
	openConn     map[string]map[string]any // app token -> {"ok":true} or {"ok":false,"error":...}
	authTestHits int
	botsInfoHits int
	openConnHits int

	// oauthAccess answers oauth.v2.access by the code it is sent; an unstaged
	// code answers invalid_code. oauthRequests records each exchange.
	oauthAccess   map[string]map[string]any
	oauthRequests []oauthAccessRequest
	revokeHits    int

	// scopes overrides, by bot token, the X-OAuth-Scopes auth.test answers
	// with, which is otherwise every scope in slackBotScopes. authStatus
	// answers auth.test for a bot token with that HTTP status and no body.
	scopes     map[string]string
	authStatus map[string]int

	// public and memberOf answer conversations.list and users.conversations
	// by bot token; truncate makes conversations.list claim another page for
	// a token. listHits counts conversations.list calls. joins records each
	// conversations.join as "<bot token> <channel>", and joinFail fails one
	// for a channel with its error code.
	public   map[string][]map[string]any
	memberOf map[string][]map[string]any
	truncate map[string]bool
	listHits int
	joins    []string
	joinFail map[string]string
}

// oauthAccessRequest is one oauth.v2.access call as the fake received it.
type oauthAccessRequest struct {
	ClientID, ClientSecret string
	BasicAuth              bool
	Form                   url.Values
}

func newFakeSlackServer(t *testing.T) *fakeSlackServer {
	t.Helper()
	f := &fakeSlackServer{
		authTest: map[string]map[string]any{},
		botsInfo: map[string]map[string]any{},
		openConn: map[string]map[string]any{},

		oauthAccess: map[string]map[string]any{},

		scopes:     map[string]string{},
		authStatus: map[string]int{},
		public:     map[string][]map[string]any{},
		memberOf:   map[string][]map[string]any{},
		truncate:   map[string]bool{},
		joinFail:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		f.mu.Lock()
		f.authTestHits++
		resp, ok := f.authTest[token]
		scopes, scoped := f.scopes[token]
		status := f.authStatus[token]
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if !ok {
			resp = map[string]any{"ok": false, "error": "invalid_auth"}
		}
		if !scoped {
			scopes = strings.Join(slackBotScopes, ",")
		}
		w.Header().Set("X-OAuth-Scopes", scopes)
		writeSlackFakeJSON(w, resp)
	})
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		f.mu.Lock()
		f.listHits++
		chans := f.public[token]
		next := ""
		if f.truncate[token] {
			next = "more"
		}
		f.mu.Unlock()
		writeSlackFakeJSON(w, map[string]any{"ok": true, "channels": chans, "response_metadata": map[string]any{"next_cursor": next}})
	})
	mux.HandleFunc("/users.conversations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		chans := f.memberOf[bearerToken(r)]
		f.mu.Unlock()
		writeSlackFakeJSON(w, map[string]any{"ok": true, "channels": chans, "response_metadata": map[string]any{"next_cursor": ""}})
	})
	mux.HandleFunc("/conversations.join", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		channel := r.FormValue("channel")
		f.mu.Lock()
		f.joins = append(f.joins, bearerToken(r)+" "+channel)
		code := f.joinFail[channel]
		f.mu.Unlock()
		if code != "" {
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": code})
			return
		}
		writeSlackFakeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/bots.info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.botsInfoHits++
		resp, ok := f.botsInfo[r.URL.Query().Get("bot")]
		f.mu.Unlock()
		if !ok {
			resp = map[string]any{"ok": false, "error": "bot_not_found"}
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
	mux.HandleFunc("/oauth.v2.access", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, secret, basic := r.BasicAuth()
		f.mu.Lock()
		f.oauthRequests = append(f.oauthRequests, oauthAccessRequest{ClientID: id, ClientSecret: secret, BasicAuth: basic, Form: r.PostForm})
		resp, ok := f.oauthAccess[r.PostForm.Get("code")]
		f.mu.Unlock()
		if !ok {
			resp = map[string]any{"ok": false, "error": "invalid_code"}
		}
		writeSlackFakeJSON(w, resp)
	})
	mux.HandleFunc("/auth.revoke", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revokeHits++
		f.mu.Unlock()
		writeSlackFakeJSON(w, map[string]any{"ok": true, "revoked": true})
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

// botIDForToken derives a deterministic bot_id for token — the fake never
// needs a real one, only one that's stable across the auth.test and
// bots.info stages within a test.
func botIDForToken(token string) string { return "B0" + token }

// stageAuthTestOK stages a successful auth.test response for token,
// including a deterministic bot_id (see botIDForToken) — the first leg of
// the connect handler's app-id derivation chain. Does NOT stage bots.info;
// callers that need the full chain to succeed should use stageConnectOK, or
// stage bots.info themselves via stageBotsInfoOK/stageBotsInfoFail for a
// test that specifically exercises that second leg.
func (f *fakeSlackServer) stageAuthTestOK(token, teamID, teamName, botUserID, enterpriseID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := map[string]any{"ok": true, "team_id": teamID, "team": teamName, "user_id": botUserID, "bot_id": botIDForToken(token)}
	if enterpriseID != "" {
		resp["enterprise_id"] = enterpriseID
	}
	f.authTest[token] = resp
}

// stageBotsInfoOK stages a successful bots.info response for botID, resolving
// to appID.
func (f *fakeSlackServer) stageBotsInfoOK(botID, appID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.botsInfo[botID] = map[string]any{"ok": true, "bot": map[string]any{"id": botID, "app_id": appID}}
}

// stageBotName names the bot behind token in its bots.info response, which
// must already be staged (stageConnectOK).
func (f *fakeSlackServer) stageBotName(token, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.botsInfo[botIDForToken(token)]["bot"].(map[string]any)["name"] = name
}

// stageBotsInfoFail stages a failing bots.info response for botID.
func (f *fakeSlackServer) stageBotsInfoFail(botID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.botsInfo[botID] = map[string]any{"ok": false, "error": reason}
}

// stageConnectOK stages the full app-id derivation chain (auth.test then
// bots.info) for a bot token that should connect successfully, resolving to
// appID.
func (f *fakeSlackServer) stageConnectOK(token, teamID, teamName, botUserID, appID, enterpriseID string) {
	f.stageAuthTestOK(token, teamID, teamName, botUserID, enterpriseID)
	f.stageBotsInfoOK(botIDForToken(token), appID)
}

// stageChannels sets what conversations.list (public) and users.conversations
// (memberOf) answer for botToken.
func (f *fakeSlackServer) stageChannels(botToken string, public, memberOf []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.public[botToken] = public
	f.memberOf[botToken] = memberOf
}

// stageScopes sets the X-OAuth-Scopes auth.test answers for botToken.
func (f *fakeSlackServer) stageScopes(botToken, scopes string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes[botToken] = scopes
}

// stageAuthTestRejected makes auth.test refuse botToken (invalid_auth).
func (f *fakeSlackServer) stageAuthTestRejected(botToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.authTest, botToken)
	delete(f.scopes, botToken)
}

// stageAuthTestStatus makes auth.test answer botToken with status and no body.
func (f *fakeSlackServer) stageAuthTestStatus(botToken string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authStatus[botToken] = status
}

// stageTruncated makes conversations.list claim another page for botToken.
func (f *fakeSlackServer) stageTruncated(botToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncate[botToken] = true
}

// stageJoinFail fails conversations.join for channel with code.
func (f *fakeSlackServer) stageJoinFail(channel, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinFail[channel] = code
}

// openConnCalls is how many apps.connections.open calls the fake has answered.
func (f *fakeSlackServer) openConnCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openConnHits
}

// listCalls is how many conversations.list calls the fake has answered.
func (f *fakeSlackServer) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listHits
}

// joined returns each conversations.join call, as "<bot token> <channel>".
func (f *fakeSlackServer) joined() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.joins...)
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
		stores:    stores,
		channels: &channelsHandler{
			tx: stores.Tx, stores: stores, az: az, client: slackHTTPClient,
			candidates: ttlcache.New[slackCandidateSet](slackCandidatesTTL),
		},
	}
	return &slackWorkspaceRig{t: t, h: h, stores: stores, hdl: hdl, fake: fake}
}

// req builds a request as callerID to a route under orgID's path, with orgID
// also the credential's active org, and, for path-valued routes, the
// {workspace_id} path value. The claims and org context are set directly (no
// cookie-session dance).
func (r *slackWorkspaceRig) req(method, path, callerID, orgID, workspaceID string, body any) *http.Request {
	r.t.Helper()
	return r.reqActive(method, path, callerID, orgID, orgID, workspaceID, body)
}

// reqActive is req with the credential's active org named apart from the org
// the path addresses.
func (r *slackWorkspaceRig) reqActive(method, path, callerID, activeOrgID, pathOrgID, workspaceID string, body any) *http.Request {
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
	req.SetPathValue("org_id", pathOrgID)
	if workspaceID != "" {
		req.SetPathValue("workspace_id", workspaceID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, activeOrgID)
	return req.WithContext(ctx)
}

// connectionsPath is the path of orgID's connections collection.
func connectionsPath(orgID string) string {
	return "/api/orgs/" + orgID + "/slack/connections"
}

// importReq is a POST …/connections/import request as callerID with body.
func (r *slackWorkspaceRig) importReq(callerID, orgID string, body any) *http.Request {
	r.t.Helper()
	return r.req(http.MethodPost, connectionsPath(orgID)+"/import", callerID, orgID, "", body)
}

// listReq is a POST …/connections/list request as callerID with body.
func (r *slackWorkspaceRig) listReq(callerID, orgID string, body any) *http.Request {
	r.t.Helper()
	return r.req(http.MethodPost, connectionsPath(orgID)+"/list", callerID, orgID, "", body)
}

// manifestReq is a GET …/slack/manifest request as callerID for transport.
func (r *slackWorkspaceRig) manifestReq(callerID, orgID, transport string) *http.Request {
	r.t.Helper()
	return r.req(http.MethodGet, "/api/orgs/"+orgID+"/slack/manifest?transport="+transport, callerID, orgID, "", nil)
}

// connectionReq is a request as callerID to one connection's route.
func (r *slackWorkspaceRig) connectionReq(method, callerID, orgID, workspaceID, apiAppID string) *http.Request {
	r.t.Helper()
	req := r.req(method, connectionsPath(orgID)+"/"+workspaceID+"/"+apiAppID, callerID, orgID, workspaceID, nil)
	req.SetPathValue("api_app_id", apiAppID)
	return req
}

// reqDelete is a DELETE …/connections/{workspace_id}/{api_app_id} request.
func (r *slackWorkspaceRig) reqDelete(callerID, orgID, workspaceID, apiAppID string) *http.Request {
	r.t.Helper()
	return r.connectionReq(http.MethodDelete, callerID, orgID, workspaceID, apiAppID)
}

// connectionList is the list route's envelope.
type connectionList struct {
	Items         []workspaceView `json:"items"`
	NextPageToken string          `json:"next_page_token"`
	TotalCount    *int            `json:"total_count"`
}

// listConnections reads orgID's first page of connections as callerID,
// failing the test on anything but a 200.
func (r *slackWorkspaceRig) listConnections(callerID, orgID string) []workspaceView {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.listReq(callerID, orgID, map[string]any{}))
	if rec.Code != http.StatusOK {
		r.t.Fatalf("list connections: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	return decodeBody[connectionList](r.t, rec).Items
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

// derefTransport is v's transport, "" when it has none.
func derefTransport(v workspaceView) string {
	if v.Transport == nil {
		return ""
	}
	return *v.Transport
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
	r.hdl.handleList(rec, r.listReq(owner, orgID, map[string]any{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("licensed org: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	entitlements.RegisterProvider(entitlements.Static())
	rec2 := httptest.NewRecorder()
	r.hdl.handleList(rec2, r.listReq(owner, orgID, map[string]any{}))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unlicensed org: status = %d, want 404", rec2.Code)
	}
}

// TestHandleConnect_NonAdminIsForbidden: a plain member can't connect a workspace —
// 403 (the org is visible to them), no row written.
func TestHandleConnect_NonAdminIsForbidden(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-nonadmin")
	member := pgtest.SeedUser(t, r.h, "slack-nonadmin-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")
	r.fake.stageAuthTestOK("xoxb-nonadmin", "T0NONADMIN", "Acme", "U0BOT", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-nonadmin", "app_token": "xapp-1"}
	r.hdl.handleConnect(rec, r.importReq(member, orgID, body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-admin); body=%s", rec.Code, rec.Body.String())
	}
	if r.fake.authTestHits != 0 {
		t.Errorf("auth.test called %d times; want 0 (gated before any Slack call)", r.fake.authTestHits)
	}
}

// ---------- connect: transport inference + validation, end to end ----------

// TestHandleConnect_AppTokenOnly_Socket: the happy path for Socket Mode —
// app.token supplied, no signing secret, transport inferred as socket,
// apps.connections.open validated, secrets + row persisted. Also pins the
// derived app id landing on the row.
func TestHandleConnect_AppTokenOnly_Socket(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-socket")
	r.fake.stageConnectOK("xoxb-socket", "T0SOCKET1", "Acme", "U0BOT1", "A0SOCKET01", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-socket", "app_token": "xapp-socket-1"}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if view.WorkspaceID != "T0SOCKET1" || derefTransport(view) != transportSocket || view.BotUserID != "U0BOT1" {
		t.Errorf("view = %+v; want workspace_id/transport/bot_user_id from auth.test", view)
	}
	if view.APIAppID != "A0SOCKET01" {
		t.Errorf("api_app_id = %q; want A0SOCKET01 (derived via bots.info)", view.APIAppID)
	}
	if r.fake.botsInfoHits != 1 {
		t.Errorf("bots.info called %d times; want 1 (app id derivation)", r.fake.botsInfoHits)
	}
	if r.fake.openConnHits != 1 {
		t.Errorf("apps.connections.open called %d times; want 1 (socket transport validates the app token)", r.fake.openConnHits)
	}
	if got := r.secretValue(orgID, "slack_ws_T0SOCKET1_A0SOCKET01_bot_token"); got != "xoxb-socket" {
		t.Errorf("stored bot token = %q; want xoxb-socket", got)
	}
	if got := r.secretValue(orgID, "slack_ws_T0SOCKET1_A0SOCKET01_app_token"); got != "xapp-socket-1" {
		t.Errorf("stored app token = %q; want xapp-socket-1", got)
	}
}

// TestHandleConnect_SigningSecretOnly_EventsAPI: signing secret supplied, no
// app token -> transport inferred as events_api, apps.connections.open
// never called.
func TestHandleConnect_SigningSecretOnly_EventsAPI(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-events")
	r.fake.stageConnectOK("xoxb-events", "T0EVENTS1", "Acme", "U0BOT2", "A0EVENTS01", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-events", "signing_secret": "shh-secret"}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if derefTransport(view) != transportEventsAPI {
		t.Errorf("transport = %q; want events_api", derefTransport(view))
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
	r.fake.stageConnectOK("xoxb-ambiguous", "T0AMBIG01", "Acme", "U0BOT3", "A0AMBIG001", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-ambiguous", "signing_secret": "shh", "app_token": "xapp-1"}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleConnect_NeitherCredential_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-neither")
	r.fake.stageConnectOK("xoxb-neither", "T0NEITHER", "Acme", "U0BOT4", "A0NEITHER1", "")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-neither"}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleConnect_BothCredentials_ExplicitChoiceWins: an explicit
// transport picks between two valid credentials.
func TestHandleConnect_BothCredentials_ExplicitChoiceWins(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-explicit")
	r.fake.stageConnectOK("xoxb-explicit", "T0EXPLICIT", "Acme", "U0BOT5", "A0EXPLICIT", "")

	rec := httptest.NewRecorder()
	body := map[string]string{
		"bot_token": "xoxb-explicit", "signing_secret": "shh", "app_token": "xapp-1", "transport": "events_api",
	}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if derefTransport(view) != transportEventsAPI {
		t.Errorf("transport = %q; want events_api (the explicit choice)", derefTransport(view))
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
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	if list := r.listConnections(owner, orgID); len(list) != 0 {
		t.Errorf("workspaces after a failed auth.test = %v; want none persisted", list)
	}
}

// TestHandleConnect_AppTokenValidationFailure_400: apps.connections.open
// rejecting the app token also surfaces as a 400, and nothing is persisted.
func TestHandleConnect_AppTokenValidationFailure_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-badapp")
	r.fake.stageConnectOK("xoxb-badapp", "T0BADAPP1", "Acme", "U0BOT6", "A0BADAPP01", "")
	r.fake.stageOpenConnFail("xapp-badapp", "invalid_auth")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-badapp", "app_token": "xapp-badapp"}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleConnect_BotsInfoFailure_400: Slack rejecting/failing bots.info
// (the second leg of app-id derivation, after a SUCCESSFUL auth.test)
// surfaces as a 400, and nothing is persisted — the app id is key material,
// so there's no fallback.
func TestHandleConnect_BotsInfoFailure_400(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-badbotsinfo")
	r.fake.stageAuthTestOK("xoxb-badbotsinfo", "T0BADBOTS1", "Acme", "U0BOT9", "")
	r.fake.stageBotsInfoFail(botIDForToken("xoxb-badbotsinfo"), "bot_not_found")

	rec := httptest.NewRecorder()
	body := map[string]string{"bot_token": "xoxb-badbotsinfo", "app_token": "xapp-1"}
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	if got := r.listConnections(owner, orgID); len(got) != 0 {
		t.Errorf("workspaces after a failed bots.info = %v; want none persisted", got)
	}
}

// TestHandleConnect_DeploymentAppToken_422: a bot token whose app is the
// deployment's own Slack app is refused before any write, so a pasted-token
// connection never holds the deployment app. A token from any other app
// connects as before with a deployment app configured.
func TestHandleConnect_DeploymentAppToken_422(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	r.hdl.deployment = deploymentApp{
		AppID:         "A0DEPLOYAPP",
		ClientID:      "1234.5678",
		ClientSecret:  "client-secret",
		SigningSecret: "signing-secret",
		Transport:     transportEventsAPI,
	}
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-deployapp")
	r.fake.stageConnectOK("xoxb-deploy", "T0DEPLOYWS", "Acme", "U0BOTDEPLOY", "A0DEPLOYAPP", "")
	r.fake.stageConnectOK("xoxb-own", "T0DEPLOYWS", "Acme", "U0BOTOWN", "A0OWNAPP01", "")

	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, map[string]string{"bot_token": "xoxb-deploy", "signing_secret": "shh"}))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	errs := decodeBody[struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}](t, rec).Errors
	want := httpx.ErrorItem{
		Reason:  httpx.ReasonInvalidField,
		Message: "This bot token belongs to this deployment's Slack app. Connect it with Add to Slack instead.",
		Field:   "bot_token",
	}
	if len(errs) != 1 || errs[0] != want {
		t.Fatalf("errors = %+v; want [%+v]", errs, want)
	}

	if got := r.listConnections(owner, orgID); len(got) != 0 {
		t.Errorf("workspaces after the refusal = %+v; want none", got)
	}
	for _, key := range []string{
		"slack_ws_T0DEPLOYWS_A0DEPLOYAPP_bot_token",
		"slack_ws_T0DEPLOYWS_A0DEPLOYAPP_signing_secret",
	} {
		if got := r.secretValue(orgID, key); got != "" {
			t.Errorf("secret %s = %q after the refusal; want none stored", key, got)
		}
	}
	if rows := r.auditRows(orgID); len(rows) != 0 {
		t.Errorf("audit rows after the refusal = %+v; want none", rows)
	}

	own := httptest.NewRecorder()
	r.hdl.handleConnect(own, r.importReq(owner, orgID, map[string]string{"bot_token": "xoxb-own", "signing_secret": "shh"}))
	if own.Code != http.StatusCreated {
		t.Fatalf("own app connect status = %d, want 201 (body=%s)", own.Code, own.Body.String())
	}
	if view := decodeBody[workspaceView](t, own); view.APIAppID != "A0OWNAPP01" {
		t.Errorf("api_app_id = %q; want A0OWNAPP01", view.APIAppID)
	}
}

// ---------- app-single-org invariant + two-apps-one-workspace ----------

// TestHandleConnect_AppBoundToOtherOrg_SameWorkspace_409Generic: org B
// connecting the exact same (workspace, app) pair org A already holds gets
// a generic 409 that never names the owning org.
func TestHandleConnect_AppBoundToOtherOrg_SameWorkspace_409Generic(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgA, ownerA, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-appinv-a")
	orgB, ownerB, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-appinv-b")
	// Two different bot tokens (plausible: org B pasted org A's app's bot
	// token, or a shared/leaked credential) resolving to the SAME team_id
	// AND the same api_app_id.
	r.fake.stageConnectOK("xoxb-a", "T0SHARED9", "Acme", "U0BOTA", "A0SHARED09", "")
	r.fake.stageConnectOK("xoxb-b", "T0SHARED9", "Acme", "U0BOTB", "A0SHARED09", "")

	recA := httptest.NewRecorder()
	r.hdl.handleConnect(recA, r.importReq(ownerA, orgA, map[string]string{"bot_token": "xoxb-a", "app_token": "xapp-a"}))
	if recA.Code != http.StatusCreated {
		t.Fatalf("org A connect status = %d, want 201 (body=%s)", recA.Code, recA.Body.String())
	}

	recB := httptest.NewRecorder()
	r.hdl.handleConnect(recB, r.importReq(ownerB, orgB, map[string]string{"bot_token": "xoxb-b", "app_token": "xapp-b"}))
	if recB.Code != http.StatusConflict {
		t.Fatalf("org B connect status = %d, want 409 (body=%s)", recB.Code, recB.Body.String())
	}
	if strings.Contains(strings.ToLower(recB.Body.String()), "slack-appinv-a") {
		t.Errorf("409 body leaks the owning org: %s", recB.Body.String())
	}
}

// TestHandleConnect_AppBoundToOtherOrg_DifferentWorkspace_409Generic proves
// the app-single-org invariant spans every workspace an app is installed
// in, not just the one it was first connected on — a DIFFERENT workspace_id
// carrying the same api_app_id under a different org still 409s, even
// though the composite (workspace_id, api_app_id) PK alone wouldn't catch
// it (the pair is entirely distinct from org A's row).
func TestHandleConnect_AppBoundToOtherOrg_DifferentWorkspace_409Generic(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgA, ownerA, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-appinv2-a")
	orgB, ownerB, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-appinv2-b")
	r.fake.stageConnectOK("xoxb-c", "T0WORKSPCA", "Acme", "U0BOTC", "A0CROSSWS1", "")
	r.fake.stageConnectOK("xoxb-d", "T0WORKSPCB", "Beta", "U0BOTD", "A0CROSSWS1", "")

	recA := httptest.NewRecorder()
	r.hdl.handleConnect(recA, r.importReq(ownerA, orgA, map[string]string{"bot_token": "xoxb-c", "app_token": "xapp-c"}))
	if recA.Code != http.StatusCreated {
		t.Fatalf("org A connect status = %d, want 201 (body=%s)", recA.Code, recA.Body.String())
	}

	recB := httptest.NewRecorder()
	r.hdl.handleConnect(recB, r.importReq(ownerB, orgB, map[string]string{"bot_token": "xoxb-d", "app_token": "xapp-d"}))
	if recB.Code != http.StatusConflict {
		t.Fatalf("org B connect status = %d, want 409 (different workspace, same app id; body=%s)", recB.Code, recB.Body.String())
	}
	if strings.Contains(strings.ToLower(recB.Body.String()), "slack-appinv2-a") {
		t.Errorf("409 body leaks the owning org: %s", recB.Body.String())
	}
}

// TestHandleConnect_TwoAppsOneWorkspace_HappyPath is the feature's
// acceptance test: two different TF orgs, each with their own Slack app,
// both installed to the SAME workspace — a prod org and an eval org running
// their own bot in one workspace, the motivating scenario for the (workspace,
// app) composite bind. Both connects succeed, both rows are live under the
// same workspace_id with distinct api_app_id, and each org sees only its own
// row.
func TestHandleConnect_TwoAppsOneWorkspace_HappyPath(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgProd, ownerProd, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-2apps-prod")
	orgEval, ownerEval, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-2apps-eval")
	r.fake.stageConnectOK("xoxb-prod", "T0SHAREDWS", "Acme", "U0BOTPROD", "A0PRODAPP1", "")
	r.fake.stageConnectOK("xoxb-eval", "T0SHAREDWS", "Acme", "U0BOTEVAL", "A0EVALAPP1", "")

	recProd := httptest.NewRecorder()
	r.hdl.handleConnect(recProd, r.importReq(ownerProd, orgProd, map[string]string{"bot_token": "xoxb-prod", "app_token": "xapp-prod"}))
	if recProd.Code != http.StatusCreated {
		t.Fatalf("prod org connect status = %d, want 201 (body=%s)", recProd.Code, recProd.Body.String())
	}

	recEval := httptest.NewRecorder()
	r.hdl.handleConnect(recEval, r.importReq(ownerEval, orgEval, map[string]string{"bot_token": "xoxb-eval", "app_token": "xapp-eval"}))
	if recEval.Code != http.StatusCreated {
		t.Fatalf("eval org connect status = %d, want 201 — a different app in the same workspace must NOT conflict (body=%s)",
			recEval.Code, recEval.Body.String())
	}

	prodView := decodeBody[workspaceView](t, recProd)
	evalView := decodeBody[workspaceView](t, recEval)
	if prodView.WorkspaceID != evalView.WorkspaceID {
		t.Fatalf("workspace ids differ: prod=%q eval=%q; want both T0SHAREDWS", prodView.WorkspaceID, evalView.WorkspaceID)
	}
	if prodView.APIAppID == evalView.APIAppID {
		t.Fatalf("both rows carry api_app_id %q; want distinct app ids", prodView.APIAppID)
	}

	gotProd := r.listConnections(ownerProd, orgProd)
	if len(gotProd) != 1 || gotProd[0].APIAppID != "A0PRODAPP1" {
		t.Errorf("prod org list = %+v; want exactly its own app row", gotProd)
	}

	gotEval := r.listConnections(ownerEval, orgEval)
	if len(gotEval) != 1 || gotEval[0].APIAppID != "A0EVALAPP1" {
		t.Errorf("eval org list = %+v; want exactly its own app row", gotEval)
	}
}

// TestHandleConnect_LeaveBlankKeepsExisting: a re-submit that leaves the
// signing secret blank preserves the previously stored value rather than
// clearing it — the "leave blank to keep current" token-field convention.
func TestHandleConnect_LeaveBlankKeepsExisting(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-keepblank")
	r.fake.stageConnectOK("xoxb-keep", "T0KEEPBLANK", "Acme", "U0BOT7", "A0KEEPBLNK", "")

	first := httptest.NewRecorder()
	r.hdl.handleConnect(first, r.importReq(owner, orgID, map[string]string{"bot_token": "xoxb-keep", "signing_secret": "first-secret"}))
	if first.Code != http.StatusCreated {
		t.Fatalf("first connect status = %d (body=%s)", first.Code, first.Body.String())
	}

	// Re-submit: same bot token, signing_secret left blank.
	second := httptest.NewRecorder()
	r.hdl.handleConnect(second, r.importReq(owner, orgID, map[string]string{"bot_token": "xoxb-keep"}))
	if second.Code != http.StatusOK {
		t.Fatalf("re-submit status = %d, want 200 (idempotent update; body=%s)", second.Code, second.Body.String())
	}
	view := decodeBody[workspaceView](t, second)
	if derefTransport(view) != transportEventsAPI {
		t.Errorf("transport after blank re-submit = %q; want events_api (kept)", derefTransport(view))
	}
	if got := r.secretValue(orgID, "slack_ws_T0KEEPBLANK_A0KEEPBLNK_signing_secret"); got != "first-secret" {
		t.Errorf("signing secret after blank re-submit = %q; want first-secret (kept, unchanged)", got)
	}
}

// ---------- delete ----------

// TestHandleDelete_RemovesRowAndSecretKeyset: disconnecting a (workspace,
// app) pair deletes the row and sweeps every secret in its keyset, present
// or not.
func TestHandleDelete_RemovesRowAndSecretKeyset(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delete")
	r.fake.stageConnectOK("xoxb-delete", "T0DELETE1", "Acme", "U0BOT8", "A0DELETE01", "")

	connect := httptest.NewRecorder()
	r.hdl.handleConnect(connect, r.importReq(owner, orgID, map[string]string{"bot_token": "xoxb-delete", "app_token": "xapp-delete"}))
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect status = %d (body=%s)", connect.Code, connect.Body.String())
	}

	del := httptest.NewRecorder()
	r.hdl.handleDelete(del, r.reqDelete(owner, orgID, "T0DELETE1", "A0DELETE01"))
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body=%s)", del.Code, del.Body.String())
	}

	for _, key := range []string{
		"slack_ws_T0DELETE1_A0DELETE01_bot_token",
		"slack_ws_T0DELETE1_A0DELETE01_app_token",
		"slack_ws_T0DELETE1_A0DELETE01_signing_secret",
	} {
		if got := r.secretValue(orgID, key); got != "" {
			t.Errorf("secret %s = %q after delete; want swept (\"\")", key, got)
		}
	}

	if got := r.listConnections(owner, orgID); len(got) != 0 {
		t.Errorf("workspaces after delete = %v; want none", got)
	}
}

// TestHandleDelete_Missing_404: deleting a (workspace, app) pair the org
// never connected is a 404.
func TestHandleDelete_Missing_404(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delete-missing")

	rec := httptest.NewRecorder()
	r.hdl.handleDelete(rec, r.reqDelete(owner, orgID, "T0NOTHERE", "A0NOTHERE1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandleDelete_OtherAppInSameWorkspace_Untouched: disconnecting one app
// in a shared workspace must not touch a different app's row (or secrets)
// in that same workspace — the composite key is the isolation boundary.
func TestHandleDelete_OtherAppInSameWorkspace_Untouched(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgProd, ownerProd, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delisolate-prod")
	orgEval, ownerEval, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delisolate-eval")
	r.fake.stageConnectOK("xoxb-iso-prod", "T0ISOSHARE", "Acme", "U0BOTP", "A0ISOPROD1", "")
	r.fake.stageConnectOK("xoxb-iso-eval", "T0ISOSHARE", "Acme", "U0BOTE", "A0ISOEVAL1", "")

	for _, c := range []struct{ ownerID, orgID, token, appToken string }{
		{ownerProd, orgProd, "xoxb-iso-prod", "xapp-iso-prod"},
		{ownerEval, orgEval, "xoxb-iso-eval", "xapp-iso-eval"},
	} {
		rec := httptest.NewRecorder()
		r.hdl.handleConnect(rec, r.importReq(c.ownerID, c.orgID, map[string]string{"bot_token": c.token, "app_token": c.appToken}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("connect(%s) status = %d, want 201 (body=%s)", c.orgID, rec.Code, rec.Body.String())
		}
	}

	del := httptest.NewRecorder()
	r.hdl.handleDelete(del, r.reqDelete(ownerProd, orgProd, "T0ISOSHARE", "A0ISOPROD1"))
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body=%s)", del.Code, del.Body.String())
	}

	gotEval := r.listConnections(ownerEval, orgEval)
	if len(gotEval) != 1 || gotEval[0].APIAppID != "A0ISOEVAL1" {
		t.Errorf("eval org's row after deleting prod's app = %+v; want untouched", gotEval)
	}
	if got := r.secretValue(orgEval, "slack_ws_T0ISOSHARE_A0ISOEVAL1_bot_token"); got != "xoxb-iso-eval" {
		t.Errorf("eval org's bot token after deleting prod's app = %q; want untouched", got)
	}
}

// ---------- manifest ----------

// TestHandleManifest_EventsAPI_NoPublicURL_409: the manifest endpoint
// refuses to advertise a request_url built from an empty deployment base.
func TestHandleManifest_EventsAPI_NoPublicURL_409(t *testing.T) {
	r := newSlackWorkspaceRig(t, "")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-manifest-nourl")

	rec := httptest.NewRecorder()
	r.hdl.handleManifest(rec, r.manifestReq(owner, orgID, "events_api"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleManifest_Socket_OK: socket manifests never need a public URL.
func TestHandleManifest_Socket_OK(t *testing.T) {
	r := newSlackWorkspaceRig(t, "")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-manifest-socket")

	rec := httptest.NewRecorder()
	r.hdl.handleManifest(rec, r.manifestReq(owner, orgID, "socket"))
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
	r.hdl.handleManifest(rec, r.manifestReq(owner, orgID, "events_api"))
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
	r.hdl.handleManifest(rec, r.manifestReq(owner, orgID, "carrier-pigeon"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---------- one active connection per workspace ----------

// connectAs posts a connect for bot token token (and app token xapp-<token>)
// as owner of orgID.
func (r *slackWorkspaceRig) connectAs(owner, orgID, token string) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.importReq(owner, orgID, map[string]string{"bot_token": token, "app_token": "xapp-" + token}))
	return rec
}

// TestHandleConnect_FirstConnectionLandsActive: the first connection in a
// workspace is the org's own app, active, named for its bot; reconnecting it
// (a rotation) keeps it active and takes the bot's current name.
func TestHandleConnect_FirstConnectionLandsActive(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-first-active")
	r.fake.stageConnectOK("xoxb-first", "T0FIRST01", "Acme", "U0BOT", "A0FIRST001", "")
	r.fake.stageBotName("xoxb-first", "triage-bot")

	rec := r.connectAs(owner, orgID, "xoxb-first")
	if rec.Code != http.StatusCreated {
		t.Fatalf("connect status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	view := decodeBody[workspaceView](t, rec)
	if view.Source != slackstore.SourceOrg || !view.Active || view.AppName != "triage-bot" {
		t.Errorf("connect response = %+v; want source org, active, app_name triage-bot", view)
	}

	r.fake.stageBotName("xoxb-first", "triage-bot (renamed)")
	rec = r.connectAs(owner, orgID, "xoxb-first")
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	view = decodeBody[workspaceView](t, rec)
	if !view.Active || view.AppName != "triage-bot (renamed)" {
		t.Errorf("reconnect response = %+v; want still active, with the new app_name", view)
	}
	stored, err := slackstore.FromStores(r.stores).Connections.GetByWorkspaceAppSystem(r.t.Context(), "T0FIRST01", "A0FIRST001")
	if err != nil || stored == nil {
		t.Fatalf("read the connection: %v (%v)", stored, err)
	}
	if !stored.Active || stored.AppName != "triage-bot (renamed)" {
		t.Errorf("stored connection = %+v; want active, app_name triage-bot (renamed)", stored)
	}
}

// TestHandleConnect_SecondAppInWorkspace_LandsInactive: a different app
// connected into a workspace where the org already has an active connection
// is stored inactive and answered as a normal import, and the active one is
// unchanged; connecting the second app again keeps it inactive.
func TestHandleConnect_SecondAppInWorkspace_LandsInactive(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-second-app")
	r.fake.stageConnectOK("xoxb-one", "T0SECOND1", "Acme", "U0BOTONE", "A0SECOND01", "")
	r.fake.stageConnectOK("xoxb-two", "T0SECOND1", "Acme", "U0BOTTWO", "A0SECOND02", "")

	if rec := r.connectAs(owner, orgID, "xoxb-one"); rec.Code != http.StatusCreated {
		t.Fatalf("first connect status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	conns := slackstore.FromStores(r.stores).Connections
	before, err := conns.ActiveForWorkspaceSystem(r.t.Context(), orgID, "T0SECOND1")
	if err != nil || before == nil {
		t.Fatalf("active connection after the first connect = %+v (%v)", before, err)
	}

	rec := r.connectAs(owner, orgID, "xoxb-two")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second app status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if view := decodeBody[workspaceView](t, rec); view.APIAppID != "A0SECOND02" || view.Active {
		t.Errorf("second app response = %+v; want A0SECOND02, inactive", view)
	}
	if v := r.secretValue(orgID, integrations.SlackConnectionKeysFor("T0SECOND1", "A0SECOND02").BotToken); v != "xoxb-two" {
		t.Errorf("second app's bot token = %q; want xoxb-two stored", v)
	}
	after, err := conns.ActiveForWorkspaceSystem(r.t.Context(), orgID, "T0SECOND1")
	if err != nil || after == nil || after.APIAppID != "A0SECOND01" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("active connection = %+v (%v); want A0SECOND01 untouched", after, err)
	}

	rec = r.connectAs(owner, orgID, "xoxb-two")
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if view := decodeBody[workspaceView](t, rec); view.Active {
		t.Errorf("reconnect response = %+v; want still inactive", view)
	}
}
