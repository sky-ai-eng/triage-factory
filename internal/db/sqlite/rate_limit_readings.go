package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// rateLimitReadingStore is the SQLite impl of db.RateLimitReadingStore. Local
// mode is one process and one org, so the table holds only what this process
// flushed; it exists so the route reads one store in both modes. The
// app/admin split collapses to the one connection, with the org asserted
// rather than gated.
type rateLimitReadingStore struct{ q queryer }

func newRateLimitReadingStore(q queryer) db.RateLimitReadingStore {
	return &rateLimitReadingStore{q: q}
}

var _ db.RateLimitReadingStore = (*rateLimitReadingStore)(nil)

const sqliteRateLimitReadingCols = `org_id, upstream, credential, bucket, remaining, bucket_limit, used, near_limit, reset_at, observed_at`

func (s *rateLimitReadingStore) List(ctx context.Context, orgID string, now time.Time) ([]domain.RateLimitReading, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteRateLimitReadingCols+`
		FROM rate_limit_readings
		WHERE org_id = ? AND (reset_at > ? OR (reset_at IS NULL AND observed_at > ?))
		ORDER BY upstream, credential, bucket`,
		orgID, now.UTC(), now.Add(-domain.RateLimitReadingMaxAge).UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RateLimitReading{}
	for rows.Next() {
		var (
			r                      domain.RateLimitReading
			remaining, limit, used sql.NullInt64
			near                   sql.NullBool
			reset                  sql.NullTime
		)
		if err := rows.Scan(&r.OrgID, &r.Upstream, &r.Credential, &r.Bucket,
			&remaining, &limit, &used, &near, &reset, &r.ObservedAt); err != nil {
			return nil, err
		}
		r.Remaining = intPtrFromNull(remaining)
		r.Limit = intPtrFromNull(limit)
		r.Used = intPtrFromNull(used)
		if near.Valid {
			v := near.Bool
			r.NearLimit = &v
		}
		if reset.Valid {
			t := reset.Time.UTC()
			r.ResetAt = &t
		}
		r.ObservedAt = r.ObservedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *rateLimitReadingStore) UpsertSystem(ctx context.Context, readings []domain.RateLimitReading) error {
	for _, r := range readings {
		if err := assertLocalOrg(r.OrgID); err != nil {
			return err
		}
		var reset any
		if r.ResetAt != nil {
			reset = r.ResetAt.UTC()
		}
		if _, err := s.q.ExecContext(ctx, `
			INSERT INTO rate_limit_readings (`+sqliteRateLimitReadingCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (org_id, upstream, credential, bucket) DO UPDATE SET
				remaining    = excluded.remaining,
				bucket_limit = excluded.bucket_limit,
				used         = excluded.used,
				near_limit   = excluded.near_limit,
				reset_at     = excluded.reset_at,
				observed_at  = excluded.observed_at
			WHERE rate_limit_readings.observed_at < excluded.observed_at`,
			r.OrgID, r.Upstream, r.Credential, r.Bucket,
			r.Remaining, r.Limit, r.Used, r.NearLimit, reset, r.ObservedAt.UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (s *rateLimitReadingStore) DeleteExpiredSystem(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM rate_limit_readings
		WHERE (reset_at IS NOT NULL AND reset_at <= ?) OR (reset_at IS NULL AND observed_at <= ?)`,
		now.UTC(), now.Add(-domain.RateLimitReadingMaxAge).UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
