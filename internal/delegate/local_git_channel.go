package delegate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

const localGitShutdownTimeout = 3 * time.Second

type localGitChannel struct {
	proxy *agentproc.GitProxyHandle
}

func (c *localGitChannel) Close() error {
	if c == nil || c.proxy == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), localGitShutdownTimeout)
	defer cancel()
	return c.proxy.Shutdown(ctx)
}

func (c *localGitChannel) handler() http.Handler {
	if c == nil {
		return nil
	}
	return c.proxy.Handler()
}

func (c *localGitChannel) cloneAuth(cloneURL string) worktree.CloneAuth {
	if c == nil || c.proxy == nil {
		return worktree.CloneAuth{}
	}
	proxyURL, token := c.proxy.Coordinates()
	return worktree.CloneAuthViaGitProxy(proxyURL, cloneHostBase(cloneURL), token)
}

func (c *localGitChannel) configPairs(gh *agentproc.GHChannelParams) [][2]string {
	if c == nil || c.proxy == nil {
		return nil
	}
	if gh == nil {
		return c.proxy.GitConfigPairsWithSSH("", "")
	}
	return c.proxy.GitConfigPairsWithSSH(gh.Host, gh.CertSourcePath)
}

// startLocalGitChannel starts the managed Git path before workspace setup, so
// the initial clone and every later agent Git command use the configured TF
// identity. A non-GitHub task with no configured GitHub credential may still
// run; there is no configured identity for Git to standardize on in that case.
func (s *Spawner) startLocalGitChannel(ctx context.Context, orgID string, task domain.Task, info agenthost.ConversationInfo) (*localGitChannel, error) {
	if runmode.Current() != runmode.ModeLocal {
		return nil, nil
	}

	s.mu.Lock()
	resolver := s.ghResolver
	s.mu.Unlock()
	if resolver == nil {
		// Unwired test fixtures have no resolver at all. Production local mode
		// always installs one; credential absence there is the HasAnyCredential
		// arm below and remains a hard failure for a GitHub task.
		return nil, nil
	}
	scoped, ok := resolver.(ghclient.ScopedResolver)
	if !ok {
		// Compatibility for narrow test resolvers that exercise no Git path.
		// The production resolver implements ScopedResolver.
		return nil, nil
	}
	hasCredential, err := scoped.HasAnyCredential(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("resolve configured GitHub credential for local Git: %w", err)
	}
	if !hasCredential {
		if task.EntitySource == "github" {
			return nil, errors.New("GitHub credentials not configured; local Git refuses to fall back to the operator's credentials")
		}
		return nil, nil
	}

	stores, storesSet := s.getStores()
	if !storesSet {
		return nil, nil
	}
	auditHost := agenthost.NewLocal(stores, info)
	auditHost.SetGitHubResolver(resolver)
	owner, _ := ownerRepoForTask(task)
	if tok, tokErr := resolver.TokenFor(ctx, orgID, owner); tokErr == nil && tok.Value != "" && tok.ExpiresAt.IsZero() {
		auditHost.SetGitHubCredential(domain.CredentialGitHubPAT)
	}

	cfg := s.managedGitGate(info, stores, auditHost)
	if cfg == nil {
		return nil, errors.New("local Git credential proxy is not configured")
	}
	// The insteadOf upstream the proxy rewrites onto. Resolved here rather than
	// in the shared gate because only this half needs it: the sandboxed sibling
	// reads the same host off its sealed bundle. A read failure leaves it empty,
	// which the proxy fills with github.com.
	if base, err := resolver.BaseURLFor(ctx, orgID); err != nil {
		delegateLog.Warn("resolve git host base for the local git proxy failed; leaving upstream empty (defaults to github.com)", "org", orgID, "error", err)
	} else {
		cfg.Upstream = base
	}
	cfg.TokenSource = func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
		tok, err := ghclient.TokenForManagedGit(ctx, scoped, orgID, owner, repo)
		if err != nil {
			return gitproxy.Token{}, err
		}
		return gitproxy.Token{Value: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
	}
	cfg.ProbeCredentials = func(ctx context.Context) error {
		ok, err := scoped.HasAnyCredential(ctx, orgID)
		if err != nil {
			return err
		}
		if !ok {
			return agentproc.ErrNoGitCredentials
		}
		return nil
	}

	proxy, err := agentproc.StartGitProxy(ctx, "127.0.0.1", false, cfg)
	if err != nil {
		return nil, fmt.Errorf("start managed local Git channel: %w", err)
	}
	delegateLog.Info("local git channel up", "conversation", info.ConversationID)
	return &localGitChannel{proxy: proxy}, nil
}
