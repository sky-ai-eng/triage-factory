package agenthost

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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
// compile-satisfy (and panic loudly if ever reached). It keys off the
// canonical integrations.Key* constants — the same source the resolver's
// own (unexported) keys are proven to match by
// internal/jira.TestForSystem_KeysMatchIntegrations — so a key rename
// can't let these tests silently fall through to "not configured".
type fakeJiraSecrets struct {
	db.SecretStore
	url string
	pat string
}

func (f fakeJiraSecrets) GetSystem(_ context.Context, _ string, key string) (string, error) {
	switch key {
	case integrations.KeyJiraURL:
		return f.url, nil
	case integrations.KeyJiraPAT:
		return f.pat, nil
	default:
		return "", nil
	}
}

// fakeJiraOrgs satisfies db.OrgsStore for NewResolver. GetSettingsSystem is the
// one method the recording path reaches (jiraSiteBase, to build a browse URL);
// it returns empty settings so the artifact URL is "" — the same as a
// production org with no Jira base configured. The Jira API calls under test
// touch no other OrgsStore method.
type fakeJiraOrgs struct {
	db.OrgsStore
}

func (fakeJiraOrgs) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, nil
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

// disabledJiraSecrets stands in for TF_ROLE=executor's disabled secret store
// (internal/db/postgres/secrets_disabled.go): every read fails with
// db.ErrSecretStoreUnavailable, the same error the real disabled store
// returns for every method.
type disabledJiraSecrets struct {
	db.SecretStore
}

func (disabledJiraSecrets) GetSystem(context.Context, string, string) (string, error) {
	return "", db.ErrSecretStoreUnavailable
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
	srv := NewServer(stores, info, nil)
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
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

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

// TestLocalClient_JiraGetIssue_ExecutorBundleFirst reproduces the latent bug
// this fix closes, then proves the fix. On TF_ROLE=executor the secret store
// is disabled — every SecretStore method returns db.ErrSecretStoreUnavailable
// — so the ForSystem-resolver path JiraGetIssue used to build its client
// through unconditionally never resolved: Jira verbs failed outright on an
// executor. With the run's ProxyCredentials set — the shape the spawner
// installs via SetProxyCreds once sidecar bring-up returns — the same call
// must instead route through the sidecar's Jira proxy and never consult the
// (disabled) secret store.
func TestLocalClient_JiraGetIssue_ExecutorBundleFirst(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{"summary":"hello"}}`)
	}))
	defer jira.Close()

	stores := db.Stores{Secrets: disabledJiraSecrets{}, Orgs: fakeJiraOrgs{}}
	lc := NewLocal(stores, RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

	t.Run("no proxy creds: hits the disabled secret store (the pre-fix executor failure)", func(t *testing.T) {
		_, err := lc.JiraGetIssue(context.Background(), "PROJ-1")
		if !errors.Is(err, db.ErrSecretStoreUnavailable) {
			t.Fatalf("JiraGetIssue without proxy creds = %v, want an error wrapping db.ErrSecretStoreUnavailable", err)
		}
	})

	t.Run("proxy creds present: routes through the sidecar Jira proxy, never touches the disabled secret store", func(t *testing.T) {
		// The jira server stands in for the sidecar's Jira-REST proxy: the client
		// presents only the placeholder as a Bearer (the proxy injects the real
		// Cloud-Basic / DC-Bearer auth upstream).
		lc.proxyCreds = &ProxyCredentials{JiraAPIURL: jira.URL, JiraAPIToken: "run-placeholder"}
		t.Cleanup(func() { lc.proxyCreds = nil })

		issue, err := lc.JiraGetIssue(context.Background(), "PROJ-1")
		if err != nil {
			t.Fatalf("JiraGetIssue with proxy creds: %v (the disabled secret store must never be consulted)", err)
		}
		if issue == nil || issue.Key != "PROJ-1" {
			t.Fatalf("unexpected issue from the proxy path: %+v", issue)
		}
	})
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
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

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
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

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
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

	newSummary := "rewritten summary"
	err := client.JiraUpdateIssue(context.Background(), "PROJ-1", jiraclient.UpdateIssueFields{Summary: &newSummary})
	if err != nil {
		t.Fatalf("JiraUpdateIssue: %v", err)
	}
	if body := rec.readBody(); !strings.Contains(body, newSummary) {
		t.Errorf("PUT body %q did not carry the updated summary — UpdateIssueFields lost a field crossing the wire", body)
	}
}

// TestServer_JiraTransition_ClientSideRejectionPropagates is the
// error-fidelity acceptance for the client-synthesized rejection: when
// the requested status isn't in the fetched transition list, TransitionTo
// builds the actionable "no transition to X (available: …)" message
// itself, and it must survive the RPC. (The sibling _APIErrorPropagates
// covers the other path — a server-side 4xx on the transition POST.)
func TestServer_JiraTransition_ClientSideRejectionPropagates(t *testing.T) {
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The transitions list deliberately omits "Done".
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"transitions":[{"id":"11","name":"Start Progress","to":{"id":"3","name":"In Progress"}}]}`)
	}))
	defer jira.Close()

	client := startJiraDaemon(t, jiraStores(jira.URL, "org-pat"),
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

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
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

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
		RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"})

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
	info := RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"}

	// Assert on the actionable "run triagefactory" guidance: it's absent
	// from the raw resolver sentinel ("jira: no system credential for
	// org: …"), so its presence proves the friendly remap fired rather
	// than the bare error leaking through.
	t.Run("local", func(t *testing.T) {
		client := NewLocal(jiraStores("", ""), info)
		_, err := client.JiraGetIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "run triagefactory") {
			t.Fatalf("err = %v, want the friendly 'not configured' guidance", err)
		}
	})

	t.Run("ipc", func(t *testing.T) {
		client := startJiraDaemon(t, jiraStores("", ""), info)
		_, err := client.JiraGetIssue(context.Background(), "PROJ-1")
		if err == nil || !strings.Contains(err.Error(), "run triagefactory") {
			t.Fatalf("err = %v, want the friendly guidance to cross the wire", err)
		}
	})
}

// TestProxyJiraClient_FollowsRelayedDeployment pins the executor half of the
// version fix: the sidecar is the only process that can read the org's Jira
// deployment off the sealed bundle, so it relays the classification and the
// proxy client must speak the REST version that classification implies —
// otherwise a Cloud org's comment write leaves the executor in the v2 shape
// v3 rejects. An absent or unrecognized value must stay on v2, which both
// backends serve.
func TestProxyJiraClient_FollowsRelayedDeployment(t *testing.T) {
	tests := []struct {
		name       string
		deployment string
		wantPath   string
		wantBody   string
	}{
		{
			name:       "cloud writes ADF over v3",
			deployment: string(jiraclient.DeploymentCloud),
			wantPath:   "/rest/api/3/issue/PROJ-1/comment",
			wantBody:   `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"looks good"}]}]}}`,
		},
		{
			name:       "data center writes a string over v2",
			deployment: string(jiraclient.DeploymentDataCenter),
			wantPath:   "/rest/api/2/issue/PROJ-1/comment",
			wantBody:   `{"body":"looks good"}`,
		},
		{
			name:       "unset falls back to v2",
			deployment: "",
			wantPath:   "/rest/api/2/issue/PROJ-1/comment",
			wantBody:   `{"body":"looks good"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu         sync.Mutex
				path, body string
			)
			jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				path, body = r.URL.Path, string(raw)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"1"}`)
			}))
			defer jira.Close()

			client, err := proxyJiraClient(&ProxyCredentials{
				JiraAPIURL:     jira.URL,
				JiraAPIToken:   "run-placeholder",
				JiraDeployment: tt.deployment,
			})
			if err != nil {
				t.Fatalf("proxyJiraClient: %v", err)
			}
			if _, err := client.AddComment(context.Background(), "PROJ-1", "looks good"); err != nil {
				t.Fatalf("AddComment: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
			if body != tt.wantBody {
				t.Errorf("body = %s, want %s", body, tt.wantBody)
			}
		})
	}
}
