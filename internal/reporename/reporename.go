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
//
// Both observe the org's current GitHub host, and Apply is told which one: a
// provider id identifies a repository only on the host that issued it, so the
// comparison, the rename and the cache eviction all stay on that host.
//
// The same observations also report the opposite case: a name TF holds for one
// repository that GitHub now serves under another id. The repository was
// deleted, or renamed away and the name reused, and TF learns which only by
// asking GitHub for the stored row's own id (Resolve): an answer under another
// name is a rename, a 404 retires the row. Either frees the name for the
// repository that has it now, which then gets a row of its own; the two are
// never merged, because a different id is a different repository.
package reporename

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Apply reconciles the repositories in observed against what TF stores for
// them, and returns how many rows it changed. Two conditions, each keyed on a
// provider id:
//
//   - A row TF holds under one name is observed under its own id with
//     another: a rename (or, for a retired row, a restore), applied through
//     RepositoryStore.RenameSystem.
//   - A name TF holds for one id is observed under another id: the row
//     holding it is resolved by its own id (Resolve). This runs first, so a
//     rename onto that name reported by the same observation lands in the
//     same pass.
//
// Observing more repositories than TF tracks is expected and cheap: one query,
// plus one transaction per actual rename and one GitHub request per reassigned
// name, and in the steady state — every cycle after the first — neither.
//
// Best-effort by construction: a change TF fails to apply is one it re-detects
// on the next observation, and no caller's real work is worth failing over it.
// A nil resolver skips the reassigned names, which need GitHub to resolve.
//
// host is the GitHub host observed was read from — the org's current one. It is
// stamped onto every observation, and only the repositories stored on it are
// compared against them.
func Apply(ctx context.Context, repos db.RepositoryStore, resolver ghclient.Resolver, log *slog.Logger, orgID, host string, observed []domain.RepoRef) int {
	if repos == nil || len(observed) == 0 {
		return 0
	}
	stamped := make([]domain.RepoRef, len(observed))
	for i, o := range observed {
		o.Host = host
		stamped[i] = o
	}
	observed = stamped
	stored, err := repos.ListIdentitiesSystem(ctx, orgID, host)
	if err != nil {
		log.WarnContext(ctx, "read repository identities failed", "org", orgID, "error", err)
		return 0
	}

	applied := 0
	if resolver != nil {
		for _, held := range domain.DetectReassignedRepoNames(stored, observed) {
			res, err := Resolve(ctx, repos, resolver, log, orgID, held)
			if err != nil {
				log.WarnContext(ctx, "resolving a repository whose name GitHub serves under another id failed",
					"org", orgID, "repo", held.Slug(), "external_id", held.ExternalID, "error", err)
				continue
			}
			if res != Kept {
				applied++
			}
		}
	}

	for _, ref := range domain.DetectRepoRenames(stored, observed) {
		out, err := repos.RenameSystem(ctx, orgID, ref)
		switch {
		case errors.Is(err, db.ErrRepoSlugOccupied), errors.Is(err, db.ErrRepoIdentityAmbiguous):
			// Terminal: stored state a rewrite cannot be applied over — a
			// durable entity/artifact still answering to the target name, a
			// bare row a team tracks, or a row holding the name under another
			// id that the pass above could not resolve. Retrying changes
			// nothing until that state does.
			//
			// Error, not Warn, and it recurs on every detection. The repetition
			// is the point: a rename this product cannot apply is exactly what
			// an operator has to be told about, and suppressing repeats would
			// need durable state to survive a restart.
			log.ErrorContext(ctx, "repository rename refused",
				"org", orgID, "repo", ref.Slug(), "external_id", ref.ExternalID, "error", err)
			continue
		case err != nil:
			log.WarnContext(ctx, "repository rename failed",
				"org", orgID, "repo", ref.Slug(), "error", err)
			continue
		case !out.Changed():
			// Another observer got there first, or the candidate went stale
			// between the read and the transaction. Losing is terminal and
			// needs no retry.
			continue
		}
		applied++
		if out.Restored {
			log.InfoContext(ctx, "retired repository restored", "org", orgID, "from", out.From, "to", out.To)
		} else {
			log.InfoContext(ctx, "repository renamed", "org", orgID, "from", out.From, "to", out.To)
		}
		invalidateCoverage(resolver, orgID, host, out.From, out.To)
	}
	return applied
}

// Resolution is what Resolve did with a row holding a name GitHub serves under
// another id.
type Resolution int

const (
	// Kept: the row still holds its name. GitHub answered for its id under
	// that name, so the observation that pointed here was stale, or the row
	// was renamed or retired by someone else first. Every error comes back
	// as Kept too, a rename onto a name that is still held among them.
	Kept Resolution = iota
	// Renamed: GitHub answered for the row's id under another name, and the
	// row moved there, freeing the one it held.
	Renamed
	// Retired: GitHub does not answer for the row's id, and the row was
	// retired, freeing its name.
	Retired
)

// Resolve asks GitHub what became of the repository a stored row holds a name
// for, by the row's own id (GET /repositories/{id}), and frees the name when
// the answer says the row no longer has it:
//
//   - GitHub answers under another name: the repository was renamed or
//     transferred, and RepositoryStore.RenameSystem applies it.
//   - GitHub answers 404: the repository was deleted, or the credential can
//     no longer see it. Either way GitHub has given the name to another id, so
//     the row must stop holding it, and RepositoryStore.RetireSystem retires
//     it.
//   - Any other failure — no credential, a network error, a 5xx, a rate
//     limit — is returned as an error and changes nothing; the next
//     observation asks again. A retire is never inferred from an error.
//
// held is the row's identity as stored: host, slug and provider id. The caller
// has seen GitHub give held's name to another id, which Resolve does not
// re-check; the store re-checks under a lock that the row is still held's.
//
// The request goes through the credential for held's owner, the account the
// name lives under, which is the one that sees what is under it now. A row
// retired because that credential cannot see the repository any more is
// restored when one that can reports its id (RenameSystem).
func Resolve(ctx context.Context, repos db.RepositoryStore, resolver ghclient.Resolver, log *slog.Logger, orgID string, held domain.RepoRef) (Resolution, error) {
	if held.ExternalID == "" {
		return Kept, fmt.Errorf("resolve %s: the row carries no provider id to ask GitHub about", held.Slug())
	}
	if resolver == nil {
		return Kept, fmt.Errorf("resolve %s: no GitHub credential resolver", held.Slug())
	}
	client, err := resolver.ClientFor(ctx, orgID, held.Owner)
	if err != nil {
		return Kept, fmt.Errorf("resolve %s: GitHub client: %w", held.Slug(), err)
	}
	meta, err := client.GetRepoByID(ctx, held.ExternalID)
	switch {
	case ghclient.IsHTTP404(err):
		row, err := repos.RetireSystem(ctx, orgID, held)
		if err != nil {
			return Kept, fmt.Errorf("retire %s: %w", held.Slug(), err)
		}
		if row == nil {
			return Kept, nil // renamed or retired since the caller read it
		}
		log.InfoContext(ctx, "repository retired: GitHub serves its name under another id and no longer answers for its own",
			"org", orgID, "repo", row.Slug(), "external_id", row.ExternalID, "repository_id", row.ID)
		invalidateCoverage(resolver, orgID, held.Host, held.Slug())
		return Retired, nil
	case err != nil:
		return Kept, fmt.Errorf("resolve %s: read repository %s from GitHub: %w", held.Slug(), held.ExternalID, err)
	}
	if got := meta.ExternalID(); got != held.ExternalID {
		return Kept, fmt.Errorf("resolve %s: GitHub answered for repository %s with id %q", held.Slug(), held.ExternalID, got)
	}
	owner, repo, ok := strings.Cut(meta.FullName, "/")
	if !ok || owner == "" || repo == "" {
		return Kept, fmt.Errorf("resolve %s: GitHub named repository %s %q, which is not an owner/repo", held.Slug(), held.ExternalID, meta.FullName)
	}
	out, err := repos.RenameSystem(ctx, orgID, domain.RepoRef{
		Source: held.Source, Host: held.Host, Owner: owner, Repo: repo, ExternalID: held.ExternalID,
	})
	if err != nil {
		return Kept, fmt.Errorf("rename %s to %s: %w", held.Slug(), meta.FullName, err)
	}
	if !out.Renamed {
		return Kept, nil
	}
	log.InfoContext(ctx, "repository renamed", "org", orgID, "from", out.From, "to", out.To)
	invalidateCoverage(resolver, orgID, held.Host, out.From, out.To)
	return Renamed, nil
}

// ReleaseName frees observed's name from a row holding it under another
// provider id, for a caller that has just seen GitHub report observed's id
// under that name (a registration by id, read off the reachable mirror). The
// holder is resolved by its own id (Resolve), and the result is whether it let
// go of the name.
//
// A row holding the name with no provider id is left as it is: GitHub
// reporting an id under the name is what identifies that row, and registering
// the id or renaming onto the name takes it in rather than refusing over it.
func ReleaseName(ctx context.Context, repos db.RepositoryStore, resolver ghclient.Resolver, log *slog.Logger, orgID string, observed domain.RepoRef) (bool, error) {
	holder, err := repos.GetByRefSystem(ctx, orgID, observed)
	if err != nil {
		return false, fmt.Errorf("read the row holding %s: %w", observed.Slug(), err)
	}
	if holder == nil || holder.ExternalID == "" || holder.ExternalID == observed.ExternalID {
		return false, nil
	}
	res, err := Resolve(ctx, repos, resolver, log, orgID, holder.Ref())
	return res != Kept, err
}

// invalidateCoverage drops the cached App-grant coverage decision for each
// slug on host. The cache keys on the slug and holds positives only, so a
// rename leaves each entry vouching for the wrong repository: the old slug's
// for one that no longer answers to that name, the new slug's for whatever
// repository was called that before. A retire leaves the old slug's vouching
// for the repository that had it. Each is re-probed rather than inherited.
func invalidateCoverage(resolver ghclient.Resolver, orgID, host string, slugs ...string) {
	inv, ok := resolver.(ghclient.RepoCoverageInvalidator)
	if !ok {
		return
	}
	for _, slug := range slugs {
		owner, repo, found := cutSlug(slug)
		if !found {
			continue
		}
		inv.InvalidateRepoCoverage(orgID, host, owner, repo)
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
