package postgres

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// systemLLMRunStore is the Postgres impl of db.SystemLLMRunStore. Wired
// against the admin pool in postgres.New: the writers (scorer,
// repo-profiler) are boot-launched background
// goroutines with no JWT-claims context, so an app-pool INSERT under the
// system_llm_runs_all RLS policy would be rejected — same reason
// repositories' `...System` writes and the PendingFirings/EventQueue
// stores route through admin. The org-scoped RLS policy gates the
// app-pool reads a future spend view will make; org_id stays in the
// INSERT as defense in depth. See TFAC-451.
type systemLLMRunStore struct{ admin queryer }

func newSystemLLMRunStore(admin queryer) db.SystemLLMRunStore {
	return &systemLLMRunStore{admin: admin}
}

var _ db.SystemLLMRunStore = (*systemLLMRunStore)(nil)

func (s *systemLLMRunStore) Record(ctx context.Context, row domain.SystemLLMRun) error {
	completedAt := row.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	// A caller-supplied row.ID is honored (parity with the SQLite impl); an
	// empty row.ID falls back to gen_random_uuid() server-side. metadata_json
	// '' → NULL. trace_id '' → NULL too (TFAC-579) — the partial unique index
	// excludes NULL, so a caller with no session id never collides with any
	// other row; ON CONFLICT DO NOTHING makes a genuine duplicate TraceID a
	// no-op instead of double-counting spend.
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO system_llm_runs
			(id, org_id, job, model, total_cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			 duration_ms, num_turns, is_error, metadata_json, started_at, completed_at, trace_id)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULLIF($13, ''), $14, $15, NULLIF($16, ''))
		ON CONFLICT (trace_id) WHERE trace_id IS NOT NULL DO NOTHING
	`,
		row.ID, row.OrgID, row.Job, row.Model, row.TotalCostUSD,
		row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreationTokens,
		row.DurationMs, row.NumTurns, row.IsError, row.MetadataJSON,
		row.StartedAt, completedAt, row.TraceID,
	)
	return err
}
