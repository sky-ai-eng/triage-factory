package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The slug rewrite a repository rename performs, in two groups:
//
//	renameRepositoryRow    — the repository's own row. The rename itself.
//	rewriteSlugDerivedKeys — text keys with a slug embedded in them.
//
// Before either, absorbBareRepository folds in a bare registry row an entity
// create minted under the target name: the one occupant of that name a rename
// can prove is the same repository.
//
// The tracked set, the worktree ledger, GitHub entities (repository_id) and
// placement overrides reference the repository by the registry row's id, so a
// rename moves the slug on one row and every one of them still resolves
// without being touched.
//
// What is left cannot become a foreign key. A pull request's key is its
// repository's name AND its number, so entities.source_id stays the composite
// display key "owner/repo#18" beside the entity's repository_id, and moves
// because it is what everything reads and shows; an artifact's target is a
// display value of the same shape. An artifact's dedup_key names no repository
// (a pull request's node id, a registry row id, a comment's id), so it never
// moves.
//
// Stored links move with the keys. entities.url is rewritten alongside
// source_id — a redirect stops the moment the freed name is re-claimed,
// leaving the link pointing at a stranger's object — and the audit ledger gets
// the same fix through its pointer column: external_actions never has its url
// touched (the record of the act, frozen with target and detail_json), but
// current_url — the maintained pointer reads coalesce over — is filled here,
// on the repository's own rows: those about its entities (entity_id) and those
// about it with no entity (repository_id).
// artifacts.url needs no help: its upsert's conflict arm refreshes it the next
// time any writer lands on the row, and every writer lands on it by its id.
//
// What stays put entirely: history (events.payload_json; external_actions'
// target and detail_json record an act under the name in force at the time),
// and a name for something outside the database that did not itself move (a
// worktree's path, as against the repository it holds). The bare clone cache
// is keyed by the repository row id, so a rename does not move it either.
//
// The rewrite stays on the renamed repository's host. Entities are the
// repository's own (repository_id), and so are the artifacts: those about its
// entities by artifacts.entity_id, its branches by the registry row id their
// keys carry, and so are the audit-ledger rows, by their entity_id or
// repository_id. A stored link is rewritten only when it is on the host
// (domain.RewriteRepoURL). A repository on another host under the same name
// is a different repository, and nothing of its is touched; nor is a retired
// repository that held the name, whose rows reference its own registry row and
// entities.
//
// A retired row restored by a rename moves its own row, entities, artifacts
// and audit-ledger pointers the same way, all selected by id.
//
// Everything here runs on the ADMIN pool. The rewrite spans tables owned by
// every team in the org — tracked sets, entities, artifacts — and no
// request-scoped identity can see all of them, so RLS cannot express the
// operation; org_id is bound by argument in every statement instead.

func (s *repoStore) ListIdentitiesSystem(ctx context.Context, orgID, host string) ([]domain.RepoIdentity, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT source, host, owner, repo, external_id, owner_external_id, retired_at IS NOT NULL
		  FROM repositories
		 WHERE org_id = $1 AND host = $2 AND external_id IS NOT NULL AND external_id <> ''
		 ORDER BY owner, repo, id
	`, orgID, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoIdentity{}
	for rows.Next() {
		var id domain.RepoIdentity
		var ownerExternalID sql.NullString
		if err := rows.Scan(&id.Source, &id.Host, &id.Owner, &id.Repo, &id.ExternalID, &ownerExternalID, &id.Retired); err != nil {
			return nil, err
		}
		id.OwnerExternalID = ownerExternalID.String
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *repoStore) RetireSystem(ctx context.Context, orgID string, held domain.RepoRef) (*domain.Repository, error) {
	source, err := domain.NormalizeRepoSource(held.Source)
	if err != nil {
		return nil, err
	}
	if err := db.RequireRepoHost(held.Host); err != nil {
		return nil, err
	}
	if held.ExternalID == "" {
		return nil, errors.New("retire repository: a provider id is required")
	}
	var out *domain.Repository
	err = inTx(ctx, s.admin, func(tx queryer) error {
		// The UPDATE's row lock serializes it against a concurrent rename or
		// retire of the same row, and READ COMMITTED re-checks the WHERE
		// against what that writer committed: a row it renamed or retired
		// first no longer matches, and this is the no-op.
		now := time.Now().UTC()
		p, err := pgScanRepositoryFull(tx.QueryRowContext(ctx, `
			UPDATE repositories
			   SET retired_at = $1, updated_at = now()
			 WHERE org_id = $2 AND source = $3 AND host = $4 AND external_id = $5
			   AND lower(owner) = lower($6) AND lower(repo) = lower($7)
			   AND retired_at IS NULL
			RETURNING `+repoProfileFullColumns,
			now, orgID, source, held.Host, held.ExternalID, held.Owner, held.Repo))
		if errors.Is(err, sql.ErrNoRows) {
			return nil // renamed or retired since the caller read it
		}
		if err != nil {
			return fmt.Errorf("retire repositories[%s]: %w", held.Slug(), err)
		}
		// poll_seq moves with the state so a poll cycle that read one of these
		// entities before the retire loses its snapshot write rather than
		// landing a snapshot of the repository that now has the name.
		if _, err := tx.ExecContext(ctx, `
			UPDATE entities
			   SET state = 'closed', closed_at = $1, poll_seq = poll_seq + 1
			 WHERE org_id = $2 AND repository_id = $3 AND state = 'active'
		`, now, orgID, p.ID); err != nil {
			return fmt.Errorf("close entities of retired %s: %w", held.Slug(), err)
		}
		out = &p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *repoStore) RenameSystem(ctx context.Context, orgID string, observed domain.RepoRef) (domain.RepoRenameOutcome, error) {
	source, err := domain.NormalizeRepoSource(observed.Source)
	if err != nil {
		return domain.RepoRenameOutcome{}, err
	}
	if err := db.RequireRepoHost(observed.Host); err != nil {
		return domain.RepoRenameOutcome{}, err
	}
	// No id, no rename. Stated first so the rest of this function may assume
	// it is comparing two identities rather than two names.
	if observed.ExternalID == "" {
		return domain.RepoRenameOutcome{}, nil
	}
	if observed.Owner == "" || observed.Repo == "" {
		return domain.RepoRenameOutcome{}, fmt.Errorf("rename target %q is not an owner/repo slug", observed.Slug())
	}

	var out domain.RepoRenameOutcome
	err = inTx(ctx, s.admin, func(tx queryer) error {
		// The lock, and the whole loser contract. Two detections of the same
		// rename serialize here; the second one's SELECT re-evaluates against
		// the row the winner committed (READ COMMITTED re-reads a row it
		// blocked on), sees the slug already current, and returns a no-op.
		stored, err := storedRowsForIdentity(ctx, tx, orgID, source, observed.Host, observed.ExternalID)
		if err != nil {
			return err
		}
		if len(stored) == 0 {
			return nil // no row carries this id — nothing to rename
		}
		for _, row := range stored {
			if !row.retired && domain.SameRepoSlug(row.slug, observed.Slug()) {
				return nil // already current: the idempotent no-op
			}
		}
		if len(stored) > 1 {
			return fmt.Errorf("%w: %s", db.ErrRepoIdentityAmbiguous, observed.ExternalID)
		}
		renamed := stored[0]
		from, to := renamed.slug, observed.Slug()
		moved := !domain.SameRepoSlug(from, to)

		occupant, err := repositoryHoldingSlug(ctx, tx, orgID, source, observed, renamed.id)
		if err != nil {
			return err
		}
		if occupant != nil {
			if err := absorbBareRepository(ctx, tx, orgID, *occupant, renamed.id, to); err != nil {
				return err
			}
		}

		if err := renameRepositoryRow(ctx, tx, orgID, renamed, observed, moved); err != nil {
			return err
		}
		if moved {
			if err := rewriteSlugDerivedKeys(ctx, tx, orgID, source, observed.Host, renamed.id, from, to); err != nil {
				return err
			}
		}
		out = domain.RepoRenameOutcome{Renamed: moved, Restored: renamed.retired, From: from, To: to}
		return nil
	})
	if err != nil {
		return domain.RepoRenameOutcome{}, err
	}
	return out, nil
}

// identityRow is one repository row carrying the provider identity a rename
// keys on: its registry id, the slug it is stored under, its clone URL ("" for
// none), which spells the slug too, the owner account id it stores ("" for
// none), and whether it is retired.
type identityRow struct {
	id, slug, cloneURL, ownerExternalID string
	retired                             bool
}

// storedRowsForIdentity returns every repository row carrying one provider
// identity on host, retired or not, locking each against a concurrent rename
// or retire. The identity index allows one, so more than one is a corrupt
// state rather than a shape the caller handles, and the plural return exists
// to let the caller SAY that instead of silently picking a row.
func storedRowsForIdentity(ctx context.Context, q queryer, orgID, source, host, externalID string) ([]identityRow, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, owner, repo, COALESCE(clone_url, ''), COALESCE(owner_external_id, ''), retired_at IS NOT NULL
		  FROM repositories
		 WHERE org_id = $1 AND source = $2 AND host = $3 AND external_id = $4
		 ORDER BY owner, repo
		 FOR UPDATE
	`, orgID, source, host, externalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identityRow
	for rows.Next() {
		var r identityRow
		var owner, repo string
		if err := rows.Scan(&r.id, &owner, &repo, &r.cloneURL, &r.ownerExternalID, &r.retired); err != nil {
			return nil, err
		}
		r.slug = owner + "/" + repo
		out = append(out, r)
	}
	return out, rows.Err()
}

// slugOccupant is a repository row, other than the one being renamed, that
// holds the target slug on the host: a retired row under it holds nothing. The
// folded identity index allows at most one.
type slugOccupant struct {
	id string
	// identified is whether the row carries a provider id.
	identified bool
}

// repositoryHoldingSlug returns the row occupying the target slug, or nil,
// locked FOR UPDATE because absorbBareRepository decides on what references
// the row. A concurrent insert of a tracking, ledger or entity row naming it
// takes a key share lock on it for its foreign key check, which conflicts with
// this one: an insert that got there first commits before this lock is
// granted and is seen by the reads after it, and one that comes later waits
// and then fails its foreign key against the deleted row.
func repositoryHoldingSlug(ctx context.Context, q queryer, orgID, source string, observed domain.RepoRef, renamedID string) (*slugOccupant, error) {
	var occ slugOccupant
	err := q.QueryRowContext(ctx, `
		SELECT id, COALESCE(external_id, '') <> '' FROM repositories
		 WHERE org_id = $1 AND source = $2 AND host = $3
		   AND lower(owner) = lower($4) AND lower(repo) = lower($5)
		   AND id <> $6 AND retired_at IS NULL
		 FOR UPDATE
	`, orgID, source, observed.Host, observed.Owner, observed.Repo, renamedID).Scan(&occ.id, &occ.identified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &occ, nil
}

// absorbBareRepository folds the row occupying the target slug into the
// renamed row, or refuses with ErrRepoSlugOccupied when it cannot.
//
// The occupant it absorbs is a bare row: no provider id and tracked by no
// team. A GitHub entity create mints exactly that when it writes under the new
// name before TF observes the rename: an agent touch on a pull request GitHub
// serves through the redirect, or a pull request read off GraphQL, which keys
// it by GitHub's current name. GitHub has just reported that the name belongs
// to the renamed row's id, so the bare row is the same repository under a
// second row. Nothing in the product lists or deletes an untracked registry
// row, so refusing over it would stall the rename for good.
//
// Every other occupant refuses:
//
//   - one with a provider id is a different repository, one GitHub no longer
//     serves under this name;
//   - one a team tracks carries that team's decision, which the delete would
//     cascade away (team_github_repos is ON DELETE CASCADE), so tracking is
//     checked here rather than left to the foreign key;
//   - one a worktree ledger row names is refused by that foreign key, which
//     has no ON DELETE action, and the violation maps to ErrRepoSlugOccupied
//     rather than surfacing as a raw constraint error.
//
// The occupant's entities move to the renamed row before the delete. Their
// keys already spell the target slug, so the source_id rewrite after this
// leaves them as they are, and a pull request with an entity under each name
// still collides there and refuses the whole rename, this absorb included.
// The branches recorded in it move too (moveAbsorbedBranches): their keys
// carry the occupant's id, which the delete frees. So do the audit-ledger rows
// about it (repository_id), which the delete would otherwise unlink, and its
// placement override, which an operator may have set on the name while the
// bare row held it, unless the renamed row has one of its own: that one wins,
// and the delete cascades the occupant's away.
//
// A bare clone cached under the occupant's id (<StateRoot>/repos/<id>.git) is
// left behind. The cache belongs to whichever process ran a checkout, often
// an executor pod rather than the one applying the rename, and the ledger
// foreign key has already established that no run still names the id. The
// reaper's TTL pass reclaims it in multi.
//
// TODO(TFAC-1152): local never reclaims it; its reaper evicts nothing.
func absorbBareRepository(ctx context.Context, q queryer, orgID string, occupant slugOccupant, renamedID, to string) error {
	if occupant.identified {
		return fmt.Errorf("%w: %s", db.ErrRepoSlugOccupied, to)
	}
	var tracked bool
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM team_github_repos WHERE org_id = $1 AND repository_id = $2)
	`, orgID, occupant.id).Scan(&tracked); err != nil {
		return fmt.Errorf("read tracking of %s: %w", to, err)
	}
	if tracked {
		return fmt.Errorf("%w: %s is tracked by a team", db.ErrRepoSlugOccupied, to)
	}

	if _, err := q.ExecContext(ctx, `
		UPDATE entities SET repository_id = $1 WHERE org_id = $2 AND repository_id = $3
	`, renamedID, orgID, occupant.id); err != nil {
		return fmt.Errorf("re-point entities of the bare %s row: %w", to, err)
	}
	if err := moveAbsorbedBranches(ctx, q, orgID, occupant.id, renamedID); err != nil {
		return fmt.Errorf("re-key branches of the bare %s row: %w", to, err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE external_actions SET repository_id = $1 WHERE org_id = $2 AND repository_id = $3
	`, renamedID, orgID, occupant.id); err != nil {
		return fmt.Errorf("re-point audit-ledger rows of the bare %s row: %w", to, err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE placement_overrides SET repository_id = $1, updated_at = now()
		 WHERE org_id = $2 AND repository_id = $3
		   AND NOT EXISTS (SELECT 1 FROM placement_overrides WHERE org_id = $2 AND repository_id = $1)
	`, renamedID, orgID, occupant.id); err != nil {
		return fmt.Errorf("move the placement override of the bare %s row: %w", to, err)
	}
	if _, err := q.ExecContext(ctx, `
		DELETE FROM repositories WHERE id = $1 AND org_id = $2
	`, occupant.id, orgID); err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: a worktree still references the bare %s row: %v", db.ErrRepoSlugOccupied, to, err)
		}
		return fmt.Errorf("delete the bare %s row: %w", to, err)
	}
	return nil
}

// moveAbsorbedBranches re-keys the branch artifacts recorded in the registry
// row fromID onto toID, the row it is absorbed into. A branch is keyed on its
// repository's row id and its ref (domain.BranchDedupKey), so a key naming
// fromID would name a deleted row: no rename would find it again, and the next
// push of the same ref, keyed on toID, would record the branch a second time.
// A ref both rows recorded is one branch under two keys, and the older
// artifact (created_at, then id) keeps it while the newer is deleted. No table
// references an artifact row.
func moveAbsorbedBranches(ctx context.Context, q queryer, orgID, fromID, toID string) error {
	from := domain.BranchDedupKeyPrefix(fromID)
	rows, err := q.QueryContext(ctx, `
		SELECT id, scope, dedup_key, created_at FROM artifacts
		 WHERE org_id = $1 AND provider = $2 AND kind = $3
		   AND left(dedup_key, length($4::text)) = $4::text
		 ORDER BY id
	`, orgID, domain.ArtifactProviderGit, domain.ArtifactKindBranch, from)
	if err != nil {
		return err
	}
	type branch struct {
		id, scope, key string
		created        time.Time
	}
	var found []branch
	for rows.Next() {
		var b branch
		if err := rows.Scan(&b.id, &b.scope, &b.key, &b.created); err != nil {
			rows.Close()
			return err
		}
		found = append(found, b)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range found {
		key := domain.BranchDedupKey(toID, strings.TrimPrefix(b.key, from))
		if key == "" {
			continue
		}
		var heldID string
		var heldCreated time.Time
		err := q.QueryRowContext(ctx, `
			SELECT id, created_at FROM artifacts WHERE org_id = $1 AND scope = $2 AND dedup_key = $3
		`, orgID, b.scope, key).Scan(&heldID, &heldCreated)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case heldCreated.Before(b.created) || (heldCreated.Equal(b.created) && heldID < b.id):
			if _, err := q.ExecContext(ctx, `DELETE FROM artifacts WHERE org_id = $1 AND id = $2`, orgID, b.id); err != nil {
				return fmt.Errorf("delete branch artifact %s, recorded again under %s: %w", b.id, key, err)
			}
			continue
		default:
			if _, err := q.ExecContext(ctx, `DELETE FROM artifacts WHERE org_id = $1 AND id = $2`, orgID, heldID); err != nil {
				return fmt.Errorf("delete branch artifact %s, recorded first under %s: %w", heldID, b.key, err)
			}
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE artifacts SET dedup_key = $1, updated_at = now() WHERE org_id = $2 AND id = $3
		`, key, orgID, b.id); err != nil {
			return fmt.Errorf("re-key branch artifact %s onto %s: %w", b.id, key, err)
		}
	}
	return nil
}

// renameRepositoryRow moves the repository's own row, by its id: a retired row
// may share the slug it is moving from. The row is keyed on a surrogate uuid,
// so only the natural-key columns move — and because every reference elsewhere
// points at that id, this UPDATE is the entire rename as far as the tracked
// set, the worktree ledger and the pinned lists are concerned. It also clears
// retired_at, which restores a retired row; moved=false is that restore alone,
// and leaves the stored casing and clone URL as they are.
//
// The clone URL moves with the name. It spells the slug, and everything that
// clones or restores a checkout clones from it, so left alone it would put the
// old name back into every checkout made until profiling next rewrites it.
// One it cannot read as the old slug's clone URL is left as it stands.
//
// The owner account id is written as domain.RenamedOwnerExternalID decides:
// the one the observation carries, else the stored one while the owner login
// is unchanged, else none, because a transfer moves the repository to another
// account.
//
// The identity index refusing means a row took the slug after
// repositoryHoldingSlug looked, which is the occupant refusal. The violation
// aborts the transaction, which is what the refusal does anyway.
func renameRepositoryRow(ctx context.Context, q queryer, orgID string, renamed identityRow, to domain.RepoRef, moved bool) error {
	ownerID := domain.RenamedOwnerExternalID(renamed.slug, renamed.ownerExternalID, to)
	query, args := `
		UPDATE repositories
		   SET owner_external_id = NULLIF($3, ''), retired_at = NULL, updated_at = now()
		 WHERE org_id = $1 AND id = $2`, []any{orgID, renamed.id, ownerID}
	if moved {
		var cloneURL sql.NullString
		if u, ok := domain.RewriteRepoCloneURL(renamed.cloneURL, to.Host, renamed.slug, to.Slug()); ok {
			cloneURL = sql.NullString{String: u, Valid: true}
		}
		query, args = `
		UPDATE repositories
		   SET owner = $3, repo = $4, clone_url = COALESCE($5, clone_url), owner_external_id = NULLIF($6, ''),
		       retired_at = NULL, updated_at = now()
		 WHERE org_id = $1 AND id = $2`, []any{orgID, renamed.id, to.Owner, to.Repo, cloneURL, ownerID}
	}
	if _, err := q.ExecContext(ctx, query, args...); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s: %v", db.ErrRepoSlugOccupied, to.Slug(), err)
		}
		return fmt.Errorf("rename repositories[%s] -> %s: %w", renamed.id, to.Slug(), err)
	}
	return nil
}

// rewriteSlugDerivedKeys moves the text keys that embed a slug — the group
// referencing a repository by row id cannot remove — and the stored links
// whose only job is to resolve to the object those keys name, on host only:
// every row it selects carries the host as its scope.
//
// The entities, the artifacts and the audit-ledger rows are repositoryID's
// own, selected by id.
func rewriteSlugDerivedKeys(ctx context.Context, q queryer, orgID, source, host, repositoryID, from, to string) error {
	// entities.url first, while source_id still spells the old slug: the URL
	// pass selects its rows by the same positional source_id predicate the
	// UPDATE below is about to rewrite.
	if err := rewriteEntityURLs(ctx, q, orgID, source, host, repositoryID, from, to); err != nil {
		return err
	}

	// entities.source_id — "owner/repo#18". Matched positionally rather than
	// with LIKE: a repository name may contain '_', which LIKE reads as a
	// wildcard, so "acme/a_i" would match "acme/api#3" and rename an entity
	// belonging to a different repository. The offsets are Go byte lengths
	// against a character-indexed substr, which agree because GitHub restricts
	// a slug to ASCII.
	//
	// entities.source and repositories.source are different vocabularies that
	// happen to coincide while GitHub is the only provider issuing either. The
	// repository's source is threaded through rather than hardcoded so a second
	// provider's entities move with its repositories — but whoever adds one
	// must check that the two vocabularies still agree.
	//
	// repository_id keeps the rewrite to this row's entities: a retired row
	// can share the slug, and its pull requests are not this repository's.
	if _, err := q.ExecContext(ctx, `
		UPDATE entities
		   SET source_id = $1 || substr(source_id, $2)
		 WHERE org_id = $3 AND source = $4 AND scope = $8 AND repository_id = $9
		   AND (lower(source_id) = lower($5)
		        OR lower(substr(source_id, 1, $6)) = lower($7))
	`,
		to, len(from)+1,
		orgID, source,
		from,
		len(from)+1, from+"#",
		host, repositoryID,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: an entity already answers to %s: %v", db.ErrRepoSlugOccupied, to, err)
		}
		return fmt.Errorf("rewrite entity source ids %s -> %s: %w", from, to, err)
	}
	if err := moveRepositoryArtifactTargets(ctx, q, orgID, source, host, repositoryID, to); err != nil {
		return err
	}
	// external_actions — the audit ledger's pointer column, and only that.
	return rewriteExternalActionURLs(ctx, q, orgID, host, repositoryID, from, to)
}

// rewriteEntityURLs moves the slug inside the stored links of the renamed
// repository's own entities. The rows are selected by the same predicate the
// source_id rewrite uses, the repository's id and the positional source_id
// match — an entity belongs to the repository, or its URL is not this rename's
// to touch, however many times it happens to mention the slug. The URL's own shape is decided in Go
// (domain.RewriteRepoURL): the slug sits mid-path after a host of unknown
// length, which SQL positional arithmetic cannot express, and a substring
// replace would move a host or branch segment that merely spells the name. A
// row whose URL is not on the host or does not continue with the old slug —
// empty because TF never learned one, or already pointing elsewhere — is left
// exactly as it stands.
func rewriteEntityURLs(ctx context.Context, q queryer, orgID, source, host, repositoryID, from, to string) error {
	rows, err := q.QueryContext(ctx, `
		SELECT id, url FROM entities
		 WHERE org_id = $1 AND source = $2 AND scope = $6 AND repository_id = $7
		   AND url IS NOT NULL AND url <> ''
		   AND (lower(source_id) = lower($3)
		        OR lower(substr(source_id, 1, $4)) = lower($5))
	`, orgID, source, from, len(from)+1, from+"#", host, repositoryID)
	if err != nil {
		return err
	}
	updates, err := collectURLRewrites(rows, host, from, to)
	if err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := q.ExecContext(ctx, `
			UPDATE entities SET url = $1 WHERE id = $2 AND org_id = $3
		`, u.url, u.id, orgID); err != nil {
			return fmt.Errorf("rewrite entity url %s for %s -> %s: %w", u.id, from, to, err)
		}
	}
	return nil
}

// rewriteExternalActionURLs maintains the audit ledger's pointer column. The
// record of the act — target, action, detail_json, credential, occurred_at,
// and the url column itself — is never modified; the rename writes ONLY
// current_url, which reads coalesce over url. The rewrite base is the
// pointer's current value (current_url once a prior rename has already moved
// it, else url), so consecutive renames chain instead of re-deriving from a
// stale link. The candidates are repositoryID's own rows, whatever name their
// target or link spells: those about one of its entities (entity_id) and those
// about the repository with no entity (repository_id). The Go matcher then
// reads the slug right after the host's path.
func rewriteExternalActionURLs(ctx context.Context, q queryer, orgID, host, repositoryID, from, to string) error {
	rows, err := q.QueryContext(ctx, `
		SELECT id, COALESCE(current_url, url) FROM external_actions
		 WHERE org_id = $1 AND COALESCE(current_url, url) IS NOT NULL
		   AND (repository_id = $2
		        OR entity_id IN (SELECT id FROM entities WHERE org_id = $1 AND repository_id = $2))
		 ORDER BY id
	`, orgID, repositoryID)
	if err != nil {
		return err
	}
	updates, err := collectURLRewrites(rows, host, from, to)
	if err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := q.ExecContext(ctx, `
			UPDATE external_actions SET current_url = $1 WHERE id = $2 AND org_id = $3
		`, u.url, u.id, orgID); err != nil {
			return fmt.Errorf("rewrite external action pointer %s for %s -> %s: %w", u.id, from, to, err)
		}
	}
	return nil
}

// urlRewrite is one (row id, rewritten link) pair a URL pass decided on.
type urlRewrite struct{ id, url string }

// collectURLRewrites drains rows of (id, url) pairs and returns the ones
// domain.RewriteRepoURL actually moves on host. The SQL side selects the
// repository's own rows; whether a link continues with the slug on host is
// Go's decision.
func collectURLRewrites(rows *sql.Rows, host, from, to string) ([]urlRewrite, error) {
	defer rows.Close()
	var out []urlRewrite
	for rows.Next() {
		var id, u string
		if err := rows.Scan(&id, &u); err != nil {
			return nil, err
		}
		rewritten, changed := domain.RewriteRepoURL(u, host, from, to)
		if !changed {
			continue
		}
		out = append(out, urlRewrite{id: id, url: rewritten})
	}
	return out, rows.Err()
}

// moveRepositoryArtifactTargets moves the targets of repositoryID's own
// artifacts. Every artifact about one of its entities takes that entity's key,
// which the source_id rewrite has just moved (artifacts.entity_id), and every
// branch recorded in it takes to: a branch has no entity, and its key carries
// the registry row id (domain.BranchDedupKeyPrefix). The rows are selected by
// id, never by the name a target spells, so a retired repository that held
// the name keeps its rows as they are, and a row written under a spelling of
// the name this rename does not know moves too. Only the target moves; the
// key names no repository.
func moveRepositoryArtifactTargets(ctx context.Context, q queryer, orgID, source, host, repositoryID, to string) error {
	if _, err := q.ExecContext(ctx, `
		UPDATE artifacts SET target = $1, updated_at = now()
		 WHERE org_id = $2 AND scope = $3 AND provider = $4 AND kind = $5
		   AND left(dedup_key, length($6::text)) = $6::text
		   AND target <> $1
	`, to, orgID, host, domain.ArtifactProviderGit, domain.ArtifactKindBranch,
		domain.BranchDedupKeyPrefix(repositoryID)); err != nil {
		return fmt.Errorf("move branch artifact targets of %s onto %s: %w", repositoryID, to, err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE artifacts a SET target = e.source_id, updated_at = now()
		  FROM entities e
		 WHERE e.org_id = $1 AND e.source = $2 AND e.scope = $3 AND e.repository_id = $4
		   AND a.org_id = $1 AND a.entity_id = e.id AND a.target <> e.source_id
	`, orgID, source, host, repositoryID); err != nil {
		return fmt.Errorf("move the targets of %s's entities' artifacts onto %s: %w", repositoryID, to, err)
	}
	return nil
}

// isUniqueViolation reports whether err is Postgres' unique-constraint
// failure, matched on SQLSTATE rather than on the message the way the SQLite
// side must — pgx surfaces a typed error, so there is no reason to read
// strings here.
//
// It exists for the composite key a rename can collide on: entities' active
// key (org_id, source, scope, source_id). The entity rename and a stamp of an
// external id on an entity reach it too. The key is reachable without any
// corruption: an entity spells the name itself, whatever registry row
// repositoryHoldingSlug finds, so a name can be spoken for by entities alone.
// Mapping the violation is what makes that a documented terminal state rather
// than a raw driver string the caller retries forever.
//
// Mapped at the write rather than pre-checked on purpose: a pre-check inside
// this transaction still races a concurrent entity insert, so the index has to
// be the authority either way, and a second query on every rename buys nothing.
func isUniqueViolation(err error) bool {
	// 23505 is SQLSTATE unique_violation. Spelled as the literal rather than
	// pulled from a constants module — one code does not earn a dependency, and
	// the codebase already names SQLSTATEs this way (42501, 23514, 22P02).
	const uniqueViolation = "23505"
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// isForeignKeyViolation reports whether err is Postgres' foreign-key failure
// (SQLSTATE 23503): a delete absorbBareRepository issues against a row that
// something without an ON DELETE action still references.
func isForeignKeyViolation(err error) bool {
	const foreignKeyViolation = "23503"
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation
}
