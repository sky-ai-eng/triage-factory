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
	"github.com/sky-ai-eng/todo-triage/internal/ai"
	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
	ghclient "github.com/sky-ai-eng/todo-triage/internal/github"
	"github.com/sky-ai-eng/todo-triage/internal/worktree"
	"github.com/sky-ai-eng/todo-triage/pkg/websocket"
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

// runConfig holds everything the generic agent runner needs.
type runConfig struct {
	scope    string // what the agent is scoped to (repo, PR, issue)
	toolsRef string // tool documentation to inject
	wtPath   string // worktree path (empty = no working directory)
	hasWT    bool   // whether a worktree was created (controls cleanup)
}

// Delegate kicks off an async agent run for any task type.
// Routes to the appropriate worktree setup based on task source.
func (s *Spawner) Delegate(task domain.Task, explicitPromptID string) (string, error) {
	s.mu.Lock()
	ghClient := s.ghClient
	model := s.model
	s.mu.Unlock()

	// Resolve prompt
	promptID, mission, err := s.resolvePrompt(task, explicitPromptID)
	if err != nil {
		return "", err
	}
	if err := db.IncrementPromptUsage(s.database, promptID); err != nil {
		log.Printf("[delegate] warning: failed to increment usage for prompt %s: %v", promptID, err)
	}

	runID := uuid.New().String()
	if err := db.CreateAgentRun(s.database, domain.AgentRun{
		ID:       runID,
		TaskID:   task.ID,
		PromptID: promptID,
		Status:   "initializing",
		Model:    model,
	}); err != nil {
		return "", fmt.Errorf("create agent run: %w", err)
	}
	s.broadcastRunUpdate(runID, "initializing")

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[runID] = cancel
	s.mu.Unlock()

	go func() {
		startTime := time.Now()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, runID)
			s.mu.Unlock()
			cancel()
		}()

		// Phase 1: set up worktree + build config based on task source
		var cfg runConfig
		var setupErr error

		switch task.Source {
		case "github":
			cfg, setupErr = s.setupGitHub(ctx, runID, task, ghClient)
		case "jira":
			cfg, setupErr = s.setupJira(ctx, runID, task, ghClient)
		default:
			setupErr = fmt.Errorf("unsupported task source: %s", task.Source)
		}

		if setupErr != nil {
			if ctx.Err() != nil {
				s.handleCancelled(runID, startTime, cfg.hasWT)
				return
			}
			s.failRun(runID, setupErr.Error())
			return
		}

		// Phase 2: run the agent
		s.runAgent(ctx, runID, task, mission, cfg, startTime, model)
	}()

	return runID, nil
}

// DelegatePR is a convenience wrapper that calls Delegate for backward compatibility.
func (s *Spawner) DelegatePR(task domain.Task, explicitPromptID string) (string, error) {
	return s.Delegate(task, explicitPromptID)
}

// setupGitHub prepares a worktree for a GitHub PR task.
func (s *Spawner) setupGitHub(ctx context.Context, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	if ghClient == nil {
		return runConfig{}, fmt.Errorf("GitHub credentials not configured")
	}

	owner, repo := parseOwnerRepo(task.Repo)
	if owner == "" || repo == "" {
		return runConfig{}, fmt.Errorf("cannot parse owner/repo from task.Repo: %q", task.Repo)
	}

	prNumber := 0
	if idx := strings.LastIndex(task.SourceID, "#"); idx >= 0 {
		fmt.Sscanf(task.SourceID[idx+1:], "%d", &prNumber)
	}
	if prNumber == 0 {
		return runConfig{}, fmt.Errorf("invalid PR number from task.SourceID: %q", task.SourceID)
	}

	s.updateStatus(runID, "fetching")
	pr, err := ghClient.GetPR(owner, repo, prNumber, false)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to fetch PR: %w", err)
	}

	s.updateStatus(runID, "cloning")
	wtPath, err := worktree.CreateForPR(ctx, owner, repo, pr.CloneURL, pr.HeadRef, prNumber, runID)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
	}

	if _, err := s.database.Exec(`UPDATE agent_runs SET worktree_path = ? WHERE id = ?`, wtPath, runID); err != nil {
		log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
	}

	return runConfig{
		scope:    fmt.Sprintf("Repository: %s/%s\nPR: #%d\nBranch: %s", owner, repo, prNumber, pr.HeadRef),
		toolsRef: ai.GHToolsTemplate,
		wtPath:   wtPath,
		hasWT:    true,
	}, nil
}

// setupJira prepares a worktree (if applicable) for a Jira task.
func (s *Spawner) setupJira(ctx context.Context, runID string, task domain.Task, ghClient *ghclient.Client) (runConfig, error) {
	// Look up matched repos from the task's scoring results
	matchedRepos, err := db.GetTaskMatchedRepos(s.database, task.ID)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to look up matched repos: %w", err)
	}

	switch len(matchedRepos) {
	case 0:
		// No repo match — pure Jira task, no worktree
		log.Printf("[delegate] Jira task %s: no matched repo, running without worktree", task.SourceID)
		return runConfig{
			scope:    fmt.Sprintf("Jira issue: %s", task.SourceID),
			toolsRef: ai.JiraToolsTemplate,
		}, nil

	case 1:
		// Single repo match — clone and create feature branch
		repoID := matchedRepos[0]
		profile, err := db.GetRepoProfile(s.database, repoID)
		if err != nil || profile == nil {
			return runConfig{}, fmt.Errorf("failed to load repo profile for %s: %v", repoID, err)
		}
		if profile.CloneURL == "" {
			return runConfig{}, fmt.Errorf("repo %s has no clone URL — try re-profiling", repoID)
		}

		s.updateStatus(runID, "cloning")
		baseBranch := profile.BaseBranch
		if baseBranch == "" {
			baseBranch = profile.DefaultBranch
		}
		featureBranch := "feature/" + task.SourceID

		wtPath, err := worktree.CreateForBranch(ctx, profile.Owner, profile.Repo, profile.CloneURL, baseBranch, featureBranch, runID)
		if err != nil {
			return runConfig{}, fmt.Errorf("failed to create worktree: %w", err)
		}

		if _, err := s.database.Exec(`UPDATE agent_runs SET worktree_path = ? WHERE id = ?`, wtPath, runID); err != nil {
			log.Printf("[delegate] warning: failed to update worktree path for run %s: %v", runID, err)
		}

		// Agent gets both GH and Jira tools when it has a repo (may need to create PRs)
		return runConfig{
			scope:    fmt.Sprintf("Repository: %s\nJira issue: %s\nBranch: %s", repoID, task.SourceID, featureBranch),
			toolsRef: ai.GHToolsTemplate + "\n\n" + ai.JiraToolsTemplate,
			wtPath:   wtPath,
			hasWT:    true,
		}, nil

	default:
		// Multiple matches — ambiguous, block for now
		return runConfig{}, fmt.Errorf("jira task %s matched %d repos (%s) — cannot determine which to clone",
			task.SourceID, len(matchedRepos), strings.Join(matchedRepos, ", "))
	}
}

// runAgent is the generic agent execution loop. Works for any task type.
func (s *Spawner) runAgent(ctx context.Context, runID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model string) {
	if cfg.hasWT {
		// Best-effort cleanup on return; the worktree ID is unique per run
		// so a failed remove just leaves a dangling directory under _worktrees.
		defer func() { _ = worktree.Remove(runID) }()
	}

	// Determine the cwd for the child claude. For tasks without a repo (Jira no-match)
	// we spin up a throwaway dir so the child's session history lands in a predictable
	// disposable ~/.claude/projects entry instead of mixing into the parent binary's
	// own project dir.
	claudeCwd := cfg.wtPath
	if claudeCwd == "" {
		var err error
		claudeCwd, err = worktree.MakeRunCwd(runID)
		if err != nil {
			s.failRun(runID, "failed to create run cwd: "+err.Error())
			return
		}
		defer worktree.RemoveRunCwd(runID)
	}
	// Nuke the ghost ~/.claude/projects/<encoded-cwd> that claude auto-creates
	// for this cwd. Safety-railed to only touch entries under $TMPDIR.
	defer worktree.RemoveClaudeProjectDir(claudeCwd)

	selfBin, err := os.Executable()
	if err != nil {
		s.failRun(runID, "failed to resolve own binary path: "+err.Error())
		return
	}

	prompt := buildPrompt(mission, cfg.scope, cfg.toolsRef, selfBin)

	s.updateStatus(runID, "agent_starting")
	if ctx.Err() != nil {
		s.handleCancelled(runID, startTime, cfg.hasWT)
		return
	}

	args := []string{
		"-p", prompt,
		"--model", model,
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", fmt.Sprintf("Bash(%s exec *),Bash(git commit *),Bash(git add *),Bash(git push *),Bash(git merge *),Bash(git rebase *),Bash(git fetch *),Bash(git checkout *),Read,Write,Edit,Glob,Grep,WebSearch,WebFetch", selfBin),
		"--max-turns", "100",
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = claudeCwd
	cmd.Env = append(os.Environ(), "TODOTRIAGE_RUN_ID="+runID, "TODOTRIAGE_REVIEW_PREVIEW=1")
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
	log.Printf("[delegate] claude started (pid: %d, pgid: %d, cwd: %s)", cmd.Process.Pid, pgid, claudeCwd)

	go func() {
		<-ctx.Done()
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			log.Printf("[delegate] warning: failed to kill process group %d: %v", pgid, err)
		}
	}()

	s.updateStatus(runID, "running")

	stream := newStreamState()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		messages, completion := stream.parseLine(line, runID)

		for _, msg := range messages {
			id, err := db.InsertAgentMessage(s.database, *msg)
			if err != nil {
				log.Printf("[delegate] error storing message: %v", err)
				continue
			}
			msg.ID = int(id)
			s.broadcastMessage(runID, msg)
		}

		if completion != nil {
			resultLink, resultSummary := "", ""
			status := "completed"
			if completion.IsError {
				status = "failed"
			}
			if parsed := parseAgentResult(completion.Result); parsed != nil {
				resultLink = parsed.PrimaryLink()
				resultSummary = parsed.Summary
				if parsed.Status == "failed" {
					status = "failed"
				}
			}
			if err := db.CompleteAgentRun(s.database, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultLink, resultSummary); err != nil {
				log.Printf("[delegate] warning: failed to record completion for run %s: %v", runID, err)
			}

			if status == "completed" {
				if pendingReview, _ := db.PendingReviewByRunID(s.database, runID); pendingReview != nil {
					status = "pending_approval"
					if _, err := s.database.Exec(`UPDATE agent_runs SET status = ? WHERE id = ?`, status, runID); err != nil {
						log.Printf("[delegate] warning: failed to set pending_approval for run %s: %v", runID, err)
					}
				}
			}

			if status == "completed" {
				if _, err := s.database.Exec(`UPDATE tasks SET status = 'done' WHERE id = ?`, task.ID); err != nil {
					log.Printf("[delegate] warning: failed to update task %s to done: %v", task.ID, err)
				}
			}
			s.broadcastRunUpdate(runID, status)
			// We've already captured the result from stdout; just drain any
			// remaining subprocess state. Exit code is not load-bearing here.
			_ = cmd.Wait()
			return
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[delegate] scanner error for run %s: %v", runID, err)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			s.handleCancelled(runID, startTime, cfg.hasWT)
			return
		}
		stderr := stderrBuf.String()
		s.failRun(runID, fmt.Sprintf("claude exited with error: %v\nstderr: %s", err, stderr))
		return
	}

	if err := db.CompleteAgentRun(s.database, runID, "completed", 0, 0, 0, "unknown", "", ""); err != nil {
		log.Printf("[delegate] warning: failed to record fallback completion for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(runID, "completed")
}

// resolvePrompt finds the mission text for a task, either from an explicit ID or the default binding.
func (s *Spawner) resolvePrompt(task domain.Task, explicitPromptID string) (string, string, error) {
	if explicitPromptID != "" {
		p, err := db.GetPrompt(s.database, explicitPromptID)
		if err != nil {
			return "", "", fmt.Errorf("failed to load prompt %s: %w", explicitPromptID, err)
		}
		if p == nil {
			return "", "", fmt.Errorf("prompt %s not found", explicitPromptID)
		}
		return p.ID, p.Body, nil
	}

	if task.EventType != "" {
		p, err := db.FindDefaultPrompt(s.database, task.EventType)
		if err != nil {
			return "", "", fmt.Errorf("failed to look up default prompt for %s: %w", task.EventType, err)
		}
		if p != nil {
			return p.ID, p.Body, nil
		}
	}

	return "", "", fmt.Errorf("no prompt available for event type %q — configure one on the Prompts page", task.EventType)
}

func (s *Spawner) handleCancelled(runID string, startTime time.Time, hasWT bool) {
	elapsed := int(time.Since(startTime).Milliseconds())
	if err := db.CompleteAgentRun(s.database, runID, "cancelled", 0, elapsed, 0, "cancelled", "", "Cancelled by user"); err != nil {
		log.Printf("[delegate] warning: failed to record cancellation for run %s: %v", runID, err)
	}
	s.broadcastRunUpdate(runID, "cancelled")
	if hasWT {
		// Best-effort cleanup; same rationale as the defer in runAgent.
		_ = worktree.Remove(runID)
	}
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

	if _, err := db.InsertAgentMessage(s.database, domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Subtype: "text",
		Content: "Error: " + errMsg,
		IsError: true,
	}); err != nil {
		log.Printf("[delegate] warning: failed to record failure message for run %s: %v", runID, err)
	}

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
	Status  string         `json:"status"`
	Link    string         `json:"link"` // legacy — single URL
	Summary string         `json:"summary"`
	Links   map[string]any `json:"links"` // new — keyed URLs (pr_review, pr, jira_issues)
}

// PrimaryLink returns the most relevant URL from the result.
func (r *agentResult) PrimaryLink() string {
	if r.Link != "" {
		return r.Link
	}
	for _, key := range []string{"pr_review", "pr"} {
		if v, ok := r.Links[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if v, ok := r.Links["jira_issues"]; ok {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// parseAgentResult extracts the structured {status, link, summary} JSON from
// the agent's final message. Handles markdown fences, leading/trailing text.
func parseAgentResult(text string) *agentResult {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var result agentResult
	if json.Unmarshal([]byte(text), &result) == nil && result.Summary != "" {
		return &result
	}

	stripped := text
	if idx := strings.Index(stripped, "```"); idx >= 0 {
		stripped = stripped[idx+3:]
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

// buildPrompt composes: mission + envelope (scope, tools, completion contract).
func buildPrompt(mission, scope, toolsRef, binaryPath string) string {
	envelope := strings.NewReplacer(
		"{{SCOPE}}", scope,
		"{{TOOLS_REFERENCE}}", toolsRef,
	).Replace(ai.EnvelopeTemplate)

	body := strings.ReplaceAll(mission, "todotriage exec", binaryPath+" exec")
	full := body + "\n\n" + envelope
	return strings.ReplaceAll(full, "{{BINARY_PATH}}", binaryPath)
}

func parseOwnerRepo(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
