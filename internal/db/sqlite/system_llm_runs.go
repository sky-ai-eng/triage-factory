package sqlite

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// systemLLMRunStore is the SQLite impl of db.SystemLLMRunStore. SQLite is
// single-tenant (local mode, N=1) with no RLS — the org_id column exists
// for parity with the Postgres baseline but every row defaults to
// LocalDefaultOrgID. The Postgres impl routes writes through the admin
// pool; here both pools collapse to the one connection. See TFAC-451.
type systemLLMRunStore struct{ q queryer }

func newSystemLLMRunStore(q queryer) db.SystemLLMRunStore { return &systemLLMRunStore{q: q} }

var _ db.SystemLLMRunStore = (*systemLLMRunStore)(nil)

func (s *systemLLMRunStore) Record(ctx context.Context, row domain.SystemLLMRun) error {
	if err := assertLocalOrg(row.OrgID); err != nil {
		return err
	}
	id := row.ID
	if id == "" {
		id = uuid.New().String()
	}
	completedAt := row.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO system_llm_runs
			(id, org_id, job, model, total_cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			 duration_ms, num_turns, is_error, metadata_json, started_at, completed_at, trace_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (trace_id) WHERE trace_id IS NOT NULL DO NOTHING
	`,
		id, row.OrgID, row.Job, row.Model, row.TotalCostUSD,
		row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreationTokens,
		row.DurationMs, row.NumTurns, row.IsError, nullIfEmpty(row.MetadataJSON),
		row.StartedAt, completedAt, nullIfEmpty(row.TraceID),
	)
	return err
}
