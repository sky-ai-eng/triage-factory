package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	linearclient "github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The Linear verb tests drive the LocalClient against a fake Linear GraphQL
// backend. The LocalClient is the one seam the multi daemon dispatches
// through, so the same assertions cover the sidecar and the local CLI.

const (
	fakeLinearIssueUUID  = "uuid-7"
	fakeLinearParentUUID = "uuid-1"
	fakeLinearCreated    = "uuid-8"
	fakeLinearViewerID   = "app-user"
	fakeLinearCommentID  = "cmt-1"
	fakeLinearPlaceholer = "run-placeholder"
)

// fakeLinearCall is one GraphQL request the fake saw.
type fakeLinearCall struct {
	op   string
	vars map[string]any
	auth string
}

// fakeLinear is a minimal Linear: one team ENG with three states and two
// labels (and a team ENGX, so a key is matched whole rather than as a
// substring), issue ENG-7 with parent ENG-1, and a search that answers two
// pages of two issues each.
type fakeLinear struct {
	mu    sync.Mutex
	calls []fakeLinearCall
}

var gqlOpName = regexp.MustCompile(`(?m)^\s*(?:query|mutation) (\w+)`)

func fakeLinearIssueNode(id, identifier string) map[string]any {
	return map[string]any{
		"id": id, "identifier": identifier, "title": "Fix the thing", "description": "body",
		"url":      "https://linear.app/acme/issue/" + identifier + "/fix-the-thing",
		"priority": 3, "priorityLabel": "Normal",
		"createdAt": "2026-10-01T00:00:00.000Z", "updatedAt": "2026-10-02T00:00:00.000Z",
		"state":    map[string]any{"id": "st-todo", "name": "Todo", "type": "unstarted", "position": 1},
		"team":     map[string]any{"id": "team-eng", "key": "ENG", "name": "Engineering"},
		"labels":   map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
		"comments": map[string]any{"nodes": []any{}},
		"children": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
	}
}

func startFakeLinear(t *testing.T) (*fakeLinear, *httptest.Server) {
	t.Helper()
	f := &fakeLinear{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.URL.Path != "/graphql" || json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m := gqlOpName.FindStringSubmatch(req.Query)
		if m == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		op := m[1]
		f.mu.Lock()
		f.calls = append(f.calls, fakeLinearCall{op: op, vars: req.Variables, auth: r.Header.Get("Authorization")})
		f.mu.Unlock()

		var data any
		switch op {
		case "Issue":
			// Linear resolves an identifier in any case.
			id, _ := req.Variables["id"].(string)
			if !strings.HasPrefix(id, "uuid-") {
				id = strings.ToUpper(id)
			}
			switch id {
			case "ENG-7", fakeLinearIssueUUID:
				data = map[string]any{"issue": fakeLinearIssueNode(fakeLinearIssueUUID, "ENG-7")}
			case "ENG-1", fakeLinearParentUUID:
				data = map[string]any{"issue": fakeLinearIssueNode(fakeLinearParentUUID, "ENG-1")}
			default:
				data = map[string]any{"issue": nil}
			}
		case "Viewer":
			data = map[string]any{"viewer": map[string]any{"id": fakeLinearViewerID, "name": "Triage Factory", "isMe": true}}
		case "Teams":
			data = map[string]any{"teams": map[string]any{
				"nodes": []any{
					map[string]any{"id": "team-engx", "key": "ENGX", "name": "Eng Experiments"},
					map[string]any{"id": "team-eng", "key": "ENG", "name": "Engineering"},
				},
				"pageInfo": map[string]any{"hasNextPage": false},
			}}
		case "TeamStates":
			data = map[string]any{"team": map[string]any{"states": map[string]any{
				"nodes": []any{
					map[string]any{"id": "st-done", "name": "Done", "type": "completed", "position": 3},
					map[string]any{"id": "st-todo", "name": "Todo", "type": "unstarted", "position": 1},
					map[string]any{"id": "st-prog", "name": "In Progress", "type": "started", "position": 2},
				},
				"pageInfo": map[string]any{"hasNextPage": false},
			}}}
		case "Labels":
			data = map[string]any{"issueLabels": map[string]any{
				"nodes": []any{
					map[string]any{"id": "lbl-bug", "name": "Bug"},
					map[string]any{"id": "lbl-ui", "name": "UI"},
				},
				"pageInfo": map[string]any{"hasNextPage": false},
			}}
		case "IssueUpdate":
			data = map[string]any{"issueUpdate": map[string]any{"success": true, "issue": fakeLinearIssueNode(fakeLinearIssueUUID, "ENG-7")}}
		case "IssueCreate":
			data = map[string]any{"issueCreate": map[string]any{"success": true, "issue": fakeLinearIssueNode(fakeLinearCreated, "ENG-8")}}
		case "CommentCreate":
			data = map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": fakeLinearCommentID}}}
		case "IssueChildren":
			data = map[string]any{"issue": map[string]any{"children": map[string]any{
				"nodes":    []any{map[string]any{"id": "uuid-9", "identifier": "ENG-9", "state": map[string]any{"id": "st-todo", "name": "Todo", "type": "unstarted"}}},
				"pageInfo": map[string]any{"hasNextPage": false},
			}}}
		case "SearchIssues":
			page := map[string]any{"nodes": []any{
				fakeLinearIssueNode("uuid-21", "ENG-21"), fakeLinearIssueNode("uuid-22", "ENG-22"),
			}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c1"}}
			if req.Variables["after"] == "c1" {
				page = map[string]any{"nodes": []any{
					fakeLinearIssueNode("uuid-23", "ENG-23"), fakeLinearIssueNode("uuid-24", "ENG-24"),
				}, "pageInfo": map[string]any{"hasNextPage": false}}
			}
			data = map[string]any{"issues": page}
		default:
			t.Errorf("fake linear: unexpected operation %s", op)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// last returns the most recent call to op, failing the test when there is none.
func (f *fakeLinear) last(t *testing.T, op string) fakeLinearCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].op == op {
			return f.calls[i]
		}
	}
	t.Fatalf("fake linear saw no %s call; saw %v", op, f.ops())
	return fakeLinearCall{}
}

func (f *fakeLinear) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.op == op {
			n++
		}
	}
	return n
}

func (f *fakeLinear) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ops lists the operations seen; the caller holds mu.
func (f *fakeLinear) ops() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.op
	}
	return out
}

func input(t *testing.T, c fakeLinearCall) map[string]any {
	t.Helper()
	in, ok := c.vars["input"].(map[string]any)
	if !ok {
		t.Fatalf("%s carried no input: %v", c.op, c.vars)
	}
	return in
}

// linearProxyClient builds a LocalClient over real SQLite stores whose Linear
// verbs reach the fake through the proxy path, the shape the sidecar's daemon
// has.
func linearProxyClient(t *testing.T, linearURL string, eventTriggered bool) (*LocalClient, db.Stores, ConversationInfo) {
	t.Helper()
	stores, info := newCaptureStores(t, eventTriggered)
	lc := NewLocal(stores, info)
	lc.proxyCreds = &ProxyCredentials{LinearAPIURL: linearURL, LinearAPIToken: fakeLinearPlaceholer}
	return lc, stores, info
}

// fakeLinearResolver hands out a client pointed at a fake Linear, standing in
// for the local resolver path's secret-store read.
type fakeLinearResolver struct {
	linearclient.Resolver
	url string
	err error
}

func (r fakeLinearResolver) ForSystem(context.Context, string) (*linearclient.Client, error) {
	if r.err != nil {
		return nil, r.err
	}
	return linearclient.NewClient(linearclient.ProxyPlaceholder(r.url, "org-key")), nil
}

func TestLinearVerbs_ResolveTheAgentsVocabulary(t *testing.T) {
	ctx := context.Background()

	t.Run("transition by state name", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		got, err := lc.LinearTransition(ctx, "ENG-7", "in progress")
		if err != nil {
			t.Fatalf("LinearTransition: %v", err)
		}
		if got.ID != "st-prog" || got.Name != "In Progress" {
			t.Errorf("moved to %+v, want In Progress", got)
		}
		upd := f.last(t, "IssueUpdate")
		if upd.vars["id"] != fakeLinearIssueUUID || input(t, upd)["stateId"] != "st-prog" {
			t.Errorf("issueUpdate vars = %v, want the issue's UUID and st-prog", upd.vars)
		}
		if upd.auth != "Bearer "+fakeLinearPlaceholer {
			t.Errorf("Authorization = %q, want the per-run placeholder", upd.auth)
		}
	})

	t.Run("transition by state id", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		if _, err := lc.LinearTransition(ctx, fakeLinearIssueUUID, "st-done"); err != nil {
			t.Fatalf("LinearTransition: %v", err)
		}
		if input(t, f.last(t, "IssueUpdate"))["stateId"] != "st-done" {
			t.Error("a state id did not select that state")
		}
	})

	t.Run("an unknown state names the team's states and writes nothing", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		_, err := lc.LinearTransition(ctx, "ENG-7", "Shipped")
		if err == nil || !strings.Contains(err.Error(), "Todo, In Progress, Done") {
			t.Errorf("err = %v, want one listing the team's states in board order", err)
		}
		if n := f.count("IssueUpdate"); n != 0 {
			t.Errorf("an unknown state still wrote %d update(s)", n)
		}
	})

	t.Run("assign targets the org's identity", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		if err := lc.LinearAssignSelf(ctx, "ENG-7"); err != nil {
			t.Fatalf("LinearAssignSelf: %v", err)
		}
		if input(t, f.last(t, "IssueUpdate"))["assigneeId"] != fakeLinearViewerID {
			t.Error("assign did not assign the viewer")
		}
	})

	t.Run("create resolves team, parent and labels", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		created, err := lc.LinearCreateIssue(ctx, LinearCreateIssueRequest{
			TeamKey: "eng", Title: "New", Parent: "ENG-1", Priority: 2, Labels: []string{"bug"},
		})
		if err != nil {
			t.Fatalf("LinearCreateIssue: %v", err)
		}
		if created.Identifier != "ENG-8" {
			t.Errorf("created %q, want ENG-8", created.Identifier)
		}
		in := input(t, f.last(t, "IssueCreate"))
		want := map[string]any{"teamId": "team-eng", "title": "New", "parentId": fakeLinearParentUUID, "priority": float64(2), "labelIds": []any{"lbl-bug"}}
		if !reflect.DeepEqual(in, want) {
			t.Errorf("issueCreate input = %v, want %v", in, want)
		}
	})

	t.Run("an unknown team key creates nothing", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		_, err := lc.LinearCreateIssue(ctx, LinearCreateIssueRequest{TeamKey: "EN", Title: "New"})
		if err == nil || !strings.Contains(err.Error(), `"EN"`) {
			t.Errorf("err = %v, want a refusal naming the key (a substring of ENG is not ENG)", err)
		}
		if n := f.count("IssueCreate"); n != 0 {
			t.Errorf("an unknown team still created %d issue(s)", n)
		}
	})

	t.Run("edit resolves label names", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		title := "Renamed"
		if err := lc.LinearUpdateIssue(ctx, "ENG-7", LinearIssueEdit{Title: &title, AddLabels: []string{"ui"}, RemoveLabels: []string{"BUG"}}); err != nil {
			t.Fatalf("LinearUpdateIssue: %v", err)
		}
		in := input(t, f.last(t, "IssueUpdate"))
		want := map[string]any{"title": "Renamed", "addedLabelIds": []any{"lbl-ui"}, "removedLabelIds": []any{"lbl-bug"}}
		if !reflect.DeepEqual(in, want) {
			t.Errorf("issueUpdate input = %v, want %v", in, want)
		}
		_, err := lc.LinearCreateIssue(ctx, LinearCreateIssueRequest{TeamKey: "ENG", Title: "x", Labels: []string{"nope"}})
		if err == nil || !strings.Contains(err.Error(), `"nope"`) {
			t.Errorf("an unknown label: err = %v, want a refusal naming it", err)
		}
	})

	t.Run("set-parent writes the parent's UUID", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		if err := lc.LinearSetParent(ctx, "ENG-7", "ENG-1"); err != nil {
			t.Fatalf("LinearSetParent: %v", err)
		}
		if input(t, f.last(t, "IssueUpdate"))["parentId"] != fakeLinearParentUUID {
			t.Error("set-parent did not write the parent's UUID")
		}
	})

	t.Run("search resolves team, states and assignee, and pages to max", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		issues, err := lc.LinearSearch(ctx, LinearSearchRequest{TeamKey: "ENG", States: []string{"todo", "In Progress"}, Assignee: LinearAssigneeMe, Max: 3})
		if err != nil {
			t.Fatalf("LinearSearch: %v", err)
		}
		var ids []string
		for _, i := range issues {
			ids = append(ids, i.Identifier)
		}
		if !reflect.DeepEqual(ids, []string{"ENG-21", "ENG-22", "ENG-23"}) {
			t.Errorf("issues = %v, want the first three across two pages", ids)
		}
		filter, _ := f.last(t, "SearchIssues").vars["filter"].(map[string]any)
		want := map[string]any{
			"team":     map[string]any{"id": map[string]any{"eq": "team-eng"}},
			"state":    map[string]any{"id": map[string]any{"in": []any{"st-todo", "st-prog"}}},
			"assignee": map[string]any{"id": map[string]any{"eq": fakeLinearViewerID}},
		}
		if !reflect.DeepEqual(filter, want) {
			t.Errorf("filter = %v, want %v", filter, want)
		}
	})

	t.Run("comment returns the comment's id", func(t *testing.T) {
		f, srv := startFakeLinear(t)
		lc, _, _ := linearProxyClient(t, srv.URL, true)
		id, err := lc.LinearAddComment(ctx, "ENG-7", "looks good")
		if err != nil {
			t.Fatalf("LinearAddComment: %v", err)
		}
		if id != fakeLinearCommentID || input(t, f.last(t, "CommentCreate"))["issueId"] != fakeLinearIssueUUID {
			t.Errorf("comment id %q, input %v", id, f.last(t, "CommentCreate").vars)
		}
	})
}

// TestLinearVerbs_BadCallsFailBeforeCallingLinear pins the validation the ops
// apply themselves, for every caller of the seam and not only the CLI.
func TestLinearVerbs_BadCallsFailBeforeCallingLinear(t *testing.T) {
	ctx := context.Background()
	f, srv := startFakeLinear(t)
	lc, _, _ := linearProxyClient(t, srv.URL, true)
	five := 5
	cases := map[string]func() error{
		"search max 0": func() error { _, err := lc.LinearSearch(ctx, LinearSearchRequest{TeamKey: "ENG"}); return err },
		"search max too big": func() error {
			_, err := lc.LinearSearch(ctx, LinearSearchRequest{TeamKey: "ENG", Max: 201})
			return err
		},
		"search assignee": func() error {
			_, err := lc.LinearSearch(ctx, LinearSearchRequest{TeamKey: "ENG", Assignee: "bob", Max: 5})
			return err
		},
		"priority 5":       func() error { return lc.LinearSetPriority(ctx, "ENG-7", 5) },
		"edit priority -1": func() error { m := -1; return lc.LinearUpdateIssue(ctx, "ENG-7", LinearIssueEdit{Priority: &m}) },
		"edit nothing":     func() error { return lc.LinearUpdateIssue(ctx, "ENG-7", LinearIssueEdit{}) },
		"create no title": func() error {
			_, err := lc.LinearCreateIssue(ctx, LinearCreateIssueRequest{TeamKey: "ENG"})
			return err
		},
		"create priority 5": func() error {
			_, err := lc.LinearCreateIssue(ctx, LinearCreateIssueRequest{TeamKey: "ENG", Title: "x", Priority: five})
			return err
		},
		"set-parent no parent": func() error { return lc.LinearSetParent(ctx, "ENG-7", " ") },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("accepted, want a refusal")
			}
		})
	}
	if n := f.total(); n != 0 {
		t.Errorf("refused calls still reached Linear %d time(s)", n)
	}
}

// TestLinearVerbs_RecordArtifacts pins the audit trail: each write lands one
// artifacts row keyed on the issue's UUID (the comment's anchored on its id)
// and one external action under the org's Linear identity, across both write
// paths; reads record nothing.
func TestLinearVerbs_RecordArtifacts(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, srv := startFakeLinear(t)
			lc, stores, info := linearProxyClient(t, srv.URL, eventTriggered)

			if _, err := lc.LinearGetIssue(ctx, "ENG-7"); err != nil {
				t.Fatalf("LinearGetIssue: %v", err)
			}
			if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 0 {
				t.Fatalf("a read recorded %d artifact(s)", len(arts))
			}
			if _, err := lc.LinearTransition(ctx, "ENG-7", "Done"); err != nil {
				t.Fatalf("LinearTransition: %v", err)
			}
			if _, err := lc.LinearAddComment(ctx, "eng-7", "on it"); err != nil {
				t.Fatalf("LinearAddComment: %v", err)
			}
			if _, err := lc.LinearCreateIssue(ctx, LinearCreateIssueRequest{TeamKey: "ENG", Title: "New"}); err != nil {
				t.Fatalf("LinearCreateIssue: %v", err)
			}

			byKey := map[string]domain.Artifact{}
			for _, a := range listConversationArtifacts(t, stores, info.ConversationID) {
				byKey[a.DedupKey] = a
			}
			issueKey := domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindIssue, fakeLinearIssueUUID, "")
			commentKey := domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindComment, fakeLinearIssueUUID, fakeLinearCommentID)
			createdKey := domain.ArtifactDedupKey(domain.ArtifactProviderLinear, domain.ArtifactKindIssue, fakeLinearCreated, "")
			if len(byKey) != 3 {
				t.Fatalf("artifacts = %v, want exactly %s, %s, %s", byKey, issueKey, commentKey, createdKey)
			}
			if a := byKey[issueKey]; a.Provider != domain.ArtifactProviderLinear || a.Kind != domain.ArtifactKindIssue ||
				a.Target != "ENG-7" || a.ExternalID != fakeLinearIssueUUID || a.State != domain.ArtifactStateIssueUpdated ||
				!strings.HasPrefix(a.URL, "https://linear.app/acme/issue/ENG-7") {
				t.Errorf("issue artifact = %+v", a)
			}
			// The comment was posted naming the issue in lower case; its row
			// carries the identifier Linear answered with.
			if a := byKey[commentKey]; a.Kind != domain.ArtifactKindComment || a.Target != "ENG-7" ||
				a.ExternalID != fakeLinearCommentID || a.State != domain.ArtifactStateCommentPosted {
				t.Errorf("comment artifact = %+v", a)
			}
			if a := byKey[createdKey]; a.Target != "ENG-8" || a.State != domain.ArtifactStateIssueCreated {
				t.Errorf("created artifact = %+v", a)
			}

			acts := listExternalActions(t, stores)
			got := map[string]domain.ExternalAction{}
			for _, a := range acts {
				if a.Credential != domain.CredentialLinearOrg || a.Provider != domain.ArtifactProviderLinear || a.ConversationID != info.ConversationID {
					t.Errorf("action %+v is not attributed to the org's Linear identity on this conversation", a)
				}
				got[a.Action] = a
			}
			if len(acts) != 3 {
				t.Fatalf("external actions = %+v, want 3", acts)
			}
			if tr := got[domain.ActionIssueTransitioned]; tr.FromState != "Todo" || tr.ToState != "Done" || tr.Target != "ENG-7" {
				t.Errorf("transition action = %+v, want Todo → Done on ENG-7", tr)
			}
			if c := got[domain.ActionIssueCommentPosted]; c.ExternalID != fakeLinearCommentID {
				t.Errorf("comment action = %+v", c)
			}
			if c := got[domain.ActionIssueCreated]; c.Target != "ENG-8" {
				t.Errorf("create action = %+v", c)
			}
		})
	}
}

// TestLinearVerbs_TouchTheIssuesEntity pins that a read and a write both
// attach the issue's entity to the conversation, keyed under the org's Linear
// workspace and the issue's UUID.
func TestLinearVerbs_TouchTheIssuesEntity(t *testing.T) {
	ctx := context.Background()
	_, srv := startFakeLinear(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	if _, err := conn.Exec(`UPDATE org_settings SET linear_workspace_id = ? WHERE org_id = ?`, testScope("linear"), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("record linear workspace: %v", err)
	}
	lc := NewLocal(stores, info)
	lc.proxyCreds = &ProxyCredentials{LinearAPIURL: srv.URL, LinearAPIToken: fakeLinearPlaceholer}

	if _, err := lc.LinearGetIssue(ctx, fakeLinearIssueUUID); err != nil {
		t.Fatalf("LinearGetIssue: %v", err)
	}
	ent, err := stores.Entities.GetByExternalIDSystem(ctx, runmode.LocalDefaultOrgID, domain.ArtifactProviderLinear, testScope("linear"), fakeLinearIssueUUID)
	if err != nil || ent == nil {
		t.Fatalf("entity for the read issue: %v, %v", ent, err)
	}
	if ent.SourceID != "ENG-7" {
		t.Errorf("entity source_id = %q, want the identifier Linear answered with", ent.SourceID)
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ? AND entity_id = ?`,
		info.ConversationID, ent.ID).Scan(&n); err != nil {
		t.Fatalf("read touches: %v", err)
	}
	if n != 1 {
		t.Errorf("touch rows = %d, want 1", n)
	}
}

// TestLinearVerb_SourceTurnedOff_RefusesBeforeCallingLinear: a turned-off
// Linear refuses at the credential funnel every Linear verb goes through, on
// both the resolver path and the sidecar's proxy path.
func TestLinearVerb_SourceTurnedOff_RefusesBeforeCallingLinear(t *testing.T) {
	linear := unreachable(t, "linear")
	policy := &fakeSourcePolicy{off: []string{eventsource.KindLinear}}
	info := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-off"}

	resolverPath := NewLocal(withPolicy(db.Stores{}, policy), info)
	resolverPath.SetLinearResolver(fakeLinearResolver{url: linear.URL})
	proxyPath := NewLocal(withPolicy(db.Stores{}, policy), info)
	proxyPath.proxyCreds = &ProxyCredentials{LinearAPIURL: linear.URL, LinearAPIToken: fakeLinearPlaceholer}

	for name, lc := range map[string]*LocalClient{"resolver": resolverPath, "proxy": proxyPath} {
		t.Run(name, func(t *testing.T) {
			_, err := lc.LinearAddComment(context.Background(), "ENG-7", "hi")
			if !errors.Is(err, eventsource.ErrDisabled) {
				t.Fatalf("err = %v, want one wrapping eventsource.ErrDisabled", err)
			}
			if !strings.Contains(err.Error(), "Linear") {
				t.Errorf("error %q does not name the source", err)
			}
		})
	}
}

// TestLinearVerb_NotConfigured pins the agent-facing guidance for an org with
// no Linear credential, identical on both paths, and that any other resolver
// failure is reported as itself.
func TestLinearVerb_NotConfigured(t *testing.T) {
	info := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-nolinear"}
	ctx := context.Background()

	unconfigured := NewLocal(db.Stores{}, info)
	unconfigured.SetLinearResolver(fakeLinearResolver{err: linearclient.ErrNoLinearSystemCredential})
	noProxy := NewLocal(db.Stores{}, info)
	noProxy.proxyCreds = &ProxyCredentials{GitHubAPIURL: "http://127.0.0.1:1"}

	for name, lc := range map[string]*LocalClient{"resolver": unconfigured, "proxy": noProxy} {
		t.Run(name, func(t *testing.T) {
			_, err := lc.LinearGetIssue(ctx, "ENG-7")
			if err == nil || err.Error() != errLinearNotConfigured.Error() {
				t.Errorf("err = %v, want %q", err, errLinearNotConfigured)
			}
		})
	}

	outage := NewLocal(db.Stores{}, info)
	outage.SetLinearResolver(fakeLinearResolver{err: errors.New("keychain locked")})
	if _, err := outage.LinearGetIssue(ctx, "ENG-7"); err == nil || !strings.Contains(err.Error(), "keychain locked") {
		t.Errorf("a resolver outage was reported as %v, want the outage itself", err)
	}
}

// TestLinearVerbs_LocalResolverPath pins that, with no proxy, a verb reaches
// Linear through the resolver with the org's own credential.
func TestLinearVerbs_LocalResolverPath(t *testing.T) {
	f, srv := startFakeLinear(t)
	lc := NewLocal(db.Stores{}, ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-local"})
	lc.SetLinearResolver(fakeLinearResolver{url: srv.URL})
	if _, err := lc.LinearListStates(context.Background(), "ENG-7"); err != nil {
		t.Fatalf("LinearListStates: %v", err)
	}
	if got := f.last(t, "TeamStates"); got.vars["id"] != "team-eng" || got.auth != "Bearer org-key" {
		t.Errorf("states read %v with %q, want the issue's team and the org credential", got.vars, got.auth)
	}
}

// TestServer_LinearOps_RoundTrip drives every Linear op through the socket the
// jailed CLI uses, so each method's wire name, args and result envelope are
// exercised end to end.
func TestServer_LinearOps_RoundTrip(t *testing.T) {
	f, linear := startFakeLinear(t)
	stores, info := newCaptureStores(t, true)
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, &ProxyCredentials{LinearAPIURL: linear.URL, LinearAPIToken: fakeLinearPlaceholer})
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	client := Dial(sockPath)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	issue, err := client.LinearGetIssue(ctx, "ENG-7")
	if err != nil || issue.ID != fakeLinearIssueUUID || issue.Team.Key != "ENG" || issue.State.Name != "Todo" {
		t.Errorf("LinearGetIssue = %+v, %v", issue, err)
	}
	if _, err := client.LinearGetIssue(ctx, "ENG-404"); !strings.Contains(errString(err), "not found") {
		t.Errorf("a missing issue crossed as %v, want Linear's not-found", err)
	}
	states, err := client.LinearListStates(ctx, "ENG-7")
	if err != nil || len(states) != 3 || states[0].Name != "Todo" {
		t.Errorf("LinearListStates = %+v, %v (want board order)", states, err)
	}
	if st, err := client.LinearTransition(ctx, "ENG-7", "Done"); err != nil || st.ID != "st-done" {
		t.Errorf("LinearTransition = %+v, %v", st, err)
	}
	if id, err := client.LinearAddComment(ctx, "ENG-7", "hi"); err != nil || id != fakeLinearCommentID {
		t.Errorf("LinearAddComment = %q, %v", id, err)
	}
	if err := client.LinearAssignSelf(ctx, "ENG-7"); err != nil {
		t.Errorf("LinearAssignSelf: %v", err)
	}
	if err := client.LinearUnassign(ctx, "ENG-7"); err != nil {
		t.Errorf("LinearUnassign: %v", err)
	}
	if in := input(t, f.last(t, "IssueUpdate")); in["assigneeId"] != nil {
		t.Errorf("unassign input = %v, want a null assignee", in)
	}
	if created, err := client.LinearCreateIssue(ctx, LinearCreateIssueRequest{TeamKey: "ENG", Title: "New", Labels: []string{"UI"}}); err != nil || created.Identifier != "ENG-8" {
		t.Errorf("LinearCreateIssue = %+v, %v", created, err)
	}
	desc := ""
	if err := client.LinearUpdateIssue(ctx, "ENG-7", LinearIssueEdit{Description: &desc}); err != nil {
		t.Errorf("LinearUpdateIssue: %v", err)
	}
	if in := input(t, f.last(t, "IssueUpdate")); !reflect.DeepEqual(in, map[string]any{"description": ""}) {
		t.Errorf("an empty description crossed the wire as %v, want it set to empty", in)
	}
	if err := client.LinearSetParent(ctx, "ENG-7", "ENG-1"); err != nil {
		t.Errorf("LinearSetParent: %v", err)
	}
	if err := client.LinearSetPriority(ctx, "ENG-7", 1); err != nil {
		t.Errorf("LinearSetPriority: %v", err)
	}
	if in := input(t, f.last(t, "IssueUpdate")); in["priority"] != float64(1) {
		t.Errorf("set-priority input = %v", in)
	}
	if children, err := client.LinearListChildren(ctx, "ENG-7"); err != nil || len(children) != 1 || children[0].Identifier != "ENG-9" {
		t.Errorf("LinearListChildren = %+v, %v", children, err)
	}
	if issues, err := client.LinearSearch(ctx, LinearSearchRequest{TeamKey: "ENG", Assignee: LinearAssigneeNone, Max: 50}); err != nil || len(issues) != 4 {
		t.Errorf("LinearSearch = %d issues, %v; want all 4 across both pages", len(issues), err)
	}
	if _, err := client.LinearSearch(ctx, LinearSearchRequest{TeamKey: "ENG", Max: 0}); !strings.Contains(errString(err), "out of range") {
		t.Errorf("a bad max crossed as %v, want the daemon's refusal", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
