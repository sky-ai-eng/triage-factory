package curator

import (
	"context"
	"log"
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
// pinned_repos is validated at the API layer to require a row in
// repo_profiles (validatePinnedRepos), so a missing profile here
// indicates a race: the user removed the repo from configured-repos
// AFTER pinning. Same handling — log + skip.
//
// tokenFor resolves the host-side clone credential for a repo's owner
// so private-repo refreshes authenticate in multi mode. It may be
// nil (tests) or return "" — CloneAuthFor then no-ops, and the refresh runs
// credential-free exactly as before. The token is scoped to the profile's
// clone URL host and only injects for an https:// origin, so a local-mode SSH
// origin is untouched.
func materializePinnedRepos(ctx context.Context, repos db.RepoStore, tokenFor func(ctx context.Context, owner string) string, orgID, projectID, projectDir string, pinnedRepos []string) {
	for _, slug := range pinnedRepos {
		if ctx.Err() != nil {
			return
		}
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			log.Printf("[curator] project %s: malformed pinned repo %q (skipping)", projectID, slug)
			continue
		}
		profile, err := repos.Get(ctx, orgID, slug)
		if err != nil {
			log.Printf("[curator] project %s: load profile for %s: %v (skipping)", projectID, slug, err)
			continue
		}
		if profile == nil {
			log.Printf("[curator] project %s: no profile for pinned repo %s — repo was removed from config after pinning (skipping)", projectID, slug)
			continue
		}
		branch := profile.BaseBranch
		if branch == "" {
			branch = profile.DefaultBranch
		}
		if branch == "" {
			log.Printf("[curator] project %s: %s has no branch in profile (skipping)", projectID, slug)
			continue
		}
		var auth worktree.CloneAuth
		if tokenFor != nil {
			auth = worktree.CloneAuthFor(profile.CloneURL, tokenFor(ctx, owner))
		}
		// Seed-on-demand (TFAC-60/-62): pass the upstream clone URL so a
		// missing bare — a private pinned repo never delegated against —
		// self-seeds via the bounded cache instead of being skipped. Auth
		// is mode-appropriate: the App token in multi (WithCloneAuth above),
		// nil/no-op over the existing anon/SSH path in local.
		if _, err := worktree.EnsureCuratorWorktree(ctx, owner, repo, branch, projectDir,
			worktree.WithCloneAuth(auth), worktree.WithCloneURL(profile.CloneURL)); err != nil {
			log.Printf("[curator] project %s: materialize %s @ %s failed: %v (skipping)", projectID, slug, branch, err)
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
func materializeSharedPinnedRepos(ctx context.Context, repos db.RepoStore, tokenFor func(ctx context.Context, owner string) string, orgID, projectID string, pinnedRepos []string) ([]agentproc.ReadOnlyRepoMount, func()) {
	var (
		mounts   []agentproc.ReadOnlyRepoMount
		releases []func()
	)
	releaseAll := func() {
		for _, r := range releases {
			r()
		}
	}
	for _, slug := range pinnedRepos {
		if ctx.Err() != nil {
			return mounts, releaseAll
		}
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			log.Printf("[curator] project %s: malformed pinned repo %q (skipping)", projectID, slug)
			continue
		}
		profile, err := repos.Get(ctx, orgID, slug)
		if err != nil {
			log.Printf("[curator] project %s: load profile for %s: %v (skipping)", projectID, slug, err)
			continue
		}
		if profile == nil {
			log.Printf("[curator] project %s: no profile for pinned repo %s — repo was removed from config after pinning (skipping)", projectID, slug)
			continue
		}
		branch := profile.BaseBranch
		if branch == "" {
			branch = profile.DefaultBranch
		}
		if branch == "" {
			log.Printf("[curator] project %s: %s has no branch in profile (skipping)", projectID, slug)
			continue
		}
		var auth worktree.CloneAuth
		if tokenFor != nil {
			auth = worktree.CloneAuthFor(profile.CloneURL, tokenFor(ctx, owner))
		}
		wtDir, release, err := worktree.EnsureSharedCuratorWorktree(ctx, orgID, owner, repo, branch,
			worktree.WithCloneAuth(auth), worktree.WithCloneURL(profile.CloneURL))
		if err != nil {
			log.Printf("[curator] project %s: materialize shared %s @ %s failed: %v (skipping)", projectID, slug, branch, err)
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
