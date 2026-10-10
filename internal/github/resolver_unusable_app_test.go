package github

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

func unusableApp(reason domain.GitHubAppUnusableReason) *domain.OrgGitHubApp {
	app := activeApp()
	app.UnusableReason = reason
	app.UnusableSince = time.Date(2026, 9, 10, 21, 13, 19, 0, time.UTC)
	return app
}

// TestResolver_UnusableAppMintsNothing pins the resolver half of an App GitHub
// no longer accepts: every entry point that would mint refuses with
// ErrGitHubAppUnusable, without a request to GitHub, and the error is neither
// ErrNoGitHubCredentials (the org did set GitHub up) nor logged above Debug by
// a pass that logs through upstream.LogLevel.
func TestResolver_UnusableAppMintsNothing(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []domain.GitHubAppUnusableReason{domain.GitHubAppMissing, domain.GitHubAppKeyRejected} {
		t.Run(string(reason), func(t *testing.T) {
			gh := newGHTestServer(t)
			r := newTestResolver(
				&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
				&fakeApps{app: unusableApp(reason), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
				&fakeOrgs{base: gh.srv.URL},
				&fakeAgents{},
				nil,
			)

			calls := map[string]func() error{
				"ClientFor": func() error {
					_, err := r.ClientFor(ctx, "org-1", "acme")
					return err
				},
				"TokenForRepoScoped": func() error {
					_, err := r.(ScopedResolver).TokenForRepoScoped(ctx, "org-1", "acme", "api", nil)
					return err
				},
				"ClientForRepoScoped": func() error {
					_, _, err := r.(ScopedRepoResolver).ClientForRepoScoped(ctx, "org-1", "acme", "api", nil)
					return err
				},
			}
			for name, call := range calls {
				err := call()
				if !errors.Is(err, ErrGitHubAppUnusable) {
					t.Errorf("%s error = %v; want ErrGitHubAppUnusable", name, err)
				}
				if errors.Is(err, ErrNoGitHubCredentials) {
					t.Errorf("%s error wraps ErrNoGitHubCredentials; callers would read it as GitHub not being set up", name)
				}
				if lvl := upstream.LogLevel(err, slog.LevelWarn); lvl != slog.LevelDebug {
					t.Errorf("%s error logs at %v through upstream.LogLevel; want Debug", name, lvl)
				}
			}
			if got := atomic.LoadInt32(&gh.mintCalls); got != 0 {
				t.Errorf("mint calls = %d; want 0 for an App GitHub no longer accepts", got)
			}
		})
	}
}

// TestInstallationToken_UnusableAppDropsCachedToken: a token minted before
// GitHub stopped accepting the App has stopped working with it, so it is
// dropped rather than served for the rest of its hour.
func TestInstallationToken_UnusableAppDropsCachedToken(t *testing.T) {
	inst := installOn("acme")
	r, cache, gh := suspensionResolver(t, inst)
	cache.Set("org-1", domain.GitHubHost(gh.srv.URL), inst.InstallationID, githubapp.Token{
		Value:     "ghs_minted_before_the_app_was_deleted",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	_, err := r.installationToken(context.Background(), "org-1", resolvedApp{org: unusableApp(domain.GitHubAppMissing)}, inst, gh.srv.URL)
	if !errors.Is(err, ErrGitHubAppUnusable) {
		t.Fatalf("installationToken error = %v; want ErrGitHubAppUnusable", err)
	}
	if _, ok := cache.Get("org-1", domain.GitHubHost(gh.srv.URL), inst.InstallationID); ok {
		t.Error("the token cached before the App was deleted is still in the cache; want it invalidated")
	}
}
