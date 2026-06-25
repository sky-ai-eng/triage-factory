package agenthost

import (
	"bytes"
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
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The GitHub tests stand a fake GitHub REST backend behind a fake resolver
// and drive the host-routed methods through both the in-process LocalClient
// and the full Server↔IPCClient loop. The load-bearing assertions are
// Property B (the credential resolves on the *host*, the agent-facing client
// never holds it) and tier coverage (App-installation token and org PAT both
// land), plus structured-error fidelity (a 406/404 survives the RPC as a
// typed github.HTTPError so the diff/download fallbacks keep working).

// fakeGitHubResolver returns a client pointed at a fake GitHub backend with a
// configurable token, standing in for either credential tier — the App
// installation token (tier 1) or the org PAT (tier 3) — without the App-mint
// plumbing. errOnRepo, when set, is returned from ClientForRepo to exercise
// the "not configured" mapping. identity is what ClientForRepoWithIdentity
// reports (App vs PAT), driving the start-review collision branch.
type fakeGitHubResolver struct {
	baseURL   string
	token     string
	errOnRepo error
	identity  ghclient.Identity
}

func (f fakeGitHubResolver) ClientFor(_ context.Context, _, _ string) (*ghclient.Client, error) {
	panic("ClientFor is not used by the host-routed gh surface")
}

func (f fakeGitHubResolver) ClientForRepo(_ context.Context, _, _, _ string) (*ghclient.Client, error) {
	if f.errOnRepo != nil {
		return nil, f.errOnRepo
	}
	return ghclient.NewClient(f.baseURL, f.token), nil
}

// ClientForRepoWithIdentity satisfies ghclient.RepoIdentityResolver so the
// collision check sees a controllable identity. It returns the configured
// token tier alongside the same client ClientForRepo builds.
func (f fakeGitHubResolver) ClientForRepoWithIdentity(_ context.Context, _, _, _ string) (*ghclient.Client, ghclient.Identity, error) {
	if f.errOnRepo != nil {
		return nil, ghclient.IdentityUnknown, f.errOnRepo
	}
	return ghclient.NewClient(f.baseURL, f.token), f.identity, nil
}

func (f fakeGitHubResolver) TokenFor(_ context.Context, _, _ string) (githubapp.Token, error) {
	panic("TokenFor is not used by the host-routed gh surface")
}

func (f fakeGitHubResolver) BaseURLFor(_ context.Context, _ string) (string, error) {
	panic("BaseURLFor is not used by the host-routed gh surface")
}

// baseOnlyResolver implements ghclient.Resolver WITHOUT the optional
// RepoIdentityResolver capability, so githubClientAndIdentityForRepo's type
// assertion fails and it falls back to IdentityUnknown — the conservative
// branch the production resolver (which always reports an identity) never hits.
type baseOnlyResolver struct {
	baseURL string
	token   string
}

func (r baseOnlyResolver) ClientFor(_ context.Context, _, _ string) (*ghclient.Client, error) {
	panic("ClientFor is not used by the host-routed gh surface")
}

func (r baseOnlyResolver) ClientForRepo(_ context.Context, _, _, _ string) (*ghclient.Client, error) {
	return ghclient.NewClient(r.baseURL, r.token), nil
}

func (r baseOnlyResolver) TokenFor(_ context.Context, _, _ string) (githubapp.Token, error) {
	panic("TokenFor is not used by the host-routed gh surface")
}

func (r baseOnlyResolver) BaseURLFor(_ context.Context, _ string) (string, error) {
	panic("BaseURLFor is not used by the host-routed gh surface")
}

// ghRecorder captures what the fake GitHub backend saw. Mutex-guarded so the
// handler goroutine and the test goroutine don't race under -race.
type ghRecorder struct {
	mu   sync.Mutex
	auth string
	body string
}

func (r *ghRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auth = req.Header.Get("Authorization")
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.body = string(b)
	}
}

func (r *ghRecorder) readAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auth
}

// startGitHubDaemon listens on a temp socket, serves with the given resolver
// + identity (stores are empty — the gh surface only touches the resolver),
// and returns a dialed IPCClient. The resolver is overridden after NewServer
// so the test's fake wins over the real one NewServer builds from stores.
func startGitHubDaemon(t *testing.T, resolver ghclient.Resolver, info RunInfo) *IPCClient {
	t.Helper()
	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(emptyStores(), info)
	srv.ghResolver = resolver
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	client := Dial(sockPath)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// emptyStores is the honest fixture for the gh surface: every host-routed gh
// method resolves its client from the (injected) resolver and touches nothing
// else on the bundle, so a zero Stores proves the methods don't reach for it.
func emptyStores() db.Stores { return db.Stores{} }

func ghInfo() RunInfo { return RunInfo{OrgID: runmode.LocalDefaultOrgID, RunID: "run-1"} }

// TestServer_GithubAddComment_RoutesHostSide is the Property-B test: the
// IPCClient (the sandbox's view) holds no credential, yet the call lands at
// GitHub carrying the org's resolved token — because the daemon built the
// client from its own host-side resolver and made the request.
func TestServer_GithubAddComment_RoutesHostSide(t *testing.T) {
	rec := &ghRecorder{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":777}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	id, err := client.GithubAddComment(context.Background(), "o", "r", 1, "looks good")
	if err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}
	if id != 777 {
		t.Errorf("comment id = %d, want 777", id)
	}
	if got := rec.readAuth(); got != "Bearer org-pat" {
		t.Errorf("GitHub saw Authorization %q, want %q — the host daemon must resolve the credential and present the token", got, "Bearer org-pat")
	}
}

// TestGithub_BothTiers_Succeed is the tier-coverage acceptance: an App-org
// (installation token) and a PAT-org (org PAT) both complete a write, with the
// resolved token landing at GitHub. The fake resolver stands in for both
// tiers — the only observable difference is which token reaches the backend.
func TestGithub_BothTiers_Succeed(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"app-installation", "app-installation-token"},
		{"org-pat", "org-pat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &ghRecorder{}
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":555}`)
			}))
			defer gh.Close()

			client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: tc.token}, ghInfo())

			reviewID, event, err := client.GithubSubmitReview(context.Background(), "o", "r", 1, "deadbeef", "COMMENT", "thoughts", nil)
			if err != nil {
				t.Fatalf("GithubSubmitReview (%s): %v", tc.name, err)
			}
			if reviewID != 555 || event != "COMMENT" {
				t.Errorf("review = (%d, %q), want (555, COMMENT)", reviewID, event)
			}
			if got, want := rec.readAuth(), "Bearer "+tc.token; got != want {
				t.Errorf("GitHub saw Authorization %q, want %q", got, want)
			}
		})
	}
}

// TestServer_GithubGetPR_RoundTrip pins a result-bearing read crossing the
// wire back to the sandbox client.
func TestServer_GithubGetPR_RoundTrip(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":42,"title":"hello","head":{"sha":"abc123"}}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	pr, err := client.GithubGetPR(context.Background(), "o", "r", 42, false)
	if err != nil {
		t.Fatalf("GithubGetPR: %v", err)
	}
	if pr == nil || pr.Number != 42 || pr.Title != "hello" || pr.HeadSHA != "abc123" {
		t.Fatalf("unexpected PR over the wire: %+v", pr)
	}
}

// TestServer_GithubAPIGet_RoundTrip pins the raw authenticated GET that every
// actions verb leans on (list-runs, job listings): the daemon resolves the
// client, makes the GET, and the response body bytes cross back to the sandbox
// for local parsing — carrying the resolved token.
func TestServer_GithubAPIGet_RoundTrip(t *testing.T) {
	rec := &ghRecorder{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"workflow_runs":[{"id":7}]}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	data, err := client.GithubAPIGet(context.Background(), "o", "r", "/repos/o/r/actions/runs?head_sha=abc")
	if err != nil {
		t.Fatalf("GithubAPIGet: %v", err)
	}
	if !strings.Contains(string(data), `"workflow_runs"`) {
		t.Fatalf("unexpected body over the wire: %q", data)
	}
	if got := rec.readAuth(); got != "Bearer org-pat" {
		t.Errorf("GitHub saw Authorization %q, want Bearer org-pat", got)
	}
}

// TestServer_GithubDownloadArtifact_RoundTrip proves the blob comes back over
// the RPC and lands in the caller's writer with the byte count intact.
func TestServer_GithubDownloadArtifact_RoundTrip(t *testing.T) {
	payload := []byte("PK\x03\x04 pretend-zip-bytes")
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	var buf bytes.Buffer
	n, err := client.GithubDownloadArtifact(context.Background(), "o", "r", "/repos/o/r/actions/runs/9/logs", &buf, 1<<20)
	if err != nil {
		t.Fatalf("GithubDownloadArtifact: %v", err)
	}
	if n != int64(len(payload)) || !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("download mismatch: n=%d body=%q want n=%d body=%q", n, buf.Bytes(), len(payload), payload)
	}
}

// TestServer_GithubDownloadArtifact_404Reconstructs is the structured-error
// acceptance for the download-logs fallback: GitHub 404s the run-level archive
// while the run is still in progress, and the sandbox must see a *typed*
// github.HTTPError (status 404) — not a bare string — so the per-job fallback
// fires. The status has to survive the RPC.
func TestServer_GithubDownloadArtifact_404Reconstructs(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	var buf bytes.Buffer
	_, err := client.GithubDownloadArtifact(context.Background(), "o", "r", "/repos/o/r/actions/runs/9/logs", &buf, 1<<20)
	if err == nil {
		t.Fatal("expected a 404 error, got nil")
	}
	var he *ghclient.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("error %T %q did not reconstruct as *github.HTTPError over the wire", err, err)
	}
	if he.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", he.StatusCode)
	}
}

// TestServer_GithubGetPRDiff_406Reconstructs pins the other status-discriminating
// fallback: GitHub rejects an oversized diff with 406, and getDiffShapes keys
// the per-file fallback off IsHTTP406 — which only works if the typed error
// crosses the RPC.
func TestServer_GithubGetPRDiff_406Reconstructs(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = io.WriteString(w, `{"message":"Sorry, the diff exceeded the maximum number of lines"}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	_, err := client.GithubGetPRDiff(context.Background(), "o", "r", 1, "")
	if err == nil {
		t.Fatal("expected a 406 error, got nil")
	}
	if !ghclient.IsHTTP406(err) {
		t.Fatalf("IsHTTP406 = false for %T %q — the 406 was flattened crossing the wire", err, err)
	}
}

// TestServer_GithubAPIError_MessagePropagates pins error-text fidelity for a
// non-fallback failure (a 403 insufficient-scope / rate-limit shape): the
// agent reasons from the message, which must survive the RPC verbatim.
func TestServer_GithubAPIError_MessagePropagates(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	_, err := client.GithubAddComment(context.Background(), "o", "r", 1, "hi")
	if err == nil {
		t.Fatal("expected the 403 to surface, got nil")
	}
	if !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("error %q dropped the structured GitHub rejection body", err.Error())
	}
}

// TestServer_GithubDownloadArtifact_OversizedClampsCleanly proves the IPC
// frame-fit clamp: a caller cap far above what one response frame can hold
// (gh actions passes 500 MB) is clamped to maxIPCArtifactBytes daemon-side, so
// an archive over that limit fails with a clean "artifact too large" error
// instead of downloading in full and overflowing the frame.
func TestServer_GithubDownloadArtifact_OversizedClampsCleanly(t *testing.T) {
	body := make([]byte, maxIPCArtifactBytes+1024)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	var buf bytes.Buffer
	_, err := client.GithubDownloadArtifact(context.Background(), "o", "r", "/repos/o/r/actions/runs/9/logs", &buf, 500*1024*1024)
	if err == nil {
		t.Fatal("expected an oversized-artifact error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error %q is not the clean over-cap message — the clamp didn't fire", err.Error())
	}
}

// TestLocalClient_GithubAddComment_DirectPath pins the unchanged local path:
// no socket, the in-process LocalClient builds the client from its own
// resolver and calls GitHub directly.
func TestLocalClient_GithubAddComment_DirectPath(t *testing.T) {
	rec := &ghRecorder{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":99}`)
	}))
	defer gh.Close()

	lc := NewLocal(emptyStores(), ghInfo())
	lc.ghResolver = fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}

	id, err := lc.GithubAddComment(context.Background(), "o", "r", 1, "direct")
	if err != nil {
		t.Fatalf("GithubAddComment (local): %v", err)
	}
	if id != 99 {
		t.Errorf("comment id = %d, want 99", id)
	}
	if got := rec.readAuth(); got != "Bearer org-pat" {
		t.Errorf("local path Authorization = %q, want Bearer org-pat", got)
	}
}

// TestGithub_NotConfigured_BothModes pins the friendly mapping: an org with no
// resolvable GitHub credential yields the same "not configured" guidance the
// gh branch printed pre-refactor — in local mode directly, and over IPC as the
// response error string.
func TestGithub_NotConfigured_BothModes(t *testing.T) {
	resolver := fakeGitHubResolver{errOnRepo: ghclient.ErrNoGitHubCredentials}

	t.Run("local", func(t *testing.T) {
		lc := NewLocal(emptyStores(), ghInfo())
		lc.ghResolver = resolver
		_, err := lc.GithubAddComment(context.Background(), "o", "r", 1, "hi")
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("err = %v, want the friendly 'not configured' guidance", err)
		}
	})

	t.Run("ipc", func(t *testing.T) {
		client := startGitHubDaemon(t, resolver, ghInfo())
		_, err := client.GithubAddComment(context.Background(), "o", "r", 1, "hi")
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("err = %v, want the friendly guidance to cross the wire", err)
		}
	})
}

// --- TFAC-469: PR node_id + pending-review host methods ---

// TestServer_GithubCreatePR_ReturnsNodeID pins node_id end-to-end: the REST
// create response's node_id crosses the daemon→IPC boundary intact (the durable
// PR handle the artifact rework keys on), alongside number and html_url.
func TestServer_GithubCreatePR_ReturnsNodeID(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42","node_id":"PR_node42"}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	number, htmlURL, nodeID, err := client.GithubCreatePR(context.Background(), "o", "r", "feat", "main", "T", "B", false)
	if err != nil {
		t.Fatalf("GithubCreatePR: %v", err)
	}
	if number != 42 || htmlURL != "https://github.com/o/r/pull/42" || nodeID != "PR_node42" {
		t.Fatalf("CreatePR = (%d, %q, %q), want (42, .../pull/42, PR_node42)", number, htmlURL, nodeID)
	}
}

// TestServer_GithubGetPendingReview_RoundTrip pins the GraphQL read crossing
// back to the sandbox: the review node id and its inline comments — including a
// null line that must stay a nil *int, not collapse to 0 — survive the RPC.
func TestServer_GithubGetPendingReview_RoundTrip(t *testing.T) {
	resp := `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[
		{"id":"PRR_mine","viewerDidAuthor":true,"comments":{"nodes":[
			{"id":"PRRC_1","path":"a.go","line":5,"startLine":null,"body":"one"},
			{"id":"PRRC_2","path":"c.go","line":null,"startLine":null,"body":"outdated"}
		]}}
	]}}}}}`
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resp)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "org-pat"}, ghInfo())

	id, comments, err := client.GithubGetPendingReview(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("GithubGetPendingReview: %v", err)
	}
	if id != "PRR_mine" {
		t.Errorf("reviewID = %q, want PRR_mine", id)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %+v, want 2", comments)
	}
	if comments[0].Line == nil || *comments[0].Line != 5 {
		t.Errorf("comment[0].Line = %v, want 5", comments[0].Line)
	}
	if comments[1].Line != nil {
		t.Errorf("comment[1].Line = %v, want nil (a null line must cross the wire as nil, not 0)", comments[1].Line)
	}
}

// TestServer_GithubAddPendingReviewComment_RoundTrip pins the GraphQL mutation:
// the new comment node id crosses back, and the daemon-resolved token reaches
// GitHub (Property B — the sandbox holds no credential of its own).
func TestServer_GithubAddPendingReviewComment_RoundTrip(t *testing.T) {
	rec := &ghRecorder{}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"addPullRequestReviewThread":{"thread":{"comments":{"nodes":[{"id":"PRRC_new"}]}}}}}`)
	}))
	defer gh.Close()

	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: gh.URL, token: "app-installation-token"}, ghInfo())

	id, err := client.GithubAddPendingReviewComment(context.Background(), "o", "r", "PRR_1", "a.go", "nit", 5, nil)
	if err != nil {
		t.Fatalf("GithubAddPendingReviewComment: %v", err)
	}
	if id != "PRRC_new" {
		t.Errorf("comment id = %q, want PRRC_new", id)
	}
	if got := rec.readAuth(); got != "Bearer app-installation-token" {
		t.Errorf("GitHub saw Authorization %q, want the host-resolved token", got)
	}
}

// pendingReviewBackend is a fake GitHub serving the two calls
// GithubCreatePendingReview makes — the GetPendingReview GraphQL read (routed by
// the /graphql path) and the REST pending-review create — so the collision
// branch is observable. A non-empty existingReview makes the read report a
// viewer-authored pending review; createNodeID is what a REST create returns;
// createFired records whether the create path was taken (it must NOT be on
// reuse / collision).
type pendingReviewBackend struct {
	mu             sync.Mutex
	url            string
	existingReview string
	createNodeID   string
	createFired    bool
}

func newPendingReviewBackend(t *testing.T, existingReview, createNodeID string) *pendingReviewBackend {
	t.Helper()
	b := &pendingReviewBackend{existingReview: existingReview, createNodeID: createNodeID}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			b.mu.Lock()
			existing := b.existingReview
			b.mu.Unlock()
			if existing == "" {
				_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[{"id":"`+existing+`","viewerDidAuthor":true,"comments":{"nodes":[]}}]}}}}}`)
			return
		}
		// REST pending-review create.
		b.mu.Lock()
		b.createFired = true
		nodeID := b.createNodeID
		b.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":1,"node_id":"`+nodeID+`"}`)
	}))
	t.Cleanup(srv.Close)
	b.url = srv.URL
	return b
}

func (b *pendingReviewBackend) didCreate() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.createFired
}

// TestServer_GithubCreatePendingReview_CreatesWhenNone is the no-collision path:
// GetPendingReview reports nothing, so the host creates a fresh pending review
// and returns its node id over the RPC.
func TestServer_GithubCreatePendingReview_CreatesWhenNone(t *testing.T) {
	b := newPendingReviewBackend(t, "", "PRR_fresh")
	client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: b.url, token: "org-pat", identity: ghclient.IdentityApp}, ghInfo())

	id, err := client.GithubCreatePendingReview(context.Background(), "o", "r", 7, "sha123", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	if id != "PRR_fresh" {
		t.Errorf("reviewID = %q, want PRR_fresh (a freshly created review)", id)
	}
	if !b.didCreate() {
		t.Error("expected the REST create to fire when no pending review exists")
	}
}

// TestGithub_CreatePendingReview_Collision is the load-bearing identity branch:
// when a pending review already exists for the acting identity, an App /
// service-account reuses it (the bot's own from a prior run) while a real-user
// PAT refuses with ErrPendingReviewCollision (never hijack a human's review).
// The PAT case runs over both the in-process and IPC paths to prove the typed
// sentinel survives the wire; neither branch may fire the REST create.
func TestGithub_CreatePendingReview_Collision(t *testing.T) {
	t.Run("app identity reuses existing (ipc)", func(t *testing.T) {
		b := newPendingReviewBackend(t, "PRR_existing", "PRR_should_not_create")
		client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: b.url, token: "app-installation-token", identity: ghclient.IdentityApp}, ghInfo())

		id, err := client.GithubCreatePendingReview(context.Background(), "o", "r", 7, "sha", nil)
		if err != nil {
			t.Fatalf("GithubCreatePendingReview: %v", err)
		}
		if id != "PRR_existing" {
			t.Errorf("reviewID = %q, want the reused PRR_existing", id)
		}
		if b.didCreate() {
			t.Error("App-identity reuse must NOT create a second pending review")
		}
	})

	t.Run("pat identity collides (local)", func(t *testing.T) {
		b := newPendingReviewBackend(t, "PRR_existing", "PRR_should_not_create")
		lc := NewLocal(emptyStores(), ghInfo())
		lc.ghResolver = fakeGitHubResolver{baseURL: b.url, token: "org-pat", identity: ghclient.IdentityPAT}

		_, err := lc.GithubCreatePendingReview(context.Background(), "o", "r", 7, "sha", nil)
		if !errors.Is(err, ErrPendingReviewCollision) {
			t.Fatalf("err = %v, want ErrPendingReviewCollision", err)
		}
		if b.didCreate() {
			t.Error("PAT-identity collision must NOT create a pending review (never hijack)")
		}
	})

	t.Run("pat identity collides over ipc (typed error survives wire)", func(t *testing.T) {
		b := newPendingReviewBackend(t, "PRR_existing", "PRR_should_not_create")
		client := startGitHubDaemon(t, fakeGitHubResolver{baseURL: b.url, token: "org-pat", identity: ghclient.IdentityPAT}, ghInfo())

		_, err := client.GithubCreatePendingReview(context.Background(), "o", "r", 7, "sha", nil)
		if !errors.Is(err, ErrPendingReviewCollision) {
			t.Fatalf("err = %v, want ErrPendingReviewCollision reconstructed over the wire", err)
		}
		if b.didCreate() {
			t.Error("PAT-identity collision must NOT create a pending review")
		}
	})

	// A resolver lacking the RepoIdentityResolver capability yields
	// IdentityUnknown, which must be treated as "can't prove it's the bot's own"
	// → collide, never silently reuse. The production resolver always reports an
	// identity, so this fallback branch is otherwise test-dead.
	t.Run("unknown identity collides (resolver without capability)", func(t *testing.T) {
		b := newPendingReviewBackend(t, "PRR_existing", "PRR_should_not_create")
		lc := NewLocal(emptyStores(), ghInfo())
		lc.ghResolver = baseOnlyResolver{baseURL: b.url, token: "org-pat"}

		_, err := lc.GithubCreatePendingReview(context.Background(), "o", "r", 7, "sha", nil)
		if !errors.Is(err, ErrPendingReviewCollision) {
			t.Fatalf("err = %v, want ErrPendingReviewCollision (unknown identity must not reuse)", err)
		}
		if b.didCreate() {
			t.Error("unknown-identity collision must NOT create a pending review")
		}
	})
}
