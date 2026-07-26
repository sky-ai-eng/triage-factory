package ghinjector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the REAL pinned gh binary against a fake GitHub Enterprise
// endpoint standing behind a real injector, and assert what the injector
// observes. They exist because the synthetic unit tests below assert the
// injector against an assumption about what gh sends rather than against gh:
// the observation path originally watched two REST mutation paths, and gh's
// porcelain never sends either — every mutation it performs is GraphQL. Only a
// test that runs gh can pin that contract, so this one takes no opt-in switch,
// no root, no runsc, no network and no real repo. It skips only when the pinned
// binary is absent.
//
// pinnedGHBinary is where the image bakes the TF-pinned gh release; the same
// path internal/agentproc bind-mounts into the jail. On a dev box without it the
// tests skip; TF_TEST_GH_BINARY points them at a local copy.
const pinnedGHBinary = "/opt/tf/bin/gh"

func ghBinary(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("TF_TEST_GH_BINARY"); override != "" {
		return override
	}
	if _, err := os.Stat(pinnedGHBinary); err != nil {
		t.Skipf("pinned gh binary not present at %s: %v", pinnedGHBinary, err)
	}
	return pinnedGHBinary
}

// ghePR is the coordinate set the fake endpoint reports for the created PR.
const (
	ghePROwner  = "octo"
	ghePRRepo   = "repo"
	ghePRNumber = 42
	ghePRNodeID = "PR_kwWireTest"
	ghePRURL    = "https://ghe.test/octo/repo/pull/42"
)

// fakeGHE answers the handful of operations `gh pr create` / `gh pr view` need.
// The bodies are gh's own wire shapes: the create mutation's selection set is
// exactly `pullRequest { id url }` and the view query nests its result under
// data.repository.pullRequest — which is the discrimination the injector has to
// get right. createExtra is spliced into the create response as an extra
// top-level member, so a test can push that one response past the buffer cap
// without disturbing anything gh reads.
func fakeGHE(t *testing.T, createExtra string) (*httptest.Server, func() string) {
	t.Helper()
	var (
		mu   sync.Mutex
		auth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		query := string(body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/meta"):
			_, _ = w.Write([]byte(`{"verifiable_password_authentication":false}`))

		case !strings.HasSuffix(r.URL.Path, "/graphql"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))

		case strings.Contains(query, "RepositoryInfo"):
			_, _ = w.Write([]byte(`{"data":{"repository":{"id":"R_wire","name":"` + ghePRRepo +
				`","owner":{"login":"` + ghePROwner + `"},"defaultBranchRef":{"name":"main"},` +
				`"viewerPermission":"WRITE","hasIssuesEnabled":true,"mergeCommitAllowed":true,` +
				`"rebaseMergeAllowed":true,"squashMergeAllowed":true}}}`))

		case strings.Contains(query, "PullRequestCreate"):
			_, _ = w.Write([]byte(`{"data":{"createPullRequest":{"pullRequest":{"id":"` + ghePRNodeID +
				`","url":"` + ghePRURL + `"}}}` + createExtra + `}`))

		case strings.Contains(query, "PullRequestByNumber"):
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"` + ghePRNodeID +
				`","number":42,"url":"` + ghePRURL + `","title":"T","state":"OPEN","body":"B",` +
				`"baseRefName":"main","headRefName":"feature","commits":{"totalCount":1},` +
				`"author":{"login":"someone"}}}}}`))

		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
}

// ghEnv stands an injector in front of upstream and returns the environment a
// gh invocation needs to reach it — the production shape: GH_HOST names the
// injector, the token in the environment is the per-run placeholder, and
// SSL_CERT_FILE points at a bundle carrying the injector's per-run leaf. The
// environment is built from scratch rather than inherited so a developer's real
// GH_TOKEN or gh config can't influence the result.
func ghEnv(t *testing.T, upstream string, observe func(context.Context, ObservedMutation)) []string {
	t.Helper()
	const placeholder = "placeholder-wire-token"

	cert, certPEM, err := GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	srv, err := New(Config{
		Upstream:      upstream + "/api/v3",
		IncomingToken: placeholder,
		Cert:          cert,
		Observe:       observe,
		TokenSource:   func(context.Context) (string, error) { return "ghs_realtoken", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	home := t.TempDir()
	trust := filepath.Join(home, "trust.pem")
	if err := os.WriteFile(trust, TrustBundlePEM(certPEM), 0o600); err != nil {
		t.Fatalf("write trust bundle: %v", err)
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"GH_CONFIG_DIR=" + filepath.Join(home, "config", "gh"),
		"GH_HOST=" + host,
		"GH_ENTERPRISE_TOKEN=" + placeholder,
		"SSL_CERT_FILE=" + trust,
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
	}
}

// runGH invokes the pinned binary and fails the test if it errors, since a gh
// failure means the fixture drifted from what gh actually sends — exactly the
// signal these tests exist to produce.
func runGH(t *testing.T, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghBinary(t), args...)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// observations collects what the injector reported, from whichever goroutine
// the proxy called back on.
type observations struct {
	mu   sync.Mutex
	seen []ObservedMutation
}

func (o *observations) record(_ context.Context, m ObservedMutation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, m)
}

func (o *observations) snap() []ObservedMutation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ObservedMutation(nil), o.seen...)
}

// TestGHWire_PRCreateIsObserved is the contract this ticket exists to pin: the
// real gh creates a PR over GraphQL, and the injector records it from the
// response alone, with coordinates parsed out of the html url.
func TestGHWire_PRCreateIsObserved(t *testing.T) {
	upstream, upstreamAuth := fakeGHE(t, "")
	var obs observations
	env := ghEnv(t, upstream.URL, obs.record)

	out := runGH(t, env, "pr", "create", "-R", ghePROwner+"/"+ghePRRepo,
		"--head", "feature", "--base", "main", "--title", "T", "--body", "B")
	if !strings.Contains(out, ghePRURL) {
		t.Errorf("gh pr create output = %q, want the created PR url", out)
	}

	seen := obs.snap()
	if len(seen) != 1 {
		t.Fatalf("observations = %d (%+v), want exactly 1 pull_request", len(seen), seen)
	}
	m := seen[0]
	if m.Kind != "pull_request" || m.Owner != ghePROwner || m.Repo != ghePRRepo || m.Number != ghePRNumber {
		t.Errorf("observation = %+v, want pull_request %s/%s#%d", m, ghePROwner, ghePRRepo, ghePRNumber)
	}
	if m.NodeID != ghePRNodeID || m.URL != ghePRURL {
		t.Errorf("observation node id / url = %q / %q, want %q / %q", m.NodeID, m.URL, ghePRNodeID, ghePRURL)
	}
	// gh's selection set carries no title or refs; those stay empty for the
	// reconciler rather than being invented here.
	if m.Title != "" || m.Head != "" || m.Base != "" {
		t.Errorf("observation = %+v, want title/head/base empty on the GraphQL path", m)
	}
	if got := upstreamAuth(); got != "token ghs_realtoken" {
		t.Errorf("upstream Authorization = %q, want the injected real token", got)
	}
}

// TestGHWire_PRViewIsNotObserved is the query/mutation discrimination: reading a
// PR must record nothing. `gh pr view` returns a pullRequest object too — just
// nested under data.repository — so a generic walk of the response would
// attribute a PR the run only read as one it produced.
func TestGHWire_PRViewIsNotObserved(t *testing.T) {
	upstream, _ := fakeGHE(t, "")
	var obs observations
	env := ghEnv(t, upstream.URL, obs.record)

	out := runGH(t, env, "pr", "view", "42", "-R", ghePROwner+"/"+ghePRRepo)
	if !strings.Contains(out, ghePRURL) {
		t.Fatalf("gh pr view output = %q, want the PR url (fixture drift?)", out)
	}

	if seen := obs.snap(); len(seen) != 0 {
		t.Errorf("observations = %+v, want none for a read-only command", seen)
	}
}

// TestGHWire_OversizedCreateReachesGHIntact is the negative space: a create
// response past the buffer cap is delivered to gh byte-for-byte — gh still
// parses it and prints the url — and observation is what degrades, not the
// agent's response.
func TestGHWire_OversizedCreateReachesGHIntact(t *testing.T) {
	// An extra top-level member gh ignores, padded past the cap.
	pad, err := json.Marshal(strings.Repeat("x", maxObserveBody+4096))
	if err != nil {
		t.Fatalf("marshal pad: %v", err)
	}
	upstream, _ := fakeGHE(t, `,"extensions":{"pad":`+string(pad)+`}`)
	var obs observations
	env := ghEnv(t, upstream.URL, obs.record)

	out := runGH(t, env, "pr", "create", "-R", ghePROwner+"/"+ghePRRepo,
		"--head", "feature", "--base", "main", "--title", "T", "--body", "B")
	if !strings.Contains(out, ghePRURL) {
		t.Errorf("gh pr create output = %q, want the created PR url — the response was corrupted", out)
	}
	if seen := obs.snap(); len(seen) != 0 {
		t.Errorf("observations = %+v, want none for an over-cap response", seen)
	}
}
