package runsidecar

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRuntime_TokenSourcesNameTheMiss pins the sentinel each sidecar token
// source wraps when the held bundle cannot answer, which is what lets a proxy
// tell a bundle that has not arrived (a retry may help) from one that will
// never carry the credential (it will not).
func TestRuntime_TokenSourcesNameTheMiss(t *testing.T) {
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	ctx := context.Background()
	rt.githubBase = "https://github.com"
	git := rt.gitProxyConfig("https://github.com", "").TokenSource
	gh := rt.ghInjectorConfig("https://api.github.com", tls.Certificate{}, "", "conv", nil).TokenSource
	api := rt.githubAPIToken

	gitErr := func() error { _, err := git(ctx, "acme", "widgets"); return err }
	ghErr := func() error { _, err := gh(ctx); return err }
	apiErr := func() error { _, err := api(ctx, "acme", "widgets"); return err }

	expect := func(t *testing.T, source string, err error, wants ...error) {
		t.Helper()
		for _, want := range wants {
			if !errors.Is(err, want) {
				t.Errorf("%s: error %v does not wrap %v", source, err, want)
			}
		}
	}

	t.Run("no bundle", func(t *testing.T) {
		expect(t, "git", gitErr(), credbundle.ErrNoBundle, agentproc.ErrNoGitCredentials)
		expect(t, "gh", ghErr(), credbundle.ErrNoBundle)
		expect(t, "api", apiErr(), credbundle.ErrNoBundle)
	})

	t.Run("bundle missing the repository and the CLI token", func(t *testing.T) {
		rt.mu.Lock()
		rt.bundle = &credbundle.Bundle{GitHub: &credbundle.GitHubCreds{
			Mode:    credbundle.GitHubModeApp,
			BaseURL: "https://github.com",
			RepoTokens: map[string]credbundle.RepoToken{
				"acme/other": {Token: "ghs_other", ExpiresAt: time.Now().Add(time.Hour)},
			},
		}}
		rt.mu.Unlock()

		expect(t, "git", gitErr(), credbundle.ErrNoRepoToken, agentproc.ErrNoGitCredentials)
		expect(t, "gh", ghErr(), credbundle.ErrNoCLIToken)
		expect(t, "api", apiErr(), credbundle.ErrNoRepoToken)

		// The repository the bundle does cover still resolves.
		if tok, err := git(ctx, "acme", "other"); err != nil || tok.Value != "ghs_other" {
			t.Errorf("git for a covered repo = (%q, %v), want ghs_other", tok.Value, err)
		}
	})

	// A re-seal resolved after the org reconnected GitHub on another URL: its
	// tokens were issued by that host, and the lanes were fixed at start on
	// this one, so none of them attaches a token. A half naming no host is
	// refused the same way.
	for _, base := range []string{"https://ghes.acme.com", ""} {
		t.Run("bundle resolved for host "+strconv.Quote(base), func(t *testing.T) {
			rt.mu.Lock()
			rt.bundle = &credbundle.Bundle{GitHub: &credbundle.GitHubCreds{
				Mode:     credbundle.GitHubModePAT,
				BaseURL:  base,
				PAT:      "ghp_other_host",
				CLIToken: &credbundle.RepoToken{Token: "ghp_other_host"},
				RepoTokens: map[string]credbundle.RepoToken{
					"acme/widgets": {Token: "ghp_other_host"},
				},
			}}
			rt.mu.Unlock()

			expect(t, "git", gitErr(), credbundle.ErrNoRepoToken, agentproc.ErrNoGitCredentials)
			expect(t, "gh", ghErr(), credbundle.ErrNoCLIToken)
			expect(t, "api", apiErr(), credbundle.ErrNoRepoToken)
		})
	}

	t.Run("bundle with no GitHub half", func(t *testing.T) {
		rt.mu.Lock()
		rt.bundle = &credbundle.Bundle{}
		rt.mu.Unlock()

		expect(t, "git", gitErr(), credbundle.ErrNoRepoToken, agentproc.ErrNoGitCredentials)
		expect(t, "gh", ghErr(), credbundle.ErrNoCLIToken)
		expect(t, "api", apiErr(), credbundle.ErrNoRepoToken)

		_, _, _, _, jerr := rt.startJiraAPIProxy("127.0.0.1", "conv")
		var miss *credbundle.SourceMissError
		if !errors.As(jerr, &miss) || miss.Kind != "jira" || miss.State != domain.SourceUnconfigured {
			t.Errorf("jira: error %v, want an unconfigured Jira miss", jerr)
		}
	})
}
