package postgres

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// systemLLMRunStore is the Postgres impl of db.SystemLLMRunStore. Wired
// against the admin pool in postgres.New: the writers (scorer,
// repo-profiler, project-classifier) are boot-launched background
// goroutines with no JWT-claims context, so an app-pool INSERT under the
// system_llm_runs_all RLS policy would be rejected — same reason
// repo_profiles' `...System` writes and the PendingFirings/EventQueue
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
	// id defaults to gen_random_uuid() server-side; metadata_json '' → NULL.
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO system_llm_runs
			(org_id, job, model, total_cost_usd,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			 duration_ms, num_turns, is_error, metadata_json, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13, $14)
	`,
		row.OrgID, row.Job, row.Model, row.TotalCostUSD,
		row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheCreationTokens,
		row.DurationMs, row.NumTurns, row.IsError, row.MetadataJSON,
		row.StartedAt, completedAt,
	)
	return err
}
