package agentproc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghchannel"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
)

// These tests drive the REAL pinned gh binary through the LOCAL-MODE channel
// composition — ghchannel.Start produces the coordinates, directGHChannelEnv
// turns them into the agent subprocess's environment, and a shell resolves and
// runs gh out of that environment. That whole chain is what a local delegated
// run performs, so testing it end to end is the only way to pin the two
// properties the channel exists for: the org's credential reaches GitHub, and
// it never reaches the subprocess.
//
// The sibling wire tests in internal/ghcli/ghinjector pin what gh sends. These pin how
// local mode wires it. Both skip when the pinned binary is absent; CI stages it
// from the same pin (docker/Dockerfile, kept in lockstep with internal/ghcli/ghpin).

const wirePinnedGH = "/opt/tf/bin/gh"

func wireGHBinary(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("TF_TEST_GH_BINARY"); override != "" {
		return override
	}
	if _, err := os.Stat(wirePinnedGH); err != nil {
		t.Skipf("pinned gh binary not present at %s: %v", wirePinnedGH, err)
	}
	return wirePinnedGH
}

// wireBinDir stages the pinned binary as "gh" in a TF-owned directory, standing
// in for what internal/ghcli/ghbin provisions at runtime.
func wireBinDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tf-bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	if err := os.Symlink(wireGHBinary(t), filepath.Join(dir, "gh")); err != nil {
		t.Fatalf("stage gh: %v", err)
	}
	return dir
}

// decoyBinDir mimics the user's own gh: a shell script that records that it ran
// and then fails. A run must never reach it — that binary would be
// authenticated as the human.
func decoyBinDir(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "user-bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir decoy dir: %v", err)
	}
	marker = filepath.Join(dir, "invoked")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 97\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write decoy gh: %v", err)
	}
	return dir, marker
}

const (
	wireOwner  = "octo"
	wireRepo   = "repo"
	wirePRNum  = 42
	wirePRNode = "PR_kwLocalChannel"
	wirePRURL  = "https://ghe.test/octo/repo/pull/42"
	wireReal   = "ghs_the_real_org_credential"
)

// fakeUpstream answers what `gh pr view` needs, in gh's own wire shapes, and
// records the Authorization header it was last called with and every GraphQL
// body it was sent.
func fakeUpstream(t *testing.T) (*httptest.Server, func() string, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		auth    string
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			queries = append(queries, string(body))
		}
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
			_, _ = w.Write([]byte(`{"data":{"repository":{"id":"R_wire","name":"` + wireRepo +
				`","owner":{"login":"` + wireOwner + `"},"defaultBranchRef":{"name":"main"},` +
				`"viewerPermission":"WRITE","hasIssuesEnabled":true,"mergeCommitAllowed":true,` +
				`"rebaseMergeAllowed":true,"squashMergeAllowed":true}}}`))
		case strings.Contains(query, "PullRequestByNumber"):
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"` + wirePRNode +
				`","number":` + strconv.Itoa(wirePRNum) + `,"url":"` + wirePRURL + `","title":"T","state":"OPEN","body":"B",` +
				`"baseRefName":"main","headRefName":"feature","headRefOid":"deadbeef",` +
				`"isCrossRepository":false,"mergeStateStatus":"CLEAN","mergeable":"MERGEABLE",` +
				`"isInMergeQueue":false,"isMergeQueueEnabled":false,` +
				`"headRepositoryOwner":{"id":"U_octo","login":"` + wireOwner + `"},` +
				`"commits":{"totalCount":1,"nodes":[{"commit":{"oid":"deadbeef"}}]},` +
				`"author":{"login":"someone"}}}}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	lastAuth := func() string {
		mu.Lock()
		defer mu.Unlock()
		return auth
	}
	graphQL := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
	return srv, lastAuth, graphQL
}

// TestLocalGHChannel_WiresRealGH is the ticket's outcome invariant, end to end:
// a local run's gh reaches GitHub through the injector with the org's real
// credential attached upstream, while the subprocess holds only the per-run
// placeholder — and the binary that runs is the TF-owned one even with the
// user's own gh on PATH.
func TestLocalGHChannel_WiresRealGH(t *testing.T) {
	upstream, upstreamAuth, _ := fakeUpstream(t)
	paths.SetForTest(t, t.TempDir())

	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: "conv-wire-1",
		Upstream:       upstream.URL + "/api/v3",
		BinDir:         wireBinDir(t),
		TokenSource:    func(context.Context) (string, error) { return wireReal, nil },
	})
	if err != nil {
		t.Fatalf("ghchannel.Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	// The user's own gh sits behind ours on PATH. Building the env through the
	// production helper is the point: whatever it produces is what a run gets.
	decoy, decoyMarker := decoyBinDir(t)
	t.Setenv("PATH", decoy+string(os.PathListSeparator)+"/usr/bin:/bin")
	home := t.TempDir()
	env := append([]string{"HOME=" + home}, directGHChannelEnv(&GHChannelParams{
		Host:           ch.Host,
		Token:          ch.Token,
		CertSourcePath: ch.CertPath,
		BinDir:         ch.BinDir,
		ConfigDir:      ch.ConfigDir,
	})...)

	// Asserted from OUTSIDE the subprocess: nothing handed to it carries the
	// org's credential. In local mode there is no jail to enforce this, so the
	// assertion is the enforcement.
	for _, e := range env {
		if strings.Contains(e, wireReal) {
			t.Fatalf("subprocess env entry %q carries the org's real credential", e)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Through a shell, so PATH resolution is the real thing rather than Go's
	// parent-env lookup — this is the shape Claude Code's Bash tool produces.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c",
		"gh pr view "+strconv.Itoa(wirePRNum)+" -R "+wireOwner+"/"+wireRepo)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh pr view failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), wirePRURL) {
		t.Errorf("gh output = %q, want the PR url (fixture drift?)", out)
	}
	if _, statErr := os.Stat(decoyMarker); statErr == nil {
		t.Error("the user's own gh ran — PATH must lead with the TF-owned binary")
	}

	if got := upstreamAuth(); got != "token "+wireReal {
		t.Errorf("upstream Authorization = %q, want the injected real credential", got)
	}
}

// gh must not read the user's ~/.config/gh: it holds their personal credential,
// and a local run executes under their real HOME.
func TestLocalGHChannel_IgnoresUserGHConfig(t *testing.T) {
	upstream, _, _ := fakeUpstream(t)
	paths.SetForTest(t, t.TempDir())

	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: "conv-wire-2",
		Upstream:       upstream.URL + "/api/v3",
		BinDir:         wireBinDir(t),
		TokenSource:    func(context.Context) (string, error) { return wireReal, nil },
	})
	if err != nil {
		t.Fatalf("ghchannel.Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	// A user config that would authenticate as the human if gh ever read it.
	home := t.TempDir()
	userGH := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(userGH, 0o700); err != nil {
		t.Fatalf("mkdir user gh config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userGH, "hosts.yml"),
		[]byte("github.com:\n    oauth_token: gho_the_humans_own_token\n    user: a-real-person\n"), 0o600); err != nil {
		t.Fatalf("write user hosts.yml: %v", err)
	}

	t.Setenv("PATH", "/usr/bin:/bin")
	env := append([]string{"HOME=" + home}, directGHChannelEnv(&GHChannelParams{
		Host:           ch.Host,
		Token:          ch.Token,
		CertSourcePath: ch.CertPath,
		BinDir:         ch.BinDir,
		ConfigDir:      ch.ConfigDir,
	})...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "gh auth status || true")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "a-real-person") || strings.Contains(string(out), "gho_the_humans_own_token") {
		t.Errorf("gh read the user's own config: %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "gh", "config.yml")); err == nil {
		t.Error("gh wrote into the user's config dir; GH_CONFIG_DIR must redirect it")
	}
}

// Close stops the listener and removes the channel directory. A run that
// outlives its channel gets connection-refused, which surfaces to the agent as
// a plain gh error — the same contract multi mode has.
func TestLocalGHChannel_CloseTearsDown(t *testing.T) {
	upstream, _, _ := fakeUpstream(t)
	paths.SetForTest(t, t.TempDir())

	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: "conv-wire-3",
		Upstream:       upstream.URL + "/api/v3",
		BinDir:         wireBinDir(t),
		TokenSource:    func(context.Context) (string, error) { return wireReal, nil },
	})
	if err != nil {
		t.Fatalf("ghchannel.Start: %v", err)
	}
	channelDir := paths.GHChannelDir("conv-wire-3")
	if _, err := os.Stat(ch.CertPath); err != nil {
		t.Fatalf("trust file missing before Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(channelDir); !os.IsNotExist(err) {
		t.Errorf("stat(%s) = %v, want the channel directory removed", channelDir, err)
	}
	if err := ch.Close(); err != nil {
		t.Errorf("second Close: %v, want it to be idempotent", err)
	}
}

// TestLocalGHChannel_InfersTheRepositoryFromTheCheckout is the reason a local
// channel serves gh on a unix socket: from a checkout whose origin is the
// upstream URL, under the run's git config and channel env, `gh pr view` with
// no -R resolves the checkout's repository and asks the injector about it.
// gh matches the remote git rewrites onto the TLS listener against GH_HOST
// after dropping the remote's port, so this resolves only while GH_HOST has
// none.
func TestLocalGHChannel_InfersTheRepositoryFromTheCheckout(t *testing.T) {
	upstream, _, graphQL := fakeUpstream(t)
	// A short state root, so the channel's socket path fits the platform's
	// limit whatever this test is called.
	root, err := os.MkdirTemp("", "tfgh")
	if err != nil {
		t.Fatalf("make state root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths.SetForTest(t, root)

	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: "conv-wire-4",
		Upstream:       upstream.URL + "/api/v3",
		BinDir:         wireBinDir(t),
		TokenSource:    func(context.Context) (string, error) { return wireReal, nil },
	})
	if err != nil {
		t.Fatalf("ghchannel.Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	if ch.Host == ch.GitHost {
		t.Fatalf("the channel came up without its socket (GH_HOST %q)", ch.Host)
	}

	// The run's git config: the managed git proxy's rewrites onto the
	// channel's TLS listener. Nothing here fetches, so the proxy's upstream is
	// never reached.
	const gitUpstream = "https://ghe.test"
	git, err := StartGitProxy(context.Background(), "127.0.0.1", false, &GitProxyConfig{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_git"}, nil
		},
		Upstream: gitUpstream,
	})
	if err != nil {
		t.Fatalf("StartGitProxy: %v", err)
	}
	t.Cleanup(func() { _ = git.Shutdown(context.Background()) })

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not installed: %v", err)
	}
	// gh reads the checkout's remotes with git, so git has to be on PATH.
	t.Setenv("PATH", filepath.Dir(gitBin)+string(os.PathListSeparator)+"/usr/bin:/bin")
	home := t.TempDir()
	base := append([]string{
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}, directGHChannelEnv(&GHChannelParams{
		Host:           ch.Host,
		GitHost:        ch.GitHost,
		Token:          ch.Token,
		CertSourcePath: ch.CertPath,
		BinDir:         ch.BinDir,
		ConfigDir:      ch.ConfigDir,
	})...)
	env := githooks.DirectAgentEnv(base, git.GitConfigPairsWithSSH(ch.GitHost, ch.CertPath)...)

	checkout := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", gitUpstream + "/" + wireOwner + "/" + wireRepo + ".git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = checkout
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "gh pr view "+strconv.Itoa(wirePRNum))
	cmd.Env = env
	cmd.Dir = checkout
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh pr view with no -R failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), wirePRURL) {
		t.Errorf("gh output = %q, want the PR url (fixture drift?)", out)
	}

	var named []string
	for _, body := range graphQL() {
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				Owner string `json:"owner"`
				Repo  string `json:"repo"`
			} `json:"variables"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode GraphQL body %q: %v", body, err)
		}
		if strings.Contains(req.Query, "PullRequestByNumber") {
			named = append(named, req.Variables.Owner+"/"+req.Variables.Repo)
		}
	}
	if want := wireOwner + "/" + wireRepo; !slices.Equal(named, []string{want}) {
		t.Errorf("PullRequestByNumber named %v, want the checkout's %s", named, want)
	}
}
