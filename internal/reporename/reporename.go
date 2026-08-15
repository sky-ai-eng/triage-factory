// Package reporename applies repository renames TF has observed.
//
// A rename is only detectable because a repository has two halves of identity:
// the slug it is called by, which moves, and the id its provider issued, which
// does not. Without the id a rename is data loss nobody notices — under
// polling a renamed repository is a 404 plus a brand-new one — so everything
// here keys on the id and treats a repository TF has no id for as not
// renamable, in either direction.
//
// The package is the seam between "TF just enumerated some repositories" and
// "the store rewrites every reference in one transaction". It exists because
// two unrelated callers observe the same fact from different requests: the
// poller sees it in an installation's repo grant (App credentials, every
// cycle, no request of its own), and the profiler sees it in the
// /repos/{owner}/{repo} response GitHub redirects to the new name (either
// credential class, once per profile TTL). Both hand what they saw to Apply.
package reporename

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Apply reconciles the slugs of the repositories in observed against what the
// provider currently calls them, and returns how many it moved.
//
// Observing more repositories than TF tracks is expected and cheap: a
// repository with no row matches nothing. The read of TF's own identities is
// the small side of the comparison (tens of rows per org against a grant that
// may hold thousands), so this is one query plus one transaction per actual
// rename — and in the steady state, which is every cycle after the first,
// zero transactions.
//
// Best-effort by construction. A rename TF fails to apply is one it re-detects
// on the next observation, and no caller's real work — a poll cycle, a
// profiling pass — is worth failing over it.
func Apply(ctx context.Context, repos db.RepoStore, resolver ghclient.Resolver, log *slog.Logger, orgID string, observed []domain.RepoRef) int {
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
			// Terminal: stored state a rewrite cannot be applied over — another
			// repository row, or a durable entity/artifact still answering to
			// the target name. Retrying changes nothing, so this reports and
			// moves on rather than counting as a failure of the caller's work.
			//
			// Error, not Warn, and it recurs on every detection until a human
			// retires whatever holds the name. That repetition is the point:
			// the condition is permanent, and a rename this product cannot
			// apply is exactly the thing an operator has to be told about.
			// The alternative — suppressing repeats — needs durable state to
			// survive a restart, which is not worth carrying for a condition
			// that should never occur.
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
	}
	return applied
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
