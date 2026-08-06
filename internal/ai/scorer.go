package ai

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// The three prompts below are this package's own: toolless system-job text for
// the batch prioritizer, closer to a SQL query than to agent instructions.
// They are the only prompts internal/ai embeds — agent-facing text lives in
// internal/agentprompt, seed content in internal/promptseed, and each other
// system job keeps its prompts next to its own consumer.

//go:embed prompts/batch-prioritize.txt
var batchPrioritizePrompt string

//go:embed prompts/batch-prioritize-system.txt
var batchPrioritizeSystemPrompt string

//go:embed prompts/batch-prioritize-user.txt
var batchPrioritizeUserPrompt string

const batchSize = 10

// TaskInput is the minimal info we send to the LLM for scoring.
type TaskInput struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Title           string `json:"title"`
	Description     string `json:"description,omitempty"` // Jira description or PR body, flattened + truncated
	EventType       string `json:"event_type,omitempty"`
	EntitySourceID  string `json:"entity_source_id,omitempty"` // e.g. "owner/repo#42"
	Severity        string `json:"severity,omitempty"`
	RelevanceReason string `json:"relevance_reason,omitempty"`
}

// descriptionMaxLen caps per-task description size sent to the LLM. Jira
// descriptions can be arbitrarily large; at ~1500 chars we get enough context
// for a useful summary without inflating the prompt budget on big batches.
const descriptionMaxLen = 1500

// TaskScore is what we get back from the LLM per task.
type TaskScore struct {
	ID                  string  `json:"id"`
	PriorityScore       float64 `json:"priority_score"`
	AutonomySuitability float64 `json:"autonomy_suitability"`
	PriorityReasoning   string  `json:"priority_reasoning"`
	Summary             string  `json:"summary"`
}

// SystemJobModel is the model the three headless background jobs (scorer,
// repo-profiler, project-classifier) run on: always haiku — fast and cheap,
// plenty capable for summarization, priority scoring, and classification. The
// user's model preference is reserved for heavier features like delegation.
// Shared here (rather than re-declared per package) so the three surfaces
// can't drift on Haiku version.
//
// This is the CLI model alias local mode passes to `claude -p --model`; the
// raw Messages API used by the multi-mode direct path doesn't accept it —
// see SystemJobModelDirect.
const SystemJobModel = "haiku"

// SystemJobModelDirect is the pinned model id the multi-mode direct-API
// path (internal/systemllm) uses in place of the "haiku" CLI alias. Must
// match a key in the pricing table (pricing.go) so cost accounting resolves.
const SystemJobModelDirect = "claude-haiku-4-5-20251001"

// batchScoreFn scores one batch of task inputs. It is the scorer's unit-test
// seam: the Runner holds one as a struct field (scoreFn), defaulted to a
// closure over the real scoreBatch that captures the recorder + system-job
// limiter, so tests can inject a stub without spawning an agent subprocess and
// without a package-level mutable var. Mirrors the repo-profiler's batchFn.
type batchScoreFn func(ctx context.Context, tasks []TaskInput, orgID string, secrets agentproc.SecretsReader) ([]TaskScore, error)

// llmResolveFunc is the RunOptions.LLMResolver shape a background LLM job
// carries — the brain-side llmcred adapter (llmcred.SystemEnvResolver) that
// mints short-lived STS creds for a role-mode Bedrock org and passes stored
// material through otherwise. nil in local mode (ambient) and in tests, where
// Run keeps its built-in raw-secret resolution.
type llmResolveFunc func(ctx context.Context, orgID string) (map[string]string, error)

// scoreTasks runs the AI scoring pipeline on a set of tasks for the Runner's
// org. It batches into chunks of batchSize and runs them in parallel via
// r.scoreFn (the injectable batch seam). The returned skippedTasks is the
// exact count of task inputs that were in failed batches — computed per-batch
// rather than inferred from failedBatches * batchSize so the final partial
// batch doesn't inflate the count, and so the number stays correct if
// batchSize changes. Failures are non-fatal: the method still returns whatever
// scores succeeded, and the caller surfaces skippedTasks as a warning toast.
func (r *Runner) scoreTasks(ctx context.Context, tasks []domain.Task) (scores []TaskScore, skippedTasks int, err error) {
	if len(tasks) == 0 {
		return nil, 0, nil
	}
	orgID := r.orgID

	// Batch-load descriptions from the dedicated entities.description column
	// (not snapshot_json — description is bulk text, kept outside diff scope).
	// Failures degrade to title-only context rather than aborting scoring.
	entityIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		entityIDs = append(entityIDs, t.EntityID)
	}
	descriptions := map[string]string{}
	if r.entities != nil {
		// The scorer is a singleton background goroutine triggered
		// by event-bus sentinels — no JWT-claims context. Route the
		// bulk description read through the admin pool variant
		// so Postgres multi-mode doesn't degrade every
		// scored task to title-only context under RLS.
		if descs, err := r.entities.DescriptionsSystem(ctx, orgID, entityIDs); err != nil {
			aiLog.Warn("load entity descriptions for scoring failed", "error", err)
		} else {
			descriptions = descs
		}
	}

	// Build inputs
	inputs := make([]TaskInput, len(tasks))
	for i, t := range tasks {
		desc := descriptions[t.EntityID]
		if desc != "" {
			desc = truncate(strings.TrimSpace(desc), descriptionMaxLen)
		}
		inputs[i] = TaskInput{
			ID:              t.ID,
			Source:          t.EntitySource,
			Title:           t.Title,
			Description:     desc,
			EventType:       t.EventType,
			EntitySourceID:  t.EntitySourceID,
			Severity:        t.Severity,
			RelevanceReason: t.RelevanceReason,
		}
	}

	// Chunk into batches
	var batches [][]TaskInput
	for i := 0; i < len(inputs); i += batchSize {
		end := i + batchSize
		if end > len(inputs) {
			end = len(inputs)
		}
		batches = append(batches, inputs[i:end])
	}

	// Run batches in parallel
	type batchResult struct {
		scores []TaskScore
		err    error
	}
	results := make([]batchResult, len(batches))
	var wg sync.WaitGroup

	for i, batch := range batches {
		wg.Add(1)
		go func(idx int, b []TaskInput) {
			defer wg.Done()
			scores, err := r.scoreFn(ctx, b, orgID, r.secrets)
			results[idx] = batchResult{scores, err}
		}(i, batch)
	}
	wg.Wait()

	// Collect results. Each failed batch's actual task count contributes to
	// skippedTasks — walking batches[i] directly so the final partial batch
	// doesn't get counted as a full batchSize and the number stays honest
	// if batchSize changes.
	var allScores []TaskScore
	skipped := 0
	for i, res := range results {
		if res.err != nil {
			// A provider-backoff skip (systemllm's circuit breaker) is an
			// anticipated, self-healing deferral, not a genuine failure —
			// log it quietly so a boot-time overload doesn't read as a wall
			// of errors.
			if systemllm.IsProviderBackoff(res.err) {
				aiLog.Info("scoring batch deferred; provider backing off, retrying next cycle", "batch", i+1, "batches", len(batches), "skipped", len(batches[i]))
			} else {
				aiLog.Warn("scoring batch failed; tasks skipped", "batch", i+1, "batches", len(batches), "skipped", len(batches[i]), "error", res.err)
			}
			skipped += len(batches[i])
			continue
		}
		allScores = append(allScores, res.scores...)
	}

	return allScores, skipped, nil
}

func scoreBatch(ctx context.Context, tasks []TaskInput, orgID string, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, recorder *systemllm.Recorder, limiter *syslimit.Limiter) ([]TaskScore, error) {
	tasksJSON, err := json.Marshal(tasks)
	if err != nil {
		return nil, fmt.Errorf("marshal tasks: %w", err)
	}

	// Bound concurrent background sandboxes across all orgs + jobs. A
	// cancelled ctx here returns the error into the per-batch skip path above
	// (the batch counts as skipped, its tasks reset to pending for retry). A
	// nil limiter is an unlimited no-op (used by tests).
	if err := limiter.Acquire(ctx); err != nil {
		return nil, err
	}
	defer limiter.Release()

	// Complete owns the mode branch: local mode runs the shared agent
	// runtime exactly as before (Message, the combined instructions+data
	// prompt); multi mode calls the org's configured Anthropic/Bedrock
	// provider directly, with the instructions as the system prompt and
	// just the task batch as the user turn. Either way the returned text is
	// the same shape the old `claude --output-format json` envelope's
	// `.result` field carried, so the post-parse logic below is unchanged.
	// ctx propagates from the Runner's stop channel so server shutdown
	// aborts in-flight scoring calls instead of waiting for the model to
	// time out. LLMResolver routes a role-mode Bedrock org through
	// internal/llmcred so the direct API call is signed with a freshly-minted
	// short-lived STS session credential (TFAC-616); nil for local/ambient.
	result, err := recorder.Complete(ctx, systemllm.CompleteOptions{
		OrgID:        orgID,
		Job:          systemllm.JobScorer,
		Message:      fmt.Sprintf(batchPrioritizePrompt, string(tasksJSON)),
		SystemPrompt: batchPrioritizeSystemPrompt,
		UserMessage:  fmt.Sprintf(batchPrioritizeUserPrompt, string(tasksJSON)),
		Model:        SystemJobModel,
		DirectModel:  SystemJobModelDirect,
		MaxTokens:    16384,
		Temperature:  0.1,
		TraceID:      "scorer-batch",
		Secrets:      secrets,
		LLMResolver:  llmResolve,
		Metadata:     map[string]any{"batch_size": len(tasks)},
	})
	if err != nil {
		return nil, fmt.Errorf("scorer agent failed: %w", err)
	}

	// The result might contain markdown fences despite the prompt — strip them
	raw := StripCodeFences([]byte(result.Text))

	var scores []TaskScore
	if err := json.Unmarshal(raw, &scores); err != nil {
		return nil, fmt.Errorf("parse response: %w, raw: %s", err, string(raw))
	}

	return scores, nil
}

// StripCodeFences removes markdown code fences from LLM output.
func StripCodeFences(b []byte) []byte {
	s := bytes.TrimSpace(b)
	// Strip ```json ... ``` or ``` ... ```
	if bytes.HasPrefix(s, []byte("```")) {
		if idx := bytes.Index(s[3:], []byte("\n")); idx >= 0 {
			s = s[3+idx+1:]
		}
		if idx := bytes.LastIndex(s, []byte("```")); idx >= 0 {
			s = s[:idx]
		}
	}
	return bytes.TrimSpace(s)
}

// truncate caps s at maxRunes codepoints. Rune-based (not byte-based) so we
// never cut a multi-byte UTF-8 sequence in half. Strict cap — the returned
// string contains at most maxRunes runes, with the last rune replaced by an
// ellipsis when truncation happens so the LLM can tell the content was cut
// rather than a genuinely short input.
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}
