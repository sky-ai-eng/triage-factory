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

// runWorktreeStore is the Postgres impl of db.RunWorktreeStore. SQL
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
type runWorktreeStore struct {
	q     queryer
	admin queryer
}

func newRunWorktreeStore(q, admin queryer) db.RunWorktreeStore {
	return &runWorktreeStore{q: q, admin: admin}
}

var _ db.RunWorktreeStore = (*runWorktreeStore)(nil)

func (s *runWorktreeStore) Insert(ctx context.Context, orgID string, w domain.RunWorktree) (bool, string, error) {
	return insertRunWorktree(ctx, s.q, orgID, w, s.GetByRepoRef)
}

func (s *runWorktreeStore) InsertSystem(ctx context.Context, orgID string, w domain.RunWorktree) (bool, string, error) {
	return insertRunWorktree(ctx, s.admin, orgID, w, s.GetByRepoRefSystem)
}

func insertRunWorktree(
	ctx context.Context,
	q queryer,
	orgID string,
	w domain.RunWorktree,
	lookup func(context.Context, string, string, string, string) (*domain.RunWorktree, error),
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
	var inserted, priorRows int
	err := q.QueryRowContext(ctx, `
		WITH prior AS (
			SELECT 1 FROM conversation_worktrees
			WHERE org_id = $2 AND conversation_id = $1 AND lower(repo_id) = lower($3)
			LIMIT 1
		), ins AS (
			INSERT INTO conversation_worktrees (conversation_id, org_id, repo_id, path, ref)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (conversation_id, repo_id, ref) DO NOTHING
			RETURNING 1
		)
		SELECT (SELECT count(*) FROM ins), (SELECT count(*) FROM prior)
	`, w.RunID, orgID, w.RepoID, w.Path, w.Ref).Scan(&inserted, &priorRows)
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
			_ = ctlbus.Publish(ctx, q, ctlbus.Message{Kind: "cred_request", OrgID: orgID, RunID: w.RunID})
		}
		return true, w.Path, nil
	}
	existing, err := lookup(ctx, orgID, w.RunID, w.RepoID, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("read existing run_worktree after conflict: %w", err)
	}
	if existing == nil {
		return false, "", fmt.Errorf("run_worktree row vanished after ON CONFLICT DO NOTHING (conversation_id=%s, repo_id=%s, ref=%s)", w.RunID, w.RepoID, w.Ref)
	}
	return false, existing.Path, nil
}

func (s *runWorktreeStore) GetByRepoRef(ctx context.Context, orgID, runID, repoID, ref string) (*domain.RunWorktree, error) {
	return getRunWorktreeByRepoRef(ctx, s.q, orgID, runID, repoID, ref)
}

func (s *runWorktreeStore) GetByRepoRefSystem(ctx context.Context, orgID, runID, repoID, ref string) (*domain.RunWorktree, error) {
	return getRunWorktreeByRepoRef(ctx, s.admin, orgID, runID, repoID, ref)
}

func getRunWorktreeByRepoRef(ctx context.Context, q queryer, orgID, runID, repoID, ref string) (*domain.RunWorktree, error) {
	row := q.QueryRowContext(ctx, `
		SELECT conversation_id, repo_id, path, ref, created_at
		FROM conversation_worktrees
		WHERE org_id = $1 AND conversation_id = $2 AND repo_id = $3 AND ref = $4
	`, orgID, runID, repoID, ref)
	var w domain.RunWorktree
	if err := row.Scan(&w.RunID, &w.RepoID, &w.Path, &w.Ref, &w.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (s *runWorktreeStore) List(ctx context.Context, orgID, runID string) ([]domain.RunWorktree, error) {
	return listRunWorktrees(ctx, s.q, orgID, runID)
}

func (s *runWorktreeStore) ListSystem(ctx context.Context, orgID, runID string) ([]domain.RunWorktree, error) {
	return listRunWorktrees(ctx, s.admin, orgID, runID)
}

func listRunWorktrees(ctx context.Context, q queryer, orgID, runID string) ([]domain.RunWorktree, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT conversation_id, repo_id, path, ref, created_at
		FROM conversation_worktrees
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY created_at ASC, repo_id ASC, ref ASC
	`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RunWorktree{}
	for rows.Next() {
		var w domain.RunWorktree
		if err := rows.Scan(&w.RunID, &w.RepoID, &w.Path, &w.Ref, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *runWorktreeStore) DeleteByRepoRef(ctx context.Context, orgID, runID, repoID, ref string) error {
	return deleteRunWorktreeByRepoRef(ctx, s.q, orgID, runID, repoID, ref)
}

func (s *runWorktreeStore) DeleteByRepoRefSystem(ctx context.Context, orgID, runID, repoID, ref string) error {
	return deleteRunWorktreeByRepoRef(ctx, s.admin, orgID, runID, repoID, ref)
}

func deleteRunWorktreeByRepoRef(ctx context.Context, q queryer, orgID, runID, repoID, ref string) error {
	_, err := q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees
		WHERE org_id = $1 AND conversation_id = $2 AND repo_id = $3 AND ref = $4
	`, orgID, runID, repoID, ref)
	return err
}

func (s *runWorktreeStore) DeleteByPathSystem(ctx context.Context, orgID, runID, path string) error {
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM conversation_worktrees
		WHERE org_id = $1 AND conversation_id = $2 AND path = $3
	`, orgID, runID, path)
	return err
}
