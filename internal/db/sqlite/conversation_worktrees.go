package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runWorktreeStore is the SQLite impl of db.ConversationWorktreeStore. The
// constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so the second arg is
// discarded. ...System variants are thin wrappers around their
// non-System counterparts.
//
// A ledger row points at a repository by the registry row's id. The store's
// surface stays the slug, because the caller is an agent's argv
// (`tfac exec workspace add owner/repo`), so every method resolves the slug
// here: a write get-or-creates the registry row, a read or a delete resolves
// it and matches nothing when there is none.
type runWorktreeStore struct{ q queryer }

func newConversationWorktreeStore(q, _ queryer) db.ConversationWorktreeStore {
	return &runWorktreeStore{q: q}
}

var _ db.ConversationWorktreeStore = (*runWorktreeStore)(nil)

func (s *runWorktreeStore) Insert(ctx context.Context, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, "", err
	}
	// A lookup, never a create. In multi mode this runs on the executor, whose
	// Postgres role holds SELECT and UPDATE on repositories and deliberately not
	// INSERT — a repository is brought into the registry by the side that polls
	// and tracks, never by a running agent's pod — and the two dialects agree on
	// the contract rather than diverging where only one of them is enforced. A
	// worktree is reserved for a repository the run was already authorized to
	// clone, so the row exists; an absent one is a broken caller.
	owner, repo := splitRepoSlug(w.RepoID)
	if owner == "" || repo == "" {
		return false, "", fmt.Errorf("run_worktree repo id %q is not an owner/repo slug", w.RepoID)
	}
	repositoryID, err := findRepositoryID(ctx, s.q, domain.RepoSourceGitHub, owner, repo)
	if err != nil {
		return false, "", fmt.Errorf("resolve repository %s: %w", w.RepoID, err)
	}
	if repositoryID == "" {
		return false, "", fmt.Errorf("no repository row for %s", w.RepoID)
	}
	res, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO conversation_worktrees (conversation_id, repository_id, path, ref)
		VALUES (?, ?, ?, ?)
	`, w.ConversationID, repositoryID, w.Path, w.Ref)
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
	existing, err := s.GetByRepoRef(ctx, orgID, w.ConversationID, w.RepoID, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("read existing run_worktree after conflict: %w", err)
	}
	if existing == nil {
		return false, "", fmt.Errorf("run_worktree row vanished after INSERT OR IGNORE conflict (conversation_id=%s, repo_id=%s, ref=%s)", w.ConversationID, w.RepoID, w.Ref)
	}
	return false, existing.Path, nil
}

func (s *runWorktreeStore) GetByRepoRef(ctx context.Context, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil, nil
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT w.conversation_id, r.owner || '/' || r.repo, w.path, w.ref, w.created_at
		FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
		WHERE w.conversation_id = ? AND LOWER(r.owner) = LOWER(?) AND LOWER(r.repo) = LOWER(?) AND w.ref = ?
	`, conversationID, owner, repo, ref)
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT w.conversation_id, r.owner || '/' || r.repo, w.path, w.ref, w.created_at
		FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
		WHERE w.conversation_id = ?
		ORDER BY w.created_at ASC, r.owner ASC, r.repo ASC, w.ref ASC
	`, conversationID)
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

func (s *runWorktreeStore) ListSystem(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error) {
	return s.List(ctx, orgID, conversationID)
}

func (s *runWorktreeStore) DeleteByRepoRef(ctx context.Context, orgID, conversationID, repoID, ref string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	owner, repo := splitRepoSlug(repoID)
	if owner == "" || repo == "" {
		return nil
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees
		 WHERE conversation_id = ? AND ref = ?
		   AND repository_id IN (
		       SELECT id FROM repositories
		        WHERE LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?))
	`, conversationID, ref, owner, repo)
	return err
}

func (s *runWorktreeStore) DeleteByPathSystem(ctx context.Context, orgID, conversationID, path string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees WHERE conversation_id = ? AND path = ?
	`, conversationID, path)
	return err
}

// --- admin-pool variants — SQLite collapses to non-System ---

func (s *runWorktreeStore) InsertSystem(ctx context.Context, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	return s.Insert(ctx, orgID, w)
}

func (s *runWorktreeStore) GetByRepoRefSystem(ctx context.Context, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error) {
	return s.GetByRepoRef(ctx, orgID, conversationID, repoID, ref)
}

func (s *runWorktreeStore) DeleteByRepoRefSystem(ctx context.Context, orgID, conversationID, repoID, ref string) error {
	return s.DeleteByRepoRef(ctx, orgID, conversationID, repoID, ref)
}
