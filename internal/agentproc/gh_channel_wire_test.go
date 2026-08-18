package agentproc

import (
	"context"
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

	"github.com/sky-ai-eng/triage-factory/internal/ghchannel"
	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// These tests drive the REAL pinned gh binary through the LOCAL-MODE channel
// composition — ghchannel.Start produces the coordinates, directGHChannelEnv
// turns them into the agent subprocess's environment, and a shell resolves and
// runs gh out of that environment. That whole chain is what a local delegated
// run performs, so testing it end to end is the only way to pin the two
// properties the channel exists for: the org's credential reaches GitHub, and
// it never reaches the subprocess.
//
// The sibling wire tests in internal/ghinjector pin what gh sends. These pin how
// local mode wires it. Both skip when the pinned binary is absent; CI stages it
// from the same pin (docker/Dockerfile, kept in lockstep with internal/ghpin).

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
// in for what internal/ghbin provisions at runtime.
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

// fakeUpstream answers what `gh pr create` needs, in gh's own wire shapes, and
// records the Authorization header it was called with.
func fakeUpstream(t *testing.T) (*httptest.Server, func() string) {
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
			_, _ = w.Write([]byte(`{"data":{"repository":{"id":"R_wire","name":"` + wireRepo +
				`","owner":{"login":"` + wireOwner + `"},"defaultBranchRef":{"name":"main"},` +
				`"viewerPermission":"WRITE","hasIssuesEnabled":true,"mergeCommitAllowed":true,` +
				`"rebaseMergeAllowed":true,"squashMergeAllowed":true}}}`))
		case strings.Contains(query, "PullRequestCreate"):
			_, _ = w.Write([]byte(`{"data":{"createPullRequest":{"pullRequest":{"id":"` + wirePRNode +
				`","url":"` + wirePRURL + `"}}}}`))
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

// TestLocalGHChannel_WiresRealGH is the ticket's outcome invariant, end to end:
// a local run's gh reaches GitHub through the injector with the org's real
// credential attached upstream, while the subprocess holds only the per-run
// placeholder — and the binary that runs is the TF-owned one even with the
// user's own gh on PATH.
func TestLocalGHChannel_WiresRealGH(t *testing.T) {
	upstream, upstreamAuth := fakeUpstream(t)
	paths.SetForTest(t, t.TempDir())

	var (
		mu   sync.Mutex
		seen []ghinjector.ObservedMutation
	)
	ch, err := ghchannel.Start(ghchannel.Config{
		ConversationID: "conv-wire-1",
		Upstream:       upstream.URL + "/api/v3",
		BinDir:         wireBinDir(t),
		TokenSource:    func(context.Context) (string, error) { return wireReal, nil },
		Observe: func(_ context.Context, m ghinjector.ObservedMutation) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, m)
		},
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
		"gh pr create -R "+wireOwner+"/"+wireRepo+" --head feature --base main --title T --body B")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh pr create failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), wirePRURL) {
		t.Errorf("gh output = %q, want the created PR url", out)
	}
	if _, statErr := os.Stat(decoyMarker); statErr == nil {
		t.Error("the user's own gh ran — PATH must lead with the TF-owned binary")
	}

	if got := upstreamAuth(); got != "token "+wireReal {
		t.Errorf("upstream Authorization = %q, want the injected real credential", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("observations = %d (%+v), want exactly 1", len(seen), seen)
	}
	if m := seen[0]; m.Kind != "pull_request" || m.Owner != wireOwner || m.Repo != wireRepo || m.Number != wirePRNum {
		t.Errorf("observation = %+v, want pull_request %s/%s#%d", m, wireOwner, wireRepo, wirePRNum)
	}
}

// gh must not read the user's ~/.config/gh: it holds their personal credential,
// and a local run executes under their real HOME.
func TestLocalGHChannel_IgnoresUserGHConfig(t *testing.T) {
	upstream, _ := fakeUpstream(t)
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
	upstream, _ := fakeUpstream(t)
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
