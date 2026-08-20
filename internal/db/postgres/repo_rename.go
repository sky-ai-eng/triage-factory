package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The slug rewrite a repository rename performs, now down to two groups:
//
//	renameRepositoryRow    — the repository's own row. The rename itself.
//	rewriteSlugDerivedKeys — text keys with a slug embedded in them.
//
// The middle group this file used to carry — columns holding a bare
// "owner/repo" and nothing else — is gone, and its deletion was the point of
// the split. The tracked set, the worktree ledger and a project's pinned repos
// reference the repository by the registry row's id now, so a rename moves the
// slug on one row and every one of them still resolves without being touched.
//
// What is left cannot become a foreign key. A pull request is identified by its
// repository AND its number, so entities.source_id stays the composite string
// "owner/repo#18"; an artifact's dedup_key is a contract between two writers
// that would re-mint every artifact if its shape moved; placement_overrides'
// key_value is polymorphic (a slug under one key_kind, a project id under
// another) and could only carry an FK by splitting the table.
//
// Stored links move with the keys. entities.url is rewritten alongside
// source_id — the redirect it used to lean on stops the moment the freed name
// is re-claimed, leaving the link pointing at a stranger's object — and the
// audit ledger gets the same fix through its pointer column: external_actions
// never has its url touched (the record of the act, frozen with target and
// detail_json), but current_url — the maintained pointer reads coalesce over —
// is filled here. artifacts.url alone self-heals without help: its upsert's
// conflict arm refreshes it the next time any writer lands on the row, which
// this rewrite guarantees by moving dedup_key.
//
// What stays put entirely: history (events.payload_json; external_actions'
// target and detail_json record an act under the name in force at the time),
// and a name for something outside the database that did not itself move (a
// worktree's path, as against the repository it holds). The directories the
// old slug named are disposed of AFTER this transaction commits — best-effort,
// guarded against live worktrees — by the rename applier (internal/reporename),
// never in here: a directory removal cannot join a transaction.
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

// renameRepositoryRow moves the repository's own row. The row is keyed on a
// surrogate uuid, so only the natural-key columns move — and because every
// reference elsewhere points at that id, this UPDATE is the entire rename as
// far as the tracked set, the worktree ledger and the pinned lists are
// concerned.
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

// rewriteSlugDerivedKeys moves the text keys that embed a slug — the group
// referencing a repository by row id cannot remove — and the stored links
// whose only job is to resolve to the object those keys name.
func rewriteSlugDerivedKeys(ctx context.Context, q queryer, orgID, source, from, to string) error {
	// entities.url first, while source_id still spells the old slug: the URL
	// pass selects its rows by the same positional source_id predicate the
	// UPDATE below is about to rewrite.
	if err := rewriteEntityURLs(ctx, q, orgID, source, from, to); err != nil {
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
	if err := rewriteArtifactSlugs(ctx, q, orgID, from, to); err != nil {
		return err
	}

	// external_actions — the audit ledger's pointer column, and only that.
	return rewriteExternalActionURLs(ctx, q, orgID, from, to)
}

// rewriteEntityURLs moves the slug inside the stored links of the renamed
// repository's own entities. The rows are selected by the same positional
// source_id predicate the source_id rewrite uses — an entity belongs to the
// repository, or its URL is not this rename's to touch, however many times it
// happens to mention the slug. The URL's own shape is decided in Go
// (domain.RewriteRepoURL): the slug sits mid-path after a host of unknown
// length, which SQL positional arithmetic cannot express, and a substring
// replace would move a host or branch segment that merely spells the name. A
// row whose URL does not lead with the old slug — empty because TF never
// learned one, or already pointing elsewhere — is left exactly as it stands.
func rewriteEntityURLs(ctx context.Context, q queryer, orgID, source, from, to string) error {
	rows, err := q.QueryContext(ctx, `
		SELECT id, url FROM entities
		 WHERE org_id = $1 AND source = $2 AND url IS NOT NULL AND url <> ''
		   AND (lower(source_id) = lower($3)
		        OR lower(substr(source_id, 1, $4)) = lower($5))
	`, orgID, source, from, len(from)+1, from+"#")
	if err != nil {
		return err
	}
	updates, err := collectURLRewrites(rows, from, to)
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
// stale link. No provider filter: the Go matcher is positional on the URL's
// leading path segments, which no other provider's link shape can spell.
func rewriteExternalActionURLs(ctx context.Context, q queryer, orgID, from, to string) error {
	rows, err := q.QueryContext(ctx, `
		SELECT id, COALESCE(current_url, url) FROM external_actions
		 WHERE org_id = $1 AND COALESCE(current_url, url) IS NOT NULL
		   AND strpos(lower(COALESCE(current_url, url)), lower($2)) > 0
	`, orgID, from)
	if err != nil {
		return err
	}
	updates, err := collectURLRewrites(rows, from, to)
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
// domain.RewriteRepoURL actually moves. The SQL side over-approximates (any
// mention of the slug, or the whole per-repo row set); the boundary decision
// is Go's.
func collectURLRewrites(rows *sql.Rows, from, to string) ([]urlRewrite, error) {
	defer rows.Close()
	var out []urlRewrite
	for rows.Next() {
		var id, u string
		if err := rows.Scan(&id, &u); err != nil {
			return nil, err
		}
		rewritten, changed := domain.RewriteRepoURL(u, from, to)
		if !changed {
			continue
		}
		out = append(out, urlRewrite{id: id, url: rewritten})
	}
	return out, rows.Err()
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
