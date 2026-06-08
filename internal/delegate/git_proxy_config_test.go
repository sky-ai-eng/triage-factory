package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeOrgsStore is a minimal db.OrgsStore whose only live method is
// GetSettingsSystem, so a test can pin the GitHub base URL the git proxy
// resolves its upstream from. Every other method is an unused stub.
type fakeOrgsStore struct {
	settings domain.OrgSettings
	err      error
}

func (f *fakeOrgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return f.settings, f.err
}
func (f *fakeOrgsStore) GetSettings(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return f.settings, f.err
}
func (f *fakeOrgsStore) GetOrg(ctx context.Context, orgID string) (*domain.Org, error) {
	return nil, nil
}
func (f *fakeOrgsStore) GetOrgSystem(ctx context.Context, orgID string) (*domain.Org, error) {
	return nil, nil
}
func (f *fakeOrgsStore) CreateLocalTenant(ctx context.Context) error            { return nil }
func (f *fakeOrgsStore) ListActiveSystem(ctx context.Context) ([]string, error) { return nil, nil }
func (f *fakeOrgsStore) UpdateSettings(ctx context.Context, orgID string, updates domain.OrgSettings) error {
	return nil
}

var _ db.OrgsStore = (*fakeOrgsStore)(nil)

// TestGitProxyConfigFor pins the per-run git-egress wiring: it is wired
// only for a multi-mode run with a resolver and a repo owner; the
// TokenSource resolves through the resolver and maps a no-credentials
// org to the typed sandbox error; and the Upstream tracks the org's
// GitHub host so a GHES org routes to its own host, not github.com.
func TestGitProxyConfigFor(t *testing.T) {
	const orgID = "org-1"
	const owner = "acme"

	newMultiSpawner := func(resolver ghclient.Resolver, orgs db.OrgsStore) *Spawner {
		runmode.SetForTest(t, runmode.ModeMulti)
		s := NewSpawner(nil, db.Stores{Orgs: orgs}, nil, nil, "", "")
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
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}}, &fakeOrgsStore{})
		if cfg := s.gitProxyConfigFor(context.Background(), orgID, ""); cfg != nil {
			t.Errorf("gitProxyConfigFor with empty owner = %+v, want nil", cfg)
		}
	})

	t.Run("no resolver disables the git proxy", func(t *testing.T) {
		s := newMultiSpawner(nil, &fakeOrgsStore{})
		if cfg := s.gitProxyConfigFor(context.Background(), orgID, owner); cfg != nil {
			t.Errorf("gitProxyConfigFor with no resolver = %+v, want nil", cfg)
		}
	})

	t.Run("github.com org leaves Upstream empty (agentproc defaults it)", func(t *testing.T) {
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}}, &fakeOrgsStore{})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		if cfg == nil {
			t.Fatal("gitProxyConfigFor = nil, want a config for a multi-mode GitHub run")
		}
		if cfg.Upstream != "" {
			t.Errorf("Upstream = %q, want empty for a github.com org", cfg.Upstream)
		}
	})

	t.Run("GHES org routes Upstream to its own host", func(t *testing.T) {
		orgs := &fakeOrgsStore{settings: domain.OrgSettings{GitHubBaseURL: "https://ghes.example.com/"}}
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs"}}, orgs)
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		if cfg == nil {
			t.Fatal("gitProxyConfigFor = nil, want a config for a GHES GitHub run")
		}
		// ResolveBaseURL trims the trailing slash; the host must NOT be github.com.
		if cfg.Upstream != "https://ghes.example.com" {
			t.Errorf("Upstream = %q, want the GHES host (so insteadOf + proxy upstream match the worktree origin)", cfg.Upstream)
		}
	})

	t.Run("TokenSource hands back the resolved App/PAT token", func(t *testing.T) {
		s := newMultiSpawner(&fakeResolver{token: githubapp.Token{Value: "ghs_resolved"}}, &fakeOrgsStore{})
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
		s := newMultiSpawner(&fakeResolver{err: ghclient.ErrNoGitHubCredentials}, &fakeOrgsStore{})
		cfg := s.gitProxyConfigFor(context.Background(), orgID, owner)
		_, err := cfg.TokenSource(context.Background())
		if !errors.Is(err, agentproc.ErrNoSandboxGitCredentials) {
			t.Errorf("TokenSource err = %v, want ErrNoSandboxGitCredentials", err)
		}
	})
}
