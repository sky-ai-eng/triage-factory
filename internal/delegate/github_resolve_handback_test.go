package delegate

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// failingClientResolver answers every client resolve with err, as the live
// resolver does on an App org whose installation-token mint failed.
type failingClientResolver struct {
	ghclient.Resolver
	err error
}

func (r failingClientResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return nil, r.err
}

func (r failingClientResolver) BaseURLFor(context.Context, string) (string, error) {
	return "", nil
}

// mintError is what the GitHub App minter returns for an installation-token
// mint against apiBase.
func mintError(t *testing.T, apiBase string) error {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	minter, err := githubapp.NewMinter(githubapp.Config{PrivateKey: key, AppID: 1, APIBase: apiBase})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	_, mintErr := minter.MintInstallationToken(context.Background(), 1)
	if mintErr == nil {
		t.Fatal("mint succeeded")
	}
	return mintErr
}

// A GitHub run's setup cannot read its pull request without a client, and on
// an App org the client is the installation token GitHub mints. When GitHub
// cannot mint one because it is down or unreachable, the setup is handed back
// on the upstream budget like any other unreachable upstream. A credential
// nobody bound is not an outage and spends the setup budget.
func TestDispatch_AGitHubResolveFailureHandsBackByItsCause(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"Service Unavailable"}`)
	}))
	t.Cleanup(unavailable.Close)

	for name, tc := range map[string]struct {
		suffix     string
		resolveErr error
		want       string
	}{
		"GitHub answered the mint 503": {
			suffix: "1101", resolveErr: mintError(t, unavailable.URL), want: "requeued_upstream",
		},
		"GitHub unreachable for the mint": {
			suffix: "1102", resolveErr: mintError(t, "http://"+closedLoopbackAddr(t)), want: "requeued_upstream",
		},
		"no credential bound": {
			suffix:     "1103",
			resolveErr: fmt.Errorf("%w: org=%s target=owner", ghclient.ErrNoGitHubCredentials, runmode.LocalDefaultOrgID),
			want:       "requeued",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newLaunchFixtureWithWorktree(t, tc.suffix, "")
			fx.s.SetRunCredentialResolvers(failingClientResolver{err: tc.resolveErr}, nil, nil)

			conv := fx.conv
			conv.OrgID = runmode.LocalDefaultOrgID
			dispatched := make(chan struct{})
			go func() {
				defer close(dispatched)
				fx.s.dispatchClaimedConversation(context.Background(), &conv, time.Now())
			}()
			select {
			case <-dispatched:
			case <-time.After(30 * time.Second):
				t.Fatal("the engagement never returned")
			}

			if got := fx.claimOutcomes(t); len(got) != 1 || got[0] != tc.want {
				t.Errorf("claim outcomes = %v, want [%s]", got, tc.want)
			}
		})
	}
}
