package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// modelAvailabilityStore is the Postgres impl of db.ModelAvailabilityStore.
// App pool throughout, under RLS: member SELECT (the model list renders the
// badge for anyone who can see the catalog), org-admin write (a probe spends
// the org's money). No admin-pool variant, because nothing without request
// claims ever writes here — a background prober would be exactly the automatic
// re-probing this design refuses.
type modelAvailabilityStore struct{ q queryer }

func newModelAvailabilityStore(q queryer) db.ModelAvailabilityStore {
	return &modelAvailabilityStore{q: q}
}

var _ db.ModelAvailabilityStore = (*modelAvailabilityStore)(nil)

func (s *modelAvailabilityStore) List(ctx context.Context, orgID string) ([]domain.ModelAvailability, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+db.ModelAvailabilityColumns+` FROM model_availability
		  WHERE org_id = $1 ORDER BY provider, model_key`, orgID)
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
	row, err := db.ScanModelAvailability(s.q.QueryRowContext(ctx,
		`SELECT `+db.ModelAvailabilityColumns+` FROM model_availability
		  WHERE org_id = $1 AND provider = $2 AND model_key = $3`, orgID, provider, modelKey).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *modelAvailabilityStore) Record(ctx context.Context, orgID, provider, modelKey, state, detail string) (domain.ModelAvailability, error) {
	// checked_at and the green-clears-detail rule are both applied here rather
	// than by the caller passing consistent values: a green row still carrying
	// the last refusal's text, or a timestamp naming a check that never ran,
	// would be claims the schema has no way to refuse.
	return db.ScanModelAvailability(s.q.QueryRowContext(ctx, `
		INSERT INTO model_availability (org_id, provider, model_key, state, checked_at, detail)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $4 = 'green' THEN '' ELSE $6 END)
		ON CONFLICT (org_id, provider, model_key) DO UPDATE SET
			state      = EXCLUDED.state,
			checked_at = EXCLUDED.checked_at,
			detail     = EXCLUDED.detail
		RETURNING `+db.ModelAvailabilityColumns,
		orgID, provider, modelKey, state, time.Now().UTC(), detail).Scan)
}
