package delegate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/gitssh"
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
	var pairs [][2]string
	if gh == nil {
		pairs = c.proxy.GitConfigPairsWithSSH("", "")
	} else {
		pairs = c.proxy.GitConfigPairsWithSSH(gh.Host, gh.CertSourcePath)
	}
	// The SSH bridge below carries a fetch by mapping each protocol-v2 command
	// onto one stateless request; v0's negotiation is stateful and has no such
	// mapping, so a v2 session is the bridge's precondition. v2 has been git's
	// default since 2.26, so this states what a managed run already gets rather
	// than changing it — and it states it where an operator's own config cannot
	// take it away.
	return append(pairs, [2]string{"protocol.version", "2"})
}

// gitSSHCommand is what a managed local run's git execs in place of ssh. git
// runs the value through a shell, which is what lets it name the binary the
// same way the hooks do — one resolution for both, tracking whatever the
// spawner exported, with the PATH fallback for a host where it exported
// nothing.
const gitSSHCommand = `"${TRIAGE_FACTORY_BIN:-triagefactory}" git-ssh`

// sshBridgeEnv routes the run's SSH-shaped git at the same proxy its HTTPS git
// already uses. The insteadOf rewrites turn the canonical SSH spellings of the
// org host into proxy HTTPS before a transport is ever chosen, and they stay
// the fast path; this is the layer beneath them, where a spelling they missed
// — or an operator pushInsteadOf that outranks them — still cannot reach the
// network as the operator's own key.
//
// GIT_SSH_VARIANT pins the option dialect git uses so it never probes the
// dispatcher to discover one. Nil when no upstream host resolves: with nothing
// to match on, the dispatcher would pass every session through anyway.
func (c *localGitChannel) sshBridgeEnv() []string {
	if c == nil || c.proxy == nil {
		return nil
	}
	host := c.proxy.UpstreamHost()
	if host == "" {
		return nil
	}
	proxyURL, token := c.proxy.Coordinates()
	return []string{
		"GIT_SSH_COMMAND=" + gitSSHCommand,
		"GIT_SSH_VARIANT=ssh",
		gitssh.UpstreamHostEnvVar + "=" + host,
		gitssh.ProxyURLEnvVar + "=" + proxyURL,
		gitssh.ProxyTokenEnvVar + "=" + token,
	}
}

// startLocalGitChannel starts the managed Git path before workspace setup, so
// the initial clone and every later agent Git command use the configured TF
// identity. It is the HTTPS half of the clone-protocol choice: an org that
// selected SSH gets no channel and keeps its own transport, for the reasons
// below. A non-GitHub task with no configured GitHub credential may still run;
// there is no configured identity for Git to standardize on in that case.
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

	// Ordered after the credential check, not before it: an org with no GitHub
	// credential cannot do GitHub work at all — the REST surface needs one
	// whatever the Git transport is — so that refusal stands for every
	// protocol.
	//
	// Past it, an org that selected the SSH clone protocol selected the
	// operator's own key as this deployment's Git identity. That is a
	// CONFIGURED identity, not the ambient credential-helper fallback the
	// refusal guards against, and the managed channel is an HTTPS credential
	// path end to end — standing one up would substitute the one transport the
	// operator explicitly chose against, on a host that may well restrict it.
	//
	// Returning nil settles the whole run rather than one decision: the clone
	// URL, the insteadOf rewrites, the SSH bridge and the push-capture
	// stand-down are all conditioned on this channel existing. Ref policy and
	// push recording then fall to the pre-push hook, which is what it is for
	// when no proxy owns them. The gaps that come with it — a --no-verify
	// bypass, and recording at pre-push time rather than on the upstream's
	// answer — are disclosed where the protocol is chosen.
	if s.useSSHCloneProtocol(ctx, orgID, info.ConversationID) {
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
