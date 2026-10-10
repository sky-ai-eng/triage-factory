package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// patHostServer is host B: the GitHub deployment an org's settings now name.
// It counts every request it receives, so a test can assert that a PAT bound
// on another host never reached it.
type patHostServer struct {
	srv      *httptest.Server
	requests int32
	lastAuth atomic.Value // string: Authorization header of the latest request
}

func newPATHostServer(t *testing.T) *patHostServer {
	t.Helper()
	p := &patHostServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p.requests, 1)
		p.lastAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"full_name":"acme/widget"}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// patBoundHost is host A: the host the org's PAT was validated on, recorded in
// the github_url secret beside it.
const patBoundHost = "https://ghe.old.example.com"

func patOnHostResolver(boundURL, orgBase string) Resolver {
	return newTestResolver(
		&fakeSecrets{vals: map[string]string{
			integrations.KeyGitHubPAT: "ghp_bound_on_a",
			integrations.KeyGitHubURL: boundURL,
		}},
		&fakeApps{app: nil},
		&fakeOrgs{base: orgBase},
		&fakeAgents{agent: &domain.Agent{GitHubOrgLogin: "octo-bot", GitHubOrgEmail: "bot@example.com"}},
		nil,
	)
}

// TestResolver_PATBoundOnAnotherHostIsRefused pins the rule that an org PAT is
// sent only to the host it was validated on. The org's settings now name host
// B; the PAT was validated on host A. Every PAT-tier entry point refuses with a
// *PATHostMismatchError naming both hosts, rather than reading as "no
// credential" or sending the token to B — and B sees no request at all.
func TestResolver_PATBoundOnAnotherHostIsRefused(t *testing.T) {
	hostB := newPATHostServer(t)
	r := patOnHostResolver(patBoundHost, hostB.srv.URL)
	scoped := r.(ScopedResolver)
	ctx := context.Background()

	calls := map[string]func() error{
		"ClientFor": func() error {
			_, err := r.ClientFor(ctx, "org-1", "acme")
			return err
		},
		"ClientForRepo": func() error {
			_, err := r.ClientForRepo(ctx, "org-1", "acme", "widget")
			return err
		},
		"ClientForRepoScoped": func() error {
			_, _, err := r.(*resolver).ClientForRepoScoped(ctx, "org-1", "acme", "widget", nil)
			return err
		},
		"TokenFor": func() error {
			_, err := r.TokenFor(ctx, "org-1", "acme")
			return err
		},
		"TokenForRepoScoped": func() error {
			_, err := scoped.TokenForRepoScoped(ctx, "org-1", "acme", "widget", nil)
			return err
		},
		"TokenForReposScoped": func() error {
			_, err := scoped.TokenForReposScoped(ctx, "org-1", "acme", []string{"widget"}, nil)
			return err
		},
		"HasAnyCredential": func() error {
			_, err := scoped.HasAnyCredential(ctx, "org-1")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, ErrPATHostMismatch) {
				t.Fatalf("err = %v; want ErrPATHostMismatch", err)
			}
			if errors.Is(err, ErrNoGitHubCredentials) {
				t.Error("the mismatch wraps ErrNoGitHubCredentials; callers would read a bound PAT as GitHub not being set up")
			}
			var mismatch *PATHostMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("err = %T; want a *PATHostMismatchError", err)
			}
			if mismatch.BoundHost != patBoundHost {
				t.Errorf("BoundHost = %q; want %q", mismatch.BoundHost, patBoundHost)
			}
			if want := domain.GitHubHost(hostB.srv.URL); mismatch.CurrentHost != want {
				t.Errorf("CurrentHost = %q; want %q", mismatch.CurrentHost, want)
			}
		})
	}

	if name, email, ok := r.OrgIdentityFor(ctx, "org-1"); ok {
		t.Errorf("OrgIdentityFor = (%q, %q, true); a PAT bound on another host lends no commit identity", name, email)
	}
	if got := atomic.LoadInt32(&hostB.requests); got != 0 {
		t.Errorf("host B received %d requests; a PAT bound on host A must never be sent there", got)
	}
}

// TestResolver_PATOnItsBoundHostResolves is the control: the same PAT on the
// host it was validated on resolves and is sent. The bound URL carries a
// trailing slash the org setting does not, since both are compared as
// GitHubHost values.
func TestResolver_PATOnItsBoundHostResolves(t *testing.T) {
	host := newPATHostServer(t)
	r := patOnHostResolver(host.srv.URL+"/", host.srv.URL)
	ctx := context.Background()

	client, err := r.ClientFor(ctx, "org-1", "acme")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Get(ctx, "/probe"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got, _ := host.lastAuth.Load().(string); got != "Bearer ghp_bound_on_a" {
		t.Errorf("request carried %q; want the PAT on its own host", got)
	}

	tok, err := r.TokenFor(ctx, "org-1", "acme")
	if err != nil || tok.Value != "ghp_bound_on_a" {
		t.Errorf("TokenFor = (%q, %v); want the PAT", tok.Value, err)
	}
	ok, err := r.(ScopedResolver).HasAnyCredential(ctx, "org-1")
	if err != nil || !ok {
		t.Errorf("HasAnyCredential = (%v, %v); want (true, nil)", ok, err)
	}
	if name, _, ok := r.OrgIdentityFor(ctx, "org-1"); !ok || name != "octo-bot" {
		t.Errorf("OrgIdentityFor = (%q, %v); want the PAT's captured identity", name, ok)
	}
}

// TestResolver_PATWithNoBoundHostUsesOrgHost pins the other side of
// GitHubPATHostMatches: a PAT with no host recorded beside it is used on the
// host the org resolves to, which is the host its bind would have recorded.
func TestResolver_PATWithNoBoundHostUsesOrgHost(t *testing.T) {
	host := newPATHostServer(t)
	r := patOnHostResolver("", host.srv.URL)

	tok, err := r.TokenFor(context.Background(), "org-1", "acme")
	if err != nil || tok.Value != "ghp_bound_on_a" {
		t.Errorf("TokenFor = (%q, %v); want the PAT", tok.Value, err)
	}
}
