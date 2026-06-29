package sqlite

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stagedInjectionStore is the SQLite impl of db.StagedInjectionStore. SQLite is
// single-tenant (local mode, N=1) with no RLS — org_id exists for parity with
// the Postgres baseline and every caller passes LocalDefaultOrgID (asserted at
// each entry). The `...System` method names mirror the interface; in SQLite
// there's one connection, so System is identical to a plain call. See TFAC-501.
type stagedInjectionStore struct{ q queryer }

func newStagedInjectionStore(q queryer) db.StagedInjectionStore { return &stagedInjectionStore{q: q} }

var _ db.StagedInjectionStore = (*stagedInjectionStore)(nil)

func (s *stagedInjectionStore) AppendSystem(ctx context.Context, orgID string, n *domain.StagedInjection) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	id := n.ID
	if id == "" {
		id = uuid.New().String()
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO staged_agent_injections (id, run_id, org_id, producer, body)
		VALUES (?, ?, ?, ?, ?)
	`, id, n.RunID, orgID, n.Producer, n.Body); err != nil {
		return err
	}
	n.ID = id
	return nil
}

func (s *stagedInjectionStore) FlushPendingSystem(ctx context.Context, orgID, runID string) ([]domain.StagedInjection, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// DELETE … RETURNING claims and removes the run's pending injections in one
	// statement, so an injection is delivered exactly once even under a resume race.
	// Oldest-first so the bundle reads in the order the producers acted.
	rows, err := s.q.QueryContext(ctx, `
		DELETE FROM staged_agent_injections
		WHERE org_id = ? AND run_id = ?
		RETURNING id, run_id, org_id, producer, body, created_at
	`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.StagedInjection
	for rows.Next() {
		var n domain.StagedInjection
		if err := rows.Scan(&n.ID, &n.RunID, &n.OrgID, &n.Producer, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// DELETE … RETURNING does not honor ORDER BY in SQLite, so sort by
	// created_at (ties broken by id) to restore oldest-first — the order the
	// bundled block reads in.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
