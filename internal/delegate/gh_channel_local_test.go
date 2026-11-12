package delegate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghinjector"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// The local gh channel answers a credential it cannot produce the way the
// sidecar's channel does. One the resolver can never produce, or a fault of
// TF's own, is a 403, which gh reports as a refusal; only a resolver that
// could not reach GitHub, or that GitHub rate-limited, keeps the 502 an
// outage is answered with. Driven through the injector the channel runs.
func TestLocalGHChannel_AnUnresolvableCredentialIsARefusalNotAnOutage(t *testing.T) {
	for name, tc := range map[string]struct {
		token      githubapp.Token
		resolveErr error
		want       int
	}{
		"no installation for the owner": {
			resolveErr: fmt.Errorf("%w: org=%s: app has no installation for owner", ghclient.ErrNoGitHubCredentials, runmode.LocalDefaultOrgID),
			want:       http.StatusForbidden,
		},
		"the keychain could not be read": {
			resolveErr: errors.New("read keychain: user interaction is not allowed"),
			want:       http.StatusForbidden,
		},
		"an empty token": {
			want: http.StatusForbidden,
		},
		"GitHub unreachable while minting": {
			resolveErr: fmt.Errorf("githubapp: mint installation token: %w", &upstream.TransportError{Err: &url.Error{
				Op: "Post", URL: "https://api.github.com/app/installations/1/access_tokens", Err: errors.New("dial tcp: lookup api.github.com: no such host"),
			}}),
			want: http.StatusBadGateway,
		},
		"a local step ran out of time": {
			resolveErr: fmt.Errorf("read installation: %w", context.DeadlineExceeded),
			want:       http.StatusForbidden,
		},
		"an unmarked transport error": {
			resolveErr: &url.Error{Op: "Get", URL: "http://127.0.0.1:1/secret", Err: errors.New("connect: connection refused")},
			want:       http.StatusForbidden,
		},
		"GitHub rate-limited the mint": {
			resolveErr: &githubapp.APIStatusError{Op: "mint installation token", StatusCode: 429, Class: upstream.RateLimited},
			want:       http.StatusBadGateway,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := &fakeResolver{token: tc.token, err: tc.resolveErr}
			injector, err := ghinjector.New(ghinjector.Config{
				Upstream:    "https://api.github.com",
				TokenSource: localGHTokenSource(resolver, runmode.LocalDefaultOrgID, "owner"),
			})
			if err != nil {
				t.Fatalf("ghinjector.New: %v", err)
			}
			rec := httptest.NewRecorder()
			injector.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v3/repos/owner/repo/pulls/7", nil))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// scopedResolverStub answers every scoped mint with err.
type scopedResolverStub struct{ err error }

func (s scopedResolverStub) TokenForRepoScoped(context.Context, string, string, string, map[string]string) (githubapp.Token, ghclient.Identity, error) {
	return githubapp.Token{}, ghclient.IdentityUnknown, s.err
}

func (s scopedResolverStub) TokenForReposScoped(context.Context, string, string, []string, map[string]string) (githubapp.Token, error) {
	return githubapp.Token{}, s.err
}

func (s scopedResolverStub) HasAnyCredential(context.Context, string) (bool, error) { return true, nil }

// TestLocalGitTokenSource_OnlyAMarkedOutageStaysRetryable: the local git
// channel keeps the resolver's error, which the git proxy answers 502, only
// when a client marked it as an upstream that was unreachable or rate
// limited. Anything else is answered as a missing credential, the 403 a setup
// clone fails on within its own budget. The shape of the error never decides
// it: a local step's own deadline looks like a timeout and a refused local
// socket like an unreachable host.
func TestLocalGitTokenSource_OnlyAMarkedOutageStaysRetryable(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		retryable bool
	}{
		"GitHub unreachable while minting": {err: &upstream.TransportError{Err: errors.New("dial tcp: connection refused")}, retryable: true},
		"GitHub rate-limited the mint":     {err: &githubapp.APIStatusError{Op: "mint installation token", StatusCode: 429, Class: upstream.RateLimited}, retryable: true},
		"a local step ran out of time":     {err: fmt.Errorf("read installation: %w", context.DeadlineExceeded)},
		"an unmarked transport error":      {err: &url.Error{Op: "Get", URL: "http://127.0.0.1:1/x", Err: errors.New("connect: connection refused")}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := localGitTokenSource(scopedResolverStub{err: tc.err}, runmode.LocalDefaultOrgID)(context.Background(), "owner", "repo")
			if got := !errors.Is(err, credbundle.ErrNoRepoToken); got != tc.retryable {
				t.Errorf("retryable = %v, want %v (err %v)", got, tc.retryable, err)
			}
		})
	}
}

// A local run whose gh channel cannot start still gets an isolation-only
// channel: an empty per-run gh config dir and no listener, so the user's own gh
// — the one on PATH when TF has no pinned binary — has nothing of the user's to
// act with. The closer removes the dir with the run.
func TestStartLocalGHChannel_DegradedRunIsolatesGHConfig(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())

	s := &Spawner{} // no GitHub resolver: the first degrade path
	gc, closer := s.startLocalGHChannel(context.Background(), runmode.LocalDefaultOrgID, "conv-iso", "", agenthost.ConversationInfo{})
	if gc == nil {
		t.Fatal("degraded channel = nil, want isolation-only params")
	}
	if gc.Live() {
		t.Errorf("degraded channel reports Live: %+v", gc)
	}
	want := filepath.Join(paths.GHChannelDir("conv-iso"), "config")
	if gc.ConfigDir != want {
		t.Errorf("ConfigDir = %q, want %q", gc.ConfigDir, want)
	}
	if entries, err := os.ReadDir(gc.ConfigDir); err != nil || len(entries) != 0 {
		t.Errorf("config dir: entries=%d err=%v, want an existing empty dir", len(entries), err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(paths.GHChannelDir("conv-iso")); !os.IsNotExist(err) {
		t.Errorf("channel dir after Close: err=%v, want it removed", err)
	}
}

// Multi mode brings its own channel through the sidecar, so the local path
// leaves it alone.
func TestStartLocalGHChannel_MultiModeReturnsNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	paths.SetForTest(t, t.TempDir())

	s := &Spawner{}
	if gc, _ := s.startLocalGHChannel(context.Background(), "org", "conv-multi", "", agenthost.ConversationInfo{}); gc != nil {
		t.Errorf("multi-mode channel = %+v, want nil", gc)
	}
}
