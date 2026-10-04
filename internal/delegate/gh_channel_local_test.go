package delegate

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
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
			resolveErr: fmt.Errorf("githubapp: mint installation token: %w", &url.Error{
				Op: "Post", URL: "https://api.github.com/app/installations/1/access_tokens", Err: errors.New("dial tcp: lookup api.github.com: no such host"),
			}),
			want: http.StatusBadGateway,
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
