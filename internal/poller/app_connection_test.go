package poller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// appHost is a GHES behind a proxy that, while down, answers every request
// with the proxy's 502 page; while up, it mints installation tokens and
// serves one installation granting octo/a, whose listing has one open PR.
type appHost struct {
	down atomic.Bool
	srv  *httptest.Server
}

func newAppHost(t *testing.T) *appHost {
	t.Helper()
	h := &appHost{}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *appHost) serve(w http.ResponseWriter, r *http.Request) {
	if h.down.Load() {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1>" + strings.Repeat("<p>nginx</p>", 200) + "</body></html>"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch p := r.URL.Path; {
	case strings.HasSuffix(p, "/access_tokens"):
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case strings.HasSuffix(p, "/app/installations"):
		_, _ = w.Write([]byte(`[{"id":1,"account":{"id":7,"login":"octo","type":"Organization"}}]`))
	case strings.HasSuffix(p, "/installation/repositories"):
		_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"id":11,"full_name":"octo/a"}]}`))
	case strings.HasSuffix(p, "/graphql"):
		_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
	case strings.HasSuffix(p, "/pulls"):
		_, _ = w.Write([]byte(`[{"number":1,"node_id":"PR_a","title":"Change","state":"open",
			"html_url":"https://github.com/octo/a/pull/1","updated_at":"2026-09-30T12:00:00Z",
			"user":{"login":"alice"},
			"head":{"sha":"abc","ref":"feature","repo":{"full_name":"octo/a"}},
			"base":{"ref":"main","repo":{"full_name":"octo/a"}}}]`))
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// mintingResolver resolves an installation's client the way the production
// resolver does on a cache miss: it mints an installation token through a
// real githubapp.Minter against the org's host and returns the mint's own
// error when that fails.
type mintingResolver struct {
	minter *githubapp.Minter
	url    string
}

func (r *mintingResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	tok, err := r.minter.MintInstallationToken(ctx, 1)
	if err != nil {
		return nil, err
	}
	return ghclient.NewClient(r.url, tok.Value).WithOrg(orgID), nil
}

func (r *mintingResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	return r.ClientFor(ctx, orgID, owner)
}

func (r *mintingResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	return r.minter.MintInstallationToken(ctx, 1)
}

func (r *mintingResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	return r.url, nil
}

func (r *mintingResolver) OrgIdentityFor(ctx context.Context, orgID string) (string, string, bool) {
	return "", "", false
}

// TestGitHubAppCycle_MintOutageIsAConnectionLoss: an App org's host goes
// behind a proxy that answers 502, so the installation listing at the top of
// the cycle and the installation-token mint both fail before any client
// request is made. That is the connection being lost: it is recorded down as
// transient with one WARN, the cycles inside the outage log nothing above
// INFO, and the first cycle that mints again restores it with one INFO.
func TestGitHubAppCycle_MintOutageIsAConnectionLoss(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	host := newAppHost(t)
	minter, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: testAppKey(t),
		AppID:      1,
		APIBase:    ghbase.APIBase(host.srv.URL),
		HTTPClient: host.srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}

	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/a"})
	seedBYOAppCredentialClass(t, stores, org)
	pub := &eventRecorder{}
	m := &Manager{
		database: database, pub: pub,
		tasks: stores.Tasks, entities: stores.Entities, eventQueue: stores.EventQueue,
		repos: stores.Repos, orgs: stores.Orgs, users: stores.Users,
		connections: stores.PollReadiness,
		apps: &fakeInstallsStore{
			app:      &domain.OrgGitHubApp{OrgID: org, AppID: "1", Active: true},
			installs: []domain.OrgGitHubAppInstallation{{InstallationID: "1", AccountLogin: "octo"}},
		},
		resolver: &mintingResolver{minter: minter, url: host.srv.URL},
		ReconcileGrant: func(ctx context.Context, _ string) error {
			_, err := minter.ListInstallations(ctx)
			return err
		},
	}
	connection := func() dbpkg.ConnectionStatus {
		t.Helper()
		st, err := stores.PollReadiness.Connection(context.Background(), org, "github")
		if err != nil {
			t.Fatalf("Connection: %v", err)
		}
		return st
	}
	aboveInfo := func(out string) bool {
		return strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") ||
			strings.Contains(out, `"level":"WARN"`) || strings.Contains(out, `"level":"ERROR"`)
	}
	logs := captureLogs(t, slog.LevelInfo)
	ctx := context.Background()

	m.runGitHubCycleForOrg(ctx, org)
	if st := connection(); st.State != dbpkg.ConnectionUp {
		t.Fatalf("after a healthy cycle the state is %+v, want up\n%s", st, logs)
	}
	logs.reset()

	host.down.Store(true)
	m.runGitHubCycleForOrg(ctx, org)
	if st := connection(); st.State != dbpkg.ConnectionDown || st.FailureClass != string(upstream.Transient) {
		t.Errorf("after the mint met a 502 the state is %+v, want down/transient", st)
	}
	if n := logs.count(slog.LevelWarn, "github connection lost"); n != 1 {
		t.Errorf("the outage's first cycle logged %d connection-lost WARNs, want 1\n%s", n, logs)
	}
	if n := logs.count(slog.LevelError, ""); n != 0 {
		t.Errorf("the outage's first cycle logged %d ERROR lines; an unreachable host is reported by the connection line\n%s", n, logs)
	}
	if out := logs.String(); strings.Contains(out, "<html>") || strings.Contains(out, "nginx") {
		t.Errorf("the proxy's page reached the log:\n%s", out)
	}
	logs.reset()

	m.runGitHubCycleForOrg(ctx, org)
	if out := logs.String(); aboveInfo(out) {
		t.Errorf("a second cycle inside the outage logged above INFO:\n%s", out)
	}
	logs.reset()

	host.down.Store(false)
	m.runGitHubCycleForOrg(ctx, org)
	if st := connection(); st.State != dbpkg.ConnectionUp {
		t.Errorf("after the host came back the state is %+v, want up", st)
	}
	if n := logs.count(slog.LevelInfo, "github connection restored"); n != 1 {
		t.Errorf("the recovering cycle logged %d connection-restored INFOs, want 1\n%s", n, logs)
	}
}

func testAppKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}
