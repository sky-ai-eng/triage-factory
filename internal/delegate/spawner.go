package delegate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-tinder/internal/ai"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
	"github.com/sky-ai-eng/todo-tinder/internal/worktree"
	"github.com/sky-ai-eng/todo-tinder/pkg/websocket"
)

// Spawner manages delegated agent runs.
type Spawner struct {
	database *sql.DB
	wsHub    *websocket.Hub

	mu       sync.Mutex
	ghClient *ghclient.Client
	model    string
	cancels  map[string]context.CancelFunc // runID → cancel the entire run
}

func NewSpawner(database *sql.DB, ghClient *ghclient.Client, wsHub *websocket.Hub, model string) *Spawner {
	return &Spawner{
		database: database,
		ghClient: ghClient,
		wsHub:    wsHub,
		model:    model,
		cancels:  make(map[string]context.CancelFunc),
	}
}

// UpdateCredentials hot-swaps the GitHub client and model without
// disrupting in-flight runs.
func (s *Spawner) UpdateCredentials(ghClient *ghclient.Client, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghClient = ghClient
	s.model = model
}

// Cancel aborts a run at any phase — clone, fetch, worktree setup, or agent execution.
// The goroutine handles cleanup (worktree removal, status update).
func (s *Spawner) Cancel(runID string) error {
	s.mu.Lock()
	cancel, ok := s.cancels[runID]
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active run %s", runID)
	}

	cancel()
	return nil
}

// DelegatePR kicks off an async PR review agent run.
// If explicitPromptID is non-empty, that prompt is used instead of the default lookup.
func (s *Spawner) DelegatePR(task domain.Task, explicitPromptID string) (string, error) {
	// Snapshot mutable config under lock
	s.mu.Lock()
	ghClient := s.ghClient
	model := s.model
	s.mu.Unlock()

	if ghClient == nil {
		return "", fmt.Errorf("GitHub credentials not configured")
	}

	// Parse owner/repo from task
	owner, repo := parseOwnerRepo(task.Repo)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("cannot parse owner/repo from task.Repo: %q", task.Repo)
	}
	prNumber := 0
	fmt.Sscanf(task.SourceID, "%d", &prNumber)
	if prNumber == 0 {
		return "", fmt.Errorf("invalid PR number from task.SourceID: %q", task.SourceID)
	}

	// Resolve which prompt to use:
	// 1. Explicit prompt_id from the frontend picker
	// 2. Default prompt for the task's event type
	// No fallback — if no prompt is found, fail loudly
	var promptID string
	var mission string
	if explicitPromptID != "" {
		p, err := db.GetPrompt(s.database, explicitPromptID)
		if err != nil {
			return "", fmt.Errorf("failed to load prompt %s: %w", explicitPromptID, err)
		}
		if p == nil {
			return "", fmt.Errorf("prompt %s not found", explicitPromptID)
		}
		promptID = p.ID
		mission = p.Body
	} else if task.EventType != "" {
		p, err := db.FindDefaultPrompt(s.database, task.EventType)
		if err != nil {
			return "", fmt.Errorf("failed to look up default prompt for %s: %w", task.EventType, err)
		}
		if p != nil {
			promptID = p.ID
			mission = p.Body
		}
	}
	if mission == "" {
		return "", fmt.Errorf("no prompt available for event type %q — configure one on the Prompts page", task.EventType)
	}
	db.IncrementPromptUsage(s.database, promptID)

	runID := uuid.New().String()

	// Create agent run record with prompt reference
	if err := db.CreateAgentRun(s.database, domain.AgentRun{
		ID:       runID,
		TaskID:   task.ID,
		PromptID: promptID,
		Status:   "cloning",
		Model:    model,
	}); err != nil {
		return "", fmt.Errorf("create agent run: %w", err)
	}

	s.broadcastRunUpdate(runID, "cloning")

	// Create cancellable context for the entire run lifecycle
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[runID] = cancel
	s.mu.Unlock()

	// Run async — pass snapshotted ghClient/model so credential changes don't affect in-flight runs
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.cancels, runID)
			s.mu.Unlock()
			cancel() // release context resources
		}()
		s.runPRReview(ctx, runID, task, owner, repo, prNumber, mission, ghClient, model)
	}()

	return runID, nil
}

func (s *Spawner) runPRReview(ctx context.Context, runID string, task domain.Task, owner, repo string, prNumber int, mission string, ghClient *ghclient.Client, model string) {
	startTime := time.Now()

	// Helper: check if we were cancelled and handle cleanup
	cancelled := func() bool {
		if ctx.Err() != nil {
			elapsed := int(time.Since(startTime).Milliseconds())
			db.CompleteAgentRun(s.database, runID, "cancelled", 0, elapsed, 0, "cancelled", "", "Cancelled by user")
			s.broadcastRunUpdate(runID, "cancelled")
			worktree.Remove(runID) // clean up any partial worktree
			return true
		}
		return false
	}

	// 1. Get PR details for clone URL and head SHA
	s.updateStatus(runID, "fetching")
	pr, err := ghClient.GetPR(owner, repo, prNumber, false)
	if err != nil {
		if cancelled() {
			return
		}
		s.failRun(runID, "failed to fetch PR: "+err.Error())
		return
	}

	// 2. Create worktree
	s.updateStatus(runID, "cloning")
	wtPath, err := worktree.Create(ctx, owner, repo, pr.CloneURL, pr.HeadSHA, prNumber, runID)
	if err != nil {
		if cancelled() {
			return
		}
		s.failRun(runID, "failed to create worktree: "+err.Error())
		return
	}
	s.updateStatus(runID, "worktree_created")
	defer worktree.Remove(runID)

	// Update run with worktree path
	if _, err := s.database.Exec(`UPDATE agent_runs SET worktree_path = ? WHERE id = ?`, wtPath, runID); err != nil {
		log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
	}

	// 3. Resolve our own binary path so the agent can call todotinder exec
	selfBin, err := os.Executable()
	if err != nil {
		s.failRun(runID, "failed to resolve own binary path: "+err.Error())
		return
	}

	// 4. Build prompt: envelope (tool guidance) + mission (what to do)
	prompt := buildPrompt(mission, owner, repo, prNumber, selfBin)

	// 5. Spawn claude in its own process group so we can kill the entire tree
	s.updateStatus(runID, "agent_starting")
	if cancelled() {
		return
	}

	args := []string{
		"-p", prompt,
		"--model", model,
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", fmt.Sprintf("Bash(%s exec *),Read,Glob,Grep,WebSearch,WebFetch", selfBin),
		"--max-turns", "100",
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = wtPath
	cmd.Env = append(os.Environ(), "TODOTINDER_RUN_ID="+runID, "TODOTINDER_REVIEW_PREVIEW=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.failRun(runID, "failed to create stdout pipe: "+err.Error())
		return
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		s.failRun(runID, "failed to start claude: "+err.Error())
		return
	}

	pgid := cmd.Process.Pid
	log.Printf("[delegate] claude started (pid: %d, pgid: %d, cwd: %s)", cmd.Process.Pid, pgid, wtPath)

	// Watch for context cancellation and kill the entire process group
	go func() {
		<-ctx.Done()
		// Kill the entire process group (negative PID = group)
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			log.Printf("[delegate] warning: failed to kill process group %d: %v", pgid, err)
		}
	}()

	s.updateStatus(runID, "running")

	// 6. Parse NDJSON stream
	stream := newStreamState()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer for large lines

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		messages, completion := stream.parseLine(line, runID)

		// Store and broadcast each completed message
		for _, msg := range messages {
			id, err := db.InsertAgentMessage(s.database, *msg)
			if err != nil {
				log.Printf("[delegate] error storing message: %v", err)
				continue
			}
			msg.ID = int(id)
			s.broadcastMessage(runID, msg)
		}

		// Handle completion
		if completion != nil {
			resultLink, resultSummary := "", ""
			status := "completed"
			if completion.IsError {
				status = "failed"
			}
			if parsed := parseAgentResult(completion.Result); parsed != nil {
				resultLink = parsed.Link
				resultSummary = parsed.Summary
				if parsed.Status == "failed" {
					status = "failed"
				}
			}
			db.CompleteAgentRun(s.database, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultLink, resultSummary)
			if status == "completed" {
				if _, err := s.database.Exec(`UPDATE tasks SET status = 'done' WHERE id = ?`, task.ID); err != nil {
					log.Printf("[delegate] warning: failed to update task %s to done: %v", task.ID, err)
				}
			}
			s.broadcastRunUpdate(runID, status)
			cmd.Wait()
			return
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[delegate] scanner error for run %s: %v", runID, err)
	}

	// Wait for process to exit
	if err := cmd.Wait(); err != nil {
		if cancelled() {
			return
		}
		stderr := stderrBuf.String()
		s.failRun(runID, fmt.Sprintf("claude exited with error: %v\nstderr: %s", err, stderr))
		return
	}

	// If we got here without a result line, mark as completed
	db.CompleteAgentRun(s.database, runID, "completed", 0, 0, 0, "unknown", "", "")
	s.broadcastRunUpdate(runID, "completed")
}

func (s *Spawner) updateStatus(runID, status string) {
	if _, err := s.database.Exec(`UPDATE agent_runs SET status = ? WHERE id = ?`, status, runID); err != nil {
		log.Printf("[delegate] warning: failed to update status for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(runID, status)
}

func (s *Spawner) failRun(runID, errMsg string) {
	log.Printf("[delegate] run %s failed: %s", runID, errMsg)
	if _, err := s.database.Exec(`UPDATE agent_runs SET status = 'failed' WHERE id = ?`, runID); err != nil {
		log.Printf("[delegate] warning: failed to mark run %s as failed: %v", runID, err)
	}

	// Store error as a message
	db.InsertAgentMessage(s.database, domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Subtype: "text",
		Content: "Error: " + errMsg,
		IsError: true,
	})

	s.broadcastRunUpdate(runID, "failed")
}

func (s *Spawner) broadcastRunUpdate(runID, status string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "agent_run_update",
		RunID: runID,
		Data:  map[string]string{"status": status},
	})
}

func (s *Spawner) broadcastMessage(runID string, msg *domain.AgentMessage) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "agent_message",
		RunID: runID,
		Data:  msg,
	})
}

type agentResult struct {
	Status  string `json:"status"`
	Link    string `json:"link"`
	Summary string `json:"summary"`
}

// parseAgentResult extracts the structured {status, link, summary} JSON from
// the agent's final message. Handles markdown fences, leading/trailing text.
func parseAgentResult(text string) *agentResult {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Try direct parse first
	var result agentResult
	if json.Unmarshal([]byte(text), &result) == nil && result.Summary != "" {
		return &result
	}

	// Strip markdown fences
	stripped := text
	if idx := strings.Index(stripped, "```"); idx >= 0 {
		stripped = stripped[idx+3:]
		// Skip optional language tag (e.g. "json")
		if nl := strings.Index(stripped, "\n"); nl >= 0 {
			stripped = stripped[nl+1:]
		}
		if end := strings.LastIndex(stripped, "```"); end >= 0 {
			stripped = stripped[:end]
		}
		stripped = strings.TrimSpace(stripped)
		if json.Unmarshal([]byte(stripped), &result) == nil && result.Summary != "" {
			return &result
		}
	}

	// Try to find a JSON object anywhere in the text
	if start := strings.Index(text, "{"); start >= 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			candidate := text[start : end+1]
			if json.Unmarshal([]byte(candidate), &result) == nil && result.Summary != "" {
				return &result
			}
		}
	}

	return nil
}

// buildPrompt composes the system envelope + mission body, with variable substitution.
// The envelope provides tool guidance, repo scoping, and completion format.
// The mission is the user/system prompt that describes what the agent should do.
func buildPrompt(mission, owner, repo string, prNumber int, binaryPath string) string {
	r := strings.NewReplacer(
		"{{OWNER}}", owner,
		"{{REPO}}", repo,
		"{{PR_NUMBER}}", fmt.Sprintf("%d", prNumber),
		"todotinder exec", binaryPath+" exec",
	)
	envelope := r.Replace(ai.EnvelopeTemplate)
	body := r.Replace(mission)
	return body + "\n\n" + envelope
}

func parseOwnerRepo(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
