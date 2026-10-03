package runsidecar

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
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
	git := rt.gitProxyConfig("https://github.com").TokenSource
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
			Mode: credbundle.GitHubModeApp,
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

	t.Run("bundle with no GitHub half", func(t *testing.T) {
		rt.mu.Lock()
		rt.bundle = &credbundle.Bundle{}
		rt.mu.Unlock()

		expect(t, "git", gitErr(), credbundle.ErrNoRepoToken, agentproc.ErrNoGitCredentials)
		expect(t, "gh", ghErr(), credbundle.ErrNoCLIToken)
		expect(t, "api", apiErr(), credbundle.ErrNoRepoToken)

		_, _, _, _, jerr := rt.startJiraAPIProxy("127.0.0.1", "", "conv")
		expect(t, "jira", jerr, credbundle.ErrNoJiraCredential)
	})
}
