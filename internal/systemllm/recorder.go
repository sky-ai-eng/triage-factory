// Package systemllm records per-call cost + token accounting for the calls TF
// makes on its own behalf — the headless jobs (scorer, repo-profiler) and the
// model-availability probe — into the system_llm_runs table. It sits one
// layer above agentproc + db so the runtime stays storage-agnostic
// (agentproc takes a SecretsReader rather than importing db, by deliberate
// convention) while the recording logic — which needs both the Run
// outcome/usage and the db store — lives in one place instead of being
// duplicated at each call site.
//
// The two jobs run their whole completion through here (Complete). The probe
// makes its own inference call, because the model it must send is the exact id
// under test rather than the one this package pins, and comes back only for
// RecordDirect — the ledger row is the shared part.
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

// recordTimeout bounds the detached accounting insert (see Record). Short
// enough that a wedged DB can't stall server teardown, generous enough for a
// local SQLite insert or an admin-pool Postgres round-trip.
const recordTimeout = 5 * time.Second

// Job discriminators for the system_llm_runs.job column.
const (
	JobScorer       = "scorer"
	JobRepoProfiler = "repo_profiler"
	// JobProbe is a model-availability probe: one minimal request TF spends to
	// learn whether an org's credentials can invoke a given model. It is not a
	// background job like the two above — a person pressed a button — but it
	// is TF-initiated spend against the org's account, so it belongs in the
	// same ledger and rolls up under system_overhead like every other call TF
	// makes for itself. A probe that stayed off the ledger would be the one
	// charge on the bill with nothing in the product to explain it.
	JobProbe = "probe"
)

// Recorder writes one system_llm_runs row per agentproc.Run call. A nil
// Recorder (or one built with a nil store) is a safe no-op, so callers in
// tests / local paths that never wire a store don't have to nil-check.
//
// breaker is the multi-mode direct-API path's shared circuit breaker (see
// breaker.go) — one Recorder is constructed once per process and handed to
// both system jobs (scorer, repo-profiler), so it doubles
// as the natural shared home for state that needs to be visible across both
// of them. The availability probe holds the same Recorder for the ledger row
// and deliberately does not consult that breaker: a person pressed a button,
// and answering them with a skip a cooldown decided on would look like the
// button did nothing.
type Recorder struct {
	store   db.SystemLLMRunStore
	breaker *providerBreaker
}

// NewRecorder wraps the store. Pass the bundle's SystemLLMRuns store.
func NewRecorder(store db.SystemLLMRunStore) *Recorder {
	return &Recorder{store: store, breaker: newProviderBreaker()}
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
		// TraceID (TFAC-579) is the idempotency key backing the store's
		// duplicate-insert no-op. Sourced from the subprocess's own Claude
		// Code session id, NOT the caller-supplied Call.Job-shaped
		// TraceID agentproc.Options takes (that one's a per-job-type
		// label like "scorer-batch", reused across every invocation — it
		// can't identify a single call). Empty when the subprocess never
		// got far enough to mint a session; the store treats that as "no
		// idempotency check" rather than colliding every such row.
		TraceID: outcome.SessionID,
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
	r.insert(ctx, row, c)
}

// insert marshals c.Metadata onto row and inserts it, detached from the
// caller's ctx. Shared by Record (local-mode, agentproc.Outcome-shaped) and
// RecordDirect (multi-mode, direct-API-shaped) — everything upstream of the
// row differs between the two call paths, but landing it in the store does
// not.
func (r *Recorder) insert(ctx context.Context, row domain.SystemLLMRun, c Call) {
	if len(c.Metadata) > 0 {
		if b, err := json.Marshal(c.Metadata); err != nil {
			log.Warn("marshal system llm run metadata failed; recording without it", "job", c.Job, "error", err)
		} else {
			row.MetadataJSON = string(b)
		}
	}

	// Detach the insert from the caller's ctx. The run we're accounting for
	// has already finished, so its ctx serves no purpose here — and when that
	// ctx was cancelled (shutdown / per-run timeout) it's precisely the run
	// that completed in the cancellation window whose row we'd otherwise drop,
	// passing the dead ctx straight into a guaranteed-failing insert. Keep the
	// ctx's values (WithoutCancel) and impose a fresh short deadline so a
	// wedged DB still can't hang teardown.
	insertCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := r.store.Record(insertCtx, row); err != nil {
		log.Warn("record system llm run failed", "job", c.Job, "org", c.OrgID, "model", c.Model, "error", err)
	}
}

// RecordDirect builds a system_llm_runs row from a direct-API completion —
// the multi-mode sibling of Record. Where Record reads cost/duration/turns/
// tokens off an agentproc.Outcome + UsageSink (the subprocess shape), the
// caller here already has them, because a direct API call gives them up
// front rather than via a terminal stream event. Same never-errors/never-
// panics contract as Record.
//
// traceID is the API response's message id (empty on an error that never
// produced a response — same "no idempotency check" semantics as Record's
// empty-SessionID case). numTurns is always 1: these are single-turn,
// non-streaming completions.
func (r *Recorder) RecordDirect(ctx context.Context, c Call, traceID string, usage DirectUsage, costUSD float64, durationMs int, isError bool) {
	if r == nil || r.store == nil {
		return
	}
	row := domain.SystemLLMRun{
		OrgID:               c.OrgID,
		Job:                 c.Job,
		Model:               c.Model,
		StartedAt:           c.StartedAt,
		CompletedAt:         time.Now().UTC(),
		TraceID:             traceID,
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		TotalCostUSD:        costUSD,
		DurationMs:          durationMs,
		NumTurns:            1,
		IsError:             isError,
	}
	r.insert(ctx, row, c)
}

// DirectUsage carries token counts from a direct-API response's usage
// block, mirroring agentproc.UsageSink's fields so the two paths' cost math
// stays identical.
type DirectUsage struct {
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int
}
