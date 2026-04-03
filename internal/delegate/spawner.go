package delegate

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	ghClient *ghclient.Client
	wsHub    *websocket.Hub
	model    string

	mu       sync.Mutex
	procs    map[string]*os.Process // runID → running claude process
}

func NewSpawner(database *sql.DB, ghClient *ghclient.Client, wsHub *websocket.Hub, model string) *Spawner {
	return &Spawner{
		database: database,
		ghClient: ghClient,
		wsHub:    wsHub,
		model:    model,
		procs:    make(map[string]*os.Process),
	}
}

// Cancel kills a running agent process and marks the run as failed.
func (s *Spawner) Cancel(runID string) error {
	s.mu.Lock()
	proc, ok := s.procs[runID]
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("no running process for run %s", runID)
	}

	if err := proc.Kill(); err != nil {
		return fmt.Errorf("kill process: %w", err)
	}

	// The goroutine will handle cleanup (worktree, status update) when cmd.Wait() returns
	return nil
}

// DelegatePR kicks off an async PR review agent run.
// It creates the run record, sets up the worktree, spawns claude, and returns the run ID immediately.
func (s *Spawner) DelegatePR(task domain.Task) (string, error) {
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

	runID := uuid.New().String()

	// Create agent run record
	if err := db.CreateAgentRun(s.database, domain.AgentRun{
		ID:     runID,
		TaskID: task.ID,
		Status: "cloning",
		Model:  s.model,
	}); err != nil {
		return "", fmt.Errorf("create agent run: %w", err)
	}

	s.broadcastRunUpdate(runID, "cloning")

	// Run async
	go s.runPRReview(runID, task, owner, repo, prNumber)

	return runID, nil
}

func (s *Spawner) runPRReview(runID string, task domain.Task, owner, repo string, prNumber int) {
	// 1. Get PR details for clone URL and head SHA
	s.updateStatus(runID, "fetching")
	pr, err := s.ghClient.GetPR(owner, repo, prNumber, false)
	if err != nil {
		s.failRun(runID, "failed to fetch PR: "+err.Error())
		return
	}

	// 2. Create worktree
	s.updateStatus(runID, "worktree_created")
	wtPath, err := worktree.Create(owner, repo, pr.CloneURL, pr.HeadSHA, runID)
	if err != nil {
		s.failRun(runID, "failed to create worktree: "+err.Error())
		return
	}
	defer func() {
		worktree.Remove(runID)
	}()

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

	// 4. Build prompt with absolute binary path
	prompt := buildPRReviewPrompt(owner, repo, prNumber, selfBin)

	// 5. Spawn claude
	s.updateStatus(runID, "agent_starting")
	args := []string{
		"-p", prompt,
		"--model", s.model,
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", fmt.Sprintf("Bash(%s exec *),Read,Glob,Grep", selfBin),
		"--max-turns", "100",
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = wtPath

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

	log.Printf("[delegate] claude started (pid: %d, cwd: %s)", cmd.Process.Pid, wtPath)

	// Track process for cancellation
	s.mu.Lock()
	s.procs[runID] = cmd.Process
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.procs, runID)
		s.mu.Unlock()
	}()

	s.updateStatus(runID, "running")
	startTime := time.Now()

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
			// Parse structured result from the agent's final text
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
		stderr := stderrBuf.String()
		// Check if this was a cancellation (killed by us)
		if strings.Contains(err.Error(), "signal: killed") {
			elapsed := int(time.Since(startTime).Milliseconds())
			db.CompleteAgentRun(s.database, runID, "cancelled", 0, elapsed, 0, "cancelled", "", "Cancelled by user")
			s.broadcastRunUpdate(runID, "cancelled")
			return
		}
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

func buildPRReviewPrompt(owner, repo string, prNumber int, binaryPath string) string {
	r := strings.NewReplacer(
		"{{OWNER}}", owner,
		"{{REPO}}", repo,
		"{{PR_NUMBER}}", fmt.Sprintf("%d", prNumber),
		"todotinder exec", binaryPath+" exec",
	)
	return r.Replace(ai.PRReviewPromptTemplate)
}

func parseOwnerRepo(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
