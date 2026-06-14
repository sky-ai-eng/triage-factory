package ai

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:embed prompts/batch-prioritize.txt
var batchPrioritizePrompt string

//go:embed prompts/envelope.txt
var EnvelopeTemplate string

//go:embed prompts/gh-tools.txt
var GHToolsTemplate string

//go:embed prompts/jira-tools.txt
var JiraToolsTemplate string

// PR review is a three-step blueprint: a security pass and a
// correctness pass each write findings to _scratch, then a cheap aggregator
// posts and submits the review. See ShippedBlueprints.
//
//go:embed prompts/pr-review-security.txt
var PRReviewSecurityPromptTemplate string

//go:embed prompts/pr-review-correctness.txt
var PRReviewCorrectnessPromptTemplate string

//go:embed prompts/pr-review-aggregate.txt
var PRReviewAggregatePromptTemplate string

//go:embed prompts/jira-implement.txt
var JiraImplementPromptTemplate string

//go:embed prompts/conflict-resolution.txt
var ConflictResolutionPromptTemplate string

//go:embed prompts/repo-profile.txt
var RepoProfilePrompt string

//go:embed prompts/ci-fix.txt
var CIFixPromptTemplate string

//go:embed prompts/fix-review-feedback.txt
var FixReviewFeedbackPromptTemplate string

//go:embed prompts/ticket-spec.txt
var TicketSpecPromptTemplate string

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

// scoringModel is always haiku — fast and cheap, plenty capable for
// summarization and priority scoring. The user's model preference
// is reserved for heavier features like delegation.
const scoringModel = "haiku"

// ScoreTasks runs the AI scoring pipeline on a set of tasks.
// It batches into chunks of batchSize and runs them in parallel.
// The returned skippedTasks is the exact count of task inputs that were
// in failed batches — computed per-batch rather than inferred from
// failedBatches * batchSize so the final partial batch doesn't inflate
// the count, and so the number stays correct if batchSize changes.
// Failures are non-fatal: the function still returns whatever scores
// succeeded, and the caller surfaces skippedTasks as a warning toast.
func ScoreTasks(ctx context.Context, database *sql.DB, entities db.EntityStore, orgID string, tasks []domain.Task, secrets agentproc.SecretsReader) (scores []TaskScore, skippedTasks int, err error) {
	if len(tasks) == 0 {
		return nil, 0, nil
	}

	// Batch-load descriptions from the dedicated entities.description column
	// (not snapshot_json — description is bulk text, kept outside diff scope).
	// Failures degrade to title-only context rather than aborting scoring.
	entityIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		entityIDs = append(entityIDs, t.EntityID)
	}
	descriptions := map[string]string{}
	if entities != nil {
		// The scorer is a singleton background goroutine triggered
		// by event-bus sentinels — no JWT-claims context. Route the
		// bulk description read through the admin pool variant
		// (SKY-296) so Postgres multi-mode doesn't degrade every
		// scored task to title-only context under RLS.
		if descs, err := entities.DescriptionsSystem(ctx, orgID, entityIDs); err != nil {
			log.Printf("[ai] warning: failed to load entity descriptions for scoring: %v", err)
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
			scores, err := scoreBatch(ctx, b, orgID, secrets)
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
	for i, r := range results {
		if r.err != nil {
			log.Printf("[ai] batch %d/%d failed (%d tasks skipped): %v", i+1, len(batches), len(batches[i]), r.err)
			skipped += len(batches[i])
			continue
		}
		allScores = append(allScores, r.scores...)
	}

	return allScores, skipped, nil
}

func scoreBatch(ctx context.Context, tasks []TaskInput, orgID string, secrets agentproc.SecretsReader) ([]TaskScore, error) {
	tasksJSON, err := json.Marshal(tasks)
	if err != nil {
		return nil, fmt.Errorf("marshal tasks: %w", err)
	}

	prompt := fmt.Sprintf(batchPrioritizePrompt, string(tasksJSON))

	// Run through the shared agent runtime. The terminal `result` event
	// populates outcome.Result.Result with the agent's final response —
	// same string the old `claude --output-format json` envelope's
	// `.result` field carried, so the post-parse logic below is
	// unchanged. NoopSink discards per-message stream events; this path
	// doesn't persist a transcript. ctx propagates from the Runner's
	// stop channel so server shutdown SIGKILLs in-flight scoring calls
	// instead of waiting for the model to time out.
	outcome, err := agentproc.Run(ctx, agentproc.RunOptions{
		Model:   scoringModel,
		Message: prompt,
		TraceID: "scorer-batch",
		OrgID:   orgID,
		Secrets: secrets,
	}, agentproc.NoopSink{})
	if err != nil {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		return nil, fmt.Errorf("scorer agent failed: %w, stderr: %s", err, stderr)
	}
	if outcome == nil || outcome.Result == nil {
		return nil, fmt.Errorf("scorer agent: no terminal result event")
	}

	raw := []byte(outcome.Result.Result)
	// The result might contain markdown fences despite the prompt — strip them
	raw = StripCodeFences(raw)

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
