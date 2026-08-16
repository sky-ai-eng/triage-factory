package curator

import (
	"context"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// materializePinnedRepos refreshes a worktree of every pinned repo
// inside the project's knowledge dir before each curator dispatch.
// One worktree per (project, repo) at <projectDir>/repos/<owner>/<repo>/,
// always reset hard to upstream HEAD on the user-configured branch
// (profile.BaseBranch || profile.DefaultBranch). The agent's mental
// model: "the canonical source for sky-ai-eng/sky lives at
// ./repos/sky-ai-eng/sky and reflects current upstream."
//
// Per-repo failures are logged but do not fail the dispatch — the
// curator should still be useful for a knowledge-base question even
// if one repo's bare clone is busted or its branch was renamed. The
// agent will see whichever subset of repos materialized successfully.
//
// A pin is validated at the API layer to name a repo the team tracks
// (validatePinnedRepos), so a missing profile here indicates a race: the user
// untracked the repo AFTER pinning it. Same handling — log + skip.
//
// authFor resolves the host-side fetch credential for a pinned repo, given its
// owner and clone URL. On the executor path it routes through the turn
// sidecar's git proxy (the orchestrator holds no token); on all/
// local it injects the App token resolved from the live store. It may be nil
// (tests) or return a zero CloneAuth — the refresh then runs credential-free
// exactly as before. The auth is scoped to the repository row's clone URL host and
// only injects for an https:// origin, so a local-mode SSH origin is untouched.
func materializePinnedRepos(ctx context.Context, repos db.RepositoryStore, authFor func(ctx context.Context, owner, cloneURL string) worktree.CloneAuth, orgID, projectID, projectDir string, pinnedRepos []string) {
	for _, slug := range pinnedRepos {
		if ctx.Err() != nil {
			return
		}
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			curatorLog.Warn("malformed pinned repo, skipping", "project", projectID, "repo", slug)
			continue
		}
		// GetSystem: this dispatch goroutine holds no synthetic-claims tx,
		// so an app-pool Get would 42501 on the grant-less authenticator in
		// multi mode and silently skip the repo (no bare seeded, no mounts
		// built); the org-scoped profile read goes through the admin pool,
		// like the spawner's *System reads from its detached goroutines. TFAC-65.
		profile, err := repos.GetSystem(ctx, orgID, slug)
		if err != nil {
			curatorLog.Warn("load pinned repository failed, skipping", "project", projectID, "repo", slug, "error", err)
			continue
		}
		if profile == nil {
			curatorLog.Warn("no repository row for pinned repo, removed from config after pinning, skipping", "project", projectID, "repo", slug)
			continue
		}
		branch := profile.BaseBranch
		if branch == "" {
			branch = profile.DefaultBranch
		}
		if branch == "" {
			curatorLog.Warn("pinned repo has no branch on its repository row, skipping", "project", projectID, "repo", slug)
			continue
		}
		var auth worktree.CloneAuth
		if authFor != nil {
			auth = authFor(ctx, owner, profile.CloneURL)
		}
		// Seed-on-demand (TFAC-60/-62): pass the upstream clone URL so a
		// missing bare — a private pinned repo never delegated against —
		// self-seeds via the bounded cache instead of being skipped. Auth
		// is mode-appropriate: the App token in multi (WithCloneAuth above),
		// nil/no-op over the existing anon/SSH path in local.
		if _, err := worktree.EnsureCuratorWorktree(ctx, owner, repo, branch, projectDir,
			worktree.WithCloneAuth(auth), worktree.WithCloneURL(profile.CloneURL)); err != nil {
			curatorLog.Warn("materialize pinned repo failed, skipping", "project", projectID, "repo", slug, "branch", branch, "error", err)
			continue
		}
	}
}

// materializeSharedPinnedRepos is the multi-mode (sandboxed) counterpart of
// materializePinnedRepos. Instead of a per-project worktree under the project
// dir, it materializes (or reuses) the single SHARED read-only worktree per
// (org, repo) and returns the read-only bind-mount descriptors agentproc adds
// to the sandbox spec, so every member's jail reads the same on-disk checkout
// rather than duplicating one per session (TFAC-61).
//
// Each successful repo registers a jail reader on its shared worktree; the
// returned release func decrements every one and MUST be deferred by the
// caller until after agentproc.Run returns (the jail has exited). Per-repo
// failures are logged and skipped — same best-effort contract as the local
// path: the agent still gets whatever subset materialized. The returned
// release is always non-nil and safe to call even when nothing materialized.
func materializeSharedPinnedRepos(ctx context.Context, repos db.RepositoryStore, authFor func(ctx context.Context, owner, cloneURL string) worktree.CloneAuth, orgID, projectID string, pinnedRepos []string) ([]agentproc.ReadOnlyRepoMount, func()) {
	var (
		mounts   []agentproc.ReadOnlyRepoMount
		releases []func()
	)
	releaseAll := func() {
		for _, r := range releases {
			r()
		}
	}
	// Dedup by slug. pinned_repos is uniqueness-validated at the API layer
	// (validatePinnedRepos), but this builds sandbox mounts from a raw slice:
	// a repeated slug would emit two bind mounts at the same /work/repos/<o>/<r>
	// destination (undefined under gVisor) and double-acquire the shared
	// reader. Guard it here so the function is self-contained.
	seen := make(map[string]bool, len(pinnedRepos))
	for _, slug := range pinnedRepos {
		if ctx.Err() != nil {
			return mounts, releaseAll
		}
		if seen[slug] {
			continue
		}
		seen[slug] = true
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			curatorLog.Warn("malformed pinned repo, skipping", "project", projectID, "repo", slug)
			continue
		}
		// GetSystem: this dispatch goroutine holds no synthetic-claims tx,
		// so an app-pool Get would 42501 on the grant-less authenticator in
		// multi mode and silently skip the repo (no bare seeded, no mounts
		// built); the org-scoped profile read goes through the admin pool,
		// like the spawner's *System reads from its detached goroutines. TFAC-65.
		profile, err := repos.GetSystem(ctx, orgID, slug)
		if err != nil {
			curatorLog.Warn("load pinned repository failed, skipping", "project", projectID, "repo", slug, "error", err)
			continue
		}
		if profile == nil {
			curatorLog.Warn("no repository row for pinned repo, removed from config after pinning, skipping", "project", projectID, "repo", slug)
			continue
		}
		branch := profile.BaseBranch
		if branch == "" {
			branch = profile.DefaultBranch
		}
		if branch == "" {
			curatorLog.Warn("pinned repo has no branch on its repository row, skipping", "project", projectID, "repo", slug)
			continue
		}
		var auth worktree.CloneAuth
		if authFor != nil {
			auth = authFor(ctx, owner, profile.CloneURL)
		}
		wtDir, release, err := worktree.EnsureSharedCuratorWorktree(ctx, orgID, owner, repo, branch,
			worktree.WithCloneAuth(auth), worktree.WithCloneURL(profile.CloneURL))
		if err != nil {
			curatorLog.Warn("materialize shared pinned repo failed, skipping", "project", projectID, "repo", slug, "branch", branch, "error", err)
			continue
		}
		releases = append(releases, release)
		mounts = append(mounts, agentproc.ReadOnlyRepoMount{
			Source:  wtDir,
			RelPath: filepath.Join("repos", owner, repo),
		})
	}
	return mounts, releaseAll
}

// splitOwnerRepo splits "owner/repo" once. Defensive — pinned_repos
// is shape-validated at the API layer, but this code path also runs
// against historical rows so don't trust the slice.
func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	for i := 0; i < len(slug); i++ {
		if slug[i] == '/' {
			if i == 0 || i == len(slug)-1 {
				return "", "", false
			}
			return slug[:i], slug[i+1:], true
		}
	}
	return "", "", false
}
