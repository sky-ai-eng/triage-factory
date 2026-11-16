package delegate

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghchannel"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
)

// A URL gh builds for git from GH_HOST (a `gh repo clone`, the remote a
// `gh pr checkout` of a fork adds) names the portless host gh is given, where
// nothing listens. Under the run's git config, a fetch of one still reaches
// the run's git handler, which forwards it with the real credential only
// after the per-run placeholder the config carries has passed its own check.
func TestLocalGitChannel_GHHostURLReachesTheRunsGitHandler(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not installed: %v", err)
	}
	// A short state root, so the gh channel's socket path fits the
	// platform's limit whatever this test is called.
	root, err := os.MkdirTemp("", "tfgh")
	if err != nil {
		t.Fatalf("make state root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths.SetForTest(t, root)

	type request struct{ path, auth string }
	reached := make(chan request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- request{r.URL.Path, r.Header.Get("Authorization")}:
		default:
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	ctx := context.Background()
	proxy, err := agentproc.StartGitProxy(ctx, "127.0.0.1", false, &agentproc.GitProxyConfig{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_real_git"}, nil
		},
		Upstream: upstream.URL,
	})
	if err != nil {
		t.Fatalf("StartGitProxy: %v", err)
	}
	git := &localGitChannel{proxy: proxy}
	t.Cleanup(func() { _ = git.Close() })

	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: "conv-gh-host",
		Upstream:       "https://api.github.com",
		BinDir:         t.TempDir(),
		TokenSource:    func(context.Context) (string, error) { return "ghs_real_cli", nil },
		GitHandler:     git.handler(),
	})
	if err != nil {
		t.Fatalf("ghchannel.Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	gh := localGHChannelParams(ch)
	if gh.Host == gh.GitHost {
		t.Fatalf("the gh channel came up without its socket (GH_HOST %q)", gh.Host)
	}

	env := githooks.DirectAgentEnv([]string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}, git.configPairs(gh)...)
	cmd := exec.Command("git", "ls-remote", "https://"+gh.Host+"/acme/widgets.git")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	// The upstream answers 404, so the command fails; what matters is where
	// the request went. The upstream records a request before it answers, and
	// git waits for the answer, so whatever reached it is recorded by now.
	out, _ := cmd.CombinedOutput()

	select {
	case got := <-reached:
		if got.path != "/acme/widgets.git/info/refs" {
			t.Errorf("upstream path = %q, want the repository's info/refs", got.path)
		}
		if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_real_git")); got.auth != want {
			t.Errorf("upstream Authorization = %q, want the git proxy's injected credential", got.auth)
		}
	default:
		t.Fatalf("the fetch never reached the run's git handler; git said:\n%s", out)
	}
}
