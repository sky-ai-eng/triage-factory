package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runWorktreeStore is the Postgres impl of db.RunWorktreeStore. SQL
// is written fresh against D3's schema: $N placeholders, explicit
// org_id bind (column NOT NULL with no default), and org_id in every
// WHERE clause as defense in depth alongside the run_worktrees_all
// RLS policy (which gates rows on the parent run's visibility via
// EXISTS).
//
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). The workspace CLI subcommand
//     (cmd/exec/workspace) routes here. A separate cmd/exec auth pass
//     owns wrapping the CLI's store calls in synthetic-claims so the
//     EXISTS subquery in run_worktrees_all can resolve the parent
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
	res, err := q.ExecContext(ctx, `
		INSERT INTO run_worktrees (conversation_id, org_id, repo_id, path, ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (conversation_id, repo_id, ref) DO NOTHING
	`, w.RunID, orgID, w.RepoID, w.Path, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("insert run_worktree: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("rows affected: %w", err)
	}
	if rows == 1 {
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
		FROM run_worktrees
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
		FROM run_worktrees
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
		DELETE FROM run_worktrees
		WHERE org_id = $1 AND conversation_id = $2 AND repo_id = $3 AND ref = $4
	`, orgID, runID, repoID, ref)
	return err
}

func (s *runWorktreeStore) DeleteByPathSystem(ctx context.Context, orgID, runID, path string) error {
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM run_worktrees
		WHERE org_id = $1 AND conversation_id = $2 AND path = $3
	`, orgID, runID, path)
	return err
}
