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

	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

//go:embed prompts/batch-prioritize.txt
var batchPrioritizePrompt string

//go:embed prompts/envelope.txt
var EnvelopeTemplate string

//go:embed prompts/pr-review.txt
var PRReviewPromptTemplate string

//go:embed prompts/repo-profile.txt
var RepoProfilePrompt string

const batchSize = 10

// TaskInput is the minimal info we send to the LLM for scoring.
type TaskInput struct {
	ID              string   `json:"id"`
	Source          string   `json:"source"`
	Title           string   `json:"title"`
	Description     string   `json:"description,omitempty"`
	Repo            string   `json:"repo,omitempty"`
	Author          string   `json:"author,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	Severity        string   `json:"severity,omitempty"`
	DiffSize        int      `json:"diff_size,omitempty"`
	FilesChanged    int      `json:"files_changed,omitempty"`
	CIStatus        string   `json:"ci_status,omitempty"`
	RelevanceReason string   `json:"relevance_reason,omitempty"`
}

// TaskScore is what we get back from the LLM per task.
type TaskScore struct {
	ID                string   `json:"id"`
	PriorityScore     float64  `json:"priority_score"`
	AgentConfidence   float64  `json:"agent_confidence"`
	PriorityReasoning string   `json:"priority_reasoning"`
	Summary           string   `json:"summary"`
	Repos             []string `json:"repos"`
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

	// Build inputs
	inputs := make([]TaskInput, len(tasks))
	for i, t := range tasks {
		inputs[i] = TaskInput{
			ID:              t.ID,
			Source:          t.Source,
			Title:           t.Title,
			Description:     truncate(t.Description, 500),
			Repo:            t.Repo,
			Author:          t.Author,
			Labels:          t.Labels,
			Severity:        t.Severity,
			DiffSize:        t.DiffAdditions + t.DiffDeletions,
			FilesChanged:    t.FilesChanged,
			CIStatus:        t.CIStatus,
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
		sb.WriteString(fmt.Sprintf("repo: %s\n%s\n\n", p.ID, p.ProfileText))
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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
