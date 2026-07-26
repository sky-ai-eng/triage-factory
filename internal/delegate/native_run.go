// The native runtime's execution path: the counterpart to runAgent for a
// conversation stamped runtime='native'. It stands up the jail's resident
// tool host, builds the loop's dependencies out of the same run context the
// SDK path uses, drives one engagement, and maps its terminal disposition
// onto the existing completion bookkeeping — so the blueprint reactor,
// the board, the ledger and the websocket all see exactly what they saw
// before.

package delegate

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// toolHostDialTimeout bounds how long setup waits for the in-jail server to
// bind its socket. The jail is already launched by then, so this covers
// process start plus a bind — generous enough to absorb a loaded host,
// short enough that a jail that never starts fails the claim rather than
// holding a concurrency slot.
const toolHostDialTimeout = 60 * time.Second

// toolHostDialInterval is the poll between dial attempts while the socket
// has not yet appeared.
const toolHostDialInterval = 100 * time.Millisecond

// runNativeAgent drives one native engagement end to end.
//
// The setup it does beyond the SDK path is exactly the two things the native
// runtime needs: the resident tool host as the jail's main process, and a
// connection to it. Everything before that — the claim, the sealed-bundle
// unseal into the sidecar, the workspace build-or-rehydrate, the cap-broker
// launch of the per-run network — is runtime-independent and already done by
// the dispatcher.
func (s *Spawner) runNativeAgent(ctx context.Context, runID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model, triggerType, creatorUserID string) {
	orgID := cfg.orgID
	namespace := memoryNamespace(cfg.blueprintRunID, runID)
	claudeCwd := cfg.wtPath

	// Ambient context the agent reads from disk, identical to the SDK path:
	// prior task memories and the project knowledge base, materialized under
	// _scratch/ before the jail starts so they are present at first read.
	materializePriorMemories(s.taskMemory, orgID, cfg.teamID, claudeCwd, task.EntityID, namespace)
	materializeProjectKnowledge(orgID, claudeCwd, cfg.projectID)

	systemPrompt, err := s.buildNativeSystemPrompt(ctx, task, mission, cfg, runID, namespace)
	if err != nil {
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "compose system prompt: "+err.Error(), domain.RunFailureUnclassified)
		return
	}

	s.updatePhase(orgID, runID, "agent_starting")
	if ctx.Err() != nil {
		s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
		return
	}

	jail, err := agentproc.LaunchToolHost(ctx, agentproc.ToolHostOptions{
		RunID:            runID,
		MemoryNamespace:  namespace,
		Worktree:         claudeCwd,
		SDKDir:           paths.SDKDir(),
		ExtraEnv:         s.nativeAgentEnv(ctx, orgID, runID, namespace, cfg, triggerType, creatorUserID),
		PrebuiltNetwork:  cfg.execSandbox.runNetwork(),
		PrebuiltProxyEnv: cfg.execSandbox.proxyEnv(),
		GHChannel:        cfg.execSandbox.ghChannel(runID),
		SkillsSourcePath: cfg.skillsSourcePath,
	})
	if err != nil {
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "launch tool host: "+err.Error(), domain.RunFailureUnclassified)
		return
	}
	defer func() { _ = jail.Close() }()

	tools, err := dialToolHostWithRetry(ctx, jail.SocketPath)
	if err != nil {
		if ctx.Err() != nil {
			s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
			return
		}
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "connect to tool host: "+err.Error(), domain.RunFailureUnclassified)
		return
	}
	defer func() { _ = tools.Close() }()

	s.updatePhase(orgID, runID, "")
	delegateLog.Info("native agent loop starting", "run", runID, "cwd", claudeCwd, "model", model)

	transcript := newNativeTranscript(s, orgID, runID, triggerType, creatorUserID)
	if err := s.mintOpeningTurn(ctx, transcript, orgID, runID, creatorUserID); err != nil {
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, "mint opening turn: "+err.Error(), domain.RunFailureUnclassified)
		return
	}

	engine := &agentloop.Engine{
		Transcript:  transcript,
		Credentials: s.nativeCredentials(cfg, model),
		Tools:       tools,
		Guards:      []agentloop.Guard{&spendGuard{spawner: s, orgID: orgID, teamID: cfg.teamID}},
		Hooks: agentloop.Hooks{
			ShouldStopAfterTurn: s.artifactContractNudge(orgID, runID, task, cfg),
		},
		Log: delegateLog,
	}

	result := engine.Run(ctx, agentloop.Spec{
		OrgID:          orgID,
		ConversationID: runID,
		Model:          model,
		SystemPrompt:   systemPrompt,
		HasBlueprint:   true,
		MaxIterations:  nativeMaxIterations(),
		UserID:         creatorUserID,
	})

	s.recordNativeResult(ctx, orgID, runID, task, cfg, namespace, claudeCwd, triggerType, creatorUserID, startTime, result)
}

// openingTurn is the delegation's first user message. The mission itself
// rides the system prompt (it is part of the cacheable prefix), so this row
// exists to open the conversation rather than to carry instructions — a
// provider call needs at least one user message, and the loop has exactly
// one way for input to arrive.
const openingTurn = "Begin. Your mission and the contract you work under are in your instructions above."

// mintOpeningTurn queues the opening turn when the conversation has no
// transcript yet.
//
// It is written pending, like every other input, so the engagement's entry
// is just its first drain — there is no first-call special case anywhere in
// the engine. Gating on an empty transcript makes it idempotent: a re-claim
// of a conversation that has already spoken adds nothing, and a crash
// between this insert and the first call leaves the row for the next claim
// to drain rather than losing the opening.
func (s *Spawner) mintOpeningTurn(ctx context.Context, transcript agentloop.Transcript, orgID, runID, creatorUserID string) error {
	rows, err := transcript.ListForAssembly(ctx, orgID, runID)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return nil
	}
	pending := false
	_, err = transcript.Insert(ctx, orgID, &domain.Message{
		ConversationID: runID,
		UserID:         creatorUserID,
		Role:           "user",
		Subtype:        "text",
		Content:        openingTurn,
		Delivered:      &pending,
	})
	return err
}

// dialToolHostWithRetry polls until the in-jail server has bound its socket.
// The jail's process start races this call by construction — the launch RPC
// returns once runsc is running, not once the server inside it is listening —
// so a connection refused here is the normal case for the first few tries,
// not an error.
func dialToolHostWithRetry(ctx context.Context, socketPath string) (agentloop.ToolHost, error) {
	deadline := time.Now().Add(toolHostDialTimeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		host, err := agentloop.DialToolHost(socketPath, 0)
		if err == nil {
			return host, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("tool host did not accept a connection within %s: %w", toolHostDialTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(toolHostDialInterval):
		}
	}
}

// buildNativeSystemPrompt assembles the native envelope from the same inputs
// buildPrompt uses on the SDK path — the task context block, the shared
// envelope body, the step's mission — with the runtime-specific completion
// contract and, on a non-terminal step, the addendum that redefines what
// stopping means.
//
// Interpolation is the same single non-re-scanning pass, applied before the
// untrusted task-context block is prepended, so no externally-authored text
// is ever interpolated. That ordering is a security property, not a style
// choice; see buildPrompt.
func (s *Spawner) buildNativeSystemPrompt(ctx context.Context, task domain.Task, mission string, cfg runConfig, runID, namespace string) (string, error) {
	selfBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own binary path: %w", err)
	}
	metadataJSON, err := s.events.GetMetadataSystem(context.Background(), cfg.orgID, task.PrimaryEventID)
	if err != nil {
		delegateLog.Warn("load event metadata for task failed; event placeholders will render empty",
			"task", task.ID, "event", task.PrimaryEventID, "error", err)
		metadataJSON = ""
	}

	agentBin := agentproc.AgentVisibleBinary(selfBin)
	agentRunRoot := agentproc.AgentVisibleRoot(cfg.runRoot)
	branchTemplate := s.resolveBranchTemplate(ctx, task)
	runURL := s.runURLFor(cfg.orgID, runID)

	body := strings.ReplaceAll(mission, "triagefactory exec", agentBin+" exec")
	replacer := BuildPromptReplacer(task, metadataJSON, runID, agentBin, agentRunRoot, namespace, branchTemplate, runURL)
	return agentloop.BuildSystemPrompt(agentloop.EnvelopeParts{
		RunContext:  nativeRunContext(branchTemplate, agentRunRoot, namespace, runID),
		TaskContext: BuildTaskContext(task, metadataJSON, cfg.prSkeleton),
		Mission:     replacer.Replace(body),
		// A delegation always executes a blueprint — a single-step one is
		// still one — so this path never builds the taskless shape. The
		// distinction exists for the conversations the loop does not drive
		// yet, where a person is present to be answered instead.
		HasBlueprint:    true,
		NonTerminalStep: cfg.appendSysPrompt != "",
	}), nil
}

// nativeRunContext renders the two facts the standing envelope refers to but
// cannot contain: the team's branch convention, and where this run writes its
// memory.
//
// They live here rather than interpolated into the envelope because the
// envelope's value is being byte-identical for every run in the fleet — one
// cached prefix instead of per-run tokens. Two lines of genuinely per-run text
// belong in the part that was never going to be cached anyway. It is
// system-authored, so unlike the task context it carries no untrusted markers.
func nativeRunContext(branchTemplate, agentRunRoot, namespace, runID string) string {
	memoryPath := path.Join(agentRunRoot, "_scratch", "entity-memory", namespace, runID+".md")
	return "<run_context>\n" +
		"Branch naming convention for this team: " + branchTemplate + "\n" +
		"Write your entity-memory notes to: " + memoryPath + "\n" +
		"</run_context>"
}

// nativeAgentEnv is the env the jail's tool host exports to every `bash`
// command it runs — the same run-identity variables the SDK path exports, so
// a `tfac`/`triagefactory exec` invocation from inside a tool resolves the
// same run.
func (s *Spawner) nativeAgentEnv(ctx context.Context, orgID, runID, namespace string, cfg runConfig, triggerType, creatorUserID string) []string {
	env := []string{
		"TRIAGE_FACTORY_CONVERSATION_ID=" + runID,
		"TRIAGE_FACTORY_CONVERSATION_ROOT=" + cfg.runRoot,
		"TRIAGE_FACTORY_BLUEPRINT_RUN_ID=" + namespace,
	}
	if cfg.owner != "" && cfg.repo != "" {
		env = append(env, "TRIAGE_FACTORY_REPO="+cfg.owner+"/"+cfg.repo)
	}
	if id := s.resolveCommitIdentity(ctx, orgID, triggerType, creatorUserID); id.CoAuthorTrailer != "" {
		env = append(env, "TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER="+id.CoAuthorTrailer)
	}
	return env
}

// nativeCredentials resolves the loop's provider account per call.
//
// In this phase the account points at the per-run credential sidecar's LLM
// proxy: the sidecar holds the unsealed bundle and injects the real key on
// the upstream hop, and its proxy env is already the resolved-credential
// vocabulary the adapter reads. The orchestrator therefore still opens no
// sealed bundle, and Property B is untouched — the placeholder the loop
// sends is the same one the SDK path sends from inside the jail.
//
// Removing that hop (the loop calling the provider directly with the sealed
// bundle's own material, and the LLM proxy disappearing along with the
// provider hosts in the egress allowlist) is the cutover phase's work, and
// is a change of what Resolve returns — nothing above it moves.
func (s *Spawner) nativeCredentials(cfg runConfig, model string) agentloop.Credentials {
	env := envSliceToMap(cfg.execSandbox.proxyEnv())
	return &agentloop.EnvCredentials{
		Resolve: func(context.Context) (map[string]string, error) { return env, nil },
		// The whitelist must name the model actually requested: bifrost reads
		// an empty whitelist as "no models", and a wildcard would let a
		// mis-stamped model id reach the provider unremarked.
		Models: []string{model},
	}
}

// envSliceToMap turns KEY=VALUE entries into a map, last-wins. Values may
// contain '='; only the first separator splits.
func envSliceToMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

// artifactNudgeTag opens the nudge note and is how a later turn recognizes
// one in the transcript. The identity lives in the tag, not the prose, so
// the wording can be reworded without silently re-arming the check on every
// conversation that is mid-flight.
const artifactNudgeTag = `<system-note kind="artifact-contract">`

// artifactNudgeNote is what the model is asked when it would stop having
// published nothing.
const artifactNudgeNote = artifactNudgeTag + "\n" +
	"You are about to finish without having produced any external artifact — no branch pushed, " +
	"no pull request or review, no ticket updated. If the work called for one, do it now. " +
	"If it genuinely did not, say so plainly and stop again; you will not be asked again.\n" +
	"</system-note>"

// artifactContractNudge builds the would-stop hook: a run whose task expects
// an external artifact, that is about to conclude having recorded none, is
// asked whether it meant to.
//
// "Expects an artifact" is deliberately narrow — the terminal step of a run
// against a GitHub or Jira entity, the shape whose whole point is to leave
// something behind. A non-terminal step is excluded because a later step
// owns the blueprint's terminal external action, which is what its addendum
// tells it.
//
// Asking twice about the same silence would be badgering — the model already
// answered. Asking again after a human has intervened is not: the premise
// changed, there is new work, and the same question about that work has not
// been put. So the check re-arms on human input and only on human input, and
// it reads that from the transcript rather than remembering it, which is
// what makes the behavior identical whether the engagement is the first or a
// crash's successor.
func (s *Spawner) artifactContractNudge(orgID, runID string, task domain.Task, cfg runConfig) func(context.Context, int, string) string {
	expects := cfg.appendSysPrompt == "" && (task.EntitySource == "github" || task.EntitySource == "jira")
	if !expects || s.artifacts == nil {
		return nil
	}
	return func(ctx context.Context, _ int, _ string) string {
		arts, err := s.artifacts.ListByRunSystem(ctx, orgID, runID)
		if err != nil {
			// Fail quiet: a read failure must not manufacture a nudge that
			// sends a finished run back to work on a false premise.
			delegateLog.Warn("read artifacts for the turn-end contract check failed; not nudging", "run", runID, "error", err)
			return ""
		}
		if len(arts) > 0 {
			return ""
		}
		rows, err := s.agentRuns.ListForAssembly(ctx, orgID, runID)
		if err != nil {
			delegateLog.Warn("read transcript for the turn-end contract check failed; not nudging", "run", runID, "error", err)
			return ""
		}
		if askedAboutArtifactAlready(rows) {
			return ""
		}
		return artifactNudgeNote
	}
}

// askedAboutArtifactAlready reports whether the nudge has been put with no
// human input since.
//
// Walking back from the newest row to the first one that speaks for someone:
// the nudge itself means the question stands answered and nothing has
// happened to change it. Any other user row means a person spoke afterwards,
// so the run is working on something the question was never asked about. The
// loop's own crash notice speaks for no one and is skipped.
func askedAboutArtifactAlready(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role != "user" {
			continue
		}
		if rows[i].Subtype == domain.MessageSubtypeInjectionExecutorChanged {
			continue
		}
		return strings.Contains(rows[i].Content, artifactNudgeTag)
	}
	return false
}

// nativeMaxIterations reads the per-engagement provider-call backstop from
// the environment, falling back to the loop's generous default. It is a
// backstop against a cheap-call loop, not a work budget — spend is the real
// brake — so it is deliberately not a per-org setting.
func nativeMaxIterations() int {
	raw := strings.TrimSpace(os.Getenv("TF_AGENT_MAX_ITERATIONS"))
	if raw == "" {
		return 0 // the engine's default
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		delegateLog.Warn("invalid TF_AGENT_MAX_ITERATIONS; using the default", "value", raw)
		return 0
	}
	return n
}

// recordNativeResult maps an engagement's terminal disposition onto the
// existing bookkeeping. Every graceful release — a conclusion, a guard park,
// a flow-control terminal — snapshots the workspace first, exactly as the
// SDK path does at its own dormancy points, which is what keeps the
// crash-loss window bounded to the current engagement and lets the next
// claim cold-rehydrate on another executor.
func (s *Spawner) recordNativeResult(
	ctx context.Context,
	orgID, runID string,
	task domain.Task,
	cfg runConfig,
	namespace, claudeCwd, triggerType, creatorUserID string,
	startTime time.Time,
	result agentloop.Result,
) {
	switch result.Disposition {
	case agentloop.DispositionCancelled:
		s.handleCancelled(orgID, runID, startTime, cfg.wtPath, triggerType, creatorUserID)
		return

	case agentloop.DispositionFailed:
		reason := "native agent loop failed"
		if result.Err != nil {
			reason = result.Err.Error()
		}
		s.failRun(orgID, runID, task.ID, triggerType, creatorUserID, reason, result.FailureKind)
		return

	case agentloop.DispositionParked:
		// A guard stopped the engagement before a call. The conversation is
		// resumable, so the snapshot must exist by the time the status
		// commits — parkRunOpen owns that ordering.
		s.parkRunOpen(liveParkContext{
			orgID:         orgID,
			runID:         runID,
			taskID:        task.ID,
			namespace:     namespace,
			claudeCwd:     claudeCwd,
			triggerType:   triggerType,
			creatorUserID: creatorUserID,
		}, "")
		return
	}

	// Concluded. Record the run's memory exactly as processCompletion does —
	// row presence means "the run terminated", NULL content means the agent
	// wrote no usable memory file.
	agentContent, fileState := readAgentMemoryFile(claudeCwd, namespace, runID)
	if err := s.taskMemory.UpsertAgentMemorySystem(context.Background(), orgID, runID, task.EntityID, cfg.blueprintRunID, agentContent); err != nil {
		delegateLog.Warn("upsert memory for run failed", "run", runID, "error", err)
	}
	if fileState != memoryFilePresent {
		delegateLog.Debug("no usable memory file at termination", "run", runID, "state", fileState)
	}
	s.attachRunMemoryEntities(context.Background(), orgID, runID, task.EntityID)

	// Snapshot before the terminal write. A `continue` hands the shared
	// workspace to the next step and an `abort` leaves a message-resumable
	// conversation; both can be picked up on an executor that never held
	// this worktree, so the blob has to exist by the time the status commits.
	if err := s.snapshotWorkspace(ctx, orgID, namespace, claudeCwd, ""); err != nil {
		delegateLog.Warn("snapshot workspace at native conclusion failed", "run", runID, "error", err)
	}

	// Only a stop_blueprint terminal carries a reason; concluding by stopping says
	// what it has to say in the summary.
	outcome := string(result.Outcome)
	outcomeReason := result.OutcomeReason

	bgCtx := context.Background()
	if triggerType == "manual" {
		if err := s.tx.SyntheticClaimsWithTx(bgCtx, orgID, creatorUserID, func(ts db.TxStores) error {
			return ts.Conversations.Complete(bgCtx, orgID, runID, "completed", 0, result.DurationMs, result.NumTurns, "", result.ResultSummary, outcome, outcomeReason, "")
		}); err != nil {
			delegateLog.Warn("record completion for run failed", "run", runID, "error", err)
		}
	} else if err := s.agentRuns.CompleteSystem(bgCtx, orgID, runID, "completed", 0, result.DurationMs, result.NumTurns, "", result.ResultSummary, outcome, outcomeReason, ""); err != nil {
		delegateLog.Warn("record completion for run failed", "run", runID, "error", err)
	}

	// costUSD is zero here on purpose, and it is not a gap: the native
	// runtime settles cost per assistant row at call time, so the ledger is
	// already complete. Passing a lump would double-count.

	s.updateBreakerCounter(task.ID, triggerType, "completed")
	s.broadcastRunUpdate(orgID, runID, "completed")
	s.recomputeTaskBoardColumn(orgID, task.ID)
	toast.Success(s.wsHub, orgID, fmt.Sprintf("Run %s completed", shortRunID(runID)))
}
