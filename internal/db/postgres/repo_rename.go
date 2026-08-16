package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The slug rewrite a repository rename performs, split into the three groups
// the schema has. The split is the design: the middle group is what becomes a
// foreign key once repositories are referenced by row id, so that day is a
// deletion rather than a re-derivation of which statements were which.
//
//	renameRepositoryRow    — the repository's own row. The rename itself.
//	rewriteRepoSlugRefs    — columns holding the bare slug and nothing else.
//	rewriteSlugDerivedKeys — text keys with a slug embedded in them. They stay
//	                         text under any FK conversion, so this group lives on.
//
// What moves: a slug TF minted, naming something that lives inside the
// database. What stays: a value observed from the provider (entities.url,
// artifacts.url — GitHub redirects those), history (events.payload_json;
// external_actions.target and detail_json record an act under the name in
// force at the time), or a name for something outside the database that did
// not itself move (a worktree's path, as against its repo_id).
//
// Membership came from grepping the migrated schema for the shape rather than
// from a list, which is how placement_overrides got here — its (key_kind,
// key_value) pair holds "owner/repo" when key_kind is 'repo'. That column is
// polymorphic, so it can never become an FK and stays with the last group.
//
// TODO(TFAC-831): entities.url and external_actions.url keep the old slug, so
// a re-claimed name leaves them linking to a stranger's object.
// TODO(TFAC-830): the directory the old slug named is never reclaimed.
//
// Everything here runs on the ADMIN pool. The rewrite spans tables owned by
// every team in the org — tracked sets, projects, entities, artifacts — and no
// request-scoped identity can see all of them, so RLS cannot express the
// operation; org_id is bound by argument in every statement instead, and
// team_github_repos (which has no org_id of its own) is bound through teams.

func (s *repoStore) ListIdentitiesSystem(ctx context.Context, orgID string) ([]domain.RepoRef, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT source, owner, repo, external_id
		  FROM repositories
		 WHERE org_id = $1 AND external_id IS NOT NULL AND external_id <> ''
		 ORDER BY owner, repo
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.RepoRef{}
	for rows.Next() {
		var ref domain.RepoRef
		if err := rows.Scan(&ref.Source, &ref.Owner, &ref.Repo, &ref.ExternalID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *repoStore) RenameSystem(ctx context.Context, orgID string, observed domain.RepoRef) (domain.RepoRenameOutcome, error) {
	source, err := domain.NormalizeRepoSource(observed.Source)
	if err != nil {
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
		stored, err := storedSlugsForIdentity(ctx, tx, orgID, source, observed.ExternalID)
		if err != nil {
			return err
		}
		if len(stored) == 0 {
			return nil // no row carries this id — nothing to rename
		}
		for _, slug := range stored {
			if domain.SameRepoSlug(slug, observed.Slug()) {
				return nil // already current: the idempotent no-op
			}
		}
		if len(stored) > 1 {
			return fmt.Errorf("%w: %s", db.ErrRepoIdentityAmbiguous, observed.ExternalID)
		}
		from, to := stored[0], observed.Slug()

		occupied, err := slugHeldByAnotherRepository(ctx, tx, orgID, source, observed)
		if err != nil {
			return err
		}
		if occupied {
			return fmt.Errorf("%w: %s", db.ErrRepoSlugOccupied, to)
		}

		if err := renameRepositoryRow(ctx, tx, orgID, source, from, observed); err != nil {
			return err
		}
		if err := rewriteRepoSlugRefs(ctx, tx, orgID, from, to); err != nil {
			return err
		}
		if err := rewriteSlugDerivedKeys(ctx, tx, orgID, source, from, to); err != nil {
			return err
		}
		out = domain.RepoRenameOutcome{Renamed: true, From: from, To: to}
		return nil
	})
	if err != nil {
		return domain.RepoRenameOutcome{}, err
	}
	return out, nil
}

// storedSlugsForIdentity returns the slug of every repository row carrying one
// provider identity, locking each against a concurrent rename. More than one
// is a corrupt state rather than a shape the caller handles, so the plural
// return exists to let the caller SAY that instead of silently picking a row.
func storedSlugsForIdentity(ctx context.Context, q queryer, orgID, source, externalID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT owner, repo FROM repositories
		 WHERE org_id = $1 AND source = $2 AND external_id = $3
		 ORDER BY owner, repo
		 FOR UPDATE
	`, orgID, source, externalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var owner, repo string
		if err := rows.Scan(&owner, &repo); err != nil {
			return nil, err
		}
		out = append(out, owner+"/"+repo)
	}
	return out, rows.Err()
}

// slugHeldByAnotherRepository reports whether a repository row other than the
// one being renamed already spells the target slug.
func slugHeldByAnotherRepository(ctx context.Context, q queryer, orgID, source string, observed domain.RepoRef) (bool, error) {
	var found int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM repositories
		 WHERE org_id = $1 AND source = $2
		   AND lower(owner) = lower($3) AND lower(repo) = lower($4)
		   AND (external_id IS NULL OR external_id <> $5)
		 LIMIT 1
	`, orgID, source, observed.Owner, observed.Repo, observed.ExternalID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// renameRepositoryRow moves the repository's own row. Postgres keys the row on
// a synthetic uuid, so only the natural-key columns move.
func renameRepositoryRow(ctx context.Context, q queryer, orgID, source, from string, to domain.RepoRef) error {
	fromOwner, fromRepo := splitRepoSlug(from)
	if _, err := q.ExecContext(ctx, `
		UPDATE repositories
		   SET owner = $1, repo = $2, updated_at = now()
		 WHERE org_id = $3 AND source = $4
		   AND lower(owner) = lower($5) AND lower(repo) = lower($6)
	`, to.Owner, to.Repo, orgID, source, fromOwner, fromRepo); err != nil {
		return fmt.Errorf("rename repositories %s -> %s: %w", from, to.Slug(), err)
	}
	return nil
}

// rewriteRepoSlugRefs moves the columns that hold a bare "owner/repo" and
// nothing else. This is the group that disappears when repositories are
// referenced by row id.
//
// Each move absorbs a row already spelling the target: the caller has proved
// no repository row holds that slug, so a reference to it is an orphan, and
// the alternative to absorbing it is a primary-key collision that fails the
// whole rename over a row nothing points at.
func rewriteRepoSlugRefs(ctx context.Context, q queryer, orgID, from, to string) error {
	fromOwner, fromRepo := splitRepoSlug(from)
	toOwner, toRepo := splitRepoSlug(to)

	// team_github_repos — the tracked set, per team. No org_id column of its
	// own, so the org is bound through teams.
	if _, err := q.ExecContext(ctx, `
		DELETE FROM team_github_repos g
		 USING teams tm
		 WHERE tm.id = g.team_id AND tm.org_id = $1
		   AND lower(g.owner) = lower($2) AND lower(g.repo) = lower($3)
		   AND EXISTS (SELECT 1 FROM team_github_repos o
		                WHERE o.team_id = g.team_id
		                  AND lower(o.owner) = lower($4) AND lower(o.repo) = lower($5))
	`, orgID, toOwner, toRepo, fromOwner, fromRepo); err != nil {
		return fmt.Errorf("absorb tracked-repo duplicate for %s: %w", to, err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE team_github_repos g
		   SET owner = $1, repo = $2
		  FROM teams tm
		 WHERE tm.id = g.team_id AND tm.org_id = $3
		   AND lower(g.owner) = lower($4) AND lower(g.repo) = lower($5)
	`, toOwner, toRepo, orgID, fromOwner, fromRepo); err != nil {
		return fmt.Errorf("rewrite tracked repos %s -> %s: %w", from, to, err)
	}

	// conversation_worktrees — the per-conversation worktree ledger.
	if _, err := q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees w
		 WHERE w.org_id = $1 AND lower(w.repo_id) = lower($2)
		   AND EXISTS (SELECT 1 FROM conversation_worktrees o
		                WHERE o.conversation_id = w.conversation_id AND o.ref = w.ref
		                  AND lower(o.repo_id) = lower($3))
	`, orgID, to, from); err != nil {
		return fmt.Errorf("absorb worktree duplicate for %s: %w", to, err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE conversation_worktrees SET repo_id = $1
		 WHERE org_id = $2 AND lower(repo_id) = lower($3)
	`, to, orgID, from); err != nil {
		return fmt.Errorf("rewrite worktree ledger %s -> %s: %w", from, to, err)
	}

	// projects.pinned_repos — a JSON array of slugs, so the rewrite is done in
	// Go rather than in SQL: the element is a string in an ordered list, and
	// the two dialects store the column as different types.
	return rewritePinnedRepos(ctx, q, orgID, from, to)
}

func rewritePinnedRepos(ctx context.Context, q queryer, orgID, from, to string) error {
	rows, err := q.QueryContext(ctx, `SELECT id, pinned_repos FROM projects WHERE org_id = $1`, orgID)
	if err != nil {
		return err
	}
	type pending struct {
		id     string
		pinned []byte
	}
	var updates []pending
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var pinned []string
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &pinned); err != nil {
				// A project whose column is not a JSON array is already broken
				// in a way this rename did not cause and cannot fix. Skipping
				// it keeps the rename from failing on unrelated damage.
				continue
			}
		}
		rewritten, changed := domain.RewritePinnedRepos(pinned, from, to)
		if !changed {
			continue
		}
		encoded, err := json.Marshal(rewritten)
		if err != nil {
			rows.Close()
			return fmt.Errorf("marshal pinned_repos for project %s: %w", id, err)
		}
		updates = append(updates, pending{id: id, pinned: encoded})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, u := range updates {
		if _, err := q.ExecContext(ctx,
			`UPDATE projects SET pinned_repos = $1::jsonb, updated_at = now() WHERE id = $2 AND org_id = $3`,
			string(u.pinned), u.id, orgID,
		); err != nil {
			return fmt.Errorf("rewrite pinned repos for project %s: %w", u.id, err)
		}
	}
	return nil
}

// rewriteSlugDerivedKeys moves the text keys that embed a slug. Referencing a
// repository by row id cannot remove these: a PR is identified by its
// repository AND its number, so the composite stays a string whatever the
// repository column becomes.
func rewriteSlugDerivedKeys(ctx context.Context, q queryer, orgID, source, from, to string) error {
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
	if _, err := q.ExecContext(ctx, `
		UPDATE entities
		   SET source_id = $1 || substr(source_id, $2)
		 WHERE org_id = $3 AND source = $4
		   AND (lower(source_id) = lower($5)
		        OR lower(substr(source_id, 1, $6)) = lower($7))
	`,
		to, len(from)+1,
		orgID, source,
		from,
		len(from)+1, from+"#",
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: an entity already answers to %s: %v", db.ErrRepoSlugOccupied, to, err)
		}
		return fmt.Errorf("rewrite entity source ids %s -> %s: %w", from, to, err)
	}

	// placement_overrides — a repo key's value is the bare slug, in a column
	// that also holds project ids under a different key_kind.
	if _, err := q.ExecContext(ctx, `
		DELETE FROM placement_overrides p
		 WHERE p.org_id = $1 AND p.key_kind = 'repo' AND lower(p.key_value) = lower($2)
		   AND EXISTS (SELECT 1 FROM placement_overrides o
		                WHERE o.org_id = p.org_id AND o.key_kind = 'repo'
		                  AND lower(o.key_value) = lower($3))
	`, orgID, to, from); err != nil {
		return fmt.Errorf("absorb placement override duplicate for %s: %w", to, err)
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE placement_overrides SET key_value = $1, updated_at = now()
		 WHERE org_id = $2 AND key_kind = 'repo' AND lower(key_value) = lower($3)
	`, to, orgID, from); err != nil {
		return fmt.Errorf("rewrite placement overrides %s -> %s: %w", from, to, err)
	}

	// artifacts — target and dedup_key both carry the slug, and dedup_key is
	// the conflict target every capture writer upserts on. Leaving it behind
	// is what turns one pull request into two rows the first time anything
	// records it under the new name.
	return rewriteArtifactSlugs(ctx, q, orgID, from, to)
}

func rewriteArtifactSlugs(ctx context.Context, q queryer, orgID, from, to string) error {
	// The SQL filter is a cheap over-approximation — any row whose key or
	// target so much as mentions the old slug — and the precise decision is
	// made in Go, where the segment boundaries of a dedup key are expressible.
	rows, err := q.QueryContext(ctx, `
		SELECT id, target, dedup_key FROM artifacts
		 WHERE org_id = $1
		   AND (strpos(lower(dedup_key), lower($2)) > 0 OR strpos(lower(target), lower($2)) > 0)
	`, orgID, from)
	if err != nil {
		return err
	}
	type pending struct{ id, target, dedupKey string }
	var updates []pending
	for rows.Next() {
		var id, target, dedupKey string
		if err := rows.Scan(&id, &target, &dedupKey); err != nil {
			rows.Close()
			return err
		}
		newTarget, targetChanged := domain.RewriteRepoSlugPrefix(target, from, to)
		newKey, keyChanged := domain.RewriteArtifactDedupKey(dedupKey, from, to)
		if !targetChanged && !keyChanged {
			continue
		}
		updates = append(updates, pending{id: id, target: newTarget, dedupKey: newKey})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, u := range updates {
		if _, err := q.ExecContext(ctx, `
			UPDATE artifacts SET target = $1, dedup_key = $2, updated_at = now()
			 WHERE id = $3 AND org_id = $4
		`, u.target, u.dedupKey, u.id, orgID); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: an artifact already answers to %s: %v", db.ErrRepoSlugOccupied, to, err)
			}
			return fmt.Errorf("rewrite artifact %s for %s -> %s: %w", u.id, from, to, err)
		}
	}
	return nil
}

// isUniqueViolation reports whether err is Postgres' unique-constraint
// failure, matched on SQLSTATE rather than on the message the way the SQLite
// side must — pgx surfaces a typed error, so there is no reason to read
// strings here.
//
// It exists for the two composite keys a rename can collide on: entities'
// (org_id, source, source_id) and artifacts' (org_id, dedup_key). Both are
// reachable without any corruption, because untracking a repository keeps its
// entities and artifacts while dropping the repositories row that
// slugHeldByAnotherRepository looks at — so a freed name can be spoken for by
// records alone. Mapping the violation is what makes that a documented
// terminal state rather than a raw driver string the caller retries forever.
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
