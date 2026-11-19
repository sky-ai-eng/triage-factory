package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

const (
	switchWorkspace = "T0SWITCH1"
	switchAppA      = "A0SWITCHA"
	switchAppB      = "A0SWITCHB"
	switchTokenA    = "xoxb-switch-a"
	switchTokenB    = "xoxb-switch-b"
)

// activationRig is one org with two of its own apps connected in
// switchWorkspace through the import route, both over the Events API with
// removalTestSecret: A first, which is active, then B, which lands inactive.
type activationRig struct {
	*removalRig
}

func newActivationRig(t *testing.T) *activationRig {
	t.Helper()
	r := &activationRig{removalRig: newRemovalRig(t)}
	for _, app := range []struct{ token, botUser, appID, name string }{
		{switchTokenA, "U0BOTA", switchAppA, "bot-a"},
		{switchTokenB, "U0BOTB", switchAppB, "bot-b"},
	} {
		r.fake.stageConnectOK(app.token, switchWorkspace, "Acme", app.botUser, app.appID, "")
		r.fake.stageBotName(app.token, app.name)
		rec := httptest.NewRecorder()
		r.hdl.handleConnect(rec, r.importReq(r.owner, r.orgID, map[string]string{
			"bot_token": app.token, "signing_secret": removalTestSecret, "transport": transportEventsAPI,
		}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("connect %s: status = %d (body=%s)", app.appID, rec.Code, rec.Body.String())
		}
	}
	if active := r.activeApp(); active != switchAppA {
		t.Fatalf("active app after the two connects = %q; want %s", active, switchAppA)
	}
	return r
}

// activeApp is the app of the org's active connection in switchWorkspace, ""
// when there is none.
func (r *activationRig) activeApp() string {
	r.t.Helper()
	c, err := slackstore.FromStores(r.stores).Connections.ActiveForWorkspaceSystem(r.t.Context(), r.orgID, switchWorkspace)
	if err != nil {
		r.t.Fatalf("read active connection: %v", err)
	}
	if c == nil {
		return ""
	}
	return c.APIAppID
}

// activeCount is how many of the org's connections in switchWorkspace are
// active.
func (r *activationRig) activeCount() int {
	r.t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`
		SELECT count(*) FROM org_slack_connections WHERE org_id = $1 AND workspace_id = $2 AND active
	`, r.orgID, switchWorkspace).Scan(&n); err != nil {
		r.t.Fatalf("count active connections: %v", err)
	}
	return n
}

// watch makes the rig's team watch exactly channels (Slack id -> name) in
// workspaceID, replacing what it watched.
func (r *activationRig) watch(workspaceID string, channels map[string]string) {
	r.t.Helper()
	ids := make([]string, 0, len(channels))
	for id, name := range channels {
		if _, err := slackstore.FromStores(r.stores).Channels.EnsureSystem(r.t.Context(), r.orgID, workspaceID, id, name); err != nil {
			r.t.Fatalf("ensure channel %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	if err := r.stores.Tx.WithTx(r.t.Context(), r.orgID, r.owner, func(tx db.TxStores) error {
		_, err := slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(r.t.Context(), r.orgID, r.teamID, ids)
		return err
	}); err != nil {
		r.t.Fatalf("watch channels: %v", err)
	}
}

// switchReq is a request as callerID to one of apiAppID's switch routes, the
// path's last segment being route.
func (r *activationRig) switchReq(method, callerID, apiAppID, route string, body any) *http.Request {
	r.t.Helper()
	req := r.req(method, connectionsPath(r.orgID)+"/"+switchWorkspace+"/"+apiAppID+"/"+route, callerID, r.orgID, switchWorkspace, body)
	req.SetPathValue("api_app_id", apiAppID)
	return req
}

func (r *activationRig) preview(callerID, apiAppID string) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.hdl.handleActivationPreview(rec, r.switchReq(http.MethodGet, callerID, apiAppID, "activation-preview", nil))
	return rec
}

// previewOK previews apiAppID as the owner, failing the test on anything but
// a 200.
func (r *activationRig) previewOK(apiAppID string) activationPreview {
	r.t.Helper()
	rec := r.preview(r.owner, apiAppID)
	if rec.Code != http.StatusOK {
		r.t.Fatalf("preview %s: status = %d (body=%s)", apiAppID, rec.Code, rec.Body.String())
	}
	return decodeBody[activationPreview](r.t, rec)
}

func (r *activationRig) activate(callerID, apiAppID string, body any) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.hdl.handleActivate(rec, r.switchReq(http.MethodPost, callerID, apiAppID, "activate", body))
	return rec
}

// activateOK previews apiAppID and activates it with the preview's
// fingerprint, failing the test on anything but a 200.
func (r *activationRig) activateOK(apiAppID string) activationResult {
	r.t.Helper()
	rec := r.activate(r.owner, apiAppID, map[string]string{"fingerprint": r.previewOK(apiAppID).Fingerprint})
	if rec.Code != http.StatusOK {
		r.t.Fatalf("activate %s: status = %d (body=%s)", apiAppID, rec.Code, rec.Body.String())
	}
	return decodeBody[activationResult](r.t, rec)
}

// errorReasons is a refusal's reasons, in order.
func errorReasons(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	body := decodeBody[struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}](t, rec)
	out := make([]string, len(body.Errors))
	for i, e := range body.Errors {
		out[i] = e.Reason
	}
	return out
}

func publicChannel(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "is_private": false}
}

// ---------- preview ----------

// TestActivationPreview_Gate: someone outside the org gets 404 and a member
// 403; an admin gets 404 for a connection the org does not have and 409 for
// the workspace's active one.
func TestActivationPreview_Gate(t *testing.T) {
	r := newActivationRig(t)
	member := pgtest.SeedUser(t, r.h, "slack-switch-member")
	pgtest.AddOrgMember(t, r.h, member, r.orgID, r.teamID, "member", "member")
	_, outsider, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-switch-outsider")

	for _, tc := range []struct {
		name   string
		caller string
		app    string
		want   int
	}{
		{"outsider", outsider, switchAppB, http.StatusNotFound},
		{"member", member, switchAppB, http.StatusForbidden},
		{"unknown connection", r.owner, "A0NOPE", http.StatusNotFound},
		{"active target", r.owner, switchAppA, http.StatusConflict},
	} {
		if rec := r.preview(tc.caller, tc.app); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if rec := r.activate(tc.caller, tc.app, map[string]string{"fingerprint": "x"}); rec.Code != tc.want {
			t.Errorf("%s: activate status = %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if got := errorReasons(t, r.preview(r.owner, switchAppA)); !slices.Equal(got, []string{httpx.ReasonConflict}) {
		t.Errorf("active target reasons = %v; want CONFLICT", got)
	}
}

// TestActivationPreview_Channels: the preview names both connections and
// answers each channel a team watches in the workspace with its status under
// the target's bot; an unwatched channel, and one watched in another
// workspace, are not listed.
func TestActivationPreview_Channels(t *testing.T) {
	r := newActivationRig(t)
	r.watch(switchWorkspace, map[string]string{"C0PUB": "eng-alerts", "C0MEM": "deploys", "G0PRIV": "secret"})
	if _, err := slackstore.FromStores(r.stores).Channels.EnsureSystem(r.t.Context(), r.orgID, switchWorkspace, "C0UNWATCHED", "random"); err != nil {
		t.Fatalf("register unwatched channel: %v", err)
	}
	r.fake.stageChannels(switchTokenB,
		[]map[string]any{publicChannel("C0PUB", "eng-alerts"), publicChannel("C0MEM", "deploys"), publicChannel("C0UNWATCHED", "random")},
		[]map[string]any{publicChannel("C0MEM", "deploys")})

	p := r.previewOK(switchAppB)
	if p.From == nil || p.From.APIAppID != switchAppA || !p.From.Active || p.From.BotUserID != "U0BOTA" {
		t.Errorf("from = %+v; want A, active, bot U0BOTA", p.From)
	}
	if p.To.APIAppID != switchAppB || p.To.Active || p.To.BotUserID != "U0BOTB" {
		t.Errorf("to = %+v; want B, inactive, bot U0BOTB", p.To)
	}
	want := []activationChannel{
		{ChannelID: "C0MEM", Name: "deploys", Status: channelMember},
		{ChannelID: "C0PUB", Name: "eng-alerts", Status: channelWillJoin},
		{ChannelID: "G0PRIV", Name: "secret", Status: channelNeedsInvite},
	}
	if !slices.Equal(p.Channels, want) {
		t.Errorf("channels = %+v; want %+v", p.Channels, want)
	}
	if len(p.Blockers) != 0 || len(p.Notes) != 0 || p.Fingerprint == "" {
		t.Errorf("blockers = %+v, notes = %+v, fingerprint = %q; want none, none, and one", p.Blockers, p.Notes, p.Fingerprint)
	}
	if got := r.fake.joined(); len(got) != 0 {
		t.Errorf("joins during a preview = %v; want none", got)
	}
}

// TestActivationPreview_Truncated: past a listing that stopped at its cap, a
// watched channel the listing did not reach is unknown.
func TestActivationPreview_Truncated(t *testing.T) {
	r := newActivationRig(t)
	withLoweredCap(t, 1)
	r.watch(switchWorkspace, map[string]string{"C0PUB": "eng-alerts", "C0LATER": "later"})
	r.fake.stageChannels(switchTokenB, []map[string]any{publicChannel("C0PUB", "eng-alerts")}, nil)
	r.fake.stageTruncated(switchTokenB)

	p := r.previewOK(switchAppB)
	want := []activationChannel{
		{ChannelID: "C0PUB", Name: "eng-alerts", Status: channelWillJoin},
		{ChannelID: "C0LATER", Name: "later", Status: channelUnknown},
	}
	if !slices.Equal(p.Channels, want) {
		t.Errorf("channels = %+v; want %+v", p.Channels, want)
	}
}

// TestActivationPreview_Blockers: a token Slack rejects is token_rejected, and
// one short a scope is missing_scopes naming it; either way the channels are
// unknown, Slack's channel listing is not read, and the answer is a 200.
func TestActivationPreview_Blockers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(f *fakeSlackServer)
		want  activationBlocker
	}{
		{"rejected", func(f *fakeSlackServer) { f.stageAuthTestRejected(switchTokenB) }, activationBlocker{Code: blockerTokenRejected}},
		{"short a scope", func(f *fakeSlackServer) { f.stageScopes(switchTokenB, "chat:write,channels:read") }, activationBlocker{Code: blockerMissingScopes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newActivationRig(t)
			r.watch(switchWorkspace, map[string]string{"C0PUB": "eng-alerts"})
			tc.stage(r.fake)
			hits := r.fake.listCalls()

			p := r.previewOK(switchAppB)
			if len(p.Blockers) != 1 || p.Blockers[0].Code != tc.want.Code {
				t.Fatalf("blockers = %+v; want one %s", p.Blockers, tc.want.Code)
			}
			if tc.want.Code == blockerMissingScopes {
				if slices.Contains(p.Blockers[0].Scopes, "chat:write") || !slices.Contains(p.Blockers[0].Scopes, "files:write") {
					t.Errorf("missing scopes = %v; want files:write and not chat:write", p.Blockers[0].Scopes)
				}
			}
			if len(p.Channels) != 1 || p.Channels[0].Status != channelUnknown {
				t.Errorf("channels = %+v; want the one channel unknown", p.Channels)
			}
			if got := r.fake.listCalls(); got != hits {
				t.Errorf("conversations.list called %d times; want none", got-hits)
			}
		})
	}
}

// TestActivationPreview_SlackUnreachable: auth.test meeting no answer is a
// 502.
func TestActivationPreview_SlackUnreachable(t *testing.T) {
	r := newActivationRig(t)
	r.fake.stageAuthTestStatus(switchTokenB, http.StatusServiceUnavailable)

	rec := r.preview(r.owner, switchAppB)
	if rec.Code != http.StatusBadGateway || !slices.Equal(errorReasons(t, rec), []string{httpx.ReasonUpstreamUnavailable}) {
		t.Errorf("status = %d (body=%s); want 502 UPSTREAM_UNAVAILABLE", rec.Code, rec.Body.String())
	}
	rec = r.activate(r.owner, switchAppB, map[string]string{"fingerprint": "x"})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("activate status = %d (body=%s); want 502", rec.Code, rec.Body.String())
	}
}

// TestActivation_SocketTargetAppToken: a target that receives over Socket Mode
// has its stored app-level token checked. Slack refusing it is
// app_token_rejected, which blocks the activation; Slack failing on its own
// side is a 502. A target over the Events API has no app-level token to check.
func TestActivation_SocketTargetAppToken(t *testing.T) {
	r := newActivationRig(t)
	const tokenC, appTokenC, appC = "xoxb-switch-c", "xapp-switch-c", "A0SWITCHC"
	r.fake.stageConnectOK(tokenC, switchWorkspace, "Acme", "U0BOTC", appC, "")
	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.importReq(r.owner, r.orgID, map[string]string{"bot_token": tokenC, "app_token": appTokenC}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("connect C over Socket Mode: status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	before := r.fake.openConnCalls()
	if p := r.previewOK(switchAppB); len(p.Blockers) != 0 {
		t.Errorf("Events API target blockers = %+v; want none", p.Blockers)
	}
	if got := r.fake.openConnCalls(); got != before {
		t.Errorf("apps.connections.open called %d times for an Events API target; want none", got-before)
	}
	if p := r.previewOK(appC); len(p.Blockers) != 0 {
		t.Errorf("Socket Mode target with a working app-level token: blockers = %+v; want none", p.Blockers)
	}
	if got := r.fake.openConnCalls(); got != before+1 {
		t.Errorf("apps.connections.open called %d times for the Socket Mode target; want 1", got-before)
	}

	r.fake.stageOpenConnFail(appTokenC, "token_revoked")
	p := r.previewOK(appC)
	if want := []activationBlocker{{Code: blockerAppTokenRejected}}; !reflect.DeepEqual(p.Blockers, want) {
		t.Errorf("blockers = %+v; want %+v", p.Blockers, want)
	}
	rec = r.activate(r.owner, appC, map[string]string{"fingerprint": p.Fingerprint})
	if rec.Code != http.StatusUnprocessableEntity || !slices.Equal(errorReasons(t, rec), []string{httpx.ReasonUpstreamRejected}) {
		t.Errorf("activate: status = %d (body=%s); want 422 UPSTREAM_REJECTED", rec.Code, rec.Body.String())
	}
	if got := r.activeApp(); got != switchAppA {
		t.Errorf("active app = %q; want %s untouched", got, switchAppA)
	}

	r.fake.stageOpenConnFail(appTokenC, "internal_error")
	if rec := r.preview(r.owner, appC); rec.Code != http.StatusBadGateway {
		t.Errorf("Slack failing on its own side: status = %d (body=%s); want 502", rec.Code, rec.Body.String())
	}
}

// TestActivationPreview_DeploymentNoteAndEmptyWorkspace: the history rate
// limit is noted only for a target through the deployment's app, and from is
// null for a workspace with no active connection.
func TestActivationPreview_DeploymentNoteAndEmptyWorkspace(t *testing.T) {
	r := newActivationRig(t)
	if _, err := r.h.AdminDB.Exec(`UPDATE org_slack_connections SET active = false WHERE org_id = $1`, r.orgID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	keys := integrations.SlackConnectionKeysFor(switchWorkspace, deployTestAppID).BotToken
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO org_slack_connections (
			workspace_id, api_app_id, org_id, source, active, workspace_name, bot_user_id, bot_token_ref
		) VALUES ($1, $2, $3, 'deployment', false, 'Acme', 'U0DEPBOT', $4)
	`, switchWorkspace, deployTestAppID, r.orgID, keys); err != nil {
		t.Fatalf("insert deployment connection: %v", err)
	}
	if err := r.stores.Tx.WithTx(r.t.Context(), r.orgID, r.owner, func(tx db.TxStores) error {
		return tx.Secrets.Put(r.t.Context(), r.orgID, keys, "xoxb-deployment", "Slack bot token")
	}); err != nil {
		t.Fatalf("store deployment bot token: %v", err)
	}
	r.fake.stageAuthTestOK("xoxb-deployment", switchWorkspace, "Acme", "U0DEPBOT", "")

	dep := r.previewOK(deployTestAppID)
	if dep.From != nil {
		t.Errorf("from = %+v; want null", dep.From)
	}
	if len(dep.Notes) != 1 || dep.Notes[0].Code != noteHistoryRateLimit || dep.Notes[0].Message == "" {
		t.Errorf("deployment target notes = %+v; want history_rate_limit", dep.Notes)
	}
	if own := r.previewOK(switchAppB); len(own.Notes) != 0 {
		t.Errorf("own app target notes = %+v; want none", own.Notes)
	}

	r.activateOK(deployTestAppID)
	if got := r.activeApp(); got != deployTestAppID {
		t.Errorf("active app = %q; want the deployment's", got)
	}
}

// TestActivationPreview_Fingerprint: the fingerprint moves when either
// connection's row is written and when a team starts or stops watching a
// channel in the workspace, and not for a channel watched in another
// workspace.
func TestActivationPreview_Fingerprint(t *testing.T) {
	r := newActivationRig(t)
	r.watch(switchWorkspace, map[string]string{"C0ONE": "one"})
	last := r.previewOK(switchAppB).Fingerprint
	step := func(name string, change func(), wantChange bool) {
		t.Helper()
		change()
		got := r.previewOK(switchAppB).Fingerprint
		if (got != last) != wantChange {
			t.Errorf("%s: fingerprint changed = %v; want %v", name, got != last, wantChange)
		}
		last = got
	}
	touch := func(app string) func() {
		return func() {
			if _, err := r.h.AdminDB.Exec(`
				UPDATE org_slack_connections SET app_name = app_name || '.' WHERE workspace_id = $1 AND api_app_id = $2
			`, switchWorkspace, app); err != nil {
				t.Fatalf("touch %s: %v", app, err)
			}
		}
	}

	step("nothing changed", func() {}, false)
	step("the active connection written", touch(switchAppA), true)
	step("the target written", touch(switchAppB), true)
	step("a channel watched", func() { r.watch(switchWorkspace, map[string]string{"C0ONE": "one", "C0TWO": "two"}) }, true)
	step("a channel unwatched", func() { r.watch(switchWorkspace, map[string]string{"C0TWO": "two"}) }, true)
	step("a channel watched in another workspace", func() {
		if _, err := slackstore.FromStores(r.stores).Channels.EnsureSystem(r.t.Context(), r.orgID, "T0ELSEWHERE", "C0ELSE", "else"); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if err := r.stores.Tx.WithTx(r.t.Context(), r.orgID, r.owner, func(tx db.TxStores) error {
			_, err := slackstore.FromTx(tx).TeamChannels.ReplaceForTeam(r.t.Context(), r.orgID, r.teamID, []string{"C0TWO", "C0ELSE"})
			return err
		}); err != nil {
			t.Fatalf("watch elsewhere: %v", err)
		}
	}, false)
}

// ---------- activate ----------

// TestActivate_Fingerprint: a body with no fingerprint is 422, and a stale
// one is 409; neither writes anything.
func TestActivate_Fingerprint(t *testing.T) {
	r := newActivationRig(t)
	auditBefore := len(r.auditRows(r.orgID))

	for _, body := range []any{map[string]any{}, map[string]any{"fingerprint": nil}, map[string]any{"fingerprint": ""}} {
		rec := r.activate(r.owner, switchAppB, body)
		if rec.Code != http.StatusUnprocessableEntity || !slices.Equal(errorReasons(t, rec), []string{httpx.ReasonMissingField}) {
			t.Errorf("body %v: status = %d (body=%s); want 422 MISSING_FIELD", body, rec.Code, rec.Body.String())
		}
	}
	if rec := r.activate(r.owner, switchAppB, map[string]any{"fingerprint": "x", "force": true}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: status = %d; want 400", rec.Code)
	}

	stale := r.previewOK(switchAppB).Fingerprint
	r.watch(switchWorkspace, map[string]string{"C0NEW": "new"})
	rec := r.activate(r.owner, switchAppB, map[string]string{"fingerprint": stale})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale fingerprint: status = %d (body=%s); want 409", rec.Code, rec.Body.String())
	}
	errs := decodeBody[struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}](t, rec).Errors
	if len(errs) != 1 || errs[0].Reason != httpx.ReasonConflict || errs[0].Message != staleActivationMessage {
		t.Errorf("stale fingerprint errors = %+v; want CONFLICT %q", errs, staleActivationMessage)
	}

	if got := r.activeApp(); got != switchAppA {
		t.Errorf("active app = %q; want %s untouched", got, switchAppA)
	}
	if got := len(r.auditRows(r.orgID)); got != auditBefore {
		t.Errorf("audit rows = %d; want %d", got, auditBefore)
	}
	if got := r.fake.joined(); len(got) != 0 {
		t.Errorf("joins = %v; want none", got)
	}
}

// TestActivate_Blocker: a target whose token Slack rejects, or that lacks a
// scope, is a 422 naming the blocker, and nothing is written.
func TestActivate_Blocker(t *testing.T) {
	r := newActivationRig(t)
	fingerprint := r.previewOK(switchAppB).Fingerprint
	r.fake.stageScopes(switchTokenB, "chat:write")

	rec := r.activate(r.owner, switchAppB, map[string]string{"fingerprint": fingerprint})
	if rec.Code != http.StatusUnprocessableEntity || !slices.Equal(errorReasons(t, rec), []string{httpx.ReasonUpstreamRejected}) {
		t.Fatalf("status = %d (body=%s); want 422 UPSTREAM_REJECTED", rec.Code, rec.Body.String())
	}
	if got := r.activeApp(); got != switchAppA {
		t.Errorf("active app = %q; want %s untouched", got, switchAppA)
	}

	r.fake.stageAuthTestRejected(switchTokenB)
	if rec := r.activate(r.owner, switchAppB, map[string]string{"fingerprint": fingerprint}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("rejected token: status = %d (body=%s); want 422", rec.Code, rec.Body.String())
	}
}

// TestActivate_Switches: activation swaps the two connections, leaving one
// active before and after, records one audit entry, joins the watched public
// channels with the target's token, reports a private one as needing an
// invite, and keeps the old connection and its token.
func TestActivate_Switches(t *testing.T) {
	r := newActivationRig(t)
	r.watch(switchWorkspace, map[string]string{"C0PUB": "eng-alerts", "C0MEM": "deploys", "G0PRIV": "secret"})
	r.fake.stageChannels(switchTokenB,
		[]map[string]any{publicChannel("C0PUB", "eng-alerts"), publicChannel("C0MEM", "deploys")},
		[]map[string]any{publicChannel("C0MEM", "deploys")})
	r.fake.stageJoinFail("G0PRIV", "method_not_supported_for_channel_type")
	if n := r.activeCount(); n != 1 {
		t.Fatalf("active connections before = %d; want 1", n)
	}
	auditBefore := len(r.auditRows(r.orgID))

	res := r.activateOK(switchAppB)
	if res.Connection.APIAppID != switchAppB || !res.Connection.Active {
		t.Errorf("connection = %+v; want B, active", res.Connection)
	}
	if want := []channelsWarning{{ChannelID: "G0PRIV", Reason: "invite_required"}}; !slices.Equal(res.Warnings, want) {
		t.Errorf("warnings = %+v; want %+v", res.Warnings, want)
	}
	if got := r.activeApp(); got != switchAppB {
		t.Errorf("active app = %q; want %s", got, switchAppB)
	}
	if n := r.activeCount(); n != 1 {
		t.Errorf("active connections after = %d; want 1", n)
	}
	joins := r.fake.joined()
	slices.Sort(joins)
	if want := []string{switchTokenB + " C0PUB", switchTokenB + " G0PRIV"}; !slices.Equal(joins, want) {
		t.Errorf("joins = %v; want %v", joins, want)
	}

	old, err := slackstore.FromStores(r.stores).Connections.GetByWorkspaceAppSystem(r.t.Context(), switchWorkspace, switchAppA)
	if err != nil || old == nil || old.Active {
		t.Errorf("old connection = %+v (%v); want kept, inactive", old, err)
	}
	if v := r.secretValue(r.orgID, integrations.SlackConnectionKeysFor(switchWorkspace, switchAppA).BotToken); v != switchTokenA {
		t.Errorf("old bot token = %q; want it kept", v)
	}

	rows := r.auditRows(r.orgID)
	if len(rows) != auditBefore+1 {
		t.Fatalf("audit rows = %d; want %d", len(rows), auditBefore+1)
	}
	var detail struct {
		Name        string `json:"name"`
		WorkspaceID string `json:"workspace_id"`
		FromAppID   string `json:"from_api_app_id"`
		ToAppID     string `json:"to_api_app_id"`
	}
	var raw string
	if err := r.h.AdminDB.QueryRow(`
		SELECT detail_json FROM access_change_log WHERE org_id = $1 AND action = $2
	`, r.orgID, domain.AccessActionSlackConnectionActivated).Scan(&raw); err != nil {
		t.Fatalf("read the activation's audit row: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if rows[len(rows)-1].Actor != r.owner || detail.Name != "Acme" || detail.WorkspaceID != switchWorkspace ||
		detail.FromAppID != switchAppA || detail.ToAppID != switchAppB {
		t.Errorf("audit row = %+v, detail %+v; want the owner switching Acme from A to B", rows[len(rows)-1], detail)
	}
}

// TestActivate_SwitchBack: a fresh preview of the old connection switches the
// workspace back, with no reinstall.
func TestActivate_SwitchBack(t *testing.T) {
	r := newActivationRig(t)
	r.activateOK(switchAppB)
	p := r.previewOK(switchAppA)
	if p.From == nil || p.From.APIAppID != switchAppB {
		t.Errorf("from = %+v; want B", p.From)
	}
	r.activateOK(switchAppA)
	if got := r.activeApp(); got != switchAppA {
		t.Errorf("active app = %q; want %s", got, switchAppA)
	}
	if n := r.activeCount(); n != 1 {
		t.Errorf("active connections = %d; want 1", n)
	}
}

// TestActivate_Concurrent: two activations of different targets in one
// workspace, each with a fingerprint from the same state: one succeeds and
// the other is a 409.
func TestActivate_Concurrent(t *testing.T) {
	r := newActivationRig(t)
	const tokenC, appC = "xoxb-switch-c", "A0SWITCHC"
	r.fake.stageConnectOK(tokenC, switchWorkspace, "Acme", "U0BOTC", appC, "")
	rec := httptest.NewRecorder()
	r.hdl.handleConnect(rec, r.importReq(r.owner, r.orgID, map[string]string{
		"bot_token": tokenC, "signing_secret": removalTestSecret, "transport": transportEventsAPI,
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("connect C: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	fingerprints := map[string]string{switchAppB: r.previewOK(switchAppB).Fingerprint, appC: r.previewOK(appC).Fingerprint}

	var wg sync.WaitGroup
	codes := map[string]int{}
	var mu sync.Mutex
	for app, fp := range fingerprints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			r.hdl.handleActivate(rec, r.switchReq(http.MethodPost, r.owner, app, "activate", map[string]string{"fingerprint": fp}))
			mu.Lock()
			codes[app] = rec.Code
			mu.Unlock()
		}()
	}
	wg.Wait()

	var won, lost []string
	for app, code := range codes {
		switch code {
		case http.StatusOK:
			won = append(won, app)
		case http.StatusConflict:
			lost = append(lost, app)
		}
	}
	if len(won) != 1 || len(lost) != 1 {
		t.Fatalf("statuses = %v; want one 200 and one 409", codes)
	}
	if got := r.activeApp(); got != won[0] {
		t.Errorf("active app = %q; want the winner %s", got, won[0])
	}
	if n := r.activeCount(); n != 1 {
		t.Errorf("active connections = %d; want 1", n)
	}
}

// TestActivate_RemovalDeliveredAfterTheSwitch: Slack's removal of the old
// app, generated after its connect and before the switch but delivered after
// the switch, still disconnects the old connection. The switch wrote the row,
// but no token, so it is not a reconnect the removal predates.
func TestActivate_RemovalDeliveredAfterTheSwitch(t *testing.T) {
	r := newActivationRig(t)
	if _, err := r.h.AdminDB.Exec(`
		UPDATE org_slack_connections SET connected_at = now() - interval '1 hour' WHERE org_id = $1
	`, r.orgID); err != nil {
		t.Fatalf("backdate the connects: %v", err)
	}
	removedAt := time.Now().Add(-30 * time.Minute)
	r.activateOK(switchAppB)

	payload := eventCallbackPayload(switchWorkspace, switchAppA, "Ev-removal-late", appUninstalledInner)
	payload["event_time"] = removedAt.Unix()
	if rec := r.postSigned(switchWorkspace, payload); rec.Code != http.StatusOK {
		t.Fatalf("removal status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := r.outcomes(); got[outcomeDisconnected] != 1 {
		t.Errorf("outcomes = %v; want one %q", got, outcomeDisconnected)
	}
	if old, err := slackstore.FromStores(r.stores).Connections.GetByWorkspaceAppSystem(r.t.Context(), switchWorkspace, switchAppA); err != nil || old != nil {
		t.Errorf("old connection after its removal = %+v (%v); want deleted", old, err)
	}
	if got := r.activeApp(); got != switchAppB {
		t.Errorf("active app = %q; want %s kept", got, switchAppB)
	}
}

// TestActivate_AfterTheSwitch: a mention delivered through the new app
// reaches the pipeline and the same mention through the old one is dropped,
// and an agent's sealed Slack credentials carry the new bot's token.
func TestActivate_AfterTheSwitch(t *testing.T) {
	r := newActivationRig(t)
	r.watch(switchWorkspace, map[string]string{"C0PUB": "eng-alerts"})
	r.fake.stageChannels(switchTokenB, []map[string]any{publicChannel("C0PUB", "eng-alerts")}, []map[string]any{publicChannel("C0PUB", "eng-alerts")})
	r.activateOK(switchAppB)

	mention := mentionInner("C0PUB", "U0PERSON", "1700000000.000100")
	if rec := r.postSigned(switchWorkspace, eventCallbackPayload(switchWorkspace, switchAppA, "Ev-old", mention)); rec.Code != http.StatusOK {
		t.Fatalf("old app delivery: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 0 {
		t.Fatalf("published after the old app's delivery = %+v; want nothing", *r.published)
	}
	if got := r.outcomes(); got[dropInactive] != 1 {
		t.Errorf("outcomes after the old app's delivery = %v; want one %q", got, dropInactive)
	}

	if rec := r.postSigned(switchWorkspace, eventCallbackPayload(switchWorkspace, switchAppB, "Ev-new", mention)); rec.Code != http.StatusOK {
		t.Fatalf("new app delivery: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*r.published) != 1 || (*r.published)[0].OrgID != r.orgID {
		t.Errorf("published after the new app's delivery = %+v; want one event for the org", *r.published)
	}

	raw, err := slackProviderCredential(t.Context(), r.stores, agenthost.ProvisionScope{OrgID: r.orgID, TeamID: r.teamID})
	if err != nil {
		t.Fatalf("seal slack credentials: %v", err)
	}
	var creds slackBundleCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("unmarshal sealed set: %v", err)
	}
	want := []slackWorkspaceToken{{WorkspaceID: switchWorkspace, APIAppID: switchAppB, BotToken: switchTokenB}}
	if !slices.Equal(creds.Workspaces, want) {
		t.Errorf("sealed %+v; want %+v", creds.Workspaces, want)
	}
}
