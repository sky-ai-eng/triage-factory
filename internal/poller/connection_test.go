package poller

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// How a repo's open-PR listing answers on githubConnServer.
const (
	listOK        = "ok"         // 200 with one open PR
	list503       = "503"        // 503 with a JSON body: transient, retried
	listHTML403   = "html403"    // 403 with a proxy's HTML page: transient, not retried
	listJSON404   = "json404"    // 404 with GitHub's JSON body: rejected
	listRateLimit = "rate_limit" // primary budget exhausted until well past the client's wait cap
)

// githubConnServer is a GitHub whose per-repo open-PR listing answers as the
// test sets it, so one cycle can meet any mix of outcomes. GraphQL answers
// every node as inaccessible, which makes Phase 2's refresh a no-op.
type githubConnServer struct {
	mu    sync.Mutex
	modes map[string]string // repo name (no owner) → listing mode
	srv   *httptest.Server
}

func newGitHubConnServer(t *testing.T, modes map[string]string) *githubConnServer {
	t.Helper()
	s := &githubConnServer{modes: modes}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *githubConnServer) set(repo, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modes[repo] = mode
}

func (s *githubConnServer) setAll(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for repo := range s.modes {
		s.modes[repo] = mode
	}
}

func (s *githubConnServer) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/graphql") {
		_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[len(parts)-1] != "pulls" {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	owner, repo := parts[len(parts)-3], parts[len(parts)-2]
	s.mu.Lock()
	mode := s.modes[repo]
	s.mu.Unlock()
	switch mode {
	case listOK:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{"number":1,"node_id":"PR_%[2]s","title":"Change","state":"open",
			"html_url":"https://github.com/%[1]s/%[2]s/pull/1","updated_at":"2026-09-30T12:00:00Z",
			"user":{"login":"alice"},
			"head":{"sha":"abc","ref":"feature","repo":{"full_name":"%[1]s/%[2]s"}},
			"base":{"ref":"main","repo":{"full_name":"%[1]s/%[2]s"}}}]`, owner, repo)
	case list503:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"Service Unavailable"}`))
	case listHTML403:
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><body><h1>403 Forbidden</h1><p>Connect to the corporate VPN.</p></body></html>`))
	case listJSON404:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`))
	case listRateLimit:
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	default:
		http.Error(w, "unexpected repo", http.StatusInternalServerError)
	}
}

// eventRecorder is a tracker.Publisher that keeps every event a cycle
// published.
type eventRecorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (p *eventRecorder) Publish(_ context.Context, evt domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, evt)
}

func (p *eventRecorder) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	p.Publish(ctx, evt)
}

// pollCompletions counts the system:poll:completed sentinels published, then
// forgets every event, so each cycle is asserted on its own.
func (p *eventRecorder) pollCompletions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e.EventType == domain.EventSystemPollCompleted {
			n++
		}
	}
	p.events = nil
	return n
}

// logCapture collects log output for assertions on the logging contract.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// captureLogs redirects every component logger into a buffer at level for the
// rest of the test.
func captureLogs(t *testing.T, level slog.Level) *logCapture {
	t.Helper()
	c := &logCapture{}
	restore := logging.SetOutput(c)
	prev := logging.Level()
	logging.SetLevel(level)
	t.Cleanup(func() {
		logging.SetLevel(prev)
		restore()
	})
	return c
}

// count returns how many lines logged at level carry msg, and forgets the
// output, so each cycle is asserted on its own. It reads both handler
// formats: text (level=WARN) and JSON ("level":"WARN").
func (c *logCapture) count(level slog.Level, msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.Contains(line, msg) &&
			(strings.Contains(line, "level="+level.String()) || strings.Contains(line, `"level":"`+level.String()+`"`)) {
			n++
		}
	}
	return n
}

func (c *logCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// connectionFixture is one org polling the repos the server knows, through a
// PAT, with its connection state persisted to a real store.
type connectionFixture struct {
	m      *Manager
	stores dbpkg.Stores
	pub    *eventRecorder
	org    string
}

func newConnectionFixture(t *testing.T, srv *githubConnServer, repos ...string) *connectionFixture {
	t.Helper()
	// Multi mode keeps the PAT path off the local-only reads (the session
	// user's login and teams, the dashboard backfill) this test is not about.
	runmode.SetForTest(t, runmode.ModeMulti)
	ghclient.SetTransientBackoffForTest(t, time.Millisecond)
	database := newMigratedSQLiteForPoller(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, repos)
	pub := &eventRecorder{}
	m := &Manager{
		database: database, pub: pub,
		tasks: stores.Tasks, entities: stores.Entities, eventQueue: stores.EventQueue,
		repos: stores.Repos, orgs: stores.Orgs, users: stores.Users,
		connections: stores.PollReadiness,
		resolver:    &fakeResolver{client: ghclient.NewClient(srv.srv.URL, "pat")},
	}
	return &connectionFixture{m: m, stores: stores, pub: pub, org: org}
}

func (f *connectionFixture) connection(t *testing.T) dbpkg.ConnectionStatus {
	t.Helper()
	st, err := f.stores.PollReadiness.Connection(context.Background(), f.org, "github")
	if err != nil {
		t.Fatalf("Connection: %v", err)
	}
	return st
}

func (f *connectionFixture) lastSuccess() time.Time {
	return f.m.successSnapshot("github")[f.org]
}

// TestGitHubCycle_ConnectionLostOnceAndRestoredOnce walks one outage end to
// end: a healthy cycle, two cycles whose every listing gets a 503, and a
// healthy cycle again. The outage is one WARN when it starts and one INFO when
// it ends, and the cycles inside it are neither successful polls nor
// completions.
func TestGitHubCycle_ConnectionLostOnceAndRestoredOnce(t *testing.T) {
	srv := newGitHubConnServer(t, map[string]string{"a": listOK, "b": listOK})
	f := newConnectionFixture(t, srv, "octo/a", "octo/b")
	logs := captureLogs(t, slog.LevelInfo)
	ctx := context.Background()

	f.m.runGitHubCycleForOrg(ctx, f.org)
	if st := f.connection(t); st.State != dbpkg.ConnectionUp {
		t.Fatalf("after a healthy cycle the state is %+v, want up", st)
	}
	if n := f.pub.pollCompletions(); n != 1 {
		t.Fatalf("a healthy cycle published %d poll completions, want 1", n)
	}
	healthyAt := f.lastSuccess()
	if healthyAt.IsZero() {
		t.Fatal("a healthy cycle was not stamped as a successful poll")
	}
	if n := logs.count(slog.LevelWarn, "github connection lost"); n != 0 {
		t.Fatalf("a healthy cycle logged %d connection-lost lines", n)
	}

	// Every listing fails.
	srv.setAll(list503)
	f.m.runGitHubCycleForOrg(ctx, f.org)
	down := f.connection(t)
	if down.State != dbpkg.ConnectionDown || down.FailureClass != string(upstream.Transient) {
		t.Fatalf("after every listing got a 503 the state is %+v, want down/transient", down)
	}
	if got := f.lastSuccess(); !got.Equal(healthyAt) {
		t.Errorf("a cycle that reached nothing was stamped as a successful poll (%v, was %v)", got, healthyAt)
	}
	if n := f.pub.pollCompletions(); n != 0 {
		t.Errorf("a cycle that reached nothing published %d poll completions, want 0", n)
	}
	if n := logs.count(slog.LevelWarn, "github connection lost"); n != 1 {
		t.Errorf("the outage's first cycle logged %d connection-lost WARNs, want exactly 1\n%s", n, logs)
	}
	if n := logs.count(slog.LevelError, "github"); n != 0 {
		t.Errorf("the outage logged %d ERROR lines; upstream failures belong at DEBUG\n%s", n, logs)
	}
	logs.reset()

	// Still failing: nothing new to say.
	f.m.runGitHubCycleForOrg(ctx, f.org)
	if st := f.connection(t); st.State != dbpkg.ConnectionDown || st.ChangedAt == nil || !st.ChangedAt.Equal(*down.ChangedAt) {
		t.Errorf("a second failing cycle left %+v, want down since %v", st, down.ChangedAt)
	}
	if n := logs.count(slog.LevelWarn, "github connection lost"); n != 0 {
		t.Errorf("a second failing cycle logged %d connection-lost WARNs, want 0", n)
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") ||
		strings.Contains(out, `"level":"WARN"`) || strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("a second failing cycle logged above INFO:\n%s", out)
	}
	logs.reset()

	// Back.
	srv.setAll(listOK)
	f.m.runGitHubCycleForOrg(ctx, f.org)
	if st := f.connection(t); st.State != dbpkg.ConnectionUp || st.FailureClass != "" {
		t.Errorf("after recovering the state is %+v, want up with no failure class", st)
	}
	if n := logs.count(slog.LevelInfo, "github connection restored"); n != 1 {
		t.Errorf("the first healthy cycle logged %d connection-restored INFOs, want exactly 1\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "down_for") {
		t.Errorf("the restored line carries no down_for:\n%s", logs)
	}
	if got := f.lastSuccess(); !got.After(healthyAt) {
		t.Errorf("the recovering cycle was not stamped as a successful poll")
	}
	if n := f.pub.pollCompletions(); n != 1 {
		t.Errorf("the recovering cycle published %d poll completions, want 1", n)
	}
}

// TestGitHubCycle_OneRepoAnsweringIsUp: one repo's listing fails and another
// succeeds. The upstream answered, so the connection is up and nothing is
// logged about it.
func TestGitHubCycle_OneRepoAnsweringIsUp(t *testing.T) {
	srv := newGitHubConnServer(t, map[string]string{"a": list503, "b": listOK})
	f := newConnectionFixture(t, srv, "octo/a", "octo/b")
	logs := captureLogs(t, slog.LevelInfo)

	f.m.runGitHubCycleForOrg(context.Background(), f.org)

	if st := f.connection(t); st.State != dbpkg.ConnectionUp {
		t.Errorf("state = %+v, want up", st)
	}
	if n := logs.count(slog.LevelWarn, "github connection lost"); n != 0 {
		t.Errorf("logged %d connection-lost WARNs for a cycle the upstream answered", n)
	}
	if f.lastSuccess().IsZero() {
		t.Error("a cycle the upstream answered was not stamped as a successful poll")
	}
}

// TestGitHubCycle_OnlyRateLimitsLeaveTheStateAlone: a rate limit is the
// upstream answering "wait", with a resume cursor behind it, and says nothing
// about the connection, so whatever was stored stays.
func TestGitHubCycle_OnlyRateLimitsLeaveTheStateAlone(t *testing.T) {
	for _, before := range []dbpkg.ConnectionState{dbpkg.ConnectionUp, dbpkg.ConnectionDown} {
		t.Run(string(before), func(t *testing.T) {
			srv := newGitHubConnServer(t, map[string]string{"a": listRateLimit, "b": listRateLimit})
			f := newConnectionFixture(t, srv, "octo/a", "octo/b")
			logs := captureLogs(t, slog.LevelInfo)
			seeded, _, err := f.stores.PollReadiness.RecordConnection(context.Background(), f.org, "github", before, string(upstream.Transient))
			if err != nil {
				t.Fatalf("seed state: %v", err)
			}

			f.m.runGitHubCycleForOrg(context.Background(), f.org)

			st := f.connection(t)
			if st.State != before || st.ChangedAt == nil || !st.ChangedAt.Equal(*seeded.ChangedAt) {
				t.Errorf("state after a rate-limited cycle = %+v, want %s since %v, untouched", st, before, seeded.ChangedAt)
			}
			if n := logs.count(slog.LevelWarn, "connection lost") + logs.count(slog.LevelInfo, "connection restored"); n != 0 {
				t.Errorf("a rate-limited cycle logged %d connection changes\n%s", n, logs)
			}
		})
	}
}

// TestGitHubCycle_HTMLForbiddenIsAnOutageNotAMissingRepo: a VPN proxy in front
// of GHES answers every listing with a 403 and an HTML page. That is not GitHub
// saying the token cannot see the repos; it is GitHub not being reached. The
// connection goes down as transient, and no repo is skipped as unreachable.
func TestGitHubCycle_HTMLForbiddenIsAnOutageNotAMissingRepo(t *testing.T) {
	srv := newGitHubConnServer(t, map[string]string{"a": listHTML403, "b": listHTML403})
	f := newConnectionFixture(t, srv, "octo/a", "octo/b")
	logs := captureLogs(t, slog.LevelDebug)

	f.m.runGitHubCycleForOrg(context.Background(), f.org)

	if st := f.connection(t); st.State != dbpkg.ConnectionDown || st.FailureClass != string(upstream.Transient) {
		t.Errorf("state = %+v, want down/transient", st)
	}
	out := logs.String()
	if strings.Contains(out, "repo unreachable") {
		t.Errorf("a proxy's HTML 403 took the repo-unreachable path:\n%s", out)
	}
	if n := logs.count(slog.LevelDebug, "discovery: list open PRs failed"); n != 2 {
		t.Errorf("logged %d failed-listing DEBUG lines, want one per repo\n%s", n, out)
	}
	if strings.Contains(out, "<html>") || strings.Contains(out, "corporate VPN") {
		t.Errorf("the proxy's page reached the log:\n%s", out)
	}
	if !f.lastSuccess().IsZero() {
		t.Error("a cycle that reached nothing was stamped as a successful poll")
	}
}

// TestGitHubCycle_JSON404RepoIsSkippedAndTheConnectionIsUp: GitHub itself
// answering 404 for one repo is an answer about that repo. It is skipped as
// unreachable, the other repo is polled, and the connection is up.
func TestGitHubCycle_JSON404RepoIsSkippedAndTheConnectionIsUp(t *testing.T) {
	srv := newGitHubConnServer(t, map[string]string{"a": listJSON404, "b": listOK})
	f := newConnectionFixture(t, srv, "octo/a", "octo/b")
	logs := captureLogs(t, slog.LevelDebug)
	ctx := context.Background()

	f.m.runGitHubCycleForOrg(ctx, f.org)

	if st := f.connection(t); st.State != dbpkg.ConnectionUp {
		t.Errorf("state = %+v, want up", st)
	}
	if n := logs.count(slog.LevelDebug, "discovery: repo unreachable"); n != 1 {
		t.Errorf("logged %d repo-unreachable lines, want 1 for the 404 repo\n%s", n, logs)
	}
	ents, err := f.stores.Entities.ListActiveSystem(ctx, f.org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 || ents[0].SourceID != "octo/b#1" {
		t.Errorf("entities = %+v, want only octo/b#1", ents)
	}
}

// TestGitHubCycle_EveryRepoMissingIsNotAnOutage: every listing 404s. Each is
// GitHub answering about its own repo, so discovery is not a failure and the
// connection is up.
func TestGitHubCycle_EveryRepoMissingIsNotAnOutage(t *testing.T) {
	srv := newGitHubConnServer(t, map[string]string{"a": listJSON404, "b": listJSON404})
	f := newConnectionFixture(t, srv, "octo/a", "octo/b")

	f.m.runGitHubCycleForOrg(context.Background(), f.org)

	if st := f.connection(t); st.State != dbpkg.ConnectionUp {
		t.Errorf("state = %+v, want up", st)
	}
	if f.lastSuccess().IsZero() {
		t.Error("a cycle GitHub answered was not stamped as a successful poll")
	}
}

// TestCycleConnection_Outcome pins the outcome table, including the failures
// of calls no client counts (the App's token mint), which the poller notes.
func TestCycleConnection_Outcome(t *testing.T) {
	transient := &ghclient.HTTPError{StatusCode: http.StatusBadGateway, Class: upstream.Transient}
	auth := &ghclient.HTTPError{StatusCode: http.StatusUnauthorized, Class: upstream.Auth}
	for _, tc := range []struct {
		name      string
		record    []upstream.Class
		note      []error
		wantState dbpkg.ConnectionState
		wantClass upstream.Class
	}{
		{name: "no requests"},
		{name: "only rate limits", record: []upstream.Class{upstream.RateLimited, upstream.RateLimited}},
		{name: "an ok beats failures", record: []upstream.Class{upstream.Transient, upstream.Auth, upstream.OK}, wantState: dbpkg.ConnectionUp},
		{name: "a rejection is an answer", record: []upstream.Class{upstream.Transient, upstream.Rejected}, wantState: dbpkg.ConnectionUp},
		{name: "auth beats transient", record: []upstream.Class{upstream.Transient, upstream.Auth, upstream.RateLimited}, wantState: dbpkg.ConnectionDown, wantClass: upstream.Auth},
		{name: "transient", record: []upstream.Class{upstream.Transient, upstream.RateLimited}, wantState: dbpkg.ConnectionDown, wantClass: upstream.Transient},
		{name: "a failed mint counts", note: []error{transient}, wantState: dbpkg.ConnectionDown, wantClass: upstream.Transient},
		{name: "a refused mint counts as auth", note: []error{auth}, record: []upstream.Class{upstream.Transient}, wantState: dbpkg.ConnectionDown, wantClass: upstream.Auth},
		{name: "a local failure is not noted", note: []error{ghclient.ErrNoGitHubCredentials}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, conn := newCycleConnection(context.Background())
			for _, c := range tc.record {
				upstream.Record(ctx, upstream.GitHub, "org-1", c)
			}
			for _, err := range tc.note {
				conn.note(err)
			}
			state, class, observed := conn.outcome()
			if observed != (tc.wantState != "") || state != tc.wantState || class != tc.wantClass {
				t.Errorf("outcome = (%q, %q, %v), want (%q, %q, %v)", state, class, observed, tc.wantState, tc.wantClass, tc.wantState != "")
			}
		})
	}
}
