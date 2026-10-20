package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// rateLimitReadingStore is the Postgres impl of db.RateLimitReadingStore.
// Split-pool: q is the app pool (RLS — member SELECT), admin serves the
// flusher's writes and the brain's expiry sweep, neither of which carries
// request claims.
type rateLimitReadingStore struct {
	q     queryer
	admin queryer
}

func newRateLimitReadingStore(q, admin queryer) db.RateLimitReadingStore {
	return &rateLimitReadingStore{q: q, admin: admin}
}

var _ db.RateLimitReadingStore = (*rateLimitReadingStore)(nil)

const pgRateLimitReadingCols = `org_id, upstream, credential, bucket, remaining, bucket_limit, used, near_limit, reset_at, observed_at`

// pgRateLimitReadingCurrent is domain.RateLimitReading.Current as a predicate
// over $2 (now) and $3 (now minus domain.RateLimitReadingMaxAge).
const pgRateLimitReadingCurrent = `(reset_at > $2 OR (reset_at IS NULL AND observed_at > $3))`

func (s *rateLimitReadingStore) List(ctx context.Context, orgID string, now time.Time) ([]domain.RateLimitReading, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgRateLimitReadingCols+`
		FROM rate_limit_readings
		WHERE org_id = $1 AND `+pgRateLimitReadingCurrent+`
		ORDER BY upstream, credential, bucket`,
		orgID, now.UTC(), now.Add(-domain.RateLimitReadingMaxAge).UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RateLimitReading{}
	for rows.Next() {
		r, err := scanRateLimitReading(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *rateLimitReadingStore) UpsertSystem(ctx context.Context, readings []domain.RateLimitReading) error {
	for _, batch := range db.RateLimitUpsertBatches(readings) {
		values := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*10)
		for _, r := range batch {
			n := len(args)
			values = append(values, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
				n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10))
			args = append(args, r.OrgID, r.Upstream, r.Credential, r.Bucket,
				nullIntPtr(r.Remaining), nullIntPtr(r.Limit), nullIntPtr(r.Used), nullBoolPtr(r.NearLimit), utcPtr(r.ResetAt), r.ObservedAt.UTC())
		}
		if _, err := s.admin.ExecContext(ctx, `
			INSERT INTO rate_limit_readings (`+pgRateLimitReadingCols+`)
			VALUES `+strings.Join(values, ", ")+`
			ON CONFLICT (org_id, upstream, credential, bucket) DO UPDATE SET
				remaining    = EXCLUDED.remaining,
				bucket_limit = EXCLUDED.bucket_limit,
				used         = EXCLUDED.used,
				near_limit   = EXCLUDED.near_limit,
				reset_at     = EXCLUDED.reset_at,
				observed_at  = EXCLUDED.observed_at
			WHERE rate_limit_readings.observed_at < EXCLUDED.observed_at`, args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *rateLimitReadingStore) DeleteExpiredSystem(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM rate_limit_readings
		WHERE (reset_at IS NOT NULL AND reset_at <= $1) OR (reset_at IS NULL AND observed_at <= $2)`,
		now.UTC(), now.Add(-domain.RateLimitReadingMaxAge).UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanRateLimitReading(rows *sql.Rows) (domain.RateLimitReading, error) {
	var (
		out                    domain.RateLimitReading
		remaining, limit, used sql.NullInt64
		near                   sql.NullBool
		reset                  sql.NullTime
	)
	if err := rows.Scan(&out.OrgID, &out.Upstream, &out.Credential, &out.Bucket,
		&remaining, &limit, &used, &near, &reset, &out.ObservedAt); err != nil {
		return domain.RateLimitReading{}, err
	}
	out.Remaining = intPtrFromNull(remaining)
	out.Limit = intPtrFromNull(limit)
	out.Used = intPtrFromNull(used)
	if near.Valid {
		v := near.Bool
		out.NearLimit = &v
	}
	if reset.Valid {
		t := reset.Time.UTC()
		out.ResetAt = &t
	}
	out.ObservedAt = out.ObservedAt.UTC()
	return out, nil
}

func nullBoolPtr(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
