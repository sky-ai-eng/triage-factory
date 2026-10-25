package agenthost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The assign tests stand a fake Jira that knows a few people on each
// deployment and records every request, so a test can assert both what an
// assign sent and that it never consulted the org's own account.

// jiraAssignCall is one request the fake saw.
type jiraAssignCall struct {
	method, path, query, body, auth string
}

type jiraAssignFake struct {
	mu    sync.Mutex
	calls []jiraAssignCall
}

func (f *jiraAssignFake) snapshot() []jiraAssignCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]jiraAssignCall(nil), f.calls...)
}

// writes is every request that was not a read.
func (f *jiraAssignFake) writes() []jiraAssignCall {
	var out []jiraAssignCall
	for _, c := range f.snapshot() {
		if c.method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// startJiraAssignFake serves Ana on both deployments (user key JIRAUSER10100 on
// Data Center, accountId 557058:ana on Cloud), a deactivated Data Center user
// JIRAUSER10200, and nobody else. A user read for anyone else is Jira's 404.
func startJiraAssignFake(t *testing.T) (*jiraAssignFake, *httptest.Server) {
	t.Helper()
	f := &jiraAssignFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, jiraAssignCall{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body), auth: r.Header.Get("Authorization"),
		})
		f.mu.Unlock()

		cloud := strings.HasPrefix(r.URL.Path, "/rest/api/3/")
		ana := `{"key":"JIRAUSER10100","name":"ana","displayName":"Ana Lyst","emailAddress":"ana@acme.test","active":true}`
		if cloud {
			ana = `{"accountId":"557058:ana","displayName":"Ana Lyst","emailAddress":"ana@acme.test","active":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/user/assignable/search"):
			_, _ = io.WriteString(w, `[`+ana+`]`)
		case strings.HasSuffix(path, "/user"):
			q := r.URL.Query()
			switch {
			case cloud && q.Get("accountId") == "557058:ana", !cloud && q.Get("key") == "JIRAUSER10100":
				_, _ = io.WriteString(w, ana)
			case !cloud && q.Get("key") == "JIRAUSER10200":
				_, _ = io.WriteString(w, `{"key":"JIRAUSER10200","name":"old","displayName":"Old Timer","active":false}`)
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"errorMessages":["The user does not exist"]}`)
			}
		case strings.HasSuffix(path, "/assignee"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.Contains(path, "/issue/"):
			key := strings.ToUpper(path[strings.LastIndex(path, "/")+1:])
			_, _ = io.WriteString(w, `{"id":"`+fakeJiraIssueID(key)+`","key":"`+key+`","fields":{"project":{"id":"id-SKY","key":"SKY"}}}`)
		default:
			t.Errorf("fake jira: unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// jiraAssignClients is the LocalClient on each credential path: the local
// resolver against a Data Center site with the org's PAT, and the sidecar's
// proxy for a Cloud site, holding only the per-run placeholder.
func jiraAssignClients(url string) map[string]*LocalClient {
	info := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-assign"}
	cloud := NewLocal(jiraStores(url, ""), info)
	cloud.proxyCreds = &ProxyCredentials{JiraAPIURL: url, JiraAPIToken: "run-placeholder", JiraDeployment: string(jiraclient.DeploymentCloud)}
	return map[string]*LocalClient{
		"data center": NewLocal(jiraStores(url, "org-pat"), info),
		"cloud":       cloud,
	}
}

// TestJiraAssign_SendsTheNamedPersonsID pins what an assign writes: the
// person's id in the shape each deployment's assignee write takes, under the
// org's credential, and never anything about the org's own account.
func TestJiraAssign_SendsTheNamedPersonsID(t *testing.T) {
	cases := map[string]struct {
		user, lookupQuery, putPath, putBody, auth string
	}{
		"data center": {"JIRAUSER10100", "key=JIRAUSER10100", "/rest/api/2/issue/SKY-9/assignee", `{"name":"ana"}`, "Bearer org-pat"},
		"cloud":       {"557058:ana", "accountId=557058%3Aana", "/rest/api/3/issue/SKY-9/assignee", `{"accountId":"557058:ana"}`, "Bearer run-placeholder"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, srv := startJiraAssignFake(t)
			user, err := jiraAssignClients(srv.URL)[name].JiraAssign(context.Background(), "SKY-9", tc.user)
			if err != nil {
				t.Fatalf("JiraAssign: %v", err)
			}
			if user.ID != tc.user || user.DisplayName != "Ana Lyst" {
				t.Errorf("assigned %+v, want Ana Lyst under %s", user, tc.user)
			}
			calls := f.snapshot()
			if len(calls) == 0 || !strings.HasSuffix(calls[0].path, "/user") || calls[0].query != tc.lookupQuery {
				t.Errorf("first call = %+v, want the user read by %s", calls, tc.lookupQuery)
			}
			writes := f.writes()
			if len(writes) != 1 || writes[0].method != http.MethodPut || writes[0].path != tc.putPath ||
				writes[0].body != tc.putBody || writes[0].auth != tc.auth {
				t.Errorf("writes = %+v, want one PUT %s %s with %s", writes, tc.putPath, tc.putBody, tc.auth)
			}
			for _, c := range calls {
				if strings.HasSuffix(c.path, "/myself") {
					t.Error("assign read the org's own account; it must never be the assignee")
				}
			}
		})
	}
}

// TestJiraAssign_RefusesWithoutAnAssignablePerson pins that an assign naming
// nobody, nobody Jira knows, or a deactivated user writes nothing, and that
// the missing person is refused before Jira is reached at all.
func TestJiraAssign_RefusesWithoutAnAssignablePerson(t *testing.T) {
	cases := map[string]struct {
		user, want  string
		reachesJira bool
	}{
		"no user":          {"", "a user is required", false},
		"blank user":       {"  ", "a user is required", false},
		"unknown user":     {"JIRAUSER99999", `no Jira user has the id "JIRAUSER99999"`, true},
		"deactivated user": {"JIRAUSER10200", "deactivated", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, srv := startJiraAssignFake(t)
			_, err := jiraAssignClients(srv.URL)["data center"].JiraAssign(context.Background(), "SKY-9", tc.user)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
			if w := f.writes(); len(w) != 0 {
				t.Errorf("a refused assign still wrote %+v", w)
			}
			if n := len(f.snapshot()); !tc.reachesJira && n != 0 {
				t.Errorf("a refused assign still reached Jira %d time(s)", n)
			}
		})
	}
}

// TestJiraSearchUsers_AsksWhoCanBeAssignedTheIssue pins the lookup verb: it
// asks Jira's assignable-user search about the named issue, in each
// deployment's parameter, and answers with the id an assign takes.
func TestJiraSearchUsers_AsksWhoCanBeAssignedTheIssue(t *testing.T) {
	cases := map[string]struct{ query, id string }{
		"data center": {"issueKey=SKY-9&maxResults=50&username=ana", "JIRAUSER10100"},
		"cloud":       {"issueKey=SKY-9&maxResults=50&query=ana", "557058:ana"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, srv := startJiraAssignFake(t)
			users, err := jiraAssignClients(srv.URL)[name].JiraSearchUsers(context.Background(), "SKY-9", "ana")
			if err != nil {
				t.Fatalf("JiraSearchUsers: %v", err)
			}
			if len(users) != 1 || users[0].ID != tc.id || users[0].DisplayName != "Ana Lyst" || users[0].Email != "ana@acme.test" || !users[0].Active {
				t.Errorf("users = %+v, want Ana under %s", users, tc.id)
			}
			if calls := f.snapshot(); len(calls) != 1 || calls[0].query != tc.query {
				t.Errorf("calls = %+v, want one search with %s", calls, tc.query)
			}
		})
	}

	t.Run("a blank query or issue is refused before Jira", func(t *testing.T) {
		f, srv := startJiraAssignFake(t)
		lc := jiraAssignClients(srv.URL)["data center"]
		if _, err := lc.JiraSearchUsers(context.Background(), "SKY-9", " "); err == nil {
			t.Error("a blank query was accepted")
		}
		if _, err := lc.JiraSearchUsers(context.Background(), "", "ana"); err == nil {
			t.Error("a search naming no issue was accepted")
		}
		if n := len(f.snapshot()); n != 0 {
			t.Errorf("refused searches reached Jira %d time(s)", n)
		}
	})
}

// TestServer_JiraAssign_RoundTrip drives the assign and the lookup through the
// socket the jailed CLI uses: the assigned user and the candidates cross back,
// and a refusal crosses as its message.
func TestServer_JiraAssign_RoundTrip(t *testing.T) {
	f, srv := startJiraAssignFake(t)
	client := startJiraDaemon(t, jiraStores(srv.URL, "org-pat"),
		ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-1"})
	ctx := context.Background()

	if user, err := client.JiraAssign(ctx, "SKY-9", "JIRAUSER10100"); err != nil || user.ID != "JIRAUSER10100" || user.DisplayName != "Ana Lyst" {
		t.Errorf("JiraAssign = %+v, %v", user, err)
	}
	if w := f.writes(); len(w) != 1 || w[0].body != `{"name":"ana"}` {
		t.Errorf("writes = %+v, want the one assign", w)
	}
	if _, err := client.JiraAssign(ctx, "SKY-9", ""); !strings.Contains(errString(err), "a user is required") {
		t.Errorf("an assign naming nobody crossed as %v, want the refusal", err)
	}
	if users, err := client.JiraSearchUsers(ctx, "SKY-9", "ana"); err != nil || len(users) != 1 || users[0].ID != "JIRAUSER10100" {
		t.Errorf("JiraSearchUsers = %+v, %v", users, err)
	}
}
