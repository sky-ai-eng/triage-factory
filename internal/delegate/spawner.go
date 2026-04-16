package delegate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
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
	owner    string // resolved GitHub owner (empty for no-repo Jira runs)
	repo     string // resolved GitHub repo (empty for no-repo Jira runs)
}

// Delegate kicks off an async agent run for any task type.
// Routes to the appropriate worktree setup based on task source.
func (s *Spawner) Delegate(task domain.Task, explicitPromptID string, triggerType string, triggerID string) (string, error) {
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

	if triggerType == "" {
		triggerType = "manual"
	}
	runID := uuid.New().String()
	if err := db.CreateAgentRun(s.database, domain.AgentRun{
		ID:          runID,
		TaskID:      task.ID,
		PromptID:    promptID,
		Status:      "initializing",
		Model:       model,
		TriggerType: triggerType,
		TriggerID:   triggerID,
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
			s.failRun(runID, task.ID, triggerType, setupErr.Error())
			return
		}

		// Phase 2: run the agent
		s.runAgent(ctx, runID, task, mission, cfg, startTime, model, triggerType)
	}()

	return runID, nil
}

// DelegatePR is a convenience wrapper that calls Delegate for backward compatibility.
func (s *Spawner) DelegatePR(task domain.Task, explicitPromptID string) (string, error) {
	return s.Delegate(task, explicitPromptID, "manual", "")
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
		owner:    owner,
		repo:     repo,
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
			owner:    profile.Owner,
			repo:     profile.Repo,
		}, nil

	default:
		// Multiple matches — ambiguous, block for now
		return runConfig{}, fmt.Errorf("jira task %s matched %d repos (%s) — cannot determine which to clone",
			task.SourceID, len(matchedRepos), strings.Join(matchedRepos, ", "))
	}
}

// runAgent is the generic agent execution loop. Works for any task type.
func (s *Spawner) runAgent(ctx context.Context, runID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model string, triggerType string) {
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
			s.failRun(runID, task.ID, triggerType, "failed to create run cwd: "+err.Error())
			return
		}
		defer worktree.RemoveRunCwd(runID)
	}
	// Nuke the ghost ~/.claude/projects/<encoded-cwd> that claude auto-creates
	// for this cwd. Safety-railed to only touch entries under $TMPDIR.
	defer worktree.RemoveClaudeProjectDir(claudeCwd)

	// Materialize any prior task memories into ./task_memory/ so the agent
	// sees what previous iterations on this task have already tried. The
	// directory is git-excluded by writeLocalExcludes (managedExcludePatterns
	// in internal/worktree/worktree.go) so nothing leaks into the PR.
	materializePriorMemories(s.database, claudeCwd, task.ID)

	selfBin, err := os.Executable()
	if err != nil {
		s.failRun(runID, task.ID, triggerType, "failed to resolve own binary path: "+err.Error())
		return
	}

	prompt := buildPrompt(mission, cfg.scope, cfg.toolsRef, selfBin, runID)

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
	cmd.Env = append(os.Environ(), "TRIAGE_FACTORY_RUN_ID="+runID, "TRIAGE_FACTORY_REVIEW_PREVIEW=1")
	// Set TRIAGE_FACTORY_REPO when the run has a resolved GitHub repo context
	// so gh subcommands can default to the right target without the agent
	// needing to pass --repo on every invocation. Left unset for Jira runs
	// with no matched repo; those commands either fall back to .git/config
	// (unlikely — no worktree) or hard-error, which is correct since they
	// shouldn't be touching GitHub.
	if cfg.owner != "" && cfg.repo != "" {
		cmd.Env = append(cmd.Env, "TRIAGE_FACTORY_REPO="+cfg.owner+"/"+cfg.repo)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.failRun(runID, task.ID, triggerType, "failed to create stdout pipe: "+err.Error())
		return
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		s.failRun(runID, task.ID, triggerType, "failed to start claude: "+err.Error())
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
	completion, streamErr := s.consumeClaudeStream(stdout, runID, stream)
	if streamErr != nil {
		log.Printf("[delegate] scanner error for run %s: %v", runID, streamErr)
	}

	if completion != nil {
		// Enforce the pre-complete task_memory write gate. If the agent
		// returned a completion JSON without writing ./task_memory/<runID>.md,
		// resume the session with a correction message (up to 2 retries).
		// Retries that produce new completions are merged into the totals
		// so cost/duration accounting reflects the full invocation, not
		// just the initial call.
		//
		// Pass model + repoEnv explicitly rather than letting the gate
		// read live spawner state, so a concurrent UpdateCredentials
		// can't silently switch models or drop repo context mid-run.
		repoEnv := ""
		if cfg.owner != "" && cfg.repo != "" {
			repoEnv = cfg.owner + "/" + cfg.repo
		}
		completion = s.runMemoryGate(ctx, runID, task.ID, claudeCwd, completion, stream.SessionID(), model, repoEnv)

		// Ingest the agent-written memory file (if present) or flag the
		// run as memory_missing. Either way the run still counts as
		// completed — we don't fail a run just because the agent skipped
		// the memory write, but we DO surface the gap.
		if memoryFileExists(claudeCwd, runID) {
			if err := ingestAgentMemory(s.database, claudeCwd, runID, task.ID); err != nil {
				log.Printf("[delegate] warning: failed to ingest memory file for run %s: %v", runID, err)
			}
		} else {
			log.Printf("[delegate] run %s: memory file missing after gate retries, flagging memory_missing", runID)
			if err := db.MarkAgentRunMemoryMissing(s.database, runID); err != nil {
				log.Printf("[delegate] warning: failed to mark memory_missing for run %s: %v", runID, err)
			}
		}

		resultLink, resultSummary := "", ""
		status := "completed"
		if completion.IsError {
			status = "failed"
		}
		if parsed := parseAgentResult(completion.Result); parsed != nil {
			resultLink = parsed.PrimaryLink()
			resultSummary = parsed.Summary
			switch parsed.Status {
			case "failed":
				status = "failed"
			case "task_unsolvable":
				status = "task_unsolvable"
			}
		}
		if err := db.CompleteAgentRun(s.database, runID, status, completion.CostUSD, completion.DurationMs, completion.NumTurns, completion.StopReason, resultLink, resultSummary); err != nil {
			log.Printf("[delegate] warning: failed to record completion for run %s: %v", runID, err)
		}

		s.updateBreakerCounter(task.ID, triggerType, status)

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

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			s.handleCancelled(runID, startTime, cfg.hasWT)
			return
		}
		stderr := stderrBuf.String()
		s.failRun(runID, task.ID, triggerType, fmt.Sprintf("claude exited with error: %v\nstderr: %s", err, stderr))
		return
	}

	s.failRun(runID, task.ID, triggerType, "claude exited cleanly without producing a result event")
}

// consumeClaudeStream scans NDJSON output from claude -p, persists each
// accumulated message via InsertAgentMessage, broadcasts them to UI
// subscribers, and returns the first `result` event seen as a
// *runCompletion. Shared between the initial agent invocation and the
// ResumeWithMessage helper so stream handling stays consistent across
// both entry points.
//
// Session id is persisted on agent_runs as soon as the `system/init`
// event surfaces it, not at stream close. Inline persistence means any
// mid-run consumer (a future concurrent gate, or a panic handler
// recovering from a crash) can read it from the database without
// waiting for the stream to complete. On resume the same stream still
// carries a fresh init event with the same session id, so writing it
// again is idempotent.
//
// Returns nil *runCompletion if the stream ended without a result event
// — the caller treats that as an involuntary failure and decides via
// cmd.Wait() whether to attribute the failure to cancellation or a
// real crash.
func (s *Spawner) consumeClaudeStream(stdout io.Reader, runID string, stream *streamState) (*runCompletion, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	sessionPersisted := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		messages, completion := stream.parseLine(line, runID)

		// Persist session id the first time it appears. Done inline so
		// mid-run consumers can read it from agent_runs without needing
		// the stream to have closed first.
		if !sessionPersisted {
			if sid := stream.SessionID(); sid != "" {
				if err := db.SetAgentRunSession(s.database, runID, sid); err != nil {
					log.Printf("[delegate] warning: failed to persist session_id for run %s: %v", runID, err)
				}
				sessionPersisted = true
			}
		}

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
			return completion, nil
		}
	}
	return nil, scanner.Err()
}

// ResumeOptions configures a ResumeWithMessage invocation. Callers that
// care about consistency with an earlier invocation should populate these
// explicitly — the fallbacks read live Spawner state and will race with
// UpdateCredentials if the user rotates auth mid-run.
type ResumeOptions struct {
	// Model overrides the live spawner model. **Always pass this** when
	// resuming within a single logical run (e.g. the memory-gate retry
	// loop) — read from the value you captured at run start, not from
	// s.model at resume time. If UpdateCredentials runs between the
	// initial invocation and a resume, the live spawner model may point
	// at a different model than the initial invocation ran under, which
	// would silently switch models mid-run.
	//
	// Empty falls back to the live spawner model, which is only the
	// right choice for callers that genuinely want "current spawner
	// state" (none exist today, but the door's open).
	Model string

	// RepoEnv, if non-empty, is passed to the resumed subprocess as
	// TRIAGE_FACTORY_REPO=<value>. Preserves the GitHub repo context that
	// the initial runAgent invocation set up for gh subcommands so
	// resumes don't lose the implicit --repo default. Format is
	// "owner/name" — composed by the caller from cfg.owner and cfg.repo.
	//
	// Left empty for Jira-no-match runs that never had repo context in
	// the first place.
	RepoEnv string
}

// ResumeOutcome bundles what ResumeWithMessage returns: the raw
// completion event from the resumed stream (nil if none was observed),
// the parsed agent result JSON (nil if the completion text didn't
// contain a parseable envelope), and captured stderr for diagnostics.
//
// Callers decide how to interpret a nil Completion — the memory-gate
// retry loop treats it as "retry again if attempts remain, else flag
// memory_missing," while a yield-resume flow might treat it as a
// session-level failure and surface an error.
type ResumeOutcome struct {
	Completion *runCompletion
	Result     *agentResult
	StderrText string
}

// ResumeWithMessage resumes a prior headless claude session with a new
// user message and streams the result through the same message-
// persistence path as the initial invocation. Used by the SKY-141
// task-memory write-gate retry loop, and designed to be reusable by
// SKY-139's yield-to-user flow once that ticket lands.
//
// Callers pass the sessionID captured during the initial run (read
// from agent_runs.session_id, populated by consumeClaudeStream), the
// cwd the original run used so the resumed subprocess sees the same
// worktree, and the user message to append to the conversation. The
// runID is reused so resumed messages append to the existing
// agent_messages stream — the UI sees one coherent conversation.
//
// This helper does NOT update agent_runs status. The caller manages
// lifecycle: the memory-gate retry loop keeps the run in its current
// state during retries and only finalizes once the gate passes or
// gives up. Mirroring the initial invocation's status updates here
// would produce double CompleteAgentRun writes with stale
// cost/duration fields overwriting the real totals.
func (s *Spawner) ResumeWithMessage(ctx context.Context, runID, sessionID, cwd, message string, opts ResumeOptions) (*ResumeOutcome, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("resume: missing session id")
	}
	if cwd == "" {
		return nil, fmt.Errorf("resume: missing cwd")
	}

	s.mu.Lock()
	model := s.model
	s.mu.Unlock()
	if opts.Model != "" {
		model = opts.Model
	}

	selfBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own binary path: %w", err)
	}

	args := []string{
		"-p", message,
		"--resume", sessionID,
		"--model", model,
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", fmt.Sprintf("Bash(%s exec *),Bash(git commit *),Bash(git add *),Bash(git push *),Bash(git merge *),Bash(git rebase *),Bash(git fetch *),Bash(git checkout *),Read,Write,Edit,Glob,Grep,WebSearch,WebFetch", selfBin),
		"--max-turns", "100",
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TRIAGE_FACTORY_RUN_ID="+runID, "TRIAGE_FACTORY_REVIEW_PREVIEW=1")
	// Preserve the initial run's GitHub repo context so gh subcommands
	// in the resumed session keep their implicit --repo default. Without
	// this, a resumed run on a GitHub task could suddenly fail any gh
	// invocation that relied on the env var set in runAgent.
	if opts.RepoEnv != "" {
		cmd.Env = append(cmd.Env, "TRIAGE_FACTORY_REPO="+opts.RepoEnv)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude resume: %w", err)
	}

	pgid := cmd.Process.Pid
	go func() {
		<-ctx.Done()
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			// Best-effort; subprocess may have already exited
			_ = err
		}
	}()

	stream := newStreamState()
	completion, streamErr := s.consumeClaudeStream(stdout, runID, stream)

	waitErr := cmd.Wait()

	outcome := &ResumeOutcome{
		Completion: completion,
		StderrText: stderrBuf.String(),
	}
	if completion != nil {
		outcome.Result = parseAgentResult(completion.Result)
	}

	// A stream error with no completion means the subprocess produced
	// malformed output or died mid-stream. Surface it to the caller so
	// the gate can decide whether to retry or give up.
	if streamErr != nil && completion == nil {
		return outcome, fmt.Errorf("resume stream: %w", streamErr)
	}

	// A wait error without a captured completion is an involuntary
	// failure — the subprocess exited without sending a result event.
	// Either cancellation (via ctx) or a genuine crash.
	if waitErr != nil && completion == nil {
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("claude resume failed: %w (stderr: %s)", waitErr, stderrBuf.String())
	}

	return outcome, nil
}

// maxMemoryRetries is the hard cap on how many times the write-gate
// will resume a run to ask the agent to write its memory file. Chosen
// in the SKY-141 design: 0 retries is too strict (one missed write
// shouldn't discard work), 3+ is overkill (if the agent ignored the
// first correction, a third attempt is almost never the one that
// works). Not a config knob because no one needs to tune it per-run.
const maxMemoryRetries = 2

// memoryFileExists returns true iff the agent wrote ./task_memory/<runID>.md
// during the run. Used by the write-gate both before retrying (is another
// attempt needed?) and after (did the retry succeed?).
func memoryFileExists(cwd, runID string) bool {
	_, err := os.Stat(filepath.Join(cwd, "task_memory", runID+".md"))
	return err == nil
}

// ingestAgentMemory reads an agent-written memory file from the worktree
// and saves it as a task_memory row. Called after the write-gate has
// verified the file is present. Returns an error only on read/DB failure —
// "file missing" is not an error here because the caller already checked.
func ingestAgentMemory(database *sql.DB, cwd, runID, taskID string) error {
	path := filepath.Join(cwd, "task_memory", runID+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read memory file %s: %w", path, err)
	}
	mem := domain.TaskMemory{
		ID:        uuid.New().String(),
		TaskID:    taskID,
		RunID:     runID,
		Content:   string(data),
		Source:    "agent",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.SaveTaskMemory(database, mem); err != nil {
		return fmt.Errorf("save memory row: %w", err)
	}
	return nil
}

// runMemoryGate enforces the pre-complete task_memory file requirement.
//
// If the agent wrote ./task_memory/<runID>.md during its initial
// invocation, returns the original completion unchanged. Otherwise
// resumes the session (up to maxMemoryRetries times) with a correction
// message and re-checks after each attempt. Completions from resumed
// sessions are merged into the returned completion so cost/duration/
// num_turns accounting reflects the full span of the run.
//
// The gate does not touch agent_runs status — that remains the caller's
// responsibility. Side effects: (a) spawns resume subprocesses via
// ResumeWithMessage, whose messages land in agent_messages via
// consumeClaudeStream's persistence, (b) logs progress for operator
// diagnosis.
//
// Model and repoEnv are passed in rather than read from live spawner
// state so the gate's retries use the same model and repo context as
// the initial invocation. If we read s.model at resume time, a
// concurrent UpdateCredentials could silently switch models mid-run.
//
// If no session id is available (shouldn't happen in practice because
// consumeClaudeStream persists the init event, but defensive), the gate
// logs and returns without retrying. The caller will see a missing
// memory file and flag memory_missing.
func (s *Spawner) runMemoryGate(
	ctx context.Context,
	runID, taskID, cwd string,
	initial *runCompletion,
	sessionID, model, repoEnv string,
) *runCompletion {
	if memoryFileExists(cwd, runID) {
		return initial
	}

	if sessionID == "" {
		log.Printf("[delegate] run %s: memory file missing and no session id available — cannot gate-retry", runID)
		return initial
	}

	resumeOpts := ResumeOptions{Model: model, RepoEnv: repoEnv}

	current := initial
	for attempt := 1; attempt <= maxMemoryRetries; attempt++ {
		log.Printf("[delegate] run %s: memory file missing after attempt %d, resuming", runID, attempt-1)
		msg := fmt.Sprintf(
			"You returned a completion JSON but did not write your memory file to ./task_memory/%s.md. "+
				"Write it now — one paragraph of what you did, one of why, one of what to try next "+
				"if this recurs — then return your completion JSON again.",
			runID,
		)
		outcome, err := s.ResumeWithMessage(ctx, runID, sessionID, cwd, msg, resumeOpts)
		if err != nil {
			log.Printf("[delegate] run %s: resume attempt %d failed: %v", runID, attempt, err)
			// Give up on further retries — the caller will mark
			// memory_missing. Don't wipe out the initial completion's
			// accounting just because the retry subprocess crashed.
			return current
		}
		if outcome.Completion != nil {
			current = mergeCompletion(current, outcome.Completion)
		}
		if memoryFileExists(cwd, runID) {
			return current
		}
	}

	return current
}

// mergeCompletion combines an initial completion event with one from a
// resumed session so final accounting reflects total cost, duration, and
// turn count across all invocations. The result text and stop_reason
// come from the resume (that's what the caller wants to report as the
// final outcome), but cost and turns are summed.
//
// If either the resume's Result or StopReason is empty, the base's
// values are preserved — partial resume outcomes shouldn't blank
// fields that were already populated.
func mergeCompletion(base, resume *runCompletion) *runCompletion {
	merged := *base
	merged.CostUSD += resume.CostUSD
	merged.DurationMs += resume.DurationMs
	merged.NumTurns += resume.NumTurns
	if resume.IsError {
		merged.IsError = true
	}
	if resume.Result != "" {
		merged.Result = resume.Result
	}
	if resume.StopReason != "" {
		merged.StopReason = resume.StopReason
	}
	return &merged
}

// materializePriorMemories writes any existing task_memory rows for the
// task into <cwd>/task_memory/<prior_run_id>.md as individual markdown
// files, so a fresh agent invocation sees what previous iterations on
// the same task have already tried. The agent is taught to read this
// directory by the envelope.
//
// Pattern: DB is the source of truth, we materialize into the worktree
// at startup, and ingest back on completion. The worktree is destroyed
// after every run, so these files never outlive their run on disk —
// only the DB rows do.
//
// Degrades gracefully: database errors, mkdir failures, or per-file
// write failures are logged but do not fail the run. An agent running
// without materialized priors is still useful, just without the
// cross-run memory benefit. This "advisory" posture only holds for
// the read side — the write-before-finish gate is enforced separately
// for NEW memories produced during the run.
func materializePriorMemories(database *sql.DB, cwd, taskID string) {
	memories, err := db.GetTaskMemoriesForTask(database, taskID)
	if err != nil {
		log.Printf("[delegate] warning: failed to load prior task memories for task %s: %v", taskID, err)
		return
	}
	if len(memories) == 0 {
		return
	}

	memDir := filepath.Join(cwd, "task_memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		log.Printf("[delegate] warning: failed to create task_memory dir at %s: %v", memDir, err)
		return
	}

	written := 0
	for _, m := range memories {
		filename := filepath.Join(memDir, m.RunID+".md")
		if err := os.WriteFile(filename, []byte(m.Content), 0644); err != nil {
			log.Printf("[delegate] warning: failed to materialize task memory %s: %v", filename, err)
			continue
		}
		written++
	}
	if written > 0 {
		log.Printf("[delegate] materialized %d prior task memories for task %s", written, taskID)
	}
}

// resolvePrompt finds the mission text for a task from an explicit prompt ID.
// Manual delegation always requires the caller to pick a prompt; auto-delegation
// supplies the prompt_id from the trigger row.
func (s *Spawner) resolvePrompt(task domain.Task, explicitPromptID string) (string, string, error) {
	if explicitPromptID == "" {
		return "", "", fmt.Errorf("no prompt specified — select one from the prompt picker")
	}

	p, err := db.GetPrompt(s.database, explicitPromptID)
	if err != nil {
		return "", "", fmt.Errorf("failed to load prompt %s: %w", explicitPromptID, err)
	}
	if p == nil {
		return "", "", fmt.Errorf("prompt %s not found", explicitPromptID)
	}
	return p.ID, p.Body, nil
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

// updateBreakerCounter adjusts the per-task consecutive failure counter
// based on the run's trigger type and terminal status.
//
// Rules:
//   - Auto runs (trigger_type != "manual") that fail or are unsolvable increment the counter.
//     When the counter reaches the trigger's breaker_threshold, a system event is emitted.
//   - Any successful run (manual or auto) resets the counter to zero.
//   - Manual runs that fail or are unsolvable do not touch the counter.
func (s *Spawner) updateBreakerCounter(taskID, triggerType, status string) {
	switch {
	case status == "completed":
		// Success resets the counter regardless of trigger type.
		if err := db.ResetTaskUnsuccessfulRuns(s.database, taskID); err != nil {
			log.Printf("[delegate] warning: failed to reset unsuccessful runs for task %s: %v", taskID, err)
		}

	case triggerType != "manual" && (status == "failed" || status == "task_unsolvable"):
		newCount, err := db.IncrementTaskUnsuccessfulRuns(s.database, taskID)
		if err != nil {
			log.Printf("[delegate] warning: failed to increment unsuccessful runs for task %s: %v", taskID, err)
			return
		}
		log.Printf("[delegate] task %s: consecutive unsuccessful auto-runs = %d", taskID, newCount)

		// Emit a suspension event exactly once — on the transition, not on
		// every subsequent failure. The actual gating (skip auto-fires when
		// counter >= breaker_threshold) is enforced by the auto-delegation hook
		// in SKY-147.
		if newCount == 2 {
			if _, err := db.RecordEvent(s.database, domain.Event{
				EventType:    domain.EventSystemTaskAutoSuspended,
				TaskID:       taskID,
				MetadataJSON: fmt.Sprintf(`{"consecutive_failures": %d}`, newCount),
			}); err != nil {
				log.Printf("[delegate] warning: failed to record auto-suspension event for task %s: %v", taskID, err)
			}
		}
	}
}

func (s *Spawner) failRun(runID, taskID, triggerType, errMsg string) {
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

	s.updateBreakerCounter(taskID, triggerType, "failed")
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

// buildPrompt composes: mission + envelope (scope, tools, task memory, completion contract).
func buildPrompt(mission, scope, toolsRef, binaryPath, runID string) string {
	envelope := strings.NewReplacer(
		"{{SCOPE}}", scope,
		"{{TOOLS_REFERENCE}}", toolsRef,
		"{{RUN_ID}}", runID,
	).Replace(ai.EnvelopeTemplate)

	body := strings.ReplaceAll(mission, "triagefactory exec", binaryPath+" exec")
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
