// Package reporename applies repository renames TF has observed.
//
// A rename is detectable only because a repository has two halves of identity:
// the slug it is called by, which moves, and the id its provider issued, which
// does not. Everything here keys on the id, so a repository TF has no id for is
// never renamable in either direction.
//
// The package is the seam between "TF just enumerated some repositories" and
// "the store rewrites every reference in one transaction", and it exists
// because two unrelated callers observe the same fact from different requests:
// the poller reads it out of an installation's repo grant, the profiler out of
// the /repos/{owner}/{repo} response GitHub redirects to the new name (the only
// path a PAT org has). Both hand what they saw to Apply.
package reporename

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Apply reconciles the slugs of the repositories in observed against what the
// provider currently calls them, and returns how many it moved.
//
// Observing more repositories than TF tracks is expected and cheap: one query
// plus one transaction per actual rename, and in the steady state — every cycle
// after the first — zero transactions.
//
// Best-effort by construction: a rename TF fails to apply is one it re-detects
// on the next observation, and no caller's real work is worth failing over it.
func Apply(ctx context.Context, repos db.RepositoryStore, resolver ghclient.Resolver, log *slog.Logger, orgID string, observed []domain.RepoRef) int {
	if repos == nil || len(observed) == 0 {
		return 0
	}
	stored, err := repos.ListIdentitiesSystem(ctx, orgID)
	if err != nil {
		log.WarnContext(ctx, "read repository identities failed", "org", orgID, "error", err)
		return 0
	}
	candidates := domain.DetectRepoRenames(stored, observed)
	if len(candidates) == 0 {
		return 0
	}

	applied := 0
	for _, ref := range candidates {
		out, err := repos.RenameSystem(ctx, orgID, ref)
		switch {
		case errors.Is(err, db.ErrRepoSlugOccupied), errors.Is(err, db.ErrRepoIdentityAmbiguous):
			// Terminal: stored state a rewrite cannot be applied over —
			// another repository row, or a durable entity/artifact still
			// answering to the target name. Retrying changes nothing.
			//
			// Error, not Warn, and it recurs on every detection until a human
			// retires whatever holds the name. The repetition is the point: a
			// rename this product cannot apply is exactly what an operator has
			// to be told about, and suppressing repeats would need durable
			// state to survive a restart.
			log.ErrorContext(ctx, "repository rename refused",
				"org", orgID, "repo", ref.Slug(), "external_id", ref.ExternalID, "error", err)
			continue
		case err != nil:
			log.WarnContext(ctx, "repository rename failed",
				"org", orgID, "repo", ref.Slug(), "error", err)
			continue
		case !out.Renamed:
			// Another observer got there first, or the candidate went stale
			// between the read and the transaction. Losing is terminal and
			// needs no retry.
			continue
		}
		applied++
		log.InfoContext(ctx, "repository renamed", "org", orgID, "from", out.From, "to", out.To)
		invalidateCoverage(resolver, orgID, out.From, out.To)
		disposeOldDirs(ctx, log, orgID, out.From)
	}
	return applied
}

// disposeOldDirs reclaims the directories the old slug named — the bare
// clone and every cold checkout it still registers — now that nothing
// derives their paths. After
// the commit and best-effort on purpose, like the coverage invalidation above:
// a directory removal cannot join the transaction, and failing to reclaim disk
// must never fail the rename. Local mode is the mode this exists for (its
// reaper is deliberately unbounded, so nothing else reclaims the orphan); in
// multi it reaches at most this pod's own disk and the TTL reaper covers the
// fleet. A tree a live worktree still holds is skipped, and nothing retries —
// the rename is the steady state afterwards — which is the accepted cost of
// never deleting under a running agent.
func disposeOldDirs(ctx context.Context, log *slog.Logger, orgID, from string) {
	owner, repo, ok := cutSlug(from)
	if !ok {
		return
	}
	if worktree.DisposeRenamedRepoDirs(orgID, owner, repo) {
		log.InfoContext(ctx, "reclaimed renamed repository's old directories", "org", orgID, "slug", from)
	}
}

// invalidateCoverage drops the cached App-grant coverage decision for BOTH
// slugs. The cache keys on the slug and holds positives only, so a rename
// leaves each entry vouching for the wrong repository: the old slug's for one
// that no longer answers to that name, the new slug's for whatever repository
// was called that before. Both are re-probed rather than inherited.
func invalidateCoverage(resolver ghclient.Resolver, orgID, from, to string) {
	inv, ok := resolver.(ghclient.RepoCoverageInvalidator)
	if !ok {
		return
	}
	for _, slug := range []string{from, to} {
		owner, repo, found := cutSlug(slug)
		if !found {
			continue
		}
		inv.InvalidateRepoCoverage(orgID, owner, repo)
	}
}

func cutSlug(slug string) (owner, repo string, ok bool) {
	for i := 0; i < len(slug); i++ {
		if slug[i] == '/' {
			return slug[:i], slug[i+1:], i > 0 && i+1 < len(slug)
		}
	}
	return "", "", false
}
