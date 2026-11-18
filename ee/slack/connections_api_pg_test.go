package slack

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// seedConnection writes an active Events API connection of the org's own app
// in workspaceID, named workspaceName, as userID (an admin of orgID).
func (r *slackWorkspaceRig) seedConnection(orgID, userID, workspaceID, workspaceName, apiAppID string) {
	r.t.Helper()
	keys := integrations.SlackConnectionKeysFor(workspaceID, apiAppID)
	if err := r.stores.Tx.WithTx(r.t.Context(), orgID, userID, func(tx db.TxStores) error {
		return upsertActiveConnection(r.t.Context(), tx, slackstore.Connection{
			WorkspaceID:        workspaceID,
			APIAppID:           apiAppID,
			OrgID:              orgID,
			WorkspaceName:      workspaceName,
			Transport:          transportEventsAPI,
			BotUserID:          "U0BOT",
			BotTokenRef:        keys.BotToken,
			SigningSecretRef:   keys.SigningSecret,
			RegisteredByUserID: userID,
		})
	}); err != nil {
		r.t.Fatalf("seed connection %s/%s: %v", workspaceID, apiAppID, err)
	}
}

// listPage calls the list route as callerID with body.
func (r *slackWorkspaceRig) listPage(callerID, orgID string, body map[string]any) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.listReq(callerID, orgID, body))
	return rec
}

// TestConnectionsAPI_AddressedByPathOrg: a user in two orgs reads the
// connections of the org that is not their active one, by naming it in the
// path.
func TestConnectionsAPI_AddressedByPathOrg(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgA, user, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-path-a")
	orgB, ownerB, teamB := pgtest.SeedOrgWithUser(t, r.h, "slack-path-b")
	pgtest.AddOrgMember(t, r.h, user, orgB, teamB, "member", "member")
	r.seedConnection(orgB, ownerB, "T0PATHB", "Bravo", "A0PATHB")

	rec := httptest.NewRecorder()
	r.hdl.handleList(rec, r.reqActive(http.MethodPost, connectionsPath(orgB)+"/list", user, orgA, orgB, "", map[string]any{}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got := decodeBody[connectionList](t, rec).Items
	if len(got) != 1 || got[0].WorkspaceID != "T0PATHB" {
		t.Fatalf("org B's connections read from org A = %+v; want org B's one", got)
	}

	one := httptest.NewRecorder()
	req := r.reqActive(http.MethodGet, connectionsPath(orgB)+"/T0PATHB/A0PATHB", user, orgA, orgB, "T0PATHB", nil)
	req.SetPathValue("api_app_id", "A0PATHB")
	r.hdl.handleGet(one, req)
	if one.Code != http.StatusOK {
		t.Fatalf("point read status = %d, want 200 (body=%s)", one.Code, one.Body.String())
	}
}

// TestConnectionsAPI_TokenSealedToItsOrg: an API token minted for one org
// gets 404 on another org's paths, though its user is a member of both.
func TestConnectionsAPI_TokenSealedToItsOrg(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgA, user, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-token-a")
	orgB, ownerB, teamB := pgtest.SeedOrgWithUser(t, r.h, "slack-token-b")
	pgtest.AddOrgMember(t, r.h, user, orgB, teamB, "admin", "admin")
	r.seedConnection(orgB, ownerB, "T0TOKB", "Bravo", "A0TOKB")

	withToken := func(req *http.Request) *http.Request {
		return req.WithContext(httpx.WithTokenAuth(req.Context(), &httpx.TokenAuth{TokenID: "tok", OrgID: orgA}))
	}
	cases := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"list", r.hdl.handleList, r.listReq(user, orgB, map[string]any{})},
		{"get", r.hdl.handleGet, r.connectionReq(http.MethodGet, user, orgB, "T0TOKB", "A0TOKB")},
		{"import", r.hdl.handleConnect, r.importReq(user, orgB, map[string]string{"bot_token": "xoxb-tok"})},
		{"delete", r.hdl.handleDelete, r.reqDelete(user, orgB, "T0TOKB", "A0TOKB")},
		{"manifest", r.hdl.handleManifest, r.manifestReq(user, orgB, transportSocket)},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		tc.fn(rec, withToken(tc.req))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with org A's token on org B = %d, want 404 (body=%s)", tc.name, rec.Code, rec.Body.String())
		}
	}

	own := httptest.NewRecorder()
	r.hdl.handleList(own, withToken(r.listReq(user, orgA, map[string]any{})))
	if own.Code != http.StatusOK {
		t.Fatalf("list with org A's token on org A = %d, want 200 (body=%s)", own.Code, own.Body.String())
	}
}

// TestConnectionsAPI_OrgGate: someone outside the org gets 404 from every
// route; a member reads but cannot import, delete or read the manifest.
func TestConnectionsAPI_OrgGate(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, r.h, "slack-orggate")
	member := pgtest.SeedUser(t, r.h, "slack-orggate-member")
	pgtest.AddOrgMember(t, r.h, member, orgID, teamID, "member", "member")
	_, outsider, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-orggate-outsider")
	r.seedConnection(orgID, owner, "T0GATE", "Acme", "A0GATE")

	type route struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		req  func(caller string) *http.Request
	}
	reads := []route{
		{"list", r.hdl.handleList, func(c string) *http.Request { return r.listReq(c, orgID, map[string]any{}) }},
		{"get", r.hdl.handleGet, func(c string) *http.Request {
			return r.connectionReq(http.MethodGet, c, orgID, "T0GATE", "A0GATE")
		}},
	}
	adminOnly := []route{
		{"import", r.hdl.handleConnect, func(c string) *http.Request {
			return r.importReq(c, orgID, map[string]string{"bot_token": "xoxb-gate"})
		}},
		{"delete", r.hdl.handleDelete, func(c string) *http.Request { return r.reqDelete(c, orgID, "T0GATE", "A0GATE") }},
		{"manifest", r.hdl.handleManifest, func(c string) *http.Request { return r.manifestReq(c, orgID, transportSocket) }},
	}
	for _, rt := range append(append([]route{}, reads...), adminOnly...) {
		rec := httptest.NewRecorder()
		rt.fn(rec, rt.req(outsider))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s as a non-member = %d, want 404 (body=%s)", rt.name, rec.Code, rec.Body.String())
		}
	}
	for _, rt := range reads {
		rec := httptest.NewRecorder()
		rt.fn(rec, rt.req(member))
		if rec.Code != http.StatusOK {
			t.Errorf("%s as a member = %d, want 200 (body=%s)", rt.name, rec.Code, rec.Body.String())
		}
	}
	for _, rt := range adminOnly {
		rec := httptest.NewRecorder()
		rt.fn(rec, rt.req(member))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s as a member = %d, want 403 (body=%s)", rt.name, rec.Code, rec.Body.String())
		}
	}
	if r.fake.authTestHits != 0 {
		t.Errorf("auth.test called %d times; want 0 (every import refused at the gate)", r.fake.authTestHits)
	}
	if got := r.listConnections(owner, orgID); len(got) != 1 {
		t.Errorf("connections after the refusals = %+v; want the seeded one untouched", got)
	}
}

// TestConnectionsAPI_ListPages: the list is ordered by workspace name, then
// workspace id, then app id; page_size 1 walks every connection once, in that
// order; page_size 0 answers only the total; an out-of-range page_size is 400.
func TestConnectionsAPI_ListPages(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-list-pages")
	r.seedConnection(orgID, owner, "T1", "Zeta", "A0Z")
	r.seedConnection(orgID, owner, "T5", "Beta", "A0B")
	r.seedConnection(orgID, owner, "T3", "Beta", "A0C")
	r.seedConnection(orgID, owner, "T9", "Acme", "A0NEW")
	insertInactiveConnection(t, r.h, r.stores, orgID, owner, "T9", "A0OLD", "")
	want := []string{"T9/A0NEW", "T9/A0OLD", "T3/A0C", "T5/A0B", "T1/A0Z"}

	var walked []string
	token := ""
	for page := 0; page <= len(want); page++ {
		body := map[string]any{"page_size": 1}
		if token != "" {
			body["page_token"] = token
		}
		rec := r.listPage(owner, orgID, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d (body=%s)", page, rec.Code, rec.Body.String())
		}
		got := decodeBody[connectionList](t, rec)
		if got.TotalCount == nil || *got.TotalCount != len(want) {
			t.Fatalf("page %d total_count = %v; want %d", page, got.TotalCount, len(want))
		}
		for _, c := range got.Items {
			walked = append(walked, c.WorkspaceID+"/"+c.APIAppID)
		}
		token = got.NextPageToken
		if token == "" {
			break
		}
	}
	if !reflect.DeepEqual(walked, want) {
		t.Fatalf("walk with page_size 1 = %v; want %v", walked, want)
	}

	count := r.listPage(owner, orgID, map[string]any{"page_size": 0})
	if count.Code != http.StatusOK {
		t.Fatalf("page_size 0 status = %d (body=%s)", count.Code, count.Body.String())
	}
	counted := decodeBody[connectionList](t, count)
	if len(counted.Items) != 0 || counted.TotalCount == nil || *counted.TotalCount != len(want) || counted.NextPageToken != "" {
		t.Fatalf("page_size 0 = %+v; want no items and total_count %d", counted, len(want))
	}

	for _, size := range []int{-1, 201} {
		rec := r.listPage(owner, orgID, map[string]any{"page_size": size})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("page_size %d status = %d, want 400 (body=%s)", size, rec.Code, rec.Body.String())
		}
		if errs := decodeErrors(t, rec); len(errs) != 1 || errs[0].Reason != httpx.ReasonOutOfRange {
			t.Errorf("page_size %d errors = %+v; want one OUT_OF_RANGE", size, errs)
		}
	}

	unknown := r.listPage(owner, orgID, map[string]any{"workspace_id": "T1"})
	if unknown.Code != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, want 400 (body=%s)", unknown.Code, unknown.Body.String())
	}
}

// TestConnectionsAPI_PointRead: the point read answers the item the list
// does, and 404 for a pair the org has no connection for. A deployment
// connection's transport is null.
func TestConnectionsAPI_PointRead(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-point-read")
	r.seedConnection(orgID, owner, "T0READ", "Acme", "A0READ")
	if err := r.stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		return upsertActiveConnection(t.Context(), tx, slackstore.Connection{
			WorkspaceID: "T0DEPLOY", APIAppID: "A0DEPLOY", OrgID: orgID, Source: slackstore.SourceDeployment,
			WorkspaceName: "Deployed", BotUserID: "U0BOT",
			BotTokenRef: integrations.SlackConnectionKeysFor("T0DEPLOY", "A0DEPLOY").BotToken,
		})
	}); err != nil {
		t.Fatalf("seed deployment connection: %v", err)
	}

	listed := r.listConnections(owner, orgID)
	if len(listed) != 2 {
		t.Fatalf("list = %+v; want both connections", listed)
	}
	for _, item := range listed {
		rec := httptest.NewRecorder()
		r.hdl.handleGet(rec, r.connectionReq(http.MethodGet, owner, orgID, item.WorkspaceID, item.APIAppID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s/%s status = %d (body=%s)", item.WorkspaceID, item.APIAppID, rec.Code, rec.Body.String())
		}
		if got := decodeBody[workspaceView](t, rec); !reflect.DeepEqual(got, item) {
			t.Errorf("GET %s/%s = %+v; want the list's item %+v", item.WorkspaceID, item.APIAppID, got, item)
		}
		if item.Source == slackstore.SourceDeployment && !strings.Contains(rec.Body.String(), `"transport":null`) {
			t.Errorf("deployment connection body = %s; want transport null", rec.Body.String())
		}
	}

	for _, pair := range [][2]string{{"T0READ", "A0OTHER"}, {"T0NOPE", "A0READ"}} {
		rec := httptest.NewRecorder()
		r.hdl.handleGet(rec, r.connectionReq(http.MethodGet, owner, orgID, pair[0], pair[1]))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s/%s status = %d, want 404 (body=%s)", pair[0], pair[1], rec.Code, rec.Body.String())
		}
	}
}

// TestConnectionsAPI_DeleteActiveLeavesSiblingInactive: deleting a
// workspace's active connection leaves its inactive sibling inactive, so the
// workspace has no active connection.
func TestConnectionsAPI_DeleteActiveLeavesSiblingInactive(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delete-active")
	r.seedConnection(orgID, owner, "T0SWITCH", "Acme", "A0ACTIVE")
	insertInactiveConnection(t, r.h, r.stores, orgID, owner, "T0SWITCH", "A0INACTIVE", "xoxb-inactive")

	rec := httptest.NewRecorder()
	r.hdl.handleDelete(rec, r.reqDelete(owner, orgID, "T0SWITCH", "A0ACTIVE"))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	got := r.listConnections(owner, orgID)
	if len(got) != 1 || got[0].APIAppID != "A0INACTIVE" || got[0].Active {
		t.Fatalf("connections after deleting the active one = %+v; want only the inactive sibling, still inactive", got)
	}
	active, err := slackstore.FromStores(r.stores).Connections.ActiveForWorkspaceSystem(t.Context(), orgID, "T0SWITCH")
	if err != nil || active != nil {
		t.Fatalf("active connection in the workspace = %+v (%v); want none", active, err)
	}
	if got := r.secretValue(orgID, integrations.SlackConnectionKeysFor("T0SWITCH", "A0INACTIVE").BotToken); got != "xoxb-inactive" {
		t.Errorf("inactive sibling's bot token = %q; want it kept", got)
	}
}

// TestConnectionsAPI_DeleteDeploymentConnection: deleting a deployment
// connection deletes its row and its bot token.
func TestConnectionsAPI_DeleteDeploymentConnection(t *testing.T) {
	r := newSlackWorkspaceRig(t, "https://tf.example")
	orgID, owner, _ := pgtest.SeedOrgWithUser(t, r.h, "slack-delete-deploy")
	keys := integrations.SlackConnectionKeysFor("T0DEPLOY", "A0DEPLOY")
	if err := r.stores.Tx.WithTx(t.Context(), orgID, owner, func(tx db.TxStores) error {
		if err := tx.Secrets.Put(t.Context(), orgID, keys.BotToken, "xoxb-deploy", "Slack bot token"); err != nil {
			return err
		}
		return upsertActiveConnection(t.Context(), tx, slackstore.Connection{
			WorkspaceID: "T0DEPLOY", APIAppID: "A0DEPLOY", OrgID: orgID, Source: slackstore.SourceDeployment,
			WorkspaceName: "Deployed", BotUserID: "U0BOT", BotTokenRef: keys.BotToken,
		})
	}); err != nil {
		t.Fatalf("seed deployment connection: %v", err)
	}

	rec := httptest.NewRecorder()
	r.hdl.handleDelete(rec, r.reqDelete(owner, orgID, "T0DEPLOY", "A0DEPLOY"))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := r.listConnections(owner, orgID); len(got) != 0 {
		t.Errorf("connections after delete = %+v; want none", got)
	}
	if got := r.secretValue(orgID, keys.BotToken); got != "" {
		t.Errorf("deployment connection's bot token = %q after delete; want deleted", got)
	}
}
