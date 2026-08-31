package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gitHubPendingBindStore is the SQLite impl of db.GitHubPendingBindStore. One
// connection — no RLS, no pool split — so the admin/app distinction the
// Postgres impl is forced into collapses here.
//
// Nothing writes this table in a local install: the bind ceremony needs a
// deployment App, and a distributed local binary ships no shared key. The impl
// is real rather than a stub so the conformance suite proves the atomic-consume
// guarantee on both backends with the same assertions.
type gitHubPendingBindStore struct{ q queryer }

func newGitHubPendingBindStore(q queryer) db.GitHubPendingBindStore {
	return &gitHubPendingBindStore{q: q}
}

var _ db.GitHubPendingBindStore = (*gitHubPendingBindStore)(nil)

// sqlitePendingBindColumns is the one projection both statements return — the
// insert's RETURNING and the consume's — so the two shapes cannot drift.
const sqlitePendingBindColumns = `nonce_hash, org_id, user_id, created_at, expires_at, consumed_at`

func scanPendingBind(row interface{ Scan(...any) error }) (domain.GitHubPendingBind, error) {
	var b domain.GitHubPendingBind
	var consumed sql.NullTime
	if err := row.Scan(&b.NonceHash, &b.OrgID, &b.UserID, &b.CreatedAt, &b.ExpiresAt, &consumed); err != nil {
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
	// Bound as UTC for the reason the delivery store states: this driver
	// renders a bound time.Time in Go's own string form, and the consume
	// compares expires_at against another bound time.Time. One writer, one
	// format, one zone.
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO github_pending_binds (nonce_hash, org_id, user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING `+sqlitePendingBindColumns,
		bind.NonceHash, bind.OrgID, bind.UserID, bind.CreatedAt.UTC(), bind.ExpiresAt.UTC())
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
	row := s.q.QueryRowContext(ctx, `
		UPDATE github_pending_binds
		   SET consumed_at = ?
		 WHERE nonce_hash = ?
		   AND consumed_at IS NULL
		   AND expires_at > ?
		RETURNING `+sqlitePendingBindColumns,
		now, nonceHash, now)
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
	_, _ = s.q.ExecContext(ctx, `
		DELETE FROM github_pending_binds WHERE expires_at < ?
	`, now.Add(-db.GitHubPendingBindPruneAge))

	return &stored, nil
}
