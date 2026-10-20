package slack

// Internal (package slack) pgtest-backed handler suite for the channel
// tracking API (TFAC-543) — mirrors workspaces_pg_test.go's rig shape
// (claims built directly via httpx.WithOrgID/WithClaims, no cookie-session
// dance).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
)

// ---------- fake Slack API (conversations.list / users.conversations / conversations.join) ----------

type fakeChannelsSlack struct {
	mu sync.Mutex
	// public/mine are keyed by bot token — the channel list each endpoint
	// answers with for that token.
	public        map[string][]map[string]any
	mine          map[string][]map[string]any
	joinFailCode  map[string]string // channel_id -> conversations.join error code (absent = success)
	joinCalls     []string
	failList      map[string]bool // bot token -> conversations.list itself should fail (ok:false)
	moreAvailable map[string]bool // bot token -> conversations.list should claim more pages exist
	// info is what conversations.info knows, by bot token then channel id (the
	// channel's name); any other channel is channel_not_found. infoFailCode
	// fails every conversations.info call for a bot token with that code.
	info         map[string]map[string]string
	infoFailCode map[string]string
	infoCalls    int
}

func newFakeChannelsSlack(t *testing.T) *fakeChannelsSlack {
	t.Helper()
	f := &fakeChannelsSlack{
		public:        map[string][]map[string]any{},
		mine:          map[string][]map[string]any{},
		joinFailCode:  map[string]string{},
		failList:      map[string]bool{},
		moreAvailable: map[string]bool{},
		info:          map[string]map[string]string{},
		infoFailCode:  map[string]string{},
	}
	withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/conversations.list"):
			f.mu.Lock()
			fail := f.failList[token]
			chans := f.public[token]
			nextCursor := ""
			if f.moreAvailable[token] {
				nextCursor = "more"
			}
			f.mu.Unlock()
			if fail {
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "internal_error"})
				return
			}
			writeSlackFakeJSON(w, map[string]any{"ok": true, "channels": chans, "response_metadata": map[string]any{"next_cursor": nextCursor}})
		case strings.HasSuffix(r.URL.Path, "/users.conversations"):
			f.mu.Lock()
			chans := f.mine[token]
			f.mu.Unlock()
			writeSlackFakeJSON(w, map[string]any{"ok": true, "channels": chans, "response_metadata": map[string]any{"next_cursor": ""}})
		case strings.HasSuffix(r.URL.Path, "/conversations.info"):
			channelID := r.URL.Query().Get("channel")
			f.mu.Lock()
			f.infoCalls++
			name, known := f.info[token][channelID]
			code := f.infoFailCode[token]
			f.mu.Unlock()
			switch {
			case code != "":
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": code})
			case known:
				writeSlackFakeJSON(w, map[string]any{"ok": true, "channel": map[string]any{"id": channelID, "name": name}})
			default:
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "channel_not_found"})
			}
		case strings.HasSuffix(r.URL.Path, "/conversations.join"):
			_ = r.ParseForm()
			channelID := r.FormValue("channel")
			f.mu.Lock()
			f.joinCalls = append(f.joinCalls, channelID)
			code := f.joinFailCode[channelID]
			f.mu.Unlock()
			if code != "" {
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": code})
				return
			}
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		default:
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "unhandled_path:" + r.URL.Path})
		}
	})
	return f
}

func (f *fakeChannelsSlack) setChannels(botToken string, public, mine []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.public[botToken] = public
	f.mine[botToken] = mine
}

func (f *fakeChannelsSlack) failConversationsList(botToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failList[botToken] = true
}

// setMoreAvailable makes conversations.list always claim a non-empty
// next_cursor for botToken, regardless of the cursor query param (this fake
// doesn't paginate by cursor — it always answers the full configured list) —
// paired with a lowered slackConversationsListCap, this exercises the
// truncated=true path: the pagination loop hits the cap on the first page,
// but Slack (per this fake) still had more.
func (f *fakeChannelsSlack) setMoreAvailable(botToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moreAvailable[botToken] = true
}

// knowChannel makes conversations.info answer for channelID under botToken.
func (f *fakeChannelsSlack) knowChannel(botToken, channelID, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.info[botToken] == nil {
		f.info[botToken] = map[string]string{}
	}
	f.info[botToken][channelID] = name
}

// failInfo makes every conversations.info call under botToken fail with code.
func (f *fakeChannelsSlack) failInfo(botToken, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoFailCode[botToken] = code
}

func (f *fakeChannelsSlack) infoCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infoCalls
}

// failJoin makes conversations.join fail with a generic, non-private-channel
// error code — the join_failed scenario (as opposed to failJoinWithCode's
// invite_required classification).
func (f *fakeChannelsSlack) failJoin(channelID string) {
	f.failJoinWithCode(channelID, "internal_error")
}

func (f *fakeChannelsSlack) failJoinWithCode(channelID, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinFailCode[channelID] = code
}

func (f *fakeChannelsSlack) joinCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.joinCalls)
}

// ---------- rig ----------

type slackChannelsRig struct {
	t    *testing.T
	h    *pgtest.Harness
	stor db.Stores
	hdl  *channelsHandler
	fake *fakeChannelsSlack
}

func newSlackChannelsRig(t *testing.T) *slackChannelsRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	az := authz.New(h.AdminDB, stores.Tx)
	fake := newFakeChannelsSlack(t)

	hdl := &channelsHandler{tx: stores.Tx, stores: stores, az: az, client: slackHTTPClient}
	return &slackChannelsRig{t: t, h: h, stor: stores, hdl: hdl, fake: fake}
}

func (r *slackChannelsRig) req(method, path, callerID, orgID, teamID string, body any) *http.Request {
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
	if teamID != "" {
		req.SetPathValue("team_id", teamID)
	}
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, orgID)
	return req.WithContext(ctx)
}

func (r *slackChannelsRig) reqPrimary(callerID, orgID, channelID string, body any) *http.Request {
	r.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/"+channelID+"/primary", bytes.NewReader(b))
	req.SetPathValue("channel_id", channelID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, orgID)
	return req.WithContext(ctx)
}

// seedWorkspace connects a Slack workspace for orgID with a bot token, so
// live-candidate + auto-join tests have a workspace to enumerate against.
func (r *slackChannelsRig) seedWorkspace(orgID, userID, workspaceID, botToken string) {
	r.t.Helper()
	apiAppID := "A0" + workspaceID
	botTokenRef := "slack_ws_" + workspaceID + "_" + apiAppID + "_bot_token"
	if err := r.stor.Tx.WithTx(r.t.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.Put(r.t.Context(), orgID, botTokenRef, botToken, "test bot token"); err != nil {
			return err
		}
		return slackstore.FromTx(tx).Workspaces.Upsert(r.t.Context(), slackstore.Workspace{
			WorkspaceID:        workspaceID,
			APIAppID:           apiAppID,
			OrgID:              orgID,
			WorkspaceName:      "Acme",
			Transport:          "socket",
			BotUserID:          "U0BOT",
			BotTokenRef:        botTokenRef,
			RegisteredByUserID: userID,
		})
	}); err != nil {
		r.t.Fatalf("seed workspace: %v", err)
	}
}

// listChannels connects a workspace for orgID whose listing holds every
// channel in channelIDs, with the bot a member of each, so a PUT naming them
// passes the gate and the auto-join has nothing to do.
func (r *slackChannelsRig) listChannels(orgID, userID string, channelIDs ...string) {
	r.t.Helper()
	r.seedWorkspace(orgID, userID, "T0LISTED1", "xoxb-listed")
	listed := make([]map[string]any, 0, len(channelIDs))
	for _, id := range channelIDs {
		listed = append(listed, map[string]any{"id": id, "name": strings.ToLower(id), "is_private": false})
	}
	r.fake.setChannels("xoxb-listed", listed, listed)
}

// decodeErrors decodes an error envelope.
func decodeErrors(t *testing.T, rec *httptest.ResponseRecorder) []httpx.ErrorItem {
	t.Helper()
	var out struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error envelope %q: %v", rec.Body.String(), err)
	}
	return out.Errors
}

func decodeChannels(t *testing.T, rec *httptest.ResponseRecorder) channelsResponse {
	t.Helper()
	var out channelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode channels response %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---------- authz matrix ----------

func TestChannelsHandler_EntitlementGate(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-gate")

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("licensed org: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	entitlements.RegisterProvider(entitlements.Static())
	rec2 := httptest.NewRecorder()
	r.hdl.handleList(rec2, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID, nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unlicensed org: status = %d, want 404", rec2.Code)
	}
}

func TestChannelsHandler_NonTeamMember_GET_404(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-nonmember")
	outsider := pgtest.SeedUser(t, r.h, "chan-nonmember-outsider")
	// Org member, but never joins the team (no memberships row) — insert
	// only the org_memberships half of what AddOrgMember would do.
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, outsider, orgID)

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", outsider, orgID, teamID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (non-team-member); body=%s", rec.Code, rec.Body.String())
	}
}

func TestChannelsHandler_MemberGET_OK_WithRole(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-member-ok")
	member := pgtest.SeedUser(t, r.h, "chan-member-ok-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", member, orgID, teamID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got := decodeChannels(t, rec)
	if got.Role != "member" {
		t.Errorf("role = %q, want member", got.Role)
	}

	rec2 := httptest.NewRecorder()
	r.hdl.handleList(rec2, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID, nil))
	got2 := decodeChannels(t, rec2)
	if got2.Role != "admin" {
		t.Errorf("owner role = %q, want admin", got2.Role)
	}
}

func TestChannelsHandler_MemberPUT_Refused(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-member-put")
	member := pgtest.SeedUser(t, r.h, "chan-member-put-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", member, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (member can't PUT); body=%s", rec.Code, rec.Body.String())
	}
}

func TestChannelsHandler_TeamAdminPUT_OK(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-admin-put")
	r.listChannels(orgID, owner, "C1")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got := decodeChannels(t, rec)
	if len(got.Channels) != 1 || !got.Channels[0].Tracked {
		t.Fatalf("channels = %+v; want one tracked channel", got.Channels)
	}
	// The response names the channel's registry row: what tracking holds and
	// a slack:message handler's channel_in names.
	row, err := slackstore.FromStores(r.stor).Channels.ResolveSystem(t.Context(), orgID, "C1")
	if err != nil || row == nil {
		t.Fatalf("registry row for C1 = %+v, %v; want the one PUT registered", row, err)
	}
	if got.Channels[0].ID != row.ID {
		t.Errorf("channel id = %q; want the registry row %q", got.Channels[0].ID, row.ID)
	}
}

// TestChannelsHandler_PUT_RejectsMalformedEntries: every blank, malformed or
// repeated entry is a fault naming its element, reported together, and a
// missing channel_ids is one too; nothing is registered or tracked.
func TestChannelsHandler_PUT_RejectsMalformedEntries(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-put-shape")
	r.listChannels(orgID, owner, "C0SHAPE01")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"", "general", "C0SHAPE01", "C0SHAPE01", "D0DIRECT1"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	want := map[string]string{
		"channel_ids[0]": httpx.ReasonMissingField,
		"channel_ids[1]": httpx.ReasonInvalidField,
		"channel_ids[3]": httpx.ReasonInvalidField,
		"channel_ids[4]": httpx.ReasonInvalidField,
	}
	errs := decodeErrors(t, rec)
	got := map[string]string{}
	for _, e := range errs {
		got[e.Field] = e.Reason
	}
	if len(errs) != len(want) {
		t.Errorf("errors = %+v; want one per bad entry, %v", errs, want)
	}
	for field, reason := range want {
		if got[field] != reason {
			t.Errorf("%s: reason %q, want %q (errors %+v)", field, got[field], reason, errs)
		}
	}
	if row, _ := slackstore.FromStores(r.stor).Channels.ResolveSystem(t.Context(), orgID, "C0SHAPE01"); row != nil {
		t.Errorf("C0SHAPE01 registered as %+v by the refused save; want no row", row)
	}

	missing := httptest.NewRecorder()
	r.hdl.handlePut(missing, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{}))
	if errs := decodeErrors(t, missing); missing.Code != http.StatusBadRequest || len(errs) != 1 || errs[0].Field != "channel_ids" || errs[0].Reason != httpx.ReasonMissingField {
		t.Errorf("missing channel_ids: status %d, errors %+v; want 400 MISSING_FIELD channel_ids", missing.Code, errs)
	}
}

// TestChannelsHandler_PUT_AddsOnlyChannelsAWorkspaceAnswersFor: a channel the
// save adds must be listed by a connected workspace (or held by the registry
// under one); anything else is a 400 naming the entry and the whole save is
// refused. An accepted channel is registered under the workspace that listed
// it.
func TestChannelsHandler_PUT_AddsOnlyChannelsAWorkspaceAnswersFor(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-put-gate")

	none := httptest.NewRecorder()
	r.hdl.handlePut(none, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C0LISTED1"}}))
	if errs := decodeErrors(t, none); none.Code != http.StatusBadRequest || len(errs) != 1 || errs[0].Field != "channel_ids[0]" {
		t.Errorf("no workspace connected: status %d, errors %+v; want 400 naming channel_ids[0]", none.Code, errs)
	}

	r.listChannels(orgID, owner, "C0LISTED1")
	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C0LISTED1", "C0UNKNOWN"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if errs := decodeErrors(t, rec); len(errs) != 1 || errs[0].Field != "channel_ids[1]" || errs[0].Reason != httpx.ReasonInvalidField {
		t.Errorf("errors = %+v; want one INVALID_FIELD naming channel_ids[1]", errs)
	}
	registry := slackstore.FromStores(r.stor).Channels
	for _, id := range []string{"C0LISTED1", "C0UNKNOWN"} {
		if row, _ := registry.ResolveSystem(t.Context(), orgID, id); row != nil {
			t.Errorf("%s registered as %+v by the refused save; want no row", id, row)
		}
	}

	ok := httptest.NewRecorder()
	r.hdl.handlePut(ok, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C0LISTED1"}}))
	if ok.Code != http.StatusOK {
		t.Fatalf("listed channel: status = %d, want 200 (body=%s)", ok.Code, ok.Body.String())
	}
	if row, _ := registry.ResolveSystem(t.Context(), orgID, "C0LISTED1"); row == nil || row.WorkspaceID != "T0LISTED1" {
		t.Errorf("registered row = %+v; want it under the workspace that listed it, T0LISTED1", row)
	}
}

// TestChannelsHandler_PUT_LooksUpChannelsPastATruncatedListing: a listing
// that stopped at its cap says nothing about the channels past it, so the gate
// asks Slack about each one. A channel Slack knows is registered under the
// workspace that answered; one no workspace can see is a 400 naming it; a
// lookup that fails is a 502; and a save needing more lookups than
// channelLookupLimit is refused before any are made.
func TestChannelsHandler_PUT_LooksUpChannelsPastATruncatedListing(t *testing.T) {
	withLoweredCap(t, 1)
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-put-truncated")
	r.seedWorkspace(orgID, owner, "T0BIG0001", "xoxb-big")
	listed := []map[string]any{{"id": "C0LIST001", "name": "listed", "is_private": false}}
	r.fake.setChannels("xoxb-big", listed, listed)
	r.fake.setMoreAvailable("xoxb-big")
	r.fake.knowChannel("xoxb-big", "C0BEYOND1", "beyond")

	put := func(ids ...string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
			map[string]any{"channel_ids": ids}))
		return rec
	}

	if rec := put("C0BEYOND1"); rec.Code != http.StatusOK {
		t.Fatalf("channel past the cap that Slack knows: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if row, _ := slackstore.FromStores(r.stor).Channels.ResolveSystem(t.Context(), orgID, "C0BEYOND1"); row == nil || row.WorkspaceID != "T0BIG0001" || row.Name != "beyond" {
		t.Errorf("registered row = %+v; want it under T0BIG0001, named beyond", row)
	}

	rec := put("C0BEYOND1", "C0NOWHERE")
	if errs := decodeErrors(t, rec); rec.Code != http.StatusBadRequest || len(errs) != 1 || errs[0].Field != "channel_ids[1]" {
		t.Errorf("channel no workspace can see: status %d, errors %+v; want 400 naming channel_ids[1]", rec.Code, errs)
	}

	many := []string{"C0BEYOND1"}
	for i := 0; i <= channelLookupLimit; i++ {
		many = append(many, fmt.Sprintf("C0MANY%03d", i))
	}
	calls := r.fake.infoCallCount()
	rec = put(many...)
	if errs := decodeErrors(t, rec); rec.Code != http.StatusBadRequest || len(errs) != 1 || errs[0].Field != "channel_ids" || errs[0].Reason != httpx.ReasonOutOfRange {
		t.Errorf("%d lookups: status %d, errors %+v; want 400 OUT_OF_RANGE channel_ids", len(many)-1, rec.Code, errs)
	}
	if got := r.fake.infoCallCount(); got != calls {
		t.Errorf("conversations.info calls = %d after the refused save; want none (%d before)", got, calls)
	}

	r.fake.failInfo("xoxb-big", "internal_error")
	rec = put("C0BEYOND1", "C0OTHER01")
	if errs := decodeErrors(t, rec); rec.Code != http.StatusBadGateway || len(errs) != 1 || errs[0].Reason != httpx.ReasonUpstreamUnavailable {
		t.Errorf("failed lookup: status %d, errors %+v; want 502 UPSTREAM_UNAVAILABLE", rec.Code, errs)
	}
}

// TestChannelsHandler_PUTAndPrimary_RetiredIDActOnTheCurrentOne: a save
// naming a channel by the id Slack moved it away from (a settings page left
// open across the share) tracks the channel's current id and keeps the moved
// row, and a primary reassignment addressed by the old id lands on it too.
func TestChannelsHandler_PUTAndPrimary_RetiredIDActOnTheCurrentOne(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-retired-put")
	r.listChannels(orgID, owner, "G0PUT0001")

	first := httptest.NewRecorder()
	r.hdl.handlePut(first, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"G0PUT0001"}}))
	if first.Code != http.StatusOK {
		t.Fatalf("setup PUT status = %d (body=%s)", first.Code, first.Body.String())
	}
	if _, err := slackstore.FromStores(r.stor).Channels.MoveSystem(t.Context(), orgID, "G0PUT0001", "C0PUT0001"); err != nil {
		t.Fatalf("MoveSystem: %v", err)
	}

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"G0PUT0001", "C0PUT0001"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var tracked []string
	for _, c := range decodeChannels(t, rec).Channels {
		if c.Tracked {
			tracked = append(tracked, c.ChannelID)
		}
	}
	if len(tracked) != 1 || tracked[0] != "C0PUT0001" {
		t.Errorf("tracked channels = %v; want only the current id C0PUT0001", tracked)
	}

	primary := httptest.NewRecorder()
	r.hdl.handlePrimary(primary, r.reqPrimary(owner, orgID, "G0PUT0001", map[string]string{"team_id": teamID}))
	if primary.Code != http.StatusOK {
		t.Fatalf("primary by the old id: status = %d, want 200 (body=%s)", primary.Code, primary.Body.String())
	}
}

func TestChannelsHandler_ArchivedTeamPUT_Refused(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-archived")
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, teamID)

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (archived team); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), httpx.ReasonTeamArchived) {
		t.Errorf("403 body missing the TEAM_ARCHIVED reason; body=%s", rec.Body.String())
	}
}

func TestChannelsHandler_Primary_NonOrgAdmin_Refused(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-primary-nonadmin")
	r.listChannels(orgID, owner, "C1")
	member := pgtest.SeedUser(t, r.h, "chan-primary-nonadmin-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")

	put := httptest.NewRecorder()
	r.hdl.handlePut(put, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if put.Code != http.StatusOK {
		t.Fatalf("setup PUT status = %d (body=%s)", put.Code, put.Body.String())
	}

	rec := httptest.NewRecorder()
	r.hdl.handlePrimary(rec, r.reqPrimary(member, orgID, "C1", map[string]string{"team_id": teamID}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-org-admin); body=%s", rec.Code, rec.Body.String())
	}
}

// TestChannelsHandler_Primary_ArchivedDestinationTeam_Refused: an archived
// team stays a tracker of a channel after archiving (tracking changes are
// forward-only — archiving never untracks), so SetPrimarySystem's own
// "does this team track the channel" check alone would let reassignment
// succeed. The handler must independently refuse an archived destination
// team — read-only-and-vanished (TFAC-448) may not become a channel's live
// routing owner — mirroring handlePut's destination-team guard.
func TestChannelsHandler_Primary_ArchivedDestinationTeam_Refused(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1 := pgtest.SeedOrgWithUser(t, r.h, "chan-primary-archived")
	r.listChannels(orgID, owner, "C1")
	team2 := pgtest.SeedTeam(t, r.h, orgID, "team2")
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, owner, team2)

	for _, teamID := range []string{team1, team2} {
		rec := httptest.NewRecorder()
		r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
			map[string]any{"channel_ids": []string{"C1"}}))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT(%s) status = %d (body=%s)", teamID, rec.Code, rec.Body.String())
		}
	}
	// team1 tracked first (primary by default); reassign to team2 so team1
	// is a non-primary tracker — the scenario the archived-team guard must
	// still refuse even though team1 isn't currently primary.
	reassign := httptest.NewRecorder()
	r.hdl.handlePrimary(reassign, r.reqPrimary(owner, orgID, "C1", map[string]string{"team_id": team2}))
	if reassign.Code != http.StatusOK {
		t.Fatalf("reassign to team2 status = %d (body=%s)", reassign.Code, reassign.Body.String())
	}

	pgtest.MustExec(t, r.h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, team1)

	rec := httptest.NewRecorder()
	r.hdl.handlePrimary(rec, r.reqPrimary(owner, orgID, "C1", map[string]string{"team_id": team1}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (archived destination team); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), httpx.ReasonTeamArchived) {
		t.Errorf("403 body missing the TEAM_ARCHIVED reason; body=%s", rec.Body.String())
	}

	// The primary must still be team2 — the refused reassignment must not
	// have partially applied.
	list := httptest.NewRecorder()
	r.hdl.handleList(list, r.req(http.MethodGet, "/api/slack/teams/"+team2+"/channels", owner, orgID, team2, nil))
	got := decodeChannels(t, list)
	if len(got.Channels) != 1 || !got.Channels[0].IsPrimary {
		t.Fatalf("team2's view after the refused reassignment = %+v; want still primary", got.Channels)
	}
}

// ---------- blueprint seeding ----------

func TestChannelsHandler_FirstTrack_SeedsDefaultBlueprint(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-seed")
	r.listChannels(orgID, owner, "C1")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var handlers []domain.EventHandler
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		handlers, _, e = tx.EventHandlers.List(t.Context(), orgID,
			db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger, TeamID: teamID}, db.ListOpts{Limit: 200})
		return e
	}); err != nil {
		t.Fatalf("list event handlers: %v", err)
	}
	var mention *domain.EventHandler
	for i := range handlers {
		if handlers[i].EventType == domain.EventSlackMessage {
			mention = &handlers[i]
		}
	}
	if mention == nil {
		t.Fatalf("no slack:message trigger seeded; handlers=%+v", handlers)
	}
	if !mention.Enabled {
		t.Errorf("seeded trigger Enabled = false, want true")
	}
	// jsonb round-trips through Postgres's own canonical formatting (e.g. a
	// space after ":"), so compare decoded rather than as an exact string.
	if mention.ScopePredicateJSON == nil {
		t.Fatal("predicate not set, want {\"channel_in\":[]}")
	}
	var predicate struct {
		ChannelIn []string `json:"channel_in"`
	}
	if err := json.Unmarshal([]byte(*mention.ScopePredicateJSON), &predicate); err != nil {
		t.Fatalf("decode predicate %q: %v", *mention.ScopePredicateJSON, err)
	}
	if len(predicate.ChannelIn) != 0 {
		t.Errorf("predicate.channel_in = %v, want empty (match-all)", predicate.ChannelIn)
	}
	if mention.Source != domain.EventHandlerSourceUser {
		t.Errorf("source = %q, want user", mention.Source)
	}

	var blueprint *domain.Blueprint
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		blueprint, e = tx.Blueprints.Get(t.Context(), orgID, mention.BlueprintID)
		return e
	}); err != nil || blueprint == nil {
		t.Fatalf("get blueprint: %v (blueprint=%v)", err, blueprint)
	}
	var steps []domain.BlueprintStep
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		steps, e = tx.Blueprints.ListSteps(t.Context(), orgID, blueprint.ID)
		return e
	}); err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("blueprint steps = %d, want 1", len(steps))
	}
}

func TestChannelsHandler_SecondPUT_NoDuplicateTrigger(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-nodup")
	r.listChannels(orgID, owner, "C1", "C2")

	first := httptest.NewRecorder()
	r.hdl.handlePut(first, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if first.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d (body=%s)", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	r.hdl.handlePut(second, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1", "C2"}}))
	if second.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d (body=%s)", second.Code, second.Body.String())
	}

	var handlers []domain.EventHandler
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		handlers, _, e = tx.EventHandlers.List(t.Context(), orgID,
			db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger, TeamID: teamID}, db.ListOpts{Limit: 200})
		return e
	}); err != nil {
		t.Fatalf("list event handlers: %v", err)
	}
	count := 0
	for _, h := range handlers {
		if h.EventType == domain.EventSlackMessage {
			count++
		}
	}
	if count != 1 {
		t.Errorf("slack:message triggers after second PUT = %d, want 1 (no duplicate)", count)
	}
}

func TestChannelsHandler_PreexistingTrigger_NoSeed(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-preexisting")
	r.listChannels(orgID, owner, "C1")

	// Team already configured its own slack:message trigger before ever
	// tracking a channel.
	promptID := "00000000-0000-0000-0000-0000000000c1"
	blueprintID := "00000000-0000-0000-0000-0000000000c2"
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		if _, err := tx.Prompts.Create(t.Context(), orgID, teamID, domain.Prompt{ID: promptID, Name: "custom", Body: "custom body", Source: "user"}); err != nil {
			return err
		}
		if _, err := tx.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{ID: blueprintID, Name: "custom", Source: "user"}); err != nil {
			return err
		}
		if _, err := tx.Blueprints.ReplaceSteps(t.Context(), orgID, blueprintID, []string{promptID}, []string{""}); err != nil {
			return err
		}
		threshold, minAutonomy := 5, 0.2
		_, e := tx.EventHandlers.Create(t.Context(), orgID, teamID, domain.EventHandler{
			ID: "00000000-0000-0000-0000-0000000000c3", Kind: domain.EventHandlerKindTrigger,
			EventType: domain.EventSlackMessage, Enabled: true, Source: domain.EventHandlerSourceUser,
			BlueprintID: blueprintID, TriggerType: domain.TriggerTypeEvent,
			BreakerThreshold: &threshold, MinAutonomySuitability: &minAutonomy,
		})
		return e
	}); err != nil {
		t.Fatalf("seed pre-existing trigger: %v", err)
	}

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	var handlers []domain.EventHandler
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		handlers, _, e = tx.EventHandlers.List(t.Context(), orgID,
			db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger, TeamID: teamID}, db.ListOpts{Limit: 200})
		return e
	}); err != nil {
		t.Fatalf("list event handlers: %v", err)
	}
	count := 0
	for _, h := range handlers {
		if h.EventType == domain.EventSlackMessage {
			count++
		}
	}
	if count != 1 {
		t.Errorf("slack:message triggers = %d, want 1 (pre-existing kept, no default seeded)", count)
	}
}

func TestChannelsHandler_DefaultDeleted_NoReseed(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-deleted-default")
	r.listChannels(orgID, owner, "C1", "C2")

	first := httptest.NewRecorder()
	r.hdl.handlePut(first, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if first.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d (body=%s)", first.Code, first.Body.String())
	}

	var seeded *domain.EventHandler
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		handlers, _, e := tx.EventHandlers.List(t.Context(), orgID,
			db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger, TeamID: teamID}, db.ListOpts{Limit: 200})
		if e != nil {
			return e
		}
		for i := range handlers {
			if handlers[i].EventType == domain.EventSlackMessage {
				seeded = &handlers[i]
			}
		}
		return nil
	}); err != nil || seeded == nil {
		t.Fatalf("find seeded trigger: %v (seeded=%v)", err, seeded)
	}

	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		return tx.EventHandlers.Delete(t.Context(), orgID, seeded.ID)
	}); err != nil {
		t.Fatalf("delete seeded trigger: %v", err)
	}

	// The team is still tracking C1 (unchanged), so the prior set is
	// non-empty — the empty->non-empty gate must not re-fire even though
	// there's a new channel added.
	second := httptest.NewRecorder()
	r.hdl.handlePut(second, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1", "C2"}}))
	if second.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d (body=%s)", second.Code, second.Body.String())
	}

	var handlers []domain.EventHandler
	if err := r.stor.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		var e error
		handlers, _, e = tx.EventHandlers.List(t.Context(), orgID,
			db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger, TeamID: teamID}, db.ListOpts{Limit: 200})
		return e
	}); err != nil {
		t.Fatalf("list event handlers: %v", err)
	}
	count := 0
	for _, h := range handlers {
		if h.EventType == domain.EventSlackMessage {
			count++
		}
	}
	if count != 0 {
		t.Errorf("slack:message triggers after delete + re-PUT = %d, want 0 (no resurrection)", count)
	}
}

// ---------- primary lifecycle ----------

func TestChannelsHandler_Primary_FirstTrackerDefault(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-primary-first")
	r.listChannels(orgID, owner, "C1")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	got := decodeChannels(t, rec)
	if len(got.Channels) != 1 || !got.Channels[0].IsPrimary {
		t.Fatalf("channels = %+v; want the first tracker primary", got.Channels)
	}
}

func TestChannelsHandler_Primary_SecondTeamNonPrimary_ThenUntrackPromotesOldest(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1 := pgtest.SeedOrgWithUser(t, r.h, "chan-primary-second")
	r.listChannels(orgID, owner, "C1")
	team2 := pgtest.SeedTeam(t, r.h, orgID, "team2")
	admin2 := pgtest.SeedUser(t, r.h, "chan-primary-second-admin2")
	pgtest.AddOrgMember(t, r.h, admin2, orgID, team2, "member", "admin")

	rec1 := httptest.NewRecorder()
	r.hdl.handlePut(rec1, r.req(http.MethodPut, "/api/slack/teams/"+team1+"/channels", owner, orgID, team1,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec1.Code != http.StatusOK {
		t.Fatalf("team1 PUT status = %d (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	r.hdl.handlePut(rec2, r.req(http.MethodPut, "/api/slack/teams/"+team2+"/channels", admin2, orgID, team2,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec2.Code != http.StatusOK {
		t.Fatalf("team2 PUT status = %d (body=%s)", rec2.Code, rec2.Body.String())
	}
	got2 := decodeChannels(t, rec2)
	if len(got2.Channels) != 1 || got2.Channels[0].IsPrimary {
		t.Fatalf("team2's view = %+v; want tracked but NOT primary (team1 tracked first)", got2.Channels)
	}

	// team1 untracks everything -> team2 (the oldest remaining tracker) is promoted.
	rec3 := httptest.NewRecorder()
	r.hdl.handlePut(rec3, r.req(http.MethodPut, "/api/slack/teams/"+team1+"/channels", owner, orgID, team1,
		map[string]any{"channel_ids": []string{}}))
	if rec3.Code != http.StatusOK {
		t.Fatalf("team1 untrack status = %d (body=%s)", rec3.Code, rec3.Body.String())
	}

	rec4 := httptest.NewRecorder()
	r.hdl.handleList(rec4, r.req(http.MethodGet, "/api/slack/teams/"+team2+"/channels", admin2, orgID, team2, nil))
	got4 := decodeChannels(t, rec4)
	if len(got4.Channels) != 1 || !got4.Channels[0].IsPrimary {
		t.Fatalf("team2's view after team1 untracks = %+v; want promoted to primary", got4.Channels)
	}
}

func TestChannelsHandler_Primary_ReassignHappyPath(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1 := pgtest.SeedOrgWithUser(t, r.h, "chan-reassign")
	r.listChannels(orgID, owner, "C1")
	team2 := pgtest.SeedTeam(t, r.h, orgID, "team2")
	// owner already has an org_memberships row (SeedOrgWithUser) — just add
	// the team2 membership, not a second org-level row.
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, owner, team2)

	for _, teamID := range []string{team1, team2} {
		rec := httptest.NewRecorder()
		r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
			map[string]any{"channel_ids": []string{"C1"}}))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT(%s) status = %d (body=%s)", teamID, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	r.hdl.handlePrimary(rec, r.reqPrimary(owner, orgID, "C1", map[string]string{"team_id": team2}))
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	list := httptest.NewRecorder()
	r.hdl.handleList(list, r.req(http.MethodGet, "/api/slack/teams/"+team2+"/channels", owner, orgID, team2, nil))
	got := decodeChannels(t, list)
	if len(got.Channels) != 1 || !got.Channels[0].IsPrimary {
		t.Fatalf("team2's view after reassign = %+v; want primary", got.Channels)
	}
}

func TestChannelsHandler_Primary_NonTrackingTeam_400(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, team1 := pgtest.SeedOrgWithUser(t, r.h, "chan-primary-nontrack")
	r.listChannels(orgID, owner, "C1")
	team2 := pgtest.SeedTeam(t, r.h, orgID, "team2")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+team1+"/channels", owner, orgID, team1,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	primary := httptest.NewRecorder()
	r.hdl.handlePrimary(primary, r.reqPrimary(owner, orgID, "C1", map[string]string{"team_id": team2}))
	if primary.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (team2 never tracked C1); body=%s", primary.Code, primary.Body.String())
	}
}

// ---------- merge / dedup ----------

func TestChannelsHandler_Merge_DedupPrecedence(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-merge")
	r.seedWorkspace(orgID, owner, "T0MERGE1", "xoxb-merge")
	listedC1 := []map[string]any{{"id": "C1", "name": "eng", "is_private": false}}
	r.fake.setChannels("xoxb-merge", listedC1, listedC1)

	// C1: tracked by this team. C2: an unclaimed sighting (registry only).
	// C3: a live Slack candidate only (never tracked or sighted).
	put := httptest.NewRecorder()
	r.hdl.handlePut(put, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body=%s)", put.Code, put.Body.String())
	}

	admin := slackstore.FromStores(r.stor)
	if _, err := admin.Channels.UpsertSightingSystem(t.Context(), orgID, "T0MERGE1", "C2", time.Now()); err != nil {
		t.Fatalf("seed C2 sighting: %v", err)
	}

	r.fake.setChannels("xoxb-merge",
		[]map[string]any{{"id": "C3", "name": "public-only", "is_private": false}},
		nil,
	)

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := decodeChannels(t, rec)
	byID := map[string]channelView{}
	for _, c := range got.Channels {
		byID[c.ChannelID] = c
	}
	if c1, ok := byID["C1"]; !ok || c1.Source != "tracked" {
		t.Errorf("C1 = %+v; want source=tracked", c1)
	}
	if c2, ok := byID["C2"]; !ok || c2.Source != "sighting" {
		t.Errorf("C2 = %+v; want source=sighting", c2)
	}
	if c3, ok := byID["C3"]; !ok || c3.Source != "slack" {
		t.Errorf("C3 = %+v; want source=slack", c3)
	}
}

// TestChannelsHandler_Merge_SlackUnreachable_Warning: with Slack's listing
// down, a save keeping the channels the team already tracks still lands, since
// nothing it adds needs asking Slack about; a save adding a channel is a 502
// and changes nothing; and GET still lists the tracked channel, with a warning.
func TestChannelsHandler_Merge_SlackUnreachable_Warning(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-unreachable")
	r.seedWorkspace(orgID, owner, "T0DOWN1", "xoxb-down")
	listedC1 := []map[string]any{{"id": "C1", "name": "eng", "is_private": false}}
	r.fake.setChannels("xoxb-down", listedC1, listedC1)

	put := httptest.NewRecorder()
	r.hdl.handlePut(put, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body=%s)", put.Code, put.Body.String())
	}
	r.fake.failConversationsList("xoxb-down")

	keep := httptest.NewRecorder()
	r.hdl.handlePut(keep, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if keep.Code != http.StatusOK {
		t.Fatalf("PUT keeping the tracked set during the outage: status = %d, want 200 (body=%s)", keep.Code, keep.Body.String())
	}

	add := httptest.NewRecorder()
	r.hdl.handlePut(add, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1", "C2"}}))
	if add.Code != http.StatusBadGateway {
		t.Fatalf("PUT adding a channel during the outage: status = %d, want 502 (body=%s)", add.Code, add.Body.String())
	}
	if errs := decodeErrors(t, add); len(errs) != 1 || errs[0].Reason != httpx.ReasonUpstreamUnavailable {
		t.Errorf("errors = %+v; want one UPSTREAM_UNAVAILABLE", errs)
	}
	if row, _ := slackstore.FromStores(r.stor).Channels.ResolveSystem(t.Context(), orgID, "C2"); row != nil {
		t.Errorf("C2 registered as %+v by the refused save; want no row", row)
	}

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID, nil))
	got := decodeChannels(t, rec)
	if len(got.Channels) != 1 || got.Channels[0].ChannelID != "C1" {
		t.Errorf("channels = %+v; want tracked C1 still returned despite the Slack outage", got.Channels)
	}
	foundWarning := false
	for _, w := range got.Warnings {
		if w.Code == "slack_unreachable" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("warnings = %+v; want slack_unreachable", got.Warnings)
	}
}

// TestChannelsHandler_Merge_ChannelsTruncated_Warning: the live-candidate
// enumeration hits slackConversationsListCap while Slack still had more
// pages — GET must surface a distinct warning (never silently under-report
// the live view as if it were exhaustive).
func TestChannelsHandler_Merge_ChannelsTruncated_Warning(t *testing.T) {
	withLoweredCap(t, 1)
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-truncated")
	r.seedWorkspace(orgID, owner, "T0TRUNC1", "xoxb-trunc")
	r.fake.setChannels("xoxb-trunc",
		[]map[string]any{
			{"id": "C1", "name": "one", "is_private": false},
			{"id": "C2", "name": "two", "is_private": false},
		},
		nil,
	)
	r.fake.setMoreAvailable("xoxb-trunc")

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.req(http.MethodGet, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := decodeChannels(t, rec)
	found := false
	for _, w := range got.Warnings {
		if w.Code == "slack_channels_truncated" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %+v; want slack_channels_truncated", got.Warnings)
	}
}

// ---------- auto-join ----------

func TestChannelsHandler_AutoJoin_PublicNonMember(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-autojoin")
	r.seedWorkspace(orgID, owner, "T0JOIN1", "xoxb-join")
	r.fake.setChannels("xoxb-join",
		[]map[string]any{{"id": "C1", "name": "eng", "is_private": false}},
		nil, // bot is not a member yet
	)

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if r.fake.joinCallCount() != 1 {
		t.Errorf("conversations.join calls = %d, want 1", r.fake.joinCallCount())
	}
	got := decodeChannels(t, rec)
	for _, w := range got.Warnings {
		if w.ChannelID == "C1" {
			t.Errorf("warnings = %+v; want none (join succeeded)", got.Warnings)
		}
	}
}

// TestChannelsHandler_AutoJoin_Private_InviteRequiredWarning: a private
// channel the bot was mentioned in (so the registry holds it under a connected
// workspace) and has since left appears in neither conversations.list nor
// users.conversations, both scoped to the bot's own visibility. The gate
// accepts it on the registry row, the handler still attempts
// conversations.join, and Slack's "channel_not_found" (what it returns for a
// private channel the bot cannot see) is classified as invite_required.
func TestChannelsHandler_AutoJoin_Private_InviteRequiredWarning(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-private")
	r.seedWorkspace(orgID, owner, "T0PRIV1", "xoxb-priv")
	if _, err := slackstore.FromStores(r.stor).Channels.UpsertSightingSystem(t.Context(), orgID, "T0PRIV1", "C1", time.Now()); err != nil {
		t.Fatalf("seed C1 sighting: %v", err)
	}
	r.fake.setChannels("xoxb-priv", nil, nil) // C1 unknown to the live view entirely
	r.fake.failJoinWithCode("C1", "channel_not_found")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if r.fake.joinCallCount() != 1 {
		t.Errorf("conversations.join calls = %d, want 1 (attempted even though unknown to the live view)", r.fake.joinCallCount())
	}
	got := decodeChannels(t, rec)
	found := false
	for _, w := range got.Warnings {
		if w.ChannelID == "C1" && w.Reason == "invite_required" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %+v; want invite_required for C1", got.Warnings)
	}
	if len(got.Channels) != 1 || !got.Channels[0].Tracked {
		t.Fatalf("channels = %+v; want the tracking row persisted despite invite_required", got.Channels)
	}
}

func TestChannelsHandler_AutoJoin_JoinFailure_Warning_RowStillPersisted(t *testing.T) {
	r := newSlackChannelsRig(t)
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "chan-joinfail")
	r.seedWorkspace(orgID, owner, "T0JOINFAIL", "xoxb-joinfail")
	r.fake.setChannels("xoxb-joinfail",
		[]map[string]any{{"id": "C1", "name": "eng", "is_private": false}},
		nil,
	)
	r.fake.failJoin("C1")

	rec := httptest.NewRecorder()
	r.hdl.handlePut(rec, r.req(http.MethodPut, "/api/slack/teams/"+teamID+"/channels", owner, orgID, teamID,
		map[string]any{"channel_ids": []string{"C1"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := decodeChannels(t, rec)
	if len(got.Channels) != 1 || !got.Channels[0].Tracked {
		t.Fatalf("channels = %+v; want the tracking row persisted despite the join failure", got.Channels)
	}
	foundWarning := false
	for _, w := range got.Warnings {
		if w.ChannelID == "C1" && w.Reason == "join_failed" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("warnings = %+v; want join_failed for C1", got.Warnings)
	}
}
