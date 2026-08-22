package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// modelAvailabilityStore is the SQLite impl of db.ModelAvailabilityStore.
// Local mode is N=1 — one process, one org — so the app/admin split the
// Postgres impl draws collapses here, with the org asserted rather than gated.
type modelAvailabilityStore struct{ q queryer }

func newModelAvailabilityStore(q queryer) db.ModelAvailabilityStore {
	return &modelAvailabilityStore{q: q}
}

var _ db.ModelAvailabilityStore = (*modelAvailabilityStore)(nil)

func (s *modelAvailabilityStore) List(ctx context.Context, orgID string) ([]domain.ModelAvailability, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+db.ModelAvailabilityColumns+` FROM model_availability
		  WHERE org_id = ? ORDER BY provider, model_key`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModelAvailability{}
	for rows.Next() {
		row, err := db.ScanModelAvailability(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *modelAvailabilityStore) Get(ctx context.Context, orgID, provider, modelKey string) (*domain.ModelAvailability, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row, err := db.ScanModelAvailability(s.q.QueryRowContext(ctx,
		`SELECT `+db.ModelAvailabilityColumns+` FROM model_availability
		  WHERE org_id = ? AND provider = ? AND model_key = ?`, orgID, provider, modelKey).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *modelAvailabilityStore) Record(ctx context.Context, orgID, provider, modelKey, state, detail string) (domain.ModelAvailability, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.ModelAvailability{}, err
	}
	// checked_at and the green-clears-detail rule are both applied here rather
	// than by the caller passing consistent values: a green row still carrying
	// the last refusal's text, or a timestamp naming a check that never ran,
	// would be claims the schema has no way to refuse.
	return db.ScanModelAvailability(s.q.QueryRowContext(ctx, `
		INSERT INTO model_availability (org_id, provider, model_key, state, checked_at, detail)
		VALUES (?, ?, ?, ?, ?, CASE WHEN ? = 'green' THEN '' ELSE ? END)
		ON CONFLICT (org_id, provider, model_key) DO UPDATE SET
			state      = excluded.state,
			checked_at = excluded.checked_at,
			detail     = excluded.detail
		RETURNING `+db.ModelAvailabilityColumns,
		orgID, provider, modelKey, state, time.Now().UTC(), state, detail).Scan)
}
