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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// toolHostDialTimeout bounds how long setup waits for the in-jail server to
// dial in. The jail is already launched by then, so this covers process start
// plus a connect — generous enough to absorb a loaded host, short enough that
// a jail that never starts fails the claim rather than holding a concurrency
// slot. A tool host that exits instead of connecting is reported immediately
// and never waits this out (ToolHostJail.Accept).
const toolHostDialTimeout = 60 * time.Second

// runNativeAgent drives one native engagement end to end.
//
// The setup it does beyond the SDK path is exactly the two things the native
// runtime needs: the resident tool host as the jail's main process, and a
// connection to it. Everything before that — the claim, the sealed-bundle
// unseal into the sidecar, the workspace build-or-rehydrate, the cap-broker
// launch of the per-run network — is runtime-independent and already done by
// the dispatcher.
//
// Nothing here fails the conversation before the agent has spoken. Standing up
// a jail is infrastructure, fallible for the same passing reasons workspace
// setup is — a broker restart, an exhausted subnet, stale runsc state — and a
// conversation that has been running for an hour must not be ended by two
// seconds of that. Every failure ahead of engine.Run therefore comes back as a
// launchErr for the dispatcher to retry; only the loop itself writes terminals.
func (s *Spawner) runNativeAgent(ctx context.Context, conversationID string, task domain.Task, mission string, cfg runConfig, startTime time.Time, model, triggerType, creatorUserID string) engagementDisposition {
	orgID := cfg.orgID
	namespace := memoryNamespace(cfg.blueprintRunID)
	claudeCwd := cfg.wtPath

	// The park a stop lands on, wherever the stop catches this engagement.
	stopped := func() engagementDisposition {
		fenced := s.parkConversationOpen(ctx, liveParkContext{
			orgID:          orgID,
			conversationID: conversationID,
			taskID:         task.ID,
			namespace:      namespace,
			claudeCwd:      claudeCwd,
			triggerType:    triggerType,
			creatorUserID:  creatorUserID,
			claimID:        cfg.claimID,
			reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
			runtime:        domain.ConversationRuntimeNative,
		}, "")
		return engagementDisposition{fenced: fenced}
	}
	// launchFailed is every pre-agent exit. A cancelled ctx is read first: a
	// user who stopped the run during bring-up asked for exactly the park the
	// later phases give them, and retrying it would restart the work they just
	// stopped.
	launchFailed := func(err error) engagementDisposition {
		if ctx.Err() != nil {
			return stopped()
		}
		return engagementDisposition{launchErr: err}
	}

	// Ambient context the agent reads from disk, identical to the SDK path:
	// prior task memories, staged before the jail starts so they are present at
	// first read. The sequencing below mirrors runAgent's exactly — a native
	// step and an SDK step of the same blueprint share one run tree, so the two
	// paths must agree on who may write in it.
	handedOff := sandbox.RunTreeHandedOff(claudeCwd)
	var owned repoFiles
	if !handedOff {
		worktree.AdoptLegacyScratchDir(ctx, claudeCwd)
		owned = scanRepoFiles(ctx, claudeCwd)
		if err := worktree.EnsureSandboxMemoryLink(ctx, claudeCwd); err != nil {
			delegateLog.Warn("plant sandbox memory symlink failed; this conversation reads no prior memory", "conversation", conversationID, "cwd", claudeCwd, "error", err)
		}
	}

	// The SDK path's twin (runAgent) — same staging, same span, so the two
	// runtimes' setup is comparable in the backend rather than only in prose.
	stagingCtx, stagingSpan := tracer.Start(ctx, "engagement.stage_context")
	memoryDir, memoryOwned := entityMemoryTarget(&cfg, conversationID, claudeCwd, owned)
	materializePriorMemories(s.taskMemory, orgID, cfg.teamID, memoryDir, task.EntityID, cfg.blueprintRunID, memoryOwned)

	// The SDK path's twin again: the task team's knowledge base plus every
	// other team's published root, copied in before the jail starts so it is
	// present at first read. Skipped on a warm step for the same reason the
	// memory clear is — the tree belongs to the sandbox identity by then.
	knowledge := ""
	if handedOff {
		delegateLog.Debug("run tree already handed to the sandbox identity; team knowledge not refreshed for this step", "conversation", conversationID, "cwd", claudeCwd)
	} else {
		knowledge = s.stageTeamKnowledge(stagingCtx, orgID, cfg.teamID, claudeCwd, owned)
	}

	priorMemory := s.prepareInheritedMemory(stagingCtx, orgID, conversationID, claudeCwd, owned, handedOff)
	stagingSpan.End()

	// Composed before the jail is launched, so the fallible half fails the claim
	// without having cost a sandbox. The system prompt is the same for every
	// native run; everything about this one rides the opening turn.
	opening, err := s.buildNativeOpeningTurn(ctx, task, mission, cfg, knowledge)
	if err != nil {
		return launchFailed(fmt.Errorf("compose opening turn: %w", err))
	}
	systemPrompt := nativeSystemPrompt(cfg.appendSysPrompt != "")

	// The stop is read before the phase write, so a run stopped during
	// bring-up parks here without ever asking the fence — the refusal below is
	// then reserved for a claim that went away with no cancel behind it.
	if ctx.Err() != nil {
		return stopped()
	}
	if s.updatePhase(ctx, orgID, conversationID, cfg.claimID, domain.ClaimPhaseAgentStarting) {
		return engagementDisposition{fenced: true}
	}

	jail, err := agentproc.LaunchToolHost(ctx, agentproc.ToolHostOptions{
		ConversationID:       conversationID,
		MemoryNamespace:      namespace,
		Worktree:             claudeCwd,
		ExtraEnv:             s.nativeAgentEnv(ctx, orgID, conversationID, namespace, cfg, triggerType, creatorUserID),
		PrebuiltNetwork:      cfg.sidecar.runNetwork(),
		PrebuiltProxyEnv:     cfg.sidecar.jailEnv(),
		AgentHostSocket:      agenthost.SocketMountFor(conversationID),
		GHChannel:            cfg.sidecar.ghChannel(conversationID),
		SkillsSourcePath:     cfg.skillsSourcePath,
		MemorySourcePath:     cfg.memorySourcePath,
		OrgID:                orgID,
		ClaimID:              cfg.claimID,
		RecordSandboxActuals: s.recordSandboxActuals,
	})
	if err != nil {
		return launchFailed(fmt.Errorf("launch tool host: %w", err))
	}
	defer func() { _ = jail.Close() }()

	conn, err := jail.Accept(ctx, toolHostDialTimeout)
	if err != nil {
		return launchFailed(fmt.Errorf("connect to tool host: %w", err))
	}
	tools := agentloop.NewToolHost(conn, 0)
	defer func() { _ = tools.Close() }()

	s.updatePhase(ctx, orgID, conversationID, cfg.claimID, "")
	delegateLog.Info("native agent loop starting", "conversation", conversationID, "cwd", claudeCwd, "model", model)

	transcript := newNativeTranscript(s, orgID, conversationID, cfg.claimID)
	if err := s.mintOpeningTurn(ctx, transcript, orgID, conversationID, creatorUserID, opening); err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			delegateLog.Error("engagement fenced out before its first turn; a successor owns the conversation", "conversation", conversationID, "claim", cfg.claimID)
			return engagementDisposition{fenced: true}
		}
		return launchFailed(fmt.Errorf("mint opening turn: %w", err))
	}

	// Resolved once, here, and handed to both the engine params and the
	// credential whitelist below — the request and the whitelist agree by
	// construction, not by two reads of the same constant. Today this is
	// always the default; a provider-aware resolution (a Bedrock deployment
	// needs a Bedrock-shaped id, not the bare Anthropic one) lands at this
	// single seam when model configuration makes "cheapest enabled"
	// resolvable.
	coldModel := agentloop.DefaultColdCompactionModel

	engine := &agentloop.Engine{
		Transcript:  transcript,
		Credentials: s.nativeCredentials(cfg, model, coldModel),
		Tools:       tools,
		Guards:      []agentloop.Guard{&spendGuard{spawner: s, orgID: orgID, teamID: cfg.teamID}},
		Hooks: agentloop.Hooks{
			BeforeToolCall:      s.ghCommandGate(orgID, conversationID),
			ShouldStopAfterTurn: s.artifactContractNudge(orgID, conversationID, task, cfg),
		},
		Log: delegateLog,
	}

	result := engine.Run(ctx, agentloop.Params{
		OrgID:          orgID,
		ConversationID: conversationID,
		Model:          model,
		SystemPrompt:   systemPrompt,
		HasBlueprint:   true,
		// Derived from the very ceiling this jail was launched under, not from
		// a constant that could drift from it.
		BashMemBudgetMB: nativeBashMemBudgetMB(agentproc.ClaimMemoryLimitMB()),
		UserID:          creatorUserID,
		// A delegation's opening turn is the control-plane-minted mission:
		// compaction pins it instead of re-injecting the first message.
		MissionAnchored:     true,
		ColdCompactionModel: coldModel,
		Workspace:           cfg.workspace,
		ExecutorChanged:     s.executorChangedSince(ctx, orgID, conversationID, cfg.claimID, cfg.workspace),
	})

	return engagementDisposition{
		fenced: s.recordNativeResult(ctx, orgID, conversationID, task, cfg, namespace, claudeCwd, triggerType, creatorUserID, startTime, result, priorMemory),
	}
}

// executorChangedSince reports whether the engagement before this one ran on
// a different executor — the one claim of the resume notice that neither the
// transcript nor the tree on disk can establish.
//
// Asked only of a workspace that was rebuilt, because that is the only case
// where anything is said at all: a warm tree is by construction the one this
// executor already held, and skipping the read there keeps the common resume
// free of a query whose answer could not change the outcome.
//
// A read failure answers false. The two ways of being wrong are not
// symmetric: a missing sentence costs the agent a fact it can rediscover from
// the tree, while a spurious one is exactly the false claim this notice is
// being made honest about.
func (s *Spawner) executorChangedSince(ctx context.Context, orgID, conversationID, claimID string, prov domain.WorkspaceProvenance) bool {
	if !prov.Rebuilt() || claimID == "" {
		return false
	}
	self, _ := s.executorIdentity()
	if self == "" {
		return false
	}
	prior, err := s.conversations.PriorClaimExecutorSystem(ctx, orgID, conversationID, claimID)
	if err != nil {
		delegateLog.Warn("read the prior claim's executor failed; the resume notice will not claim the executor changed",
			"conversation", conversationID, "claim", claimID, "error", err)
		return false
	}
	return prior != "" && prior != self
}

// mintOpeningTurn queues the delegation's opening turn — the mission and the
// task context it is about — when the conversation has no transcript yet.
//
// It is written pending, like every other input, so the engagement's entry
// is just its first drain — there is no first-call special case anywhere in
// the engine. Gating on an empty transcript makes it idempotent: a re-claim
// of a conversation that has already spoken adds nothing, and a crash
// between this insert and the first call leaves the row for the next claim
// to drain rather than losing the opening.
//
// One row, not several: the mission and the context it is about are one
// statement of what to do. Compaction pins either shape — the anchored span is
// every leading row before the first assistant turn — so either would work.
func (s *Spawner) mintOpeningTurn(ctx context.Context, transcript agentloop.Transcript, orgID, conversationID, creatorUserID, opening string) error {
	rows, err := transcript.ListForAssembly(ctx, orgID, conversationID)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return nil
	}
	_, err = transcript.Insert(ctx, orgID, pendingUserInput(conversationID, creatorUserID, opening))
	return err
}

// nativeSystemPrompt is the run's whole system prompt: the composed framework
// blocks, plus the handoff addendum when a later step follows this one.
//
// Nothing about the particular run is in it. The task context renders PR
// titles, commit subjects and issue bodies — text an outsider can write, which
// is why it carries untrusted markers — and text sitting inside the agent's own
// instructions reads as instruction. It rides the opening turn instead, which
// also leaves this prompt identical for every run with the same spec and step
// position, so the cached prefix reaches the end of the instructions.
func nativeSystemPrompt(nonTerminalStep bool) string {
	if nonTerminalStep {
		return agentprompt.BuildNonTerminalStep(nativeSpec())
	}
	return agentprompt.Build(nativeSpec())
}

// buildNativeOpeningTurn resolves this run's inputs and composes the
// conversation's first user message. The fallible resolutions live here; the
// composition itself is composeNativeOpeningTurn.
func (s *Spawner) buildNativeOpeningTurn(ctx context.Context, task domain.Task, mission string, cfg runConfig, knowledge string) (string, error) {
	selfBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own binary path: %w", err)
	}
	metadataJSON, err := s.events.GetMetadataSystem(context.WithoutCancel(ctx), cfg.orgID, task.PrimaryEventID)
	if err != nil {
		delegateLog.Warn("load event metadata for task failed; the task context will carry no event fields",
			"task", task.ID, "event", task.PrimaryEventID, "error", err)
		metadataJSON = ""
	}
	return composeNativeOpeningTurn(task, metadataJSON, cfg.prSkeleton, mission,
		agentproc.AgentVisibleBinary(selfBin), s.resolveBranchTemplate(ctx, task), knowledge), nil
}

// composeNativeOpeningTurn assembles what the loop says first: run context,
// task context, then the mission — the instruction last, after the external
// material it is about.
//
// The mission is composed on its own, so externally-authored text is never run
// through resolveCLIPath. That ordering is a security property, not a style
// choice; buildPrompt does the same on the SDK path.
//
// The native blocks name the in-jail paths and the `tfac` applet outright, so
// the run context carries only the branch convention and this run's staged
// knowledge — the two facts that differ per team and per run, and the ones
// those blocks cannot state for themselves.
func composeNativeOpeningTurn(task domain.Task, metadataJSON, skeleton, mission, binaryPath, branchTemplate, knowledge string) string {
	return joinSections(
		runContext("", "", branchTemplate, "", knowledge),
		BuildTaskContext(task, metadataJSON, skeleton),
		resolveCLIPath(mission, binaryPath),
	)
}

// nativeSpec is the prompt selector for every native engagement.
//
// Mode is pinned to multi rather than read from runmode: a native engagement
// runs its tools inside a jail whose launch hard-requires a prebuilt per-run
// network, which only multi provisions, so a native run reaching here in local
// mode is a wiring bug. Passing the ambient mode would compose a local-mode
// prompt for it and let it proceed on paths that do not exist; pinning makes
// agentprompt refuse, which is the same answer the launch gives moments later.
func nativeSpec() agentprompt.Spec {
	return agentprompt.Spec{
		Surface: agentprompt.SurfaceMachinist,
		Runtime: agentprompt.RuntimeNative,
		Family:  agentprompt.FamilyClaude,
		Mode:    agentprompt.ModeMulti,
	}
}

// prepareInheritedMemory readies the fixed memory write path for this claim,
// returning the fingerprint termination uses to tell the agent's own notes from
// a file it merely inherited (nil meaning "nothing to distrust").
//
// A predecessor step's memory sits at that path, because a blueprint's steps
// share one run tree and all write the same filename. Delete it where the tree
// is still ours; where it is not — a warm step, whose tree belongs to the
// sandbox identity — digest it instead, so termination can refuse to ingest
// content this agent never wrote.
//
// Both are skipped for a conversation that has already been driven: a parked
// engagement picking up a steer, whose own memory file is at that path. The
// transcript is this runtime's "has it run before". It stands in for the SDK
// path's prior session id, which the native runtime has no counterpart to, and
// it is the same signal mintOpeningTurn reads to answer the same question a few
// calls later.
func (s *Spawner) prepareInheritedMemory(ctx context.Context, orgID, conversationID, cwd string, owned repoFiles, handedOff bool) *memoryFingerprint {
	driven, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		// An unreadable transcript is treated as a fresh claim. The two ways of
		// being wrong are not symmetric: crediting this run with a predecessor's
		// memory corrupts the entity's durable record, while distrusting a
		// re-claimed engagement's own notes costs one run's notes and nothing
		// downstream.
		delegateLog.Warn("read transcript to classify the inherited memory file failed; treating this claim as fresh", "conversation", conversationID, "error", err)
	} else if len(driven) > 0 {
		return nil
	}
	if !handedOff {
		clearAgentMemoryFile(cwd, owned)
	}
	return fingerprintAgentMemoryFile(cwd)
}

// nativeAgentEnv is the env the jail's tool host exports to every `bash`
// command it runs — the same run-identity variables the SDK path exports, so
// a `tfac`/`triagefactory exec` invocation from inside a tool resolves the
// same run.
func (s *Spawner) nativeAgentEnv(ctx context.Context, orgID, conversationID, namespace string, cfg runConfig, triggerType, creatorUserID string) []string {
	env := []string{
		"TRIAGE_FACTORY_CONVERSATION_ID=" + conversationID,
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
// The account points at this claim's credential sidecar, the one process
// holding the unsealed bundle: it injects the real key on the upstream hop, so
// what travels here is a per-run placeholder and the loop holds no provider
// credential. The material behind it arrived sealed to this claim and nowhere
// else — no secret store is reachable from an executor to resolve it any other
// way.
//
// Read off the sidecar's LLM coordinates rather than the sandbox env, and that
// distinction is the whole of the cell shrink: those coordinates now go to
// exactly one place, this call, and the jail is built without them.
func (s *Spawner) nativeCredentials(cfg runConfig, model, coldModel string) agentloop.Credentials {
	env := envSliceToMap(cfg.sidecar.engineLLMEnv())
	return &agentloop.EnvCredentials{
		Resolve: func(context.Context) (map[string]string, error) { return env, nil },
		// The whitelist must name the models actually requested: bifrost
		// reads an empty whitelist as "no models", and a wildcard would let a
		// mis-stamped model id reach the provider unremarked. The forced-shape
		// compaction model is the caller's resolved value, not a constant read
		// of its own — the same resolution feeds the engine's params, so the
		// compaction request and this whitelist cannot drift apart.
		Models: []string{model, coldModel},
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
func (s *Spawner) artifactContractNudge(orgID, conversationID string, task domain.Task, cfg runConfig) func(context.Context, int, string) string {
	expects := cfg.appendSysPrompt == "" && (task.EntitySource == "github" || task.EntitySource == "jira")
	if !expects || s.artifacts == nil {
		return nil
	}
	return func(ctx context.Context, _ int, _ string) string {
		arts, err := s.artifacts.ListByConversationSystem(ctx, orgID, conversationID)
		if err != nil {
			// Fail quiet: a read failure must not manufacture a nudge that
			// sends a finished run back to work on a false premise.
			delegateLog.Warn("read artifacts for the turn-end contract check failed; not nudging", "conversation", conversationID, "error", err)
			return ""
		}
		if len(arts) > 0 {
			return ""
		}
		rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
		if err != nil {
			delegateLog.Warn("read transcript for the turn-end contract check failed; not nudging", "conversation", conversationID, "error", err)
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
// happened to change it; genuine human input means a person spoke
// afterwards, so the run is working on something the question was never
// asked about. Everything else the system wrote on the agent's behalf — a
// park's stop-note, a staged event note, the crash notice — speaks for no
// one and is skipped, using the same closed human set the engine's drain
// keys on. The nudge check comes first because the nudge row itself is
// system-authored: the human filter would skip it.
func askedAboutArtifactAlready(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role != "user" {
			continue
		}
		if strings.Contains(rows[i].Content, artifactNudgeTag) {
			return true
		}
		if agentloop.IsHumanInput(rows[i]) {
			return false
		}
	}
	return false
}

// The per-command bash memory budget, as a policy rather than a knob: a cap,
// the headroom it leaves under the jail's ceiling, and a floor.
//
// The headroom is what the rest of the session needs while one command runs —
// the resident tool host, page cache, and the files the session has already
// written. The cap keeps a generous ceiling from handing one command the whole
// allowance. The floor keeps a small ceiling from producing a budget too tight
// for an ordinary build step, at the cost of leaving less headroom than the
// subtraction asked for; the jail ceiling is still underneath either way.
const (
	bashMemBudgetCapMB      = 3072
	bashMemBudgetHeadroomMB = 1024
	bashMemBudgetFloorMB    = 512
)

// nativeBashMemBudgetMB derives one engagement's per-command budget from the
// jail ceiling it runs under.
//
// A disabled ceiling disables the budget. The derivation exists to keep a
// single command from consuming a shared allowance, and an operator who set
// TF_CLAIM_MEMORY_LIMIT_MB=0 has said there is no allowance to protect —
// inventing one here would impose a limit they explicitly turned off.
func nativeBashMemBudgetMB(ceilingMB int) int {
	if ceilingMB <= 0 {
		return 0
	}
	budget := ceilingMB - bashMemBudgetHeadroomMB
	if budget > bashMemBudgetCapMB {
		budget = bashMemBudgetCapMB
	}
	if budget < bashMemBudgetFloorMB {
		budget = bashMemBudgetFloorMB
	}
	return budget
}

// recordNativeResult maps an engagement's terminal disposition onto the
// existing bookkeeping. Every graceful release — a conclusion, a guard park,
// a flow-control terminal — snapshots the workspace first, exactly as the
// SDK path does at its own dormancy points, which is what keeps the
// crash-loss window bounded to the current engagement and lets the next
// claim cold-rehydrate on another executor.
//
// Returns fenced: true when the engagement's writes were refused because its
// claim is released. Nothing further is recorded and nothing is reacted to —
// a successor owns the conversation's disposition now.
func (s *Spawner) recordNativeResult(
	ctx context.Context,
	orgID, conversationID string,
	task domain.Task,
	cfg runConfig,
	namespace, claudeCwd, triggerType, creatorUserID string,
	startTime time.Time,
	result agentloop.Result,
	priorMemory *memoryFingerprint,
) (fenced bool) {
	// A fence trip inside the loop (a transcript insert, a drain flush)
	// surfaces as the engagement's failure. It is not a failure to record:
	// the refusal IS the record, and writing a terminal here is exactly what
	// the fence exists to prevent.
	if result.Err != nil && errors.Is(result.Err, db.ErrClaimReleased) {
		delegateLog.Error("engagement fenced out mid-flight; a successor owns the conversation",
			"conversation", conversationID, "claim", cfg.claimID, "error", result.Err)
		return true
	}

	switch result.Kind {
	case agentloop.ResultCancelled:
		return s.parkConversationOpen(ctx, liveParkContext{
			orgID:          orgID,
			conversationID: conversationID,
			taskID:         task.ID,
			namespace:      namespace,
			claudeCwd:      claudeCwd,
			triggerType:    triggerType,
			creatorUserID:  creatorUserID,
			claimID:        cfg.claimID,
			reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
			runtime:        domain.ConversationRuntimeNative,
		}, "")

	case agentloop.ResultFailed:
		reason := "native agent loop failed"
		if result.Err != nil {
			reason = result.Err.Error()
		}
		return s.failConversation(orgID, conversationID, task.ID, cfg.claimID, triggerType, creatorUserID, reason, result.FailureKind)

	case agentloop.ResultParked:
		// The engagement stopped without concluding — a guard before a call,
		// or a stop reason only a person can resolve. The conversation is
		// resumable either way, so the snapshot must exist by the time the
		// status commits — parkConversationOpen owns that ordering.
		_ = s.parkConversationOpen(ctx, liveParkContext{
			orgID:          orgID,
			conversationID: conversationID,
			taskID:         task.ID,
			namespace:      namespace,
			claudeCwd:      claudeCwd,
			triggerType:    triggerType,
			creatorUserID:  creatorUserID,
			reason:         db.ParkIdle(),
			runtime:        domain.ConversationRuntimeNative,
		}, "")
		return false
	}

	// Concluded. Record the conversation's memory exactly as processCompletion
	// does — row presence means "the conversation terminated", NULL content
	// means the agent wrote no usable memory file.
	agentContent, fileState := readConversationMemory(claudeCwd, priorMemory)
	if _, err := s.taskMemory.UpsertAgentMemorySystem(context.WithoutCancel(ctx), orgID, conversationID, task.EntityID, cfg.blueprintRunID, agentContent); err != nil {
		delegateLog.Warn("upsert memory for conversation failed", "conversation", conversationID, "error", err)
	}
	if fileState != memoryFilePresent {
		delegateLog.Debug("no usable memory file at termination", "conversation", conversationID, "state", fileState)
	}
	s.attachConversationMemoryEntities(context.WithoutCancel(ctx), orgID, conversationID, task.EntityID)

	// Snapshot before the terminal write. A `continue` hands the shared
	// workspace to the next step and an `abort` leaves a message-resumable
	// conversation; both can be picked up on an executor that never held
	// this worktree, so the blob has to exist by the time the status commits.
	if err := s.snapshotWorkspace(ctx, orgID, conversationID, namespace, cfg.claimID, claudeCwd, "", domain.ConversationRuntimeNative); err != nil {
		delegateLog.Warn("snapshot workspace at native conclusion failed", "conversation", conversationID, "error", err)
	}

	// Only a stop_blueprint terminal carries a reason; concluding by stopping says
	// what it has to say in the summary.
	outcome := string(result.Outcome)
	outcomeReason := result.OutcomeReason

	// One fenced write for both trigger types — the fence's ownership check
	// stands in for the RLS pass the former synthetic-claims route provided,
	// the same trade the SDK completion path makes.
	//
	// costUSD is zero here on purpose, and it is not a gap: the native
	// runtime settles cost per assistant row at call time, so the ledger is
	// already complete. Passing a lump would double-count.
	bgCtx := context.WithoutCancel(ctx)
	updated, err := s.conversations.CompleteForClaimSystem(bgCtx, orgID, conversationID, cfg.claimID, "completed", 0, result.DurationMs, result.NumTurns, result.ResultSummary, outcome, outcomeReason, "")
	if err != nil {
		if errors.Is(err, db.ErrClaimReleased) {
			delegateLog.Error("engagement fenced out at conclusion; a successor owns the conversation",
				"conversation", conversationID, "claim", cfg.claimID)
			return true
		}
		delegateLog.Warn("record completion for conversation failed", "conversation", conversationID, "error", err)
	}

	broadcastStatus := "completed"
	if updated != nil {
		broadcastStatus = updated.Status
	}
	s.updateBreakerCounter(task.ID, triggerType, "completed")
	s.broadcastConversationUpdate(orgID, conversationID, broadcastStatus)
	s.recomputeTaskBoardColumn(orgID, task.ID)
	toast.Success(s.wsHub, orgID, fmt.Sprintf("Run %s completed", shortConversationID(conversationID)))
	return false
}
