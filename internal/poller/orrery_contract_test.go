package poller_test

// The GitHub poll path against Orrery (github.com/sky-ai-eng/orrery), a
// deterministic GitHub served from real git: a real poller.Manager over real
// SQLite stores polls a world built through Orrery's GitHub API, and every
// transition the world goes through must come out as exactly the events the
// tracker's taxonomy (docs/concepts/tracked-events.md) promises.
//
// Opt-in: it runs only when ORRERY_DIR names an Orrery checkout, the variable
// e2e/sso/run.sh reads too, and skips otherwise, so a contributor without one
// never runs it. A relative ORRERY_DIR is read from the repository root. The
// test builds Orrery there with cargo (a no-op when nothing changed) and serves
// the binary it built. Locally, with Orrery checked out beside this
// repository:
//
//	ORRERY_DIR=../orrery go test ./internal/poller -run TestPollerAgainstOrrery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestPollerAgainstOrrery(t *testing.T) {
	dir := os.Getenv("ORRERY_DIR")
	if dir == "" {
		t.Skip("ORRERY_DIR is not set; see the comment at the top of this file")
	}
	bin := buildOrrery(t, dir)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()

	w := startOrrery(t, bin)
	w.actor("acme", "org")
	alice := w.actor("alice", "user")
	bob := w.actor("bob", "user")
	carol := w.actor("carol", "user")
	ci := w.actor("ci", "bot")
	w.githubREST(alice, "POST", "/api/v3/orgs/acme/repos", map[string]any{"name": "webapp"}, 201)

	// main, then three branches off it: one for checks, one that will
	// conflict with main, one for reviews. main then changes the line the
	// conflict branch changed.
	repo := w.clone(t, alice, "acme/webapp")
	repo.commit("app.txt", "version 1\n", "init")
	repo.push("main")
	repo.branch("checks", "main")
	repo.commit("checks.txt", "checks\n", "add checks")
	repo.branch("conflict", "main")
	repo.commit("app.txt", "version 2 (branch)\n", "bump on a branch")
	repo.branch("review", "main")
	repo.commit("review.txt", "review\n", "add review")
	repo.push("checks", "conflict", "review")
	repo.checkout("main")
	repo.commit("app.txt", "version 2 (main)\n", "bump on main")
	repo.push("main")

	pulls := "/api/v3/repos/acme/webapp/pulls"
	checksPR := w.githubREST(alice, "POST", pulls, map[string]any{"title": "Checks", "head": "checks", "base": "main"}, 201)
	w.githubREST(alice, "POST", pulls, map[string]any{"title": "Conflict", "head": "conflict", "base": "main"}, 201)
	reviewPR := w.githubREST(alice, "POST", pulls, map[string]any{"title": "Review", "head": "review", "base": "main"}, 201)

	checkRuns := "/api/v3/repos/acme/webapp/check-runs"
	checksHead := sha(checksPR)
	w.githubREST(ci, "POST", checkRuns, map[string]any{"name": "ci/test", "head_sha": checksHead, "status": "completed", "conclusion": "success"}, 201)
	w.githubREST(ci, "POST", checkRuns, map[string]any{"name": "ci/lint", "head_sha": checksHead, "status": "completed", "conclusion": "failure"}, 201)
	running := w.githubREST(ci, "POST", checkRuns, map[string]any{"name": "ci/test", "head_sha": sha(reviewPR), "status": "in_progress"}, 201)
	w.githubREST(bob, "POST", pulls+"/1/reviews", map[string]any{"event": "APPROVE", "body": "ship it"}, 200)
	w.githubREST(bob, "POST", pulls+"/2/reviews", map[string]any{"event": "COMMENT", "body": "this one conflicts"}, 200)
	w.githubREST(carol, "POST", pulls+"/3/reviews", map[string]any{"event": "REQUEST_CHANGES", "body": "needs a test"}, 200)

	p := newPollRig(t, ctx, w)

	// The first sight of each pull request reports what is already on it.
	p.cycle(t, ctx, map[string]int{
		"acme/webapp#1 " + domain.EventGitHubPRCICheckPassed:          1,
		"acme/webapp#1 " + domain.EventGitHubPRCICheckFailed:          1,
		"acme/webapp#1 " + domain.EventGitHubPRReviewApproved:         1,
		"acme/webapp#2 " + domain.EventGitHubPRConflicts:              1,
		"acme/webapp#2 " + domain.EventGitHubPRReviewCommented:        1,
		"acme/webapp#3 " + domain.EventGitHubPRReviewChangesRequested: 1,
	})

	// Nothing changed: no events, and the pull request list answers 304.
	pullsList := "GET /api/v3/repos/acme/webapp/pulls"
	before := p.tr.count(pullsList, http.StatusNotModified)
	p.cycle(t, ctx, map[string]int{})
	if p.tr.count(pullsList, http.StatusNotModified) == before {
		t.Error("the unchanged cycle's pull request list was not a 304")
	}

	// Every kind of change the world can make through GitHub's API today.
	w.githubREST(ci, "PATCH", fmt.Sprintf("%s/%v", checkRuns, running["id"]), map[string]any{"status": "completed", "conclusion": "success"}, 200)
	w.githubREST(bob, "POST", pulls+"/3/reviews", map[string]any{"event": "APPROVE", "body": "thanks"}, 200)
	w.githubREST(carol, "POST", "/api/v3/repos/acme/webapp/issues/3/comments", map[string]any{"body": "merging after lunch"}, 201)
	w.githubREST(alice, "PATCH", pulls+"/3", map[string]any{"body": "Adds review.txt."}, 200)
	repo.checkout("checks")
	repo.commit("checks.txt", "checks, again\n", "another commit")
	repo.push("checks")
	w.githubREST(alice, "PATCH", pulls+"/2", map[string]any{"state": "closed"}, 200)
	p.cycle(t, ctx, map[string]int{
		"acme/webapp#1 " + domain.EventGitHubPRNewCommits:     1,
		"acme/webapp#2 " + domain.EventGitHubPRClosed:         1,
		"acme/webapp#3 " + domain.EventGitHubPRCICheckPassed:  1,
		"acme/webapp#3 " + domain.EventGitHubPRReviewApproved: 1,
		"acme/webapp#3 " + domain.EventGitHubPRCommented:      1,
		"acme/webapp#3 " + domain.EventGitHubPRBodyUpdated:    1,
	})

	w.githubREST(alice, "PUT", pulls+"/3/merge", map[string]any{}, 200)
	p.cycle(t, ctx, map[string]int{
		"acme/webapp#3 " + domain.EventGitHubPRMerged: 1,
	})

	if n := len(p.tr.notMocked()); n > 0 {
		t.Errorf("%d request(s) reached surface Orrery does not mock: %v", n, p.tr.notMocked())
	}
}

// --- the poller --------------------------------------------------------------

type pollRig struct {
	mgr    *poller.Manager
	stores db.Stores
	orgID  string
	pub    *recordingPublisher
	tr     *countingTransport
	errs   []string
	errsMu sync.Mutex
}

// newPollRig wires a poller over in-memory SQLite, tracking acme/webapp on the
// org whose GitHub is the Orrery world, the way the poll-bench harness does.
func newPollRig(t *testing.T, ctx context.Context, w *orreryWorld) *pollRig {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	database.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatal(err)
	}
	stores := sqlitestore.New(database)
	orgID := runmode.LocalDefaultOrgID
	if _, err := stores.Orgs.SetSourceBaseURL(ctx, orgID, "github", w.base); err != nil {
		t.Fatal(err)
	}
	host, err := db.OrgGitHubHostSystem(ctx, stores.Orgs, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, orgID, runmode.LocalDefaultTeamID, host,
		[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "webapp"}}); err != nil {
		t.Fatal(err)
	}

	// The poll credential is an actor of its own, as an org's PAT would be.
	token := w.actor("tf-poller", "user")
	tr := &countingTransport{base: http.DefaultTransport}
	resolver := &orreryResolver{
		client: ghclient.NewClientWithHTTPClient(w.base, token, &http.Client{Transport: tr}),
		base:   w.base,
	}
	r := &pollRig{stores: stores, orgID: orgID, pub: &recordingPublisher{}, tr: tr}
	r.mgr = poller.NewManager(database, r.pub,
		stores.Users, stores.Tasks, stores.Entities, stores.Repos, stores.Artifacts, stores.EventQueue, stores.Orgs,
		stores.JiraStatusRules, stores.LinearTeamRules, stores.TeamGitHubGroups, stores.Secrets, stores.GitHubApps, stores.PollReadiness,
		resolver, nil)
	r.mgr.OnError = func(source, org string, err error) {
		r.errsMu.Lock()
		r.errs = append(r.errs, fmt.Sprintf("%s: %v", source, err))
		r.errsMu.Unlock()
	}
	return r
}

// cycle runs one GitHub poll cycle and compares the github:* events it
// emitted, keyed "<source id> <event type>", with want.
func (r *pollRig) cycle(t *testing.T, ctx context.Context, want map[string]int) {
	t.Helper()
	r.pub.take()
	r.mgr.PollGitHubOnce(ctx, r.orgID)
	r.errsMu.Lock()
	errs := r.errs
	r.errs = nil
	r.errsMu.Unlock()
	for _, e := range errs {
		t.Errorf("poll cycle error: %s", e)
	}

	got := map[string]int{}
	for _, evt := range r.pub.take() {
		key := evt.EventType
		if evt.EntityID != nil {
			e, err := r.stores.Entities.GetSystem(ctx, r.orgID, *evt.EntityID)
			if err == nil && e != nil {
				key = e.SourceID + " " + evt.EventType
			}
		}
		got[key]++
	}
	if !equalCounts(got, want) {
		t.Errorf("events:\n got  %s\n want %s", describe(got), describe(want))
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func describe(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		keys = append(keys, fmt.Sprintf("%s ×%d", k, v))
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}

// recordingPublisher keeps every github:* event the tracker emits, on either
// side of its durable write.
type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (p *recordingPublisher) Publish(_ context.Context, evt domain.Event) {
	if !strings.HasPrefix(evt.EventType, "github:") {
		return
	}
	p.mu.Lock()
	p.events = append(p.events, evt)
	p.mu.Unlock()
}

func (p *recordingPublisher) PublishPreEnqueued(ctx context.Context, evt domain.Event) {
	p.Publish(ctx, evt)
}

func (p *recordingPublisher) take() []domain.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.events
	p.events = nil
	return out
}

// orreryResolver hands every poll-path lookup the one client pointed at the
// world.
type orreryResolver struct {
	client *ghclient.Client
	base   string
}

func (r *orreryResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *orreryResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r *orreryResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (r *orreryResolver) BaseURLFor(context.Context, string) (string, error) {
	return r.base, nil
}

func (r *orreryResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}

// countingTransport counts responses by request and status, and keeps the
// requests Orrery answered with its 501 not-mocked envelope.
type countingTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	counts   map[string]int // "<method> <path> <status>"
	unmocked []string
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[fmt.Sprintf("%s %s %d", req.Method, req.URL.Path, resp.StatusCode)]++
	if resp.StatusCode == http.StatusNotImplemented {
		c.unmocked = append(c.unmocked, req.Method+" "+req.URL.Path)
	}
	c.mu.Unlock()
	return resp, nil
}

// count is how many responses to request ("<method> <path>") had status.
func (c *countingTransport) count(request string, status int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[fmt.Sprintf("%s %d", request, status)]
}

func (c *countingTransport) notMocked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.unmocked...)
}

// --- the world ---------------------------------------------------------------

type orreryWorld struct {
	t    *testing.T
	base string
}

// buildOrrery builds the orrery binary in the checkout at dir and returns its
// path. Having opted in, a missing checkout or toolchain fails the test rather
// than skipping it.
func buildOrrery(t *testing.T, dir string) string {
	t.Helper()
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoRoot(t), dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err != nil {
		t.Fatalf("ORRERY_DIR=%s is not an Orrery checkout: %v", dir, err)
	}
	cmd := exec.Command("cargo", "build", "--quiet", "-p", "orrery")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cargo build in %s: %v\n%s", dir, err, out)
	}
	target := os.Getenv("CARGO_TARGET_DIR")
	switch {
	case target == "":
		target = filepath.Join(dir, "target")
	case !filepath.IsAbs(target):
		target = filepath.Join(dir, target)
	}
	return filepath.Join(target, "debug", "orrery")
}

// repoRoot is the directory holding this module's go.mod. go test runs in the
// package's directory, so a relative path is read from there by default.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no go.mod above %s", wd)
		}
	}
}

// startOrrery serves a fresh world on a free loopback port for the length of
// the test. The port is found by binding port 0 and closing it before Orrery
// binds it, so another process can take it in between; Orrery then exits on
// its bind error and the start is retried on a new port.
func startOrrery(t *testing.T, bin string) *orreryWorld {
	t.Helper()
	var lastLog string
	for attempt := 0; attempt < 3; attempt++ {
		w, log, ok := tryStartOrrery(t, bin)
		if ok {
			return w
		}
		lastLog = log
	}
	t.Fatalf("orrery did not start in 3 attempts; last log:\n%s", lastLog)
	return nil
}

func tryStartOrrery(t *testing.T, bin string) (*orreryWorld, string, bool) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "orrery.toml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("[instance]\ndir = %q\n\n[server]\nlisten = %q\n", filepath.Join(dir, "instance"), addr)), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	cmd := exec.Command(bin, "serve", "--config", cfg)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orrery: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
	}

	w := &orreryWorld{t: t, base: "http://" + addr}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-exited:
			return nil, logs.String(), false
		default:
		}
		resp, err := http.Get(w.base + "/_orrery/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Cleanup(func() {
					stop()
					if t.Failed() {
						t.Logf("orrery log:\n%s", logs.String())
					}
				})
				return w, "", true
			}
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("orrery did not become healthy at %s:\n%s", w.base, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuffer is Orrery's log: os/exec copies the process's output into it from
// its own goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// actor mints an actor on the control plane and returns its token.
func (w *orreryWorld) actor(login, kind string) string {
	w.t.Helper()
	out := w.call("", "POST", "/_orrery/actors", map[string]any{"login": login, "kind": kind}, http.StatusCreated)
	token, _ := out["token"].(string)
	return token
}

// githubREST calls the GitHub REST API Orrery serves, as the actor holding
// token.
func (w *orreryWorld) githubREST(token, method, path string, body map[string]any, want int) map[string]any {
	w.t.Helper()
	return w.call(token, method, path, body, want)
}

func (w *orreryWorld) call(token, method, path string, body map[string]any, want int) map[string]any {
	w.t.Helper()
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.base+path, reqBody)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		w.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, got)
	}
	out := map[string]any{}
	_ = json.Unmarshal(got, &out)
	return out
}

func (w *orreryWorld) clock() int64 {
	w.t.Helper()
	out := w.call("", "GET", "/_orrery/clock", nil, http.StatusOK)
	n, _ := out["clock_unix"].(float64)
	return int64(n)
}

func sha(pull map[string]any) string {
	head, _ := pull["head"].(map[string]any)
	s, _ := head["sha"].(string)
	return s
}

// gitRepo is a working copy whose commits are dated by the world's clock, so
// the world stays deterministic.
type gitRepo struct {
	w      *orreryWorld
	dir    string
	remote string
}

func (w *orreryWorld) clone(t *testing.T, token, fullName string) *gitRepo {
	t.Helper()
	r := &gitRepo{
		w:      w,
		dir:    t.TempDir(),
		remote: strings.Replace(w.base, "http://", "http://x-access-token:"+token+"@", 1) + "/" + fullName + ".git",
	}
	r.git("init", "-q", "-b", "main", ".")
	return r
}

func (r *gitRepo) git(args ...string) {
	r.w.t.Helper()
	date := fmt.Sprintf("%d +0000", r.w.clock())
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=alice", "GIT_AUTHOR_EMAIL=alice@orrery.invalid",
		"GIT_COMMITTER_NAME=alice", "GIT_COMMITTER_EMAIL=alice@orrery.invalid",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *gitRepo) commit(file, content, message string) {
	r.w.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(content), 0o644); err != nil {
		r.w.t.Fatal(err)
	}
	r.git("add", file)
	r.git("commit", "-q", "-m", message)
}

func (r *gitRepo) branch(name, from string) { r.git("checkout", "-q", "-b", name, from) }
func (r *gitRepo) checkout(name string)     { r.git("checkout", "-q", name) }
func (r *gitRepo) push(branches ...string) {
	r.git(append([]string{"push", "-q", r.remote}, branches...)...)
}
