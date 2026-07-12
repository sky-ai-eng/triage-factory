package delegate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// bundleRepoToken resolves the GitHub credential a bundle carries for
// (owner, repo): an exact repo-scoped installation token when repo is
// known, falling through to the single org-wide PAT (a PAT can't be
// narrowed, so it answers for every repo) if no repo-scoped entry matches.
//
// repo == "" (the caller has no specific repo in view) resolves ONLY when
// owner has exactly one RepoTokens entry — mirroring the production
// resolver's installationFor ambiguity rule ("an empty target only
// resolves when there's exactly one installation"). Map iteration order is
// otherwise undefined, and a bundle's RepoTokens are scoped per-repo
// (unlike the live resolver's account-wide App installation token), so
// picking an arbitrary sibling repo's token when the run's authorized set
// covers more than one repo under owner would 403 every call it's used
// for. Every real caller passes a concrete repo (see ownerRepoForTask); the
// ambiguous-repo case only falls through to the PAT, never to a wrong
// App token.
func bundleRepoToken(gh *credbundle.GitHubCreds, owner, repo string) (token string, expiresAt time.Time, ok bool) {
	if gh == nil {
		return "", time.Time{}, false
	}
	if repo != "" {
		for repoID, rt := range gh.RepoTokens {
			o, r, cut := strings.Cut(repoID, "/")
			if cut && strings.EqualFold(o, owner) && strings.EqualFold(r, repo) {
				return rt.Token, rt.ExpiresAt, true
			}
		}
	} else {
		var match credbundle.RepoToken
		matches := 0
		for repoID, rt := range gh.RepoTokens {
			o, _, cut := strings.Cut(repoID, "/")
			if cut && strings.EqualFold(o, owner) {
				matches++
				match = rt
			}
		}
		if matches == 1 {
			return match.Token, match.ExpiresAt, true
		}
	}
	if gh.PAT != "" {
		return gh.PAT, time.Time{}, true
	}
	return "", time.Time{}, false
}

// bundleGHClient builds a *ghclient.Client straight from a bundle's
// GitHub credential — the TF_ROLE=executor mirror of resolveGHClient's
// resolver.ClientFor path, with no secret-store read: the token was
// already minted host-side by the brain and sealed into this run's
// bundle. repo disambiguates exactly like bundleRepoToken's.
func bundleGHClient(gh *credbundle.GitHubCreds, owner, repo string) *ghclient.Client {
	token, _, ok := bundleRepoToken(gh, owner, repo)
	if !ok {
		return nil
	}
	base := ghclient.DefaultBaseURL
	if gh.BaseURL != "" {
		base = gh.BaseURL
	}
	return ghclient.NewClient(base, token)
}

// bundleGitProxyConfigFor is gitProxyConfigFor's TF_ROLE=executor
// counterpart (TFAC-614): every credential-needing closure re-reads
// run_credentials and unseals fresh on each call — via tryUnsealBundle,
// the same helper the awaiting-credentials wait uses — rather than
// closing over the static bundle ctx carried at run start. This is what
// lets the brain's refresh sweep (re-minted GitHub tokens for a
// long-running run) actually reach the sandbox: the TokenSource always
// sees the newest sealed bundle, not a snapshot from claim time.
func (s *Spawner) bundleGitProxyConfigFor(ctx context.Context, info agenthost.RunInfo, stores db.Stores) *agentproc.GitProxyConfig {
	orgID := info.OrgID
	_, myBootEpoch := s.executorIdentity()

	currentGitHub := func(ctx context.Context) (*credbundle.GitHubCreds, error) {
		bundle, ok := s.tryUnsealBundle(ctx, orgID, info.RunID, myBootEpoch)
		if !ok {
			return nil, fmt.Errorf("%w: org %s (no current credential bundle for run %s)", agentproc.ErrNoSandboxGitCredentials, orgID, info.RunID)
		}
		return bundle.GitHub, nil
	}

	// Probe once at wiring time only to decide whether to wire a git proxy
	// at all — a Jira-only / no-GitHub org gets none, matching
	// gitProxyConfigFor's own gate. The per-request closures below always
	// re-check live regardless.
	gh, err := currentGitHub(ctx)
	if err != nil || gh == nil {
		return nil
	}

	denialHost := agenthost.NewLocal(stores, info)
	return &agentproc.GitProxyConfig{
		Upstream: gh.BaseURL,
		TokenSource: func(ctx context.Context, owner, repo string) (gitproxy.Token, error) {
			gh, err := currentGitHub(ctx)
			if err != nil {
				return gitproxy.Token{}, err
			}
			token, expiresAt, ok := bundleRepoToken(gh, owner, repo)
			if !ok {
				return gitproxy.Token{}, fmt.Errorf("%w: org %s repo %s/%s", agentproc.ErrNoSandboxGitCredentials, orgID, owner, repo)
			}
			return gitproxy.Token{Value: token, ExpiresAt: expiresAt}, nil
		},
		ProbeCredentials: func(ctx context.Context) error {
			_, err := currentGitHub(ctx)
			return err
		},
		Authorize: func(ctx context.Context, owner, repo string) (gitproxy.Decision, error) {
			return gitAuthorizeDecision(ctx, stores, info, owner, repo)
		},
		RecordDenial: func(ctx context.Context, denied gitproxy.DeniedGitOp) {
			denialHost.RecordGitDenied(ctx, denied.Owner, denied.Repo, denied.Ref, denied.Op, denied.Reason)
		},
	}
}

// bundleAgentHostCredentials returns the per-request bundle accessor
// agenthost.Start threads into its Server (see Server.bundleFunc) so the
// daemon's exec-gh/exec-jira verbs can resolve credentials from this run's
// sealed bundle on the executor path. Mirrors bundleGitProxyConfigFor's
// currentGitHub closure: re-reads run_credentials and unseals fresh on every
// call rather than a captured snapshot, so the brain's refresh sweep reaches
// every subsequent gh/jira verb exactly like it already reaches the git
// proxy's TokenSource.
func (s *Spawner) bundleAgentHostCredentials(orgID, runID string) func(ctx context.Context) (*credbundle.Bundle, bool) {
	_, myBootEpoch := s.executorIdentity()
	return func(ctx context.Context) (*credbundle.Bundle, bool) {
		return s.tryUnsealBundle(ctx, orgID, runID, myBootEpoch)
	}
}
