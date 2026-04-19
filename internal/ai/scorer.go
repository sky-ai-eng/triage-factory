package ai

import (
	"bytes"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"

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

//go:embed prompts/pr-review.txt
var PRReviewPromptTemplate string

//go:embed prompts/jira-implement.txt
var JiraImplementPromptTemplate string

//go:embed prompts/conflict-resolution.txt
var ConflictResolutionPromptTemplate string

//go:embed prompts/repo-profile.txt
var RepoProfilePrompt string

//go:embed prompts/ci-fix.txt
var CIFixPromptTemplate string

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
	ID                  string   `json:"id"`
	PriorityScore       float64  `json:"priority_score"`
	AutonomySuitability float64  `json:"autonomy_suitability"`
	PriorityReasoning   string   `json:"priority_reasoning"`
	Summary             string   `json:"summary"`
	Repos               []string `json:"repos"`
}

// scoringModel is always haiku — fast and cheap, plenty capable for
// summarization and priority scoring. The user's model preference
// is reserved for heavier features like delegation.
const scoringModel = "haiku"

// ScoreTasks runs the AI scoring pipeline on a set of tasks.
// It batches into chunks of batchSize and runs them in parallel.
func ScoreTasks(database *sql.DB, tasks []domain.Task) ([]TaskScore, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	// Load repo profiles for context injection.
	repoContext := "(no repo profiles available)"
	if database != nil {
		profiles, err := db.GetRepoProfilesWithContent(database)
		if err != nil {
			log.Printf("[ai] error loading repo profiles: %v", err)
		} else {
			repoContext = formatRepoProfiles(profiles)
		}
	}

	// Batch-load descriptions from the dedicated entities.description column
	// (not snapshot_json — description is bulk text, kept outside diff scope).
	// Failures degrade to title-only context rather than aborting scoring.
	entityIDs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		entityIDs = append(entityIDs, t.EntityID)
	}
	descriptions := map[string]string{}
	if database != nil {
		if descs, err := db.GetEntityDescriptions(database, entityIDs); err != nil {
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
			scores, err := scoreBatch(b, repoContext)
			results[idx] = batchResult{scores, err}
		}(i, batch)
	}
	wg.Wait()

	// Collect results
	var allScores []TaskScore
	for i, r := range results {
		if r.err != nil {
			log.Printf("[ai] batch %d/%d failed: %v", i+1, len(batches), r.err)
			continue
		}
		allScores = append(allScores, r.scores...)
	}

	return allScores, nil
}

func scoreBatch(tasks []TaskInput, repoContext string) ([]TaskScore, error) {
	tasksJSON, err := json.Marshal(tasks)
	if err != nil {
		return nil, fmt.Errorf("marshal tasks: %w", err)
	}

	prompt := fmt.Sprintf(batchPrioritizePrompt, repoContext, string(tasksJSON))

	args := []string{
		"-p", prompt,
		"--model", scoringModel,
		"--output-format", "json",
	}

	cmd := exec.Command("claude", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude command failed: %w, stderr: %s", err, stderr.String())
	}

	// claude --output-format json wraps the response in a JSON object with a "result" field
	var envelope struct {
		Result string `json:"result"`
	}
	raw := stdout.Bytes()

	// Try parsing as envelope first (claude JSON output format)
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != "" {
		raw = []byte(envelope.Result)
	}

	// The result might contain markdown fences despite the prompt — strip them
	raw = StripCodeFences(raw)

	var scores []TaskScore
	if err := json.Unmarshal(raw, &scores); err != nil {
		return nil, fmt.Errorf("parse response: %w, raw: %s", err, string(raw))
	}

	return scores, nil
}

// formatRepoProfiles renders profiles as a compact text block for the prompt.
func formatRepoProfiles(profiles []domain.RepoProfile) string {
	if len(profiles) == 0 {
		return "(no repo profiles available)"
	}
	var sb strings.Builder
	for _, p := range profiles {
		fmt.Fprintf(&sb, "repo: %s\n%s\n\n", p.ID, p.ProfileText)
	}
	return strings.TrimSpace(sb.String())
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
