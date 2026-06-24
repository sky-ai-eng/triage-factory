// Package systemllm records per-call cost + token accounting for the
// headless LLM jobs (scorer, repo-profiler, project-classifier) into the
// system_llm_runs table. It sits one layer above agentproc + db so the
// runtime stays storage-agnostic (agentproc takes a SecretsReader rather
// than importing db, by deliberate convention) while the recording logic
// — which needs both the Run outcome/usage and the db store — lives in
// one place instead of being duplicated at each of the three call sites.
// See TFAC-451.
package systemllm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var log = logging.Component("systemllm")

// Job discriminators for the system_llm_runs.job column.
const (
	JobScorer       = "scorer"
	JobRepoProfiler = "repo_profiler"
	JobClassifier   = "classifier"
)

// Recorder writes one system_llm_runs row per agentproc.Run call. A nil
// Recorder (or one built with a nil store) is a safe no-op, so callers in
// tests / local paths that never wire a store don't have to nil-check.
type Recorder struct {
	store db.SystemLLMRunStore
}

// NewRecorder wraps the store. Pass the bundle's SystemLLMRuns store.
func NewRecorder(store db.SystemLLMRunStore) *Recorder {
	return &Recorder{store: store}
}

// Call carries the per-call context the call site supplies. Cost /
// duration / turns / is_error come from the Run outcome; tokens from the
// usage sink; the rest from here.
type Call struct {
	OrgID     string
	Job       string // one of the Job* constants
	Model     string
	StartedAt time.Time
	// Metadata is optional per-job context (e.g. {"batch_size": 10}).
	// nil/empty serializes to SQL NULL.
	Metadata map[string]any
}

// Record builds a system_llm_runs row from the Run outcome + usage sink
// and inserts it. It NEVER returns an error and NEVER panics — a
// recording failure is logged and swallowed so accounting can't break the
// job whose cost it's trying to capture.
//
// Recording happens whenever outcome != nil, including a failed-but-
// completed run (outcome.Result.IsError) — that run still cost tokens. If
// Run returned a non-nil err with a nil outcome, the call site passes
// outcome == nil and Record no-ops, letting the existing error handling
// proceed. An outcome with a nil Result (subprocess exited without a
// terminal event) is still recorded — the token counts from the sink are
// real even when the cost accounting never arrived — flagged is_error.
func (r *Recorder) Record(ctx context.Context, c Call, outcome *agentproc.Outcome, usage *agentproc.UsageSink) {
	if r == nil || r.store == nil || outcome == nil {
		return
	}

	row := domain.SystemLLMRun{
		OrgID:       c.OrgID,
		Job:         c.Job,
		Model:       c.Model,
		StartedAt:   c.StartedAt,
		CompletedAt: time.Now().UTC(),
	}
	if usage != nil {
		row.InputTokens = usage.InputTokens
		row.OutputTokens = usage.OutputTokens
		row.CacheReadTokens = usage.CacheReadTokens
		row.CacheCreationTokens = usage.CacheCreationTokens
	}
	if res := outcome.Result; res != nil {
		row.TotalCostUSD = res.CostUSD
		row.DurationMs = res.DurationMs
		row.NumTurns = res.NumTurns
		row.IsError = res.IsError
	} else {
		// No terminal result event — the run didn't complete cleanly even
		// though it produced some output. Tokens (if any) are still real.
		row.IsError = true
	}
	if len(c.Metadata) > 0 {
		if b, err := json.Marshal(c.Metadata); err != nil {
			log.Warn("marshal system llm run metadata failed; recording without it", "job", c.Job, "error", err)
		} else {
			row.MetadataJSON = string(b)
		}
	}

	if err := r.store.Record(ctx, row); err != nil {
		log.Warn("record system llm run failed", "job", c.Job, "org", c.OrgID, "model", c.Model, "error", err)
	}
}
