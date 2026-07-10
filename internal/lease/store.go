package lease

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// queryer is the minimal *sql.DB surface the store needs — a plain pooled
// connection is fine for acquire/renew/read (short single-statement
// UPDATEs/INSERTs); only the tf_ctl LISTEN side (internal/ctlbus) needs a
// dedicated session-mode connection.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the persistence seam Manager drives. Postgres only — see the
// package doc for why local/TF_ROLE=all never construct one.
type Store interface {
	// EnsureRow seeds the (name) row if it doesn't exist yet, with
	// holder_id='' and renewed_at far enough in the past that the very
	// next TryAcquire succeeds immediately. INSERT ... ON CONFLICT DO
	// NOTHING — safe to call every boot; two pods racing this converge on
	// exactly one row via the insert's own conflict handling, and the
	// eventual holder is decided by TryAcquire's row lock, not this call.
	EnsureRow(ctx context.Context, name string) error

	// TryAcquire attempts to become holder of name: succeeds only when the
	// row's renewed_at is older than ttl. On success, term is the FRESH
	// fencing token (existing term + 1) and acquired is true. On failure
	// (someone else's lease is still fresh) acquired is false with a nil
	// error — that's an expected outcome, not a fault.
	TryAcquire(ctx context.Context, name, holderID string, ttl time.Duration) (term int64, acquired bool, err error)

	// Renew extends the current holder's lease. renewed is false when the
	// (name, holderID, term) triple no longer matches the row — someone
	// else's TryAcquire already won, and the caller must demote
	// immediately (see Manager.tick).
	Renew(ctx context.Context, name, holderID string, term int64) (renewed bool, err error)

	// Read returns the row's current (holder_id, term) for observability —
	// a standby's /readyz reporting who currently holds the lease. Zero
	// values, nil error for a name with no row yet.
	Read(ctx context.Context, name string) (holderID string, term int64, err error)
}

// pgStore is the Postgres implementation. All-time comparisons run in DB
// time (now() / make_interval), never Go's wall clock — see the package
// doc and the leases table's migration comment for why.
type pgStore struct{ db queryer }

// NewPostgresStore builds a Store against a pooled *sql.DB (the admin
// pool — leases carries no org_id, RLS deny-by-default, same posture as
// the instances table).
func NewPostgresStore(db queryer) Store { return &pgStore{db: db} }

func (s *pgStore) EnsureRow(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (name, holder_id, term, acquired_at, renewed_at)
		VALUES ($1, '', 0, now(), 'epoch'::timestamptz)
		ON CONFLICT (name) DO NOTHING
	`, name)
	return err
}

func (s *pgStore) TryAcquire(ctx context.Context, name, holderID string, ttl time.Duration) (term int64, acquired bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		UPDATE leases
		SET holder_id = $2, term = term + 1, acquired_at = now(), renewed_at = now()
		WHERE name = $1 AND renewed_at < now() - make_interval(secs => $3)
		RETURNING term
	`, name, holderID, ttl.Seconds()).Scan(&term)
	if errors.Is(err, sql.ErrNoRows) {
		// The lease is fresh under someone else (or the seed row doesn't
		// exist yet — the caller's boot-time EnsureRow handles that).
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return term, true, nil
}

func (s *pgStore) Renew(ctx context.Context, name, holderID string, term int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE leases SET renewed_at = now()
		WHERE name = $1 AND holder_id = $2 AND term = $3
	`, name, holderID, term)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *pgStore) Read(ctx context.Context, name string) (string, int64, error) {
	var holderID string
	var term int64
	err := s.db.QueryRowContext(ctx, `SELECT holder_id, term FROM leases WHERE name = $1`, name).Scan(&holderID, &term)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return holderID, term, err
}
