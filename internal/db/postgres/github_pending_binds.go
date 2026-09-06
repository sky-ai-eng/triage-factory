package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gitHubPendingBindStore is the Postgres impl of db.GitHubPendingBindStore.
// Admin pool only, and forced rather than chosen: the consume reads the row by
// its nonce hash and learns the org from it, so the org an RLS policy would
// gate on is the read's output rather than an input. The nonce is the
// authorization; the org-admin check that authorizes the create runs in the
// handler.
type gitHubPendingBindStore struct{ admin queryer }

func newGitHubPendingBindStore(admin queryer) db.GitHubPendingBindStore {
	return &gitHubPendingBindStore{admin: admin}
}

var _ db.GitHubPendingBindStore = (*gitHubPendingBindStore)(nil)

// pgPendingBindColumns is the one projection both statements return — the
// insert's RETURNING and the consume's — so the two shapes cannot drift.
const pgPendingBindColumns = `nonce_hash, org_id, user_id, leg, account_login, created_at, expires_at, consumed_at`

func scanPendingBind(row interface{ Scan(...any) error }) (domain.GitHubPendingBind, error) {
	var b domain.GitHubPendingBind
	var consumed sql.NullTime
	if err := row.Scan(&b.NonceHash, &b.OrgID, &b.UserID, &b.Leg, &b.AccountLogin, &b.CreatedAt, &b.ExpiresAt, &consumed); err != nil {
		return domain.GitHubPendingBind{}, err
	}
	if consumed.Valid {
		b.ConsumedAt = consumed.Time.UTC()
	}
	b.CreatedAt = b.CreatedAt.UTC()
	b.ExpiresAt = b.ExpiresAt.UTC()
	return b, nil
}

func (s *gitHubPendingBindStore) CreateSystem(ctx context.Context, bind domain.GitHubPendingBind) (domain.GitHubPendingBind, error) {
	row := s.admin.QueryRowContext(ctx, `
		INSERT INTO github_pending_binds (nonce_hash, org_id, user_id, leg, account_login, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+pgPendingBindColumns,
		bind.NonceHash, bind.OrgID, bind.UserID, bind.Leg, bind.AccountLogin, bind.CreatedAt.UTC(), bind.ExpiresAt.UTC())
	stored, err := scanPendingBind(row)
	if err != nil {
		return domain.GitHubPendingBind{}, fmt.Errorf("insert github_pending_binds: %w", err)
	}
	return stored, nil
}

func (s *gitHubPendingBindStore) ConsumeSystem(ctx context.Context, nonceHash string, now time.Time) (*domain.GitHubPendingBind, error) {
	now = now.UTC()
	// One conditional UPDATE decides everything: absent, expired and
	// already-consumed all match nothing, and a second caller arriving on the
	// same row finds consumed_at already set. There is no read to lose a race
	// against.
	row := s.admin.QueryRowContext(ctx, `
		UPDATE github_pending_binds
		   SET consumed_at = $1
		 WHERE nonce_hash = $2
		   AND consumed_at IS NULL
		   AND expires_at > $1
		RETURNING `+pgPendingBindColumns,
		now, nonceHash)
	stored, err := scanPendingBind(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to spend. The caller refuses identically for every reason a
		// row could be missing, so the miss is an answer rather than an error.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume github_pending_binds: %w", err)
	}

	// Best-effort: a missed prune grows the table a little until the next
	// successful call, which is never a reason to fail the bind this call is
	// completing.
	_, _ = s.admin.ExecContext(ctx, `
		DELETE FROM github_pending_binds WHERE expires_at < $1
	`, now.Add(-db.GitHubPendingBindPruneAge))

	return &stored, nil
}
