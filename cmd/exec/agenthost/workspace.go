package agenthost

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Workspace materialization, host-side (TFAC-546).
//
// `workspace add` used to run its git materialization in the calling process.
// Local mode that IS the host, so everything lined up; in the sandbox it built
// a jail-local checkout (ephemeral /tmp, jail-local bare, jail path recorded in
// conversation_worktrees) that the host could never see — the push gate authorized zero
// refs, the workspace snapshot captured nothing, and resume restored an empty
// run root. Moving the create behind the Client seam puts the git work on the
// host in both modes: the LocalClient body below runs in the exec process in
// local mode and inside the agenthost daemon in sandbox mode, reusing the
// shared blobless bare cache and landing the checkout in the REAL host run
// root, which the sandbox sees appear under /work.
//
// The create re-derives everything security-relevant host-side — repository row,
// clone URLs, team-tracking gate, ref validation — and takes only
// (owner, repo, ref, pr) from the caller. The workspace CLI performs the same
// checks first for friendlier agent-facing errors, but a sandboxed process
// speaking the RPC directly cannot skip them, and in particular cannot steer
// the host into cloning an arbitrary URL with the org's App credential.

// worktree-create seams so agenthost tests can exercise the orchestration
// (gates, PR URL derivation, auth threading) without spawning real git.
// Production wiring is the worktree package.
var (
	workspaceCreateCheckout = worktree.CreateForCheckoutInRoot
	workspaceCreatePR       = worktree.CreateForPRInRoot
)

// WorkspaceRoots implements Client. The LocalClient runs in the same path
// namespace as the run root (the host in local mode; the daemon's host process
// in sandbox mode — but then the SANDBOXED caller reaches this through the
// daemon dispatch, which substitutes the /work agent view), so both views are
// the same string here.
//
// The host root is the run's recorded worktree_path — the directory the agent
// process was started in (and, in multi mode, the one bind-mounted at /work).
// That beats re-deriving worktree.RunRoot(conversationID): after a cold rehydrate the
// run root is rebuilt keyed by the run's memory namespace (the blueprint run
// id), so the conversationID-derived path and the actual cwd diverge — worktree_path is
// the value the resume path maintains. The derivation is only the fallback for
// a run whose worktree_path write failed at setup.
func (c *LocalClient) WorkspaceRoots(ctx context.Context) (hostRoot, agentRoot string, err error) {
	conv, err := c.GetConversation(ctx)
	if err != nil {
		return "", "", fmt.Errorf("load conversation for workspace roots: %w", err)
	}
	root := ""
	if conv != nil {
		root = conv.WorktreePath
	}
	if root == "" {
		root = worktree.RunRoot(c.info.ConversationID)
	}
	return root, root, nil
}

// CreateWorkspaceCheckout implements Client: it materializes the checkout for
// (owner/repo, ref|pr) into the run's host run root and returns the created
// path in HOST view (the workspace CLI translates to the agent view). The
// caller is expected to have reserved the conversation_worktrees row first
// (materializeWorkspace's ordering); this method only does the git work.
//
// Everything authorization-relevant is re-derived here rather than trusted
// from the arguments: the repo must be org-configured (repository row exists,
// with a clone URL) and tracked by the run's team — the same gates the git
// proxy's Authorize applies, so a checkout is only ever created for a repo the
// proxy will then let the agent push to. Clone URLs come from the stored
// profile / the host-side PR fetch, never the wire. In multi mode the clone
// carries the org's App installation token (host-scoped env injection, exactly
// like the eager PR path); local mode stays uninjected — the operator's own
// git credentials, byte-for-byte the pre-TFAC-546 behavior.
func (c *LocalClient) CreateWorkspaceCheckout(ctx context.Context, owner, repo, ref string, prNumber int) (string, error) {
	hostRoot, _, err := c.WorkspaceRoots(ctx)
	if err != nil {
		return "", fmt.Errorf("create workspace checkout: %w", err)
	}
	return c.createWorkspaceCheckoutIn(ctx, hostRoot, owner, repo, ref, prNumber)
}

// materializeWorkspaceCheckout is the full host-side `workspace add`: resolve the
// run root once, build the checkout under it, and hand the tree to the sandbox
// uid. It writes the shared bare cache and the run-root, so it runs ONLY where
// those are owned — an all/local process, or the orchestrator serving a sidecar's
// relayed op. The capless credential sidecar owns neither (its uid can't write
// either), so its dispatch relays here instead of calling this.
func (c *LocalClient) materializeWorkspaceCheckout(ctx context.Context, owner, repo, ref string, prNumber int) (string, error) {
	// One WorkspaceRoots read serves the create AND the post-create chown/cleanup
	// containment gate, so both judge against the same root.
	hostRoot, _, err := c.WorkspaceRoots(ctx)
	if err != nil {
		return "", err
	}
	path, err := c.createWorkspaceCheckoutIn(ctx, hostRoot, owner, repo, ref, prNumber)
	if err != nil {
		return "", err
	}
	// The create ran as the host process; hand the subtree to the sandbox uid or
	// the jailed agent can't write its own checkout. On failure, remove the
	// checkout so a released reservation can retry cleanly (git clone refuses a
	// non-empty target) — but ONLY when the path is provably a strict descendant
	// of the run root, since a regressed create contract would otherwise turn
	// RemoveAll into deleting an arbitrary host directory.
	if cerr := agentproc.ChownWorkspaceCheckoutForSandbox(ctx, hostRoot, path); cerr != nil {
		if strictlyWithin(hostRoot, path) {
			_ = sandbox.RemoveRunTree(context.Background(), path)
		} else {
			agenthostLog.Warn("leaving un-chowned checkout in place; path is not verifiably inside the run root", "path", path, "host_root", hostRoot)
		}
		return "", fmt.Errorf("hand checkout to sandbox user: %w", cerr)
	}
	return path, nil
}

// createWorkspaceCheckoutIn is CreateWorkspaceCheckout with the host run root
// pre-resolved. The daemon dispatch calls this variant so ONE WorkspaceRoots
// read serves the create AND the post-create chown/cleanup containment gate —
// evaluating both against the same root, rather than two point-in-time reads
// that could in principle disagree.
func (c *LocalClient) createWorkspaceCheckoutIn(ctx context.Context, hostRoot, owner, repo, ref string, prNumber int) (string, error) {
	if ref != "" && prNumber > 0 {
		return "", fmt.Errorf("create workspace checkout: ref and pr are mutually exclusive")
	}
	if prNumber < 0 {
		return "", fmt.Errorf("create workspace checkout: invalid PR number %d", prNumber)
	}
	if ref != "" {
		if err := worktree.ValidateCheckoutRef(ref); err != nil {
			return "", fmt.Errorf("create workspace checkout: %w", err)
		}
	}

	repoID := owner + "/" + repo
	profile, err := c.GetRepo(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("create workspace checkout: load repository: %w", err)
	}
	if profile == nil {
		return "", fmt.Errorf("create workspace checkout: repo %s is not configured in Triage Factory", repoID)
	}
	if tracks, err := c.TeamTracksRepo(ctx, profile.Owner, profile.Repo); err != nil {
		return "", fmt.Errorf("create workspace checkout: check team tracking: %w", err)
	} else if !tracks {
		return "", fmt.Errorf("create workspace checkout: repo %s is not tracked by this conversation's team", repoID)
	}
	if profile.CloneURL == "" {
		return "", fmt.Errorf("create workspace checkout: repo %s has no clone URL on its repository row", repoID)
	}

	if prNumber > 0 {
		pr, err := c.GithubGetPR(ctx, profile.Owner, profile.Repo, prNumber, false)
		if err != nil {
			return "", fmt.Errorf("create workspace checkout: fetch PR #%d on %s: %w", prNumber, repoID, err)
		}
		if pr == nil {
			return "", fmt.Errorf("create workspace checkout: PR #%d not found on %s", prNumber, repoID)
		}
		upstream, head := prCloneURLs(profile.CloneURL, pr)
		return workspaceCreatePR(ctx, profile.Owner, profile.Repo, upstream, head, pr.HeadRef, prNumber, c.info.ConversationID, hostRoot,
			// WithBaseBranch refreshes origin/<base> so the worktree-local `pr diff`
			// frames against a current base, not a clone-time-frozen ref (TFAC-505).
			worktree.WithBaseBranch(pr.BaseRef),
			worktree.WithCloneAuth(c.workspaceCloneAuth(ctx, profile.Owner, upstream)))
	}
	return workspaceCreateCheckout(ctx, profile.Owner, profile.Repo, profile.CloneURL, ref, c.info.ConversationID, hostRoot,
		worktree.WithCloneAuth(c.workspaceCloneAuth(ctx, profile.Owner, profile.CloneURL)))
}

// prCloneURLs derives the (upstream, head) clone URLs to hand CreateForPRInRoot,
// keeping both in the SAME protocol as the bare's origin (profile.CloneURL) so
// CreateForPR's own-repo-vs-fork comparison (head != upstream) stays honest and
// repairOriginURL never flips the bare between SSH and HTTPS. The upstream is
// always the configured repo's own clone URL — the PR's base is owner/repo by
// construction. The head is the fork's URL in the matching protocol, or "" for
// a deleted-fork PR (head.repo == null), which the create materializes
// read-only (still reviewable via the upstream's refs/pull/<n>/head).
func prCloneURLs(originURL string, pr *ghclient.PRView) (upstream, head string) {
	upstream = originURL
	if strings.HasPrefix(originURL, "https://") {
		return upstream, pr.CloneURL
	}
	// SSH (or any non-HTTPS) origin: match with the SSH head form.
	return upstream, pr.SSHURL
}

// workspaceCloneAuth resolves the HTTPS credential for the host-side clone of a
// repo, in the form the run's placement dictates:
//
//   - Local mode: no injection. The operator's own git (SSH agent / anonymous
//     HTTPS) authenticates, byte-for-byte the pre-relocation behavior.
//   - Executor sidecar (proxyCreds set): route the clone through the run's git
//     proxy, presenting only the per-run placeholder. The real installation
//     token lives solely in the sidecar's bundle-backed proxy and is injected on
//     the upstream hop — this daemon, which parses hostile agent traffic, never
//     holds it. Same proxy, authz gate, and audit path as the agent's own git
//     and the eager-PR clone (internal/delegate). If the run started no git
//     proxy (GitProxyURL empty), inject nothing and let the clone surface its
//     own auth error rather than reaching the resolver, which is nil here.
//   - All/local in-process: mint a real repo-scoped token from the live secret
//     store (the tiered resolver; tests inject a fake). "" on any resolution
//     failure — the clone then proceeds uninjected and surfaces the auth error
//     itself if the repo is private. A failure is logged so a backend outage
//     isn't silent.
func (c *LocalClient) workspaceCloneAuth(ctx context.Context, owner, cloneURL string) worktree.CloneAuth {
	if runmode.Current() == runmode.ModeLocal {
		return worktree.CloneAuth{}
	}
	if c.proxyCreds != nil {
		if c.proxyCreds.GitProxyURL == "" {
			return worktree.CloneAuth{}
		}
		return worktree.CloneAuthViaGitProxy(c.proxyCreds.GitProxyURL, cloneHostBase(cloneURL), c.proxyCreds.GitProxyToken)
	}
	tok, err := c.githubResolver().TokenFor(ctx, c.info.OrgID, owner)
	if err != nil {
		agenthostLog.Warn("resolve workspace clone token failed; clone proceeds uninjected", "org", c.info.OrgID, "target", owner, "error", err)
		return worktree.CloneAuth{}
	}
	return worktree.CloneAuthFor(cloneURL, tok.Value)
}

// cloneHostBase reduces a clone URL to its scheme://host origin — the insteadOf
// prefix CloneAuthViaGitProxy rewrites onto the git proxy. The host-side clone
// and the in-jail agent must present the identical rewrite so both transit the
// one proxy the same way. Returns the input unchanged when it isn't a parseable
// absolute URL, leaving CloneAuthViaGitProxy to no-op on the malformed value.
func cloneHostBase(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return cloneURL
	}
	return u.Scheme + "://" + u.Host
}
