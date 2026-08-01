package ghinjector_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// This file is the shared-origin contract end to end, with a real git client on
// one side and a real git server on the other: one TLS listener at the address
// GH_HOST names serves the GitHub API and git smart-HTTP alike, exactly as a
// real GHE host does.
//
// The stand-in for github.com is `git http-backend` over a local bare repo, so
// nothing here reaches the network. Between the client and it sit the two
// production pieces in their production arrangement — the gh injector fronting
// the run's gitproxy handler — and the client is configured the way the sandbox
// is: the upstream URL rewritten onto the shared origin, the per-run placeholder
// as the Basic password, the injector's cert as the CA.

// startGitUpstream serves bareDir over git's own smart-HTTP CGI backend and
// returns its base URL. It stands in for github.com.
func startGitUpstream(t *testing.T, root string) string {
	t.Helper()
	backend := filepath.Join(gitExecPath(t), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not present at %s: %v", backend, err)
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			// The upstream is a throwaway local repo; the access control under
			// test is the proxy's ref gate, not the backend's.
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=tester",
		},
	})
	t.Cleanup(srv.Close)
	return srv.URL
}

func gitExecPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not installed: %v", err)
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// seedBareRepo creates <root>/<owner>/<repo>.git with one commit on main and
// returns the project root. The layout mirrors GitHub's /{owner}/{repo}.git
// paths, which is what the gitproxy gate parses.
func seedBareRepo(t *testing.T, owner, repo string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, owner, repo+".git")
	work := filepath.Join(t.TempDir(), "seed")

	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	// Accept pushes into the checked-out branch of a bare repo's clone below.
	runGit(t, bare, "config", "http.receivepack", "true")

	runGit(t, "", "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.email", "seed@example.test")
	runGit(t, work, "config", "user.name", "Seed")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "origin", "main")
	return root
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func gitOutput(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A clean environment: the surrounding CI/dev box may carry its own
	// GIT_CONFIG_*, http_proxy, or insteadOf rewrites, and this test's whole
	// subject is which git config is in force.
	cmd.Env = append([]string{
		"HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sharedOrigin stands up the production arrangement: a gitproxy over upstream,
// fronted by a gh injector on one TLS listener. Returns the origin host, the
// git-config env the sandbox would carry, and the API placeholder.
func sharedOrigin(t *testing.T, upstream string, authorize func(context.Context, string, string) (gitproxy.Decision, error)) (host string, gitEnv []string, apiPlaceholder string, denials <-chan gitproxy.DeniedGitOp, apiAuth <-chan string) {
	t.Helper()
	const gitPlaceholder = "git-per-run-placeholder"
	const apiToken = "api-per-run-placeholder"

	denied := make(chan gitproxy.DeniedGitOp, 8)
	gp, err := gitproxy.New(gitproxy.Config{
		Upstream:      upstream,
		IncomingToken: gitPlaceholder,
		Authorize:     authorize,
		RecordDenial:  func(_ context.Context, d gitproxy.DeniedGitOp) { denied <- d },
		// The real credential the proxy injects upstream. The local
		// git-http-backend ignores it; what matters is that the client never
		// holds it.
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_REAL_INSTALLATION_TOKEN"}, nil
		},
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}

	cert, certPEM, err := ghinjector.GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	// The API upstream is separate from the git one only because this test's git
	// upstream is a CGI backend with no REST surface. On a real host they are
	// the same origin; what matters here is that the shared listener still
	// routes API paths to the API side and injects the real credential there.
	seenAuth := make(chan string, 4)
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(apiUpstream.Close)

	inj, err := ghinjector.New(ghinjector.Config{
		Upstream:      apiUpstream.URL,
		IncomingToken: apiToken,
		Cert:          cert,
		GitHandler:    gp.Handler(),
		TokenSource:   func(context.Context) (string, error) { return "ghs_REAL_CLI_TOKEN", nil },
	})
	if err != nil {
		t.Fatalf("ghinjector.New: %v", err)
	}
	addr, err := inj.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("injector Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = inj.Shutdown(ctx)
	})

	caPath := filepath.Join(t.TempDir(), "origin-ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}

	// The production listener is on 443 so GH_HOST is portless; a test cannot
	// bind that, so the host carries the ephemeral port here. Everything else —
	// the rewrite target, the placeholder, the CA — is the sandbox's own shape.
	host = addr
	base := "https://" + host + "/"
	pairs := [][2]string{
		{"url." + base + ".insteadOf", strings.TrimRight(upstream, "/") + "/"},
		{"http." + base + ".extraHeader", "Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte("x-run:"+gitPlaceholder))},
		{"http." + base + ".sslCAInfo", caPath},
	}
	gitEnv = []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, p := range pairs {
		gitEnv = append(gitEnv,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+p[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+p[1],
		)
	}
	return host, gitEnv, apiToken, denied, seenAuth
}

// TestSharedOrigin_GitAndAPIOverOneListener is the ticket's core claim: from a
// worktree whose upstream is rewritten onto the shared origin, a real git client
// clones, fetches, and pushes through it, an allowed push lands, a push the ref
// gate refuses is refused with the gate's own message, and the API half of the
// same listener still injects the real credential.
func TestSharedOrigin_GitAndAPIOverOneListener(t *testing.T) {
	root := seedBareRepo(t, "acme", "widgets")
	upstream := startGitUpstream(t, root)

	// The gate the run's policy produces: this repo, and only the run's own
	// branch — refs/heads/main is the protected base branch.
	authorize := func(_ context.Context, owner, repo string) (gitproxy.Decision, error) {
		if owner != "acme" || repo != "widgets" {
			return gitproxy.Decision{Allowed: false, DenyReason: "repo-not-authorized"}, nil
		}
		return gitproxy.Decision{
			Allowed:       true,
			AllowedRefs:   []string{"refs/heads/agent-work"},
			ProtectedRefs: []string{"refs/heads/main"},
		}, nil
	}
	host, gitEnv, apiPlaceholder, denials, apiAuth := sharedOrigin(t, upstream, authorize)

	// Clone by the UPSTREAM url — insteadOf is what routes it at the shared
	// origin, which is exactly how the sandbox's git resolves its origin.
	dir := t.TempDir()
	clone := filepath.Join(dir, "widgets")
	if out, err := gitOutput(dir, gitEnv, "clone", upstream+"/acme/widgets.git", clone); err != nil {
		t.Fatalf("clone through the shared origin failed: %v\n%s", err, out)
	}

	// `git remote -v` reports the rewritten URL, and that is what gh reads to
	// infer the repo. Its host must be the shared origin's — the whole point.
	remotes, err := gitOutput(clone, gitEnv, "remote", "-v")
	if err != nil {
		t.Fatalf("git remote -v: %v\n%s", err, remotes)
	}
	if !strings.Contains(remotes, "https://"+host+"/acme/widgets.git") {
		t.Errorf("git remote -v = %q; want a remote on the shared origin %q, which is what gh matches against GH_HOST", remotes, host)
	}

	if out, err := gitOutput(clone, gitEnv, "fetch", "origin"); err != nil {
		t.Fatalf("fetch through the shared origin failed: %v\n%s", err, out)
	}

	// An allowed push lands.
	mustCommit(t, clone, gitEnv, "agent-work", "agent.txt")
	if out, err := gitOutput(clone, gitEnv, "push", "origin", "agent-work"); err != nil {
		t.Fatalf("allowed push through the shared origin failed: %v\n%s", err, out)
	}
	if refs := runGit(t, filepath.Join(root, "acme", "widgets.git"), "for-each-ref", "--format=%(refname)"); !strings.Contains(refs, "refs/heads/agent-work") {
		t.Errorf("upstream refs = %q, want the pushed branch to have landed", refs)
	}

	// The ref gate survived the re-homing: a base-branch push is refused, and
	// the refusal is the gate's, not a transport error.
	mustCommit(t, clone, gitEnv, "main", "sneaky.txt")
	out, err := gitOutput(clone, gitEnv, "push", "origin", "main")
	if err == nil {
		t.Fatalf("push to the protected base branch succeeded through the shared origin; the ref gate did not survive re-homing\n%s", out)
	}
	// And it is the REF gate that refused it, naming the protected ref — not a
	// repo-level denial or a transport failure that happens to look like one.
	select {
	case d := <-denials:
		if d.Ref != "refs/heads/main" || d.Op != "push" || d.Owner != "acme" || d.Repo != "widgets" {
			t.Errorf("recorded denial = %+v, want a push denial on acme/widgets refs/heads/main", d)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("no denial recorded for the refused push (transport output was %q)", out)
	}
	mainSHA := runGit(t, filepath.Join(root, "acme", "widgets.git"), "rev-parse", "refs/heads/main")
	localMain := runGit(t, clone, "rev-parse", "main")
	if strings.TrimSpace(mainSHA) == strings.TrimSpace(localMain) {
		t.Error("the refused push still updated the upstream base branch")
	}

	// The API half of the same listener is untouched by any of this, verified
	// against the very trust file git was handed.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPoolFrom(t, gitEnv)}}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/api/v3/repos/acme/widgets", nil)
	req.Header.Set("Authorization", "token "+apiPlaceholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("API request on the shared listener: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("API status on the shared listener = %d, want 200; git routing must not have swallowed the API path", resp.StatusCode)
	}
	select {
	case auth := <-apiAuth:
		if auth != "token ghs_REAL_CLI_TOKEN" {
			t.Errorf("API upstream saw %q, want the real credential injected on the upstream hop", auth)
		}
	case <-time.After(2 * time.Second):
		t.Error("the API request never reached the API upstream")
	}
}

// mustCommit checks out branch (creating it from HEAD if needed) and adds one
// commit, so a push has something to carry.
func mustCommit(t *testing.T, repo string, env []string, branch, file string) {
	t.Helper()
	runGitEnv(t, repo, env, "config", "user.email", "agent@example.test")
	runGitEnv(t, repo, env, "config", "user.name", "Agent")
	if out, err := gitOutput(repo, env, "checkout", branch); err != nil {
		if out2, err2 := gitOutput(repo, env, "checkout", "-b", branch); err2 != nil {
			t.Fatalf("checkout %s: %v\n%s\n%s", branch, err, out, out2)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, file), []byte(file+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	runGitEnv(t, repo, env, "add", file)
	runGitEnv(t, repo, env, "commit", "-m", "add "+file)
}

func runGitEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	if out, err := gitOutput(dir, env, args...); err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// caPoolFrom loads the CA the git config names, so the API assertion trusts the
// very file git was handed rather than a second copy.
func caPoolFrom(t *testing.T, gitEnv []string) *x509.CertPool {
	t.Helper()
	var caPath string
	for i, kv := range gitEnv {
		if strings.HasSuffix(kv, ".sslCAInfo") && i+1 < len(gitEnv) {
			_, caPath, _ = strings.Cut(gitEnv[i+1], "=")
		}
	}
	if caPath == "" {
		t.Fatal("git config carries no sslCAInfo")
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read CA %s: %v", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("append CA %s to pool failed", caPath)
	}
	return pool
}
