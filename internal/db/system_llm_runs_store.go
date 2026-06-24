package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SystemLLMRunStore owns the system_llm_runs table — the per-call cost +
// token accounting for headless LLM invocations made by background system
// jobs (scorer, repo-profiler, project-classifier). The table is
// system-written and org-scoped, the same shape as repo_profiles: every
// write routes through the admin pool in Postgres (the writers are
// boot-launched goroutines with no JWT-claims context), and the
// org-scoped RLS policy gates the app-pool reads a future spend view will
// make. SQLite is N=1 and unscoped.
//
// row.OrgID is required on every call; local mode passes
// runmode.LocalDefaultOrgID. See TFAC-451.
type SystemLLMRunStore interface {
	// Record inserts one row. row.ID may be empty — the SQLite impl
	// generates a uuid, Postgres defaults gen_random_uuid(). row.CompletedAt
	// defaults to now when zero. Callers own the "never break the job on a
	// recording failure" contract; this method just returns the insert
	// error for the caller to log and swallow.
	Record(ctx context.Context, row domain.SystemLLMRun) error
}
