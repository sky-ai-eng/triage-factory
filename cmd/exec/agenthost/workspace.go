package agenthost

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Workspace materialization, host-side (TFAC-546).
//
// `workspace add` used to run its git materialization in the calling process.
// Local mode that IS the host, so everything lined up; in the sandbox it built
// a jail-local checkout (ephemeral /tmp, jail-local bare, jail path recorded in
// run_worktrees) that the host could never see — the push gate authorized zero
// refs, the workspace snapshot captured nothing, and resume restored an empty
// run root. Moving the create behind the Client seam puts the git work on the
// host in both modes: the LocalClient body below runs in the exec process in
// local mode and inside the agenthost daemon in sandbox mode, reusing the
// shared blobless bare cache and landing the checkout in the REAL host run
// root, which the sandbox sees appear under /work.
//
// The create re-derives everything security-relevant host-side — repo profile,
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
// That beats re-deriving worktree.RunRoot(runID): after a cold rehydrate the
// run root is rebuilt keyed by the run's memory namespace (the blueprint run
// id), so the runID-derived path and the actual cwd diverge — worktree_path is
// the value the resume path maintains. The derivation is only the fallback for
// a run whose worktree_path write failed at setup.
func (c *LocalClient) WorkspaceRoots(ctx context.Context) (hostRoot, agentRoot string, err error) {
	run, err := c.GetAgentRun(ctx)
	if err != nil {
		return "", "", fmt.Errorf("load run for workspace roots: %w", err)
	}
	root := ""
	if run != nil {
		root = run.WorktreePath
	}
	if root == "" {
		root = worktree.RunRoot(c.info.RunID)
	}
	return root, root, nil
}

// CreateWorkspaceCheckout implements Client: it materializes the checkout for
// (owner/repo, ref|pr) into the run's host run root and returns the created
// path in HOST view (the workspace CLI translates to the agent view). The
// caller is expected to have reserved the run_worktrees row first
// (materializeWorkspace's ordering); this method only does the git work.
//
// Everything authorization-relevant is re-derived here rather than trusted
// from the arguments: the repo must be org-configured (repo profile exists,
// with a clone URL) and tracked by the run's team — the same gates the git
// proxy's Authorize applies, so a checkout is only ever created for a repo the
// proxy will then let the agent push to. Clone URLs come from the stored
// profile / the host-side PR fetch, never the wire. In multi mode the clone
// carries the org's App installation token (host-scoped env injection, exactly
// like the eager PR path); local mode stays uninjected — the operator's own
// git credentials, byte-for-byte the pre-TFAC-546 behavior.
func (c *LocalClient) CreateWorkspaceCheckout(ctx context.Context, owner, repo, ref string, prNumber int) (string, error) {
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
		return "", fmt.Errorf("create workspace checkout: load repo profile: %w", err)
	}
	if profile == nil {
		return "", fmt.Errorf("create workspace checkout: repo %s is not configured in Triage Factory", repoID)
	}
	if tracks, err := c.TeamTracksRepo(ctx, profile.Owner, profile.Repo); err != nil {
		return "", fmt.Errorf("create workspace checkout: check team tracking: %w", err)
	} else if !tracks {
		return "", fmt.Errorf("create workspace checkout: repo %s is not tracked by this run's team", repoID)
	}
	if profile.CloneURL == "" {
		return "", fmt.Errorf("create workspace checkout: repo %s has no clone URL on its profile", repoID)
	}

	hostRoot, _, err := c.WorkspaceRoots(ctx)
	if err != nil {
		return "", fmt.Errorf("create workspace checkout: %w", err)
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
		return workspaceCreatePR(ctx, profile.Owner, profile.Repo, upstream, head, pr.HeadRef, prNumber, c.info.RunID, hostRoot,
			// WithBaseBranch refreshes origin/<base> so the worktree-local `pr diff`
			// frames against a current base, not a clone-time-frozen ref (TFAC-505).
			worktree.WithBaseBranch(pr.BaseRef),
			worktree.WithCloneAuth(worktree.CloneAuthFor(upstream, c.workspaceCloneToken(ctx, profile.Owner))))
	}
	return workspaceCreateCheckout(ctx, profile.Owner, profile.Repo, profile.CloneURL, ref, c.info.RunID, hostRoot,
		worktree.WithCloneAuth(worktree.CloneAuthFor(profile.CloneURL, c.workspaceCloneToken(ctx, profile.Owner))))
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

// workspaceCloneToken resolves the App installation token for the host-side
// clone of a repo owned by owner — the same tiered resolver every host-routed
// gh call uses, so the API client and the git clone/fetch share one cached
// installation token. Mirrors the spawner's resolveCloneToken contract:
// multi-mode only by design (local clones keep the operator's SSH key /
// anonymous HTTPS, byte-for-byte unchanged), and "" on any resolution failure —
// the clone then proceeds uninjected and surfaces the auth error itself if the
// repo is private. A resolve failure is logged so a real backend outage (e.g.
// vault down) isn't silent.
func (c *LocalClient) workspaceCloneToken(ctx context.Context, owner string) string {
	if runmode.Current() == runmode.ModeLocal {
		return ""
	}
	tok, err := c.githubResolver().TokenFor(ctx, c.info.OrgID, owner)
	if err != nil {
		agenthostLog.Warn("resolve workspace clone token failed; clone proceeds uninjected", "org", c.info.OrgID, "target", owner, "error", err)
		return ""
	}
	return tok.Value
}
