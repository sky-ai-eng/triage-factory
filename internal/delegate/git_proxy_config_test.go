package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitProxyConfigFor pins the per-run git-egress wiring: it is wired
// only for a multi-mode run with a resolver and a repo owner; the
// TokenSource resolves through the resolver and maps a no-credentials
// org to the typed sandbox error; and the Upstream tracks the org's
// GitHub host (via the resolver's authoritative base resolution) so a
// GHES org routes to its own host, not github.com.
func TestGitProxyConfigFor(t *testing.T) {
	const orgID = "org-1"
	const owner = "acme"

	newMultiSpawner := func(resolver ghclient.Resolver) *Spawner {
		runmode.SetForTest(t, runmode.ModeMulti)
		s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
		if resolver != nil {
			s.SetRunCredentialResolvers(resolver, nil, nil)
		}
		return s
	}

	t.Run("local mode disables the git proxy", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeLocal)
		s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
		s.SetRunCredentialResolvers(&fakeResolver{token: githubapp.Token{Value: "ghs"}}, nil, nil)
		if cfg := s.gitProxyConfigFor(context.Background(), orgID, owner); cfg != nil {
			t.Errorf("gitProxyConfigFor in local mode = %+v, want nil", cfg)
		}
	})

	t.Run("empty owner disables the git proxy", func(t *testing.T) {
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}})
		if cfg := s.gitProxyConfigFor(context.Background(), orgID, ""); cfg != nil {
			t.Errorf("gitProxyConfigFor with empty owner = %+v, want nil", cfg)
		}
	})

	t.Run("no resolver disables the git proxy", func(t *testing.T) {
		s := newMultiSpawner(nil)
		if cfg := s.gitProxyConfigFor(context.Background(), orgID, owner); cfg != nil {
			t.Errorf("gitProxyConfigFor with no resolver = %+v, want nil", cfg)
		}
	})

	t.Run("github.com org routes Upstream to github.com", func(t *testing.T) {
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		if cfg == nil {
			t.Fatal("gitProxyConfigFor = nil, want a config for a multi-mode GitHub run")
		}
		if cfg.Upstream != ghclient.DefaultBaseURL {
			t.Errorf("Upstream = %q, want %q for a github.com org", cfg.Upstream, ghclient.DefaultBaseURL)
		}
	})

	t.Run("GHES org routes Upstream to its own host (incl. the github_url-secret fallback)", func(t *testing.T) {
		// The resolver's BaseURLFor folds in org_settings AND the legacy
		// github_url secret; the fake stands in for whichever source carried
		// the host. The proxy upstream must be the GHES host, not github.com.
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}, baseURL: "https://ghes.example.com"})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		if cfg == nil {
			t.Fatal("gitProxyConfigFor = nil, want a config for a GHES GitHub run")
		}
		if cfg.Upstream != "https://ghes.example.com" {
			t.Errorf("Upstream = %q, want the GHES host (so insteadOf + proxy upstream match the worktree origin)", cfg.Upstream)
		}
	})

	t.Run("base-resolution error degrades Upstream to empty (agentproc defaults github.com)", func(t *testing.T) {
		// A resolver whose base read fails must not be paired with github.com
		// silently — gitProxyConfigFor logs and leaves Upstream empty, and a
		// wrong base only makes the push fail closed at the egress allowlist.
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}, err: errors.New("settings read failed")})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		if cfg == nil {
			t.Fatal("gitProxyConfigFor = nil, want a config")
		}
		if cfg.Upstream != "" {
			t.Errorf("Upstream = %q, want empty on a base-resolution error", cfg.Upstream)
		}
	})

	t.Run("TokenSource hands back the resolved App/PAT token", func(t *testing.T) {
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs_resolved"}})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		tok, err := cfg.TokenSource(context.Background())
		if err != nil {
			t.Fatalf("TokenSource: %v", err)
		}
		if tok.Value != "ghs_resolved" {
			t.Errorf("token value = %q, want ghs_resolved", tok.Value)
		}
	})

	t.Run("no-credentials org maps to the typed sandbox error", func(t *testing.T) {
		s := newMultiSpawner(&fakeResolver{err: ghclient.ErrNoGitHubCredentials})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		_, err := cfg.TokenSource(context.Background())
		if !errors.Is(err, agentproc.ErrNoSandboxGitCredentials) {
			t.Errorf("TokenSource err = %v, want ErrNoSandboxGitCredentials", err)
		}
	})
}
