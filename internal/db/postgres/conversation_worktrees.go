package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runWorktreeStore is the Postgres impl of db.ConversationWorktreeStore. SQL
// is written fresh against D3's schema: $N placeholders, explicit
// org_id bind (column NOT NULL with no default), and org_id in every
// WHERE clause as defense in depth alongside the conversation_worktrees_all
// RLS policy (which gates rows on the parent run's visibility via
// EXISTS).
//
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). The workspace CLI subcommand
//     (cmd/exec/workspace) routes here. A separate cmd/exec auth pass
//     owns wrapping the CLI's store calls in synthetic-claims so the
//     EXISTS subquery in conversation_worktrees_all can resolve the parent
//     run row.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The delegate
//     spawner's runAgent + chain orchestrator cleanup defers route
//     here. org_id stays bound as defense in depth.
//
// A ledger row points at a repository by the registry row's id. The store's
// surface stays the slug, because the caller is an agent's argv
// (`tfac exec workspace add owner/repo`), so every method resolves the slug
// here: a write get-or-creates the registry row, a read or a delete resolves it
// and matches nothing when there is none.
type runWorktreeStore struct {
	q     queryer
	admin queryer
}

func newConversationWorktreeStore(q, admin queryer) db.ConversationWorktreeStore {
	return &runWorktreeStore{q: q, admin: admin}
}

var _ db.ConversationWorktreeStore = (*runWorktreeStore)(nil)

func (s *runWorktreeStore) Insert(ctx context.Context, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	return insertRunWorktree(ctx, s.q, orgID, w, s.GetByRepoRef)
}

func (s *runWorktreeStore) InsertSystem(ctx context.Context, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	return insertRunWorktree(ctx, s.admin, orgID, w, s.GetByRepoRefSystem)
}

// resolveWorktreeRepositoryID maps the slug a caller passes to the registry
// row's id. A lookup, never a create: this runs on the executor, whose Postgres
// role holds SELECT and UPDATE on repositories and deliberately not INSERT — a
// repository is brought into the registry by the side that polls and tracks,
// never by a running agent's pod. A worktree is reserved for a repository the
// run was already authorized to clone, so the row exists; an absent one is a
// broken caller and says so rather than minting a repository nobody tracks.
func resolveWorktreeRepositoryID(ctx context.Context, q queryer, orgID, repoID string) (string, error) {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("run_worktree repo id %q is not an owner/repo slug", repoID)
	}
	id, err := findRepositoryID(ctx, q, orgID, domain.RepoSourceGitHub, owner, repo)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("no repository row for %s", repoID)
	}
	return id, nil
}

func insertRunWorktree(
	ctx context.Context,
	q queryer,
	orgID string,
	w domain.ConversationWorktree,
	lookup func(context.Context, string, string, string, string) (*domain.ConversationWorktree, error),
) (bool, string, error) {
	// The path the caller passes is the deterministic target
	// ({runRoot}/{owner}/{repo}/{ref-slug}) — the InRoot create funcs
	// always land there, so we can record the path BEFORE creating the
	// worktree on disk. That ordering matters: if create runs
	// before insert, both racing processes try `git worktree add`
	// against the same target dir and the second fails on "dir
	// already exists" before we ever reach the PK conflict that's
	// supposed to handle them. With insert first, the loser sees
	// inserted=false and returns the winner's path without
	// touching git.
	//
	// priorRows counts this run's EXISTING rows for the same repo, evaluated
	// against the statement's own snapshot so it never sees the row this
	// statement is inserting. It decides the credential doorbell below:
	// credentials are minted per repo, not per (repo, ref), so a second
	// checkout in a repo the run already holds widens nothing.
	repositoryID, err := resolveWorktreeRepositoryID(ctx, q, orgID, w.RepoID)
	if err != nil {
		return false, "", fmt.Errorf("resolve repository %s: %w", w.RepoID, err)
	}
	var inserted, priorRows int
	err = q.QueryRowContext(ctx, `
		WITH prior AS (
			SELECT 1 FROM conversation_worktrees
			WHERE org_id = $2 AND conversation_id = $1 AND repository_id = $3
			LIMIT 1
		), ins AS (
			INSERT INTO conversation_worktrees (conversation_id, org_id, repository_id, path, ref)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (conversation_id, repository_id, ref) DO NOTHING
			RETURNING 1
		)
		SELECT (SELECT count(*) FROM ins), (SELECT count(*) FROM prior)
	`, w.ConversationID, orgID, repositoryID, w.Path, w.Ref).Scan(&inserted, &priorRows)
	if err != nil {
		return false, "", fmt.Errorf("insert run_worktree: %w", err)
	}
	if inserted == 1 {
		if priorRows == 0 {
			// A repo the run did not hold before, so its authorized repo set
			// genuinely widened. The sealed credential bundle is a point-in-time
			// snapshot of the OLD set, so without this the new repo is
			// authorized-but-uncredentialed: the git proxy's Authorize relays live
			// and admits it, while the bundle carries no token to clone it with.
			// Ring the same doorbell a claim's own credential request rings
			// (MarkAwaitingCredentials) so the brain re-seals; the provisioner
			// recomputes the authorized set from scratch, so the next bundle
			// covers this repo by construction.
			//
			// Gated on priorRows because the provisioner dedups by repo id: a
			// second ref in a repo already in the set (a run reviewing two PRs in
			// one repo) needs no new token, and re-sealing for it would spend
			// GitHub App mint quota to produce a byte-identical grant. Two
			// concurrent adds of two refs of the SAME new repo each see priorRows=0
			// and both ring — an extra doorbell, which is the safe direction; a
			// missed one is the failure that matters.
			//
			// Published on q — the SAME connection that ran the INSERT, never a
			// separate pool handle. pg_notify inside a transaction is delivered
			// at COMMIT, and this insert runs inside a tx on the non-event path;
			// publishing elsewhere would fire BEFORE the commit and let the brain
			// seal a bundle from a pre-insert read, reintroducing the very bug
			// through its own fix.
			//
			// Lossy like every ctlbus message: a dropped notification just defers
			// the re-seal to the periodic refresh sweep, and the checkout op is
			// what actually waits for a bundle newer than this row.
			_ = ctlbus.Publish(ctx, q, ctlbus.Message{Kind: "cred_request", OrgID: orgID, ConversationID: w.ConversationID})
		}
		return true, w.Path, nil
	}
	existing, err := lookup(ctx, orgID, w.ConversationID, w.RepoID, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("read existing run_worktree after conflict: %w", err)
	}
	if existing == nil {
		return false, "", fmt.Errorf("run_worktree row vanished after ON CONFLICT DO NOTHING (conversation_id=%s, repo_id=%s, ref=%s)", w.ConversationID, w.RepoID, w.Ref)
	}
	return false, existing.Path, nil
}

func (s *runWorktreeStore) GetByRepoRef(ctx context.Context, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error) {
	return getConversationWorktreeByRepoRef(ctx, s.q, orgID, conversationID, repoID, ref)
}

func (s *runWorktreeStore) GetByRepoRefSystem(ctx context.Context, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error) {
	return getConversationWorktreeByRepoRef(ctx, s.admin, orgID, conversationID, repoID, ref)
}

func getConversationWorktreeByRepoRef(ctx context.Context, q queryer, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error) {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil, nil
	}
	row := q.QueryRowContext(ctx, `
		SELECT w.conversation_id, r.owner || '/' || r.repo, w.path, w.ref, w.created_at
		FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
		WHERE w.org_id = $1 AND w.conversation_id = $2
		  AND lower(r.owner) = lower($3) AND lower(r.repo) = lower($4) AND w.ref = $5
	`, orgID, conversationID, owner, repo, ref)
	var w domain.ConversationWorktree
	if err := row.Scan(&w.ConversationID, &w.RepoID, &w.Path, &w.Ref, &w.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (s *runWorktreeStore) List(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error) {
	return listConversationWorktrees(ctx, s.q, orgID, conversationID)
}

func (s *runWorktreeStore) ListSystem(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error) {
	return listConversationWorktrees(ctx, s.admin, orgID, conversationID)
}

func listConversationWorktrees(ctx context.Context, q queryer, orgID, conversationID string) ([]domain.ConversationWorktree, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT w.conversation_id, r.owner || '/' || r.repo, w.path, w.ref, w.created_at
		FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
		WHERE w.org_id = $1 AND w.conversation_id = $2
		ORDER BY w.created_at ASC, r.owner ASC, r.repo ASC, w.ref ASC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ConversationWorktree{}
	for rows.Next() {
		var w domain.ConversationWorktree
		if err := rows.Scan(&w.ConversationID, &w.RepoID, &w.Path, &w.Ref, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *runWorktreeStore) DeleteByRepoRef(ctx context.Context, orgID, conversationID, repoID, ref string) error {
	return deleteConversationWorktreeByRepoRef(ctx, s.q, orgID, conversationID, repoID, ref)
}

func (s *runWorktreeStore) DeleteByRepoRefSystem(ctx context.Context, orgID, conversationID, repoID, ref string) error {
	return deleteConversationWorktreeByRepoRef(ctx, s.admin, orgID, conversationID, repoID, ref)
}

func deleteConversationWorktreeByRepoRef(ctx context.Context, q queryer, orgID, conversationID, repoID, ref string) error {
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees w
		WHERE w.org_id = $1 AND w.conversation_id = $2 AND w.ref = $3
		  AND EXISTS (
		      SELECT 1 FROM repositories r
		       WHERE r.id = w.repository_id
		         AND lower(r.owner) = lower($4) AND lower(r.repo) = lower($5))
	`, orgID, conversationID, ref, owner, repo)
	return err
}

func (s *runWorktreeStore) DeleteByPathSystem(ctx context.Context, orgID, conversationID, path string) error {
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM conversation_worktrees
		WHERE org_id = $1 AND conversation_id = $2 AND path = $3
	`, orgID, conversationID, path)
	return err
}
