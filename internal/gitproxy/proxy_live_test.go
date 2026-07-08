//go:build live

// Live tests in this file mint a real GitHub App installation token,
// run it through the proxy, and exercise actual git operations
// (ls-remote + fetch) against a real test repo on github.com.
//
// Build-tagged so CI doesn't run them by default; no API spend, but
// requires operator-side credentials.
//
// To run: `go test -tags live ./internal/gitproxy/ -v -run TestLive`
//
// Env vars required:
//
//   - GITHUB_APP_PRIVATE_KEY_PATH: path to the App's PEM file
//   - GITHUB_APP_ID:               numeric App ID
//   - GITHUB_INSTALLATION_ID:      installation ID to mint against
//   - GITHUB_TEST_REPO_HTTPS:      full HTTPS clone URL of a repo
//                                  the installation has access to,
//                                  e.g. https://github.com/myorg/test.git

package gitproxy_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// liveMinter assembles a real Minter from env vars, skipping the
// surrounding test if any required var is missing.
func liveMinter(t *testing.T) (*githubapp.Minter, int64) {
	t.Helper()
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	if keyPath == "" {
		t.Skip("GITHUB_APP_PRIVATE_KEY_PATH not set; skipping live test")
	}
	appIDStr := os.Getenv("GITHUB_APP_ID")
	if appIDStr == "" {
		t.Skip("GITHUB_APP_ID not set; skipping live test")
	}
	installIDStr := os.Getenv("GITHUB_INSTALLATION_ID")
	if installIDStr == "" {
		t.Skip("GITHUB_INSTALLATION_ID not set; skipping live test")
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		t.Fatalf("GITHUB_APP_ID parse: %v", err)
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		t.Fatalf("GITHUB_INSTALLATION_ID parse: %v", err)
	}

	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	key, err := githubapp.ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m, installID
}

// liveTokenSource adapts a real Minter to a gitproxy.TokenSource
// closure for a specific installation. Same shape production wiring
// would use; the live test exercises exactly that path.
func liveTokenSource(m *githubapp.Minter, installID int64) gitproxy.TokenSource {
	return func(ctx context.Context) (gitproxy.Token, error) {
		tok, err := m.MintInstallationToken(ctx, installID)
		if err != nil {
			return gitproxy.Token{}, err
		}
		return gitproxy.Token{Value: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
	}
}

// TestLive_ProxyAuthAcceptedByGitHubAPI is the cheapest live check —
// no git binary involved, just a direct HTTP call to the GitHub REST
// API through the proxy. Validates that the basic-auth shape we inject
// is accepted by the real GitHub edge.
//
// We hit github.com (not api.github.com) because that's the host the
// proxy is configured for. github.com serves the API under the same
// hostname for some endpoints (older patterns) but the cleanest test
// is /api/v3-style — except api.github.com doesn't accept the x-access-
// token basic-auth shape (it wants Bearer for tokens). So we use the
// /<owner>/<repo>/info/refs endpoint, which IS what git clients use
// and IS the basic-auth path.
func TestLive_ProxyAuthAcceptedByGitHubAPI(t *testing.T) {
	repoURL := os.Getenv("GITHUB_TEST_REPO_HTTPS")
	if repoURL == "" {
		t.Skip("GITHUB_TEST_REPO_HTTPS not set; skipping live test")
	}
	m, installID := liveMinter(t)

	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: liveTokenSource(m, installID),
		Upstream:    "https://github.com",
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	proxyURL := "http://" + addr

	// Derive the repo path from the HTTPS URL. e.g.
	// https://github.com/foo/bar.git → /foo/bar.git
	repoPath := strings.TrimPrefix(repoURL, "https://github.com")
	if repoPath == repoURL {
		t.Fatalf("GITHUB_TEST_REPO_HTTPS must start with https://github.com; got %q", repoURL)
	}

	// info/refs is what `git fetch` hits first — the smart-HTTP
	// service-advertisement endpoint. A 200 with a service header
	// means the auth was accepted.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET",
		proxyURL+repoPath+"/info/refs?service=git-upload-pack", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 — token auth not accepted by GitHub", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "git-upload-pack") {
		t.Errorf("Content-Type = %q, want application/x-git-upload-pack-advertisement", ct)
	}

	if got := srv.MintCount(); got != 1 {
		t.Errorf("MintCount = %d, want 1", got)
	}
	if got := srv.RequestCount(); got != 1 {
		t.Errorf("RequestCount = %d, want 1", got)
	}
}

// TestLive_GitCloneThroughProxy is the load-bearing acceptance
// criterion: a real `git clone` of a private repo through the proxy
// succeeds, and the worktree's .git/config + the child's env show
// only the proxy URL — no installation token.
//
// Uses http://github.com/... (not https) as the remote URL so git
// uses HTTP forward-proxy mode rather than CONNECT-tunneled HTTPS
// (the reverse-proxy handler can't terminate CONNECT without a
// custom CA, which is out of scope for this primitive).
//
// Skips if git isn't installed.
func TestLive_GitCloneThroughProxy(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not installed: %v", err)
	}
	repoHTTPS := os.Getenv("GITHUB_TEST_REPO_HTTPS")
	if repoHTTPS == "" {
		t.Skip("GITHUB_TEST_REPO_HTTPS not set")
	}
	m, installID := liveMinter(t)

	// Bind on the default loopback address — Start("") below resolves
	// to 127.0.0.1:0. The future sandbox-veth case (binding to the
	// host-side veth IP) requires AllowNonLoopback=true, but that's
	// wired up at sandbox integration time; for the live-test path
	// here, loopback is sufficient and avoids needing routable
	// interface coordination.
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: liveTokenSource(m, installID),
		Upstream:    "https://github.com",
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	proxyURL := "http://" + addr

	// Transform https:// to http:// so git uses forward-proxy mode.
	// The proxy then upgrades to HTTPS upstream.
	repoHTTP := strings.Replace(repoHTTPS, "https://", "http://", 1)

	tmpHome := t.TempDir()
	cloneDir := filepath.Join(tmpHome, "repo")
	gitconfigPath := filepath.Join(tmpHome, ".gitconfig")
	gitconfigBody := "[http]\n\tproxy = " + proxyURL + "\n"
	if err := os.WriteFile(gitconfigPath, []byte(gitconfigBody), 0600); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	childEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + tmpHome,
		"GIT_TERMINAL_PROMPT=0", // never prompt for credentials interactively
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "clone", "--depth=1", repoHTTP, cloneDir)
	cmd.Env = childEnv
	cmd.Dir = tmpHome
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone failed: %v\nout: %s", err, out)
	}

	// Property B: scan the worktree's .git/config + the temp .gitconfig
	// for anything that smells like an installation token. They start
	// with "ghs_" by convention; the proxy URL contains the bound port
	// but not the credential.
	for _, p := range []string{
		gitconfigPath,
		filepath.Join(cloneDir, ".git", "config"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			continue
		}
		if strings.Contains(string(b), "ghs_") {
			t.Errorf("%s contains an installation token (ghs_ prefix) — Property B violated:\n%s", p, b)
		}
		if strings.Contains(string(b), "x-access-token") {
			t.Errorf("%s contains x-access-token literal — credential leaked into config:\n%s", p, b)
		}
	}

	if got := srv.RequestCount(); got == 0 {
		t.Errorf("RequestCount = 0 after successful clone — clone bypassed the proxy")
	}
	t.Logf("clone succeeded; proxy observed %d upstream call(s); minted %d token(s)",
		srv.RequestCount(), srv.MintCount())
}
