package postgres

import (
	"context"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stagedInjectionStore is the Postgres impl of db.StagedInjectionStore — the durable
// "stage for next resume" agent-injection queue (TFAC-501). Wired against the ADMIN
// (BYPASSRLS) pool in postgres.New: both the producer (an eventbus subscriber)
// and the consumer (a detached resume goroutine) run without JWT claims, so an
// app-pool/RLS statement would see nothing. The table still inherits runs'
// team-scoped RLS via its (run_id, org_id) FK, and org_id stays bound in every
// WHERE/INSERT clause as defense in depth. Mirrors authEventStore's single-pool
// shape; FlushPendingSystem is the one DELETE path.
type stagedInjectionStore struct{ admin queryer }

func newStagedInjectionStore(admin queryer) db.StagedInjectionStore {
	return &stagedInjectionStore{admin: admin}
}

var _ db.StagedInjectionStore = (*stagedInjectionStore)(nil)

func (s *stagedInjectionStore) AppendSystem(ctx context.Context, orgID string, n *domain.StagedInjection) error {
	// A caller-supplied n.ID is honored (parity with SQLite); an empty n.ID falls
	// back to gen_random_uuid() server-side. created_at is left to the column
	// DEFAULT now(). RETURNING id::text writes the minted id back to the caller.
	var id string
	if err := s.admin.QueryRowContext(ctx, `
		INSERT INTO staged_agent_injections (id, run_id, org_id, producer, body)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()), $2::uuid, $3::uuid, $4, $5)
		RETURNING id::text
	`, n.ID, n.RunID, orgID, n.Producer, n.Body).Scan(&id); err != nil {
		return err
	}
	n.ID = id
	return nil
}

func (s *stagedInjectionStore) FlushPendingSystem(ctx context.Context, orgID, runID string) ([]domain.StagedInjection, error) {
	// DELETE … RETURNING claims and removes the run's pending injections in one
	// statement, so an injection is delivered exactly once even under a resume race.
	rows, err := s.admin.QueryContext(ctx, `
		DELETE FROM staged_agent_injections
		WHERE org_id = $1::uuid AND run_id = $2::uuid
		RETURNING id::text, run_id::text, org_id::text, producer, body, created_at
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
	// DELETE … RETURNING does not honor ORDER BY, so sort by created_at (ties
	// broken by id) to restore oldest-first — the order the bundled block reads
	// in. Mirrors the SQLite impl. now() is microsecond-resolution here, so the
	// created_at tie the SQLite twin documents (1-second CURRENT_TIMESTAMP) is
	// effectively impossible — the id tiebreaker is a formality.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *stagedInjectionStore) DeleteSystem(ctx context.Context, orgID, id string) error {
	_, err := s.admin.ExecContext(ctx, `
		DELETE FROM staged_agent_injections WHERE org_id = $1::uuid AND id = $2::uuid
	`, orgID, id)
	return err
}
