package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SystemLLMRunStore owns the system_llm_runs table — the per-call cost +
// token accounting for headless LLM invocations made by background system
// jobs (scorer, repo-profiler). The table is
// system-written and org-scoped, the same shape as repositories: every
// write routes through the admin pool in Postgres (the writers are
// boot-launched goroutines with no JWT-claims context), and the
// org-scoped RLS policy gates the app-pool reads a future spend view will
// make. SQLite is N=1 and unscoped.
//
// row.OrgID is required on every call; local mode passes
// runmode.LocalDefaultOrgID. See TFAC-451.
type SystemLLMRunStore interface {
	// Record inserts one row. row.ID may be empty — both impls then
	// generate a uuid (SQLite app-side, Postgres via gen_random_uuid());
	// a non-empty row.ID is honored on both (Postgres requires it parse as
	// a uuid). row.CompletedAt defaults to now when zero. Callers own the
	// "never break the job on a recording failure" contract; this method
	// just returns the insert error for the caller to log and swallow.
	//
	// row.TraceID (TFAC-579) is the idempotency key: a duplicate Record()
	// call for the same invocation (retry, double-call) is a no-op rather
	// than double-counting spend, enforced by a partial unique index on
	// trace_id in both backends. Empty TraceID never collides with itself
	// (the index excludes NULL/empty) — every insert with no id just lands.
	//
	// Exempt from the returned-row rule: fire-and-forget telemetry. The row is
	// written on the way past and spent by a later aggregate read.
	Record(ctx context.Context, row domain.SystemLLMRun) error
}
