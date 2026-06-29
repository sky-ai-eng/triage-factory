package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runWorktreeStore is the SQLite impl of db.RunWorktreeStore. The
// constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so the second arg is
// discarded. ...System variants are thin wrappers around their
// non-System counterparts.
type runWorktreeStore struct{ q queryer }

func newRunWorktreeStore(q, _ queryer) db.RunWorktreeStore {
	return &runWorktreeStore{q: q}
}

var _ db.RunWorktreeStore = (*runWorktreeStore)(nil)

func (s *runWorktreeStore) Insert(ctx context.Context, orgID string, w domain.RunWorktree) (bool, string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, "", err
	}
	res, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO run_worktrees (run_id, repo_id, path, ref)
		VALUES (?, ?, ?, ?)
	`, w.RunID, w.RepoID, w.Path, w.Ref)
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
	existing, err := s.GetByRepoRef(ctx, orgID, w.RunID, w.RepoID, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("read existing run_worktree after conflict: %w", err)
	}
	if existing == nil {
		return false, "", fmt.Errorf("run_worktree row vanished after INSERT OR IGNORE conflict (run_id=%s, repo_id=%s, ref=%s)", w.RunID, w.RepoID, w.Ref)
	}
	return false, existing.Path, nil
}

func (s *runWorktreeStore) GetByRepoRef(ctx context.Context, orgID, runID, repoID, ref string) (*domain.RunWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT run_id, repo_id, path, ref, created_at
		FROM run_worktrees
		WHERE run_id = ? AND repo_id = ? AND ref = ?
	`, runID, repoID, ref)
	var w domain.RunWorktree
	if err := row.Scan(&w.RunID, &w.RepoID, &w.Path, &w.Ref, &w.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (s *runWorktreeStore) List(ctx context.Context, orgID, runID string) ([]domain.RunWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT run_id, repo_id, path, ref, created_at
		FROM run_worktrees
		WHERE run_id = ?
		ORDER BY created_at ASC, repo_id ASC, ref ASC
	`, runID)
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

func (s *runWorktreeStore) ListSystem(ctx context.Context, orgID, runID string) ([]domain.RunWorktree, error) {
	return s.List(ctx, orgID, runID)
}

func (s *runWorktreeStore) DeleteByRepoRef(ctx context.Context, orgID, runID, repoID, ref string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM run_worktrees WHERE run_id = ? AND repo_id = ? AND ref = ?
	`, runID, repoID, ref)
	return err
}

func (s *runWorktreeStore) DeleteByPathSystem(ctx context.Context, orgID, runID, path string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM run_worktrees WHERE run_id = ? AND path = ?
	`, runID, path)
	return err
}

// --- SKY-302 admin-pool variants — SQLite collapses to non-System ---

func (s *runWorktreeStore) InsertSystem(ctx context.Context, orgID string, w domain.RunWorktree) (bool, string, error) {
	return s.Insert(ctx, orgID, w)
}

func (s *runWorktreeStore) GetByRepoRefSystem(ctx context.Context, orgID, runID, repoID, ref string) (*domain.RunWorktree, error) {
	return s.GetByRepoRef(ctx, orgID, runID, repoID, ref)
}

func (s *runWorktreeStore) DeleteByRepoRefSystem(ctx context.Context, orgID, runID, repoID, ref string) error {
	return s.DeleteByRepoRef(ctx, orgID, runID, repoID, ref)
}
