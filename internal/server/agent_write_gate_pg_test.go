package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// conversationGateRig is the viewer rig plus the agent routes, a spawner over
// the Postgres stores, and a fourth caller: an org member on another team, who
// can see an org-visible conversation and not a team one.
type conversationGateRig struct {
	*viewerRig
	mux      *http.ServeMux
	outsider string
}

func newConversationGateRig(t *testing.T) *conversationGateRig {
	t.Helper()
	r := newViewerRig(t)
	otherTeam := pgtest.SeedTeam(t, r.h, r.orgID, "other-team")
	outsider := pgtest.SeedUser(t, r.h, "outsider")
	pgtest.AddOrgMember(t, r.h, outsider, r.orgID, otherTeam, "member", "member")

	stores := pgstore.New(r.h.AdminDB, r.h.AppDB, pgtest.SecretKey)
	spawner := delegate.NewSpawner(r.h.AdminDB, stores, nil, r.s.ws, "claude-sonnet-4-6")
	ag := &agentHandler{
		tx: r.s.tx, az: r.s.az, ws: r.s.ws,
		spawner:    func() *delegate.Spawner { return spawner },
		reconciler: func() *reconcile.Reconciler { return nil },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agent/conversations/{conversationID}", ag.handleAgentStatus)
	mux.HandleFunc("POST /api/agent/conversations/list", ag.handleConversations)
	mux.HandleFunc("POST /api/agent/conversations/{conversationID}/stop", ag.handleAgentStop)
	mux.HandleFunc("POST /api/agent/conversations/{conversationID}/message", ag.handleMessage)
	mux.HandleFunc("POST /api/agent/conversations/{conversationID}/permissions/{toolCallID}", ag.handleAgentPermission)
	return &conversationGateRig{viewerRig: r, mux: mux, outsider: outsider}
}

// seedConversation inserts a queued native conversation on the rig's team
// with the given visibility and creator. Queued, so a stop that passes the
// gate has something to stop; native, so a message that passes it is queued
// as a transcript row rather than refused for want of a live process.
func (r *conversationGateRig) seedConversation(t *testing.T, visibility, creator string) string {
	t.Helper()
	id := uuid.NewString()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, runtime, visibility)
		VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'native', $5)
	`, id, r.orgID, r.teamID, creator, visibility)
	return id
}

func (r *conversationGateRig) do(t *testing.T, caller, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: caller})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func (r *conversationGateRig) messageRows(t *testing.T, conversationID string) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = $1`, conversationID).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// gateAnswer is what the conversation gate made of a request: refused as
// invisible, refused as view-only, or let through to the route's own answer.
type gateAnswer string

const (
	gateNotFound  gateAnswer = "404"
	gateForbidden gateAnswer = "403"
	gatePassed    gateAnswer = "passed"
)

// classify reads a response as the gate's answer. A route that passed the gate
// can still answer 404 for its own reasons (a permission prompt that is not
// pending), so only the conversation's 404 counts as the gate's.
func classify(t *testing.T, rec *httptest.ResponseRecorder) gateAnswer {
	t.Helper()
	var env struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	switch {
	case rec.Code == http.StatusForbidden:
		if len(env.Errors) != 1 || env.Errors[0].Reason != httpx.ReasonForbidden || env.Errors[0].Message != conversationViewOnlyMessage {
			t.Errorf("403 body = %s, want one FORBIDDEN error with the view-only message", rec.Body.String())
		}
		return gateForbidden
	case rec.Code == http.StatusNotFound && len(env.Errors) == 1 && env.Errors[0].Message == "conversation not found":
		return gateNotFound
	default:
		return gatePassed
	}
}

// TestConversationWriteGate_Postgres: stop, message, and a permission answer
// share one gate, the conversations_update policy's rule, under live RLS. A
// team viewer reads the team's conversation and may not act on it; on an
// org-visible conversation only an org admin may act; a private one is its
// creator's alone and invisible to everyone else.
func TestConversationWriteGate_Postgres(t *testing.T) {
	r := newConversationGateRig(t)

	team := r.seedConversation(t, "team", r.admin)
	org := r.seedConversation(t, "org", r.admin)
	private := r.seedConversation(t, "private", r.member)

	type caller struct{ name, id string }
	admin := caller{"team and org admin", r.admin}
	member := caller{"team member", r.member}
	viewer := caller{"team viewer", r.viewer}
	outsider := caller{"member of another team", r.outsider}

	cases := []struct {
		conversation string
		label        string
		want         map[caller]gateAnswer
	}{
		{team, "team", map[caller]gateAnswer{
			admin: gatePassed, member: gatePassed, viewer: gateForbidden, outsider: gateNotFound,
		}},
		{org, "org", map[caller]gateAnswer{
			admin: gatePassed, member: gateForbidden, viewer: gateForbidden, outsider: gateForbidden,
		}},
		{private, "private", map[caller]gateAnswer{
			member: gatePassed, admin: gateNotFound, viewer: gateNotFound, outsider: gateNotFound,
		}},
	}
	routes := []struct {
		name, suffix string
		body         any
	}{
		{"stop", "/stop", nil},
		{"message", "/message", map[string]string{"text": "pick this back up"}},
		{"permission", "/permissions/toolu_1", map[string]string{"behavior": "allow"}},
	}

	for _, tc := range cases {
		for c, want := range tc.want {
			for _, rt := range routes {
				before := r.messageRows(t, tc.conversation)
				rec := r.do(t, c.id, http.MethodPost, "/api/agent/conversations/"+tc.conversation+rt.suffix, rt.body)
				got := classify(t, rec)
				if got != want {
					t.Errorf("%s conversation, %s, %s: gate answered %s (status %d, body %s), want %s",
						tc.label, c.name, rt.name, got, rec.Code, rec.Body.String(), want)
				}
				if got != gatePassed {
					if after := r.messageRows(t, tc.conversation); after != before {
						t.Errorf("%s conversation, %s, %s: refused request recorded %d message rows", tc.label, c.name, rt.name, after-before)
					}
				}
			}
		}
	}

	// Past the gate a writer gets the route's own answer: the message is
	// queued as one transcript row, and the stop is recorded.
	fresh := r.seedConversation(t, "team", r.admin)
	if rec := r.do(t, r.member, http.MethodPost, "/api/agent/conversations/"+fresh+"/message", map[string]string{"text": "hello"}); rec.Code != http.StatusOK {
		t.Errorf("member message to the team conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if n := r.messageRows(t, fresh); n != 1 {
		t.Errorf("member message recorded %d rows, want 1", n)
	}
	if rec := r.do(t, r.member, http.MethodPost, "/api/agent/conversations/"+fresh+"/stop", nil); rec.Code != http.StatusOK {
		t.Errorf("member stop of the team conversation = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestConversationCanWrite_Postgres: the detail and list reads carry the
// gate's answer as can_write, so a client offers the controls only to a
// caller the routes accept.
func TestConversationCanWrite_Postgres(t *testing.T) {
	r := newConversationGateRig(t)

	team := r.seedConversation(t, "team", r.admin)
	org := r.seedConversation(t, "org", r.admin)

	detail := func(caller, id string) any {
		t.Helper()
		rec := r.do(t, caller, http.MethodGet, "/api/agent/conversations/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET conversation %s as %s = %d; body=%s", id, caller, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode conversation: %v", err)
		}
		return body["can_write"]
	}
	listed := func(caller string) map[string]any {
		t.Helper()
		rec := r.do(t, caller, http.MethodPost, "/api/agent/conversations/list", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("list conversations as %s = %d; body=%s", caller, rec.Code, rec.Body.String())
		}
		var body struct {
			Runs map[string][]map[string]any `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		out := map[string]any{}
		for _, rows := range body.Runs {
			for _, row := range rows {
				out[row["ID"].(string)] = row["can_write"]
			}
		}
		return out
	}

	cases := []struct {
		name, caller, conversation string
		want                       bool
	}{
		{"admin on team", r.admin, team, true},
		{"member on team", r.member, team, true},
		{"viewer on team", r.viewer, team, false},
		{"admin on org", r.admin, org, true},
		{"member on org", r.member, org, false},
		{"viewer on org", r.viewer, org, false},
	}
	for _, tc := range cases {
		if got := detail(tc.caller, tc.conversation); got != tc.want {
			t.Errorf("%s: detail can_write = %v, want %v", tc.name, got, tc.want)
		}
		if got := listed(tc.caller)[tc.conversation]; got != tc.want {
			t.Errorf("%s: list can_write = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestConversationDeliverablesCanWrite_Postgres: each row of a conversation's
// deliverable list carries the deliverable write gate's answer, a writer of
// the deliverable's team, rather than the conversation's. On an org-visible
// conversation an org admin who is not on the team may act on the
// conversation and still may not approve or dismiss its deliverables.
func TestConversationDeliverablesCanWrite_Postgres(t *testing.T) {
	r := newConversationGateRig(t)
	ag := &agentHandler{tx: r.s.tx, az: r.s.az, ws: r.s.ws}
	r.mux.HandleFunc("POST /api/agent/conversations/{conversationID}/deliverables/list", ag.handleAgentDeliverables)

	offTeamAdmin := pgtest.SeedUser(t, r.h, "off-team-org-admin")
	otherTeam := pgtest.SeedTeam(t, r.h, r.orgID, "admin-team")
	pgtest.AddOrgMember(t, r.h, offTeamAdmin, r.orgID, otherTeam, "admin", "member")

	seed := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey)
	if _, _, err := seed.Orgs.AdoptSourceBaseURL(t.Context(), r.orgID, domain.ProviderGitHub, dbtest.TestGitHubHost); err != nil {
		t.Fatalf("set the org's GitHub host: %v", err)
	}
	seedDeliverable := func(conversationID string, number int) string {
		t.Helper()
		d := domain.NewPullRequestDeliverable("octo/repo", number, fmt.Sprintf("PR_octo_repo_%d", number), "feature/x", "main",
			fmt.Sprintf("https://github.com/octo/repo/pull/%d", number), "title", "body", true)
		d.OrgID, d.TeamID, d.Scope, d.ConversationID = r.orgID, r.teamID, dbtest.TestGitHubHost, conversationID
		row, err := seed.Deliverables.UpsertSystem(t.Context(), r.orgID, d)
		if err != nil {
			t.Fatalf("seed deliverable: %v", err)
		}
		return row.ID
	}

	team := r.seedConversation(t, "team", r.admin)
	teamDeliverable := seedDeliverable(team, 1)
	org := r.seedConversation(t, "org", r.admin)
	orgDeliverable := seedDeliverable(org, 2)

	listed := func(caller, conversationID string) map[string]any {
		t.Helper()
		rec := r.do(t, caller, http.MethodPost, "/api/agent/conversations/"+conversationID+"/deliverables/list", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("list deliverables of %s as %s = %d; body=%s", conversationID, caller, rec.Code, rec.Body.String())
		}
		page := httpxtest.DecodeList[map[string]any](t, rec)
		out := map[string]any{}
		for _, row := range page.Items {
			out[row["id"].(string)] = row["can_write"]
		}
		return out
	}

	cases := []struct {
		name, caller, conversation, deliverable string
		want                                    bool
	}{
		{"team admin", r.admin, team, teamDeliverable, true},
		{"team member", r.member, team, teamDeliverable, true},
		{"team viewer", r.viewer, team, teamDeliverable, false},
		{"team admin on the org conversation", r.admin, org, orgDeliverable, true},
		{"off-team org admin on the org conversation", offTeamAdmin, org, orgDeliverable, false},
	}
	for _, tc := range cases {
		if got := listed(tc.caller, tc.conversation)[tc.deliverable]; got != tc.want {
			t.Errorf("%s: can_write = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The conversation's own answer differs for the off-team org admin, which
	// is why the deliverable rows carry their own.
	rec := r.do(t, offTeamAdmin, http.MethodGet, "/api/agent/conversations/"+org, nil)
	var conv map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &conv); err != nil || conv["can_write"] != true {
		t.Errorf("off-team org admin's org conversation: can_write = %v (status %d), want true", conv["can_write"], rec.Code)
	}
}
