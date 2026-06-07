package agenthost

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The Jira tests stand a fake Jira REST backend behind a fake SecretStore
// and drive the host-routed methods through both the in-process
// LocalClient and the full Server↔IPCClient loop. The load-bearing
// assertion across them is Property B: the credential resolves on the
// *host* (the daemon's stores), the agent-facing client never holds it,
// and the call still lands with the bot token attached.

// fakeJiraSecrets serves the org's jira_url + jira_pat through GetSystem
// and embeds db.SecretStore so the resolver's unexercised methods
// compile-satisfy (and panic loudly if ever reached). The two keys
// mirror the resolver's own (unexported) constants verbatim — they are
// the stable wire-level secret names, not something this package owns.
type fakeJiraSecrets struct {
	db.SecretStore
	url string
	pat string
}

func (f fakeJiraSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	switch key {
	case "jira_url":
		return f.url, nil
	case "jira_pat":
		return f.pat, nil
	default:
		return "", nil
	}
}

// fakeJiraOrgs satisfies db.OrgsStore for NewResolver. ForSystem never
// reads org settings (only ForUser does), so no method needs a body.
type fakeJiraOrgs struct {
	db.OrgsStore
}

// jiraStores builds a db.Stores whose Secrets resolve to the given Jira
// host + token. Only Secrets/Orgs are populated — the host-routed Jira
// methods touch nothing else on the bundle, so a minimal struct is the
// honest fixture (and proves the methods don't reach for other stores).
func jiraStores(url, pat string) db.Stores {
	return db.Stores{
		Secrets: fakeJiraSecrets{url: url, pat: pat},
		Orgs:    fakeJiraOrgs{},
	}
}

// jiraRecorder captures what the fake Jira backend saw. Mutex-guarded so
// the handler goroutine and the test goroutine don't race under -race
// (the same reason internal/jira's client_test guards its capture).
type jiraRecorder struct {
	mu   sync.Mutex
	auth string
	body string
}

func (r *jiraRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auth = req.Header.Get("Authorization")
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.body = string(b)
	}
}

func (r *jiraRecorder) readAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth
}

func (r *jiraRecorder) readBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body
}

// startJiraDaemon listens on a temp socket, serves with the given stores
// + identity, and returns a dialed IPCClient — the sandbox-side seam.
// Cleanup tears down the client, listener, and daemon.
func startJiraDaemon(t *testing.T, stores db.Stores, info RunInfo) *IPCClient {
	t.Helper()
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	client := Dial(sockPath)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestServer_JiraGetIssue_RoutesHostSide is the Property-B test: the
// IPCClient (the sandbox's view) holds no credential, yet the call lands
// at Jira carrying the org's bot token — because the daemon built the
// ForSystem client from its own host-side stores and made the request.
func TestServer_JiraGetIssue_RoutesHostSide(t *testing.T) {
	rec := &jiraRecorder{}
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{"summary":"hello"}}`)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	issue, err := client.JiraGetIssue(context.Background(), "PROJ-1")
	if err != nil {
		t.Fatalf("JiraGetIssue: %v", err)
	}
	if issue == nil || issue.Key != "PROJ-1" || issue.Fields.Summary != "hello" {
		t.Fatalf("unexpected issue over the wire: %+v", issue)
	}
	if got := rec.readAuth(); got != "Bearer org-pat" {
		t.Errorf("Jira saw Authorization %q, want %q — the host daemon must resolve ForSystem and present the bot token", got, "Bearer org-pat")
	}
}

// TestServer_JiraCreateIssue_RoundTrip pins a result-bearing write: the
// created key crosses the wire back to the sandbox client.
func TestServer_JiraCreateIssue_RoundTrip(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"PROJ-42"}`)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	key, err := client.JiraCreateIssue(context.Background(), "PROJ", "Task", "do a thing", "", "", "")
	if err != nil {
		t.Fatalf("JiraCreateIssue: %v", err)
	}
	if key != "PROJ-42" {
		t.Errorf("created key = %q, want PROJ-42", key)
	}
}

// TestServer_JiraSearchIssues_RoundTrip pins a slice result shape.
func TestServer_JiraSearchIssues_RoundTrip(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issues":[{"key":"PROJ-1"},{"key":"PROJ-2"}]}`)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	issues, err := client.JiraSearchIssues(context.Background(), "project = PROJ", nil, 50)
	if err != nil {
		t.Fatalf("JiraSearchIssues: %v", err)
	}
	if len(issues) != 2 || issues[0].Key != "PROJ-1" || issues[1].Key != "PROJ-2" {
		t.Errorf("unexpected issues: %+v", issues)
	}
}

// TestServer_JiraUpdateIssue_FieldsCrossWire proves UpdateIssueFields —
// whose pointer fields carry no JSON tags — survives the IPC round-trip:
// the daemon receives the set summary and forwards it to Jira.
func TestServer_JiraUpdateIssue_FieldsCrossWire(t *testing.T) {
	rec := &jiraRecorder{}
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	newSummary := "rewritten summary"
	err := client.JiraUpdateIssue(context.Background(), "PROJ-1", jiraclient.UpdateIssueFields{Summary: &newSummary})
	if err != nil {
		t.Fatalf("JiraUpdateIssue: %v", err)
	}
	if body := rec.readBody(); !strings.Contains(body, newSummary) {
		t.Errorf("PUT body %q did not carry the updated summary — UpdateIssueFields lost a field crossing the wire", body)
	}
}

// TestServer_JiraTransition_RejectionPropagates is the error-fidelity
// acceptance: when the requested status isn't a legal move, the agent
// must get back the actionable "no transition to X (available: …)"
// message — discovered via list-transitions — not a bare failure.
func TestServer_JiraTransition_RejectionPropagates(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The transitions list deliberately omits "Done".
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start Progress","to":{"id":"3","name":"In Progress"}}]}`)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	err := client.JiraTransitionTo(context.Background(), "PROJ-1", "Done")
	if err == nil {
		t.Fatal("expected a workflow-rejection error, got nil")
	}
	if !strings.Contains(err.Error(), "no transition to") || !strings.Contains(err.Error(), "In Progress") {
		t.Errorf("error %q dropped the actionable available-transitions detail", err.Error())
	}
}

// TestServer_JiraTransition_APIErrorPropagates pins fidelity of a raw
// Jira 4xx: the structured error body the API returns when the workflow
// forbids a move reaches the agent intact through the RPC.
func TestServer_JiraTransition_APIErrorPropagates(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"transitions":[{"id":"31","name":"Done","to":{"id":"5","name":"Done"}}]}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errorMessages":["workflow forbids To Do to Done"]}`)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	err := client.JiraTransitionTo(context.Background(), "PROJ-1", "Done")
	if err == nil {
		t.Fatal("expected the API rejection to surface, got nil")
	}
	if !strings.Contains(err.Error(), "workflow forbids To Do to Done") {
		t.Errorf("error %q dropped the structured Jira rejection body", err.Error())
	}
}

// TestLocalClient_JiraGetIssue_DirectPath pins the unchanged local path:
// no socket, the in-process LocalClient builds ForSystem from its own
// stores and calls Jira directly — still bot-attributed.
func TestLocalClient_JiraGetIssue_DirectPath(t *testing.T) {
	rec := &jiraRecorder{}
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"PROJ-7"}`)
	}))
	defer jira.Close()

	client := NewLocal(jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"})

	issue, err := client.JiraGetIssue(context.Background(), "PROJ-7")
	if err != nil {
		t.Fatalf("JiraGetIssue (local): %v", err)
	}
	if issue == nil || issue.Key != "PROJ-7" {
		t.Fatalf("unexpected issue: %+v", issue)
	}
	if got := rec.readAuth(); got != "Bearer org-pat" {
		t.Errorf("local path Authorization = %q, want Bearer org-pat", got)
	}
}

// TestJira_NotConfigured_BothModes pins the friendly mapping: an org with
// no stored Jira credential yields the same "not configured" guidance the
// CLI printed pre-refactor — in local mode directly, and over IPC as the
// response error string.
func TestJira_NotConfigured_BothModes(t *testing.T) {
	info := RunInfo{OrgID: runmode.LocalDefaultOrg, RunID: "run-1"}

	t.Run("local", func(t *testing.T) {
		client := NewLocal(jiraStores("", ""), info)
		_, err := client.JiraGetIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "Jira not configured") {
			t.Fatalf("err = %v, want a friendly 'Jira not configured' message", err)
		}
	})

	t.Run("ipc", func(t *testing.T) {
		client := startJiraDaemon(t, jiraStores("", ""), info)
		_, err := client.JiraGetIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "Jira not configured") {
			t.Fatalf("err = %v, want the friendly message to cross the wire", err)
		}
	})
}
