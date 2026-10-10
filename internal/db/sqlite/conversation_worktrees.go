package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// conversationWorktreeStore is the SQLite impl of db.ConversationWorktreeStore. The
// constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so the second arg is
// discarded. ...System variants are thin wrappers around their
// non-System counterparts.
//
// A ledger row points at a repository by the registry row's id, and so does
// every method here: the agent's argv (`tfac exec workspace add owner/repo`)
// is resolved to a row on the org's GitHub host before a reservation is made,
// and a slug alone would name a repository on whichever host answered first.
// Reads join the row back for its "owner/repo".
type conversationWorktreeStore struct{ q queryer }

func newConversationWorktreeStore(q, _ queryer) db.ConversationWorktreeStore {
	return &conversationWorktreeStore{q: q}
}

var _ db.ConversationWorktreeStore = (*conversationWorktreeStore)(nil)

// worktreeColumns is the projection scanWorktree reads: the ledger row plus the
// repository's slug, joined as r.
const worktreeColumns = `w.conversation_id, w.repository_id, r.owner || '/' || r.repo, w.path, w.ref, w.created_at`

func scanWorktree(row rowScanner) (domain.ConversationWorktree, error) {
	var w domain.ConversationWorktree
	err := row.Scan(&w.ConversationID, &w.RepositoryID, &w.RepoID, &w.Path, &w.Ref, &w.CreatedAt)
	return w, err
}

// checkWorktreeRepositoryID confirms the registry id a caller passes names a
// repository. A lookup, never a create. In multi mode this runs on the
// executor, whose Postgres role holds SELECT and UPDATE on repositories and
// deliberately not INSERT — a repository is brought into the registry by the
// side that polls and tracks, never by a running agent's pod — and the two
// dialects agree on the contract rather than diverging where only one of them
// is enforced. A worktree is reserved for a repository the conversation was
// already authorized to clone, so the row exists; an absent one is a broken
// caller.
func checkWorktreeRepositoryID(ctx context.Context, q queryer, repositoryID string) error {
	var found int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM repositories WHERE id = ?`, repositoryID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", db.ErrNoSuchRepository, repositoryID)
	}
	return err
}

func (s *conversationWorktreeStore) Insert(ctx context.Context, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, "", err
	}
	if err := checkWorktreeRepositoryID(ctx, s.q, w.RepositoryID); err != nil {
		return false, "", fmt.Errorf("resolve repository %s: %w", w.RepositoryID, err)
	}
	res, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO conversation_worktrees (conversation_id, repository_id, path, ref)
		VALUES (?, ?, ?, ?)
	`, w.ConversationID, w.RepositoryID, w.Path, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("insert conversation_worktree: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("rows affected: %w", err)
	}
	if rows == 1 {
		return true, w.Path, nil
	}
	existing, err := s.GetByRepoRef(ctx, orgID, w.ConversationID, w.RepositoryID, w.Ref)
	if err != nil {
		return false, "", fmt.Errorf("read existing conversation_worktree after conflict: %w", err)
	}
	if existing == nil {
		return false, "", fmt.Errorf("conversation_worktree row vanished after INSERT OR IGNORE conflict (conversation_id=%s, repository_id=%s, ref=%s)", w.ConversationID, w.RepositoryID, w.Ref)
	}
	return false, existing.Path, nil
}

func (s *conversationWorktreeStore) GetByRepoRef(ctx context.Context, orgID, conversationID, repositoryID, ref string) (*domain.ConversationWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+worktreeColumns+`
		FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
		WHERE w.conversation_id = ? AND w.repository_id = ? AND w.ref = ?
	`, conversationID, repositoryID, ref)
	w, err := scanWorktree(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (s *conversationWorktreeStore) List(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+worktreeColumns+`
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
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *conversationWorktreeStore) ListSystem(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error) {
	return s.List(ctx, orgID, conversationID)
}

func (s *conversationWorktreeStore) DeleteByRepoRef(ctx context.Context, orgID, conversationID, repositoryID, ref string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees
		 WHERE conversation_id = ? AND ref = ? AND repository_id = ?
	`, conversationID, ref, repositoryID)
	return err
}

func (s *conversationWorktreeStore) DeleteByPathSystem(ctx context.Context, orgID, conversationID, path string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		DELETE FROM conversation_worktrees WHERE conversation_id = ? AND path = ?
	`, conversationID, path)
	return err
}

func (s *conversationWorktreeStore) ListForTaskSystem(ctx context.Context, orgID, taskID string) ([]domain.ConversationWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+worktreeColumns+`
		FROM conversation_worktrees w
		JOIN repositories r ON r.id = w.repository_id
		JOIN conversations c ON c.id = w.conversation_id
		WHERE c.task_id = ?
		ORDER BY w.created_at ASC, w.conversation_id ASC, r.owner ASC, r.repo ASC, w.ref ASC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ConversationWorktree{}
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *conversationWorktreeStore) RecordForClaimSystem(ctx context.Context, orgID, claimID string, w domain.ConversationWorktree) (domain.ConversationWorktree, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.ConversationWorktree{}, err
	}
	var stored domain.ConversationWorktree
	err := inTx(ctx, s.q, func(q queryer) error {
		if err := assertClaimActive(ctx, q, orgID, w.ConversationID, claimID); err != nil {
			return err
		}
		if err := checkWorktreeRepositoryID(ctx, q, w.RepositoryID); err != nil {
			return fmt.Errorf("resolve repository %s: %w", w.RepositoryID, err)
		}
		if err := q.QueryRowContext(ctx, `
			INSERT INTO conversation_worktrees (conversation_id, repository_id, path, ref)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (conversation_id, repository_id, ref) DO UPDATE SET path = excluded.path
			RETURNING conversation_id, repository_id, path, ref, created_at
		`, w.ConversationID, w.RepositoryID, w.Path, w.Ref).Scan(&stored.ConversationID, &stored.RepositoryID, &stored.Path, &stored.Ref, &stored.CreatedAt); err != nil {
			return fmt.Errorf("record conversation_worktree: %w", err)
		}
		// The slug is the registry row's spelling, as every read returns it.
		return q.QueryRowContext(ctx, `SELECT owner || '/' || repo FROM repositories WHERE id = ?`, stored.RepositoryID).Scan(&stored.RepoID)
	})
	if err != nil {
		return domain.ConversationWorktree{}, err
	}
	return stored, nil
}

// --- admin-pool variants — SQLite collapses to non-System ---

func (s *conversationWorktreeStore) InsertSystem(ctx context.Context, orgID string, w domain.ConversationWorktree) (bool, string, error) {
	return s.Insert(ctx, orgID, w)
}

func (s *conversationWorktreeStore) GetByRepoRefSystem(ctx context.Context, orgID, conversationID, repositoryID, ref string) (*domain.ConversationWorktree, error) {
	return s.GetByRepoRef(ctx, orgID, conversationID, repositoryID, ref)
}

func (s *conversationWorktreeStore) DeleteByRepoRefSystem(ctx context.Context, orgID, conversationID, repositoryID, ref string) error {
	return s.DeleteByRepoRef(ctx, orgID, conversationID, repositoryID, ref)
}
