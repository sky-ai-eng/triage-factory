package curator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// projectSession is the per-project goroutine handle. One queue, one
// in-flight cancel handle, one ctx that bounds the whole goroutine's
// lifetime. The Curator type holds these in a map keyed by project id.
type projectSession struct {
	curator   *Curator
	projectID string
	// orgID is the project's owning tenant. Captured at session start
	// so shutdown() and any other code path that fires without an
	// in-scope queueItem can still scope broadcast events to the right
	// org. A project belongs to exactly one org for its lifetime, so
	// this is stable across the session.
	orgID string
	queue chan queueItem

	// ctx + stopAll bound the lifetime of the whole goroutine. Closed
	// during Curator.Shutdown or CancelProject; the goroutine drops
	// any in-flight subprocess via the per-message inFlightCancel and
	// then exits.
	ctx     context.Context
	stopAll context.CancelFunc

	// done closes when the run() goroutine returns. Shutdown blocks
	// on this so the process exits cleanly: the goroutine writes its
	// terminal claim release BEFORE we let the database close out
	// from under it. Without the wait, a graceful shutdown would
	// race with the goroutine's last DB write and log spurious
	// "database is closed" errors.
	done chan struct{}

	// inFlightMu guards the in-flight turn state — the per-message ctx is
	// recreated for each agentproc.Run invocation and the cancel button /
	// shutdown read it from outside the goroutine. inFlightConversationID
	// lets shutdown release the active claim without the queueItem in scope.
	inFlightMu             sync.Mutex
	inFlightCancel         context.CancelFunc
	inFlightRequestID      string
	inFlightConversationID string
}

// run drains the queue serially. Exits when ctx is cancelled (via
// shutdown) or the queue is closed. On exit, any in-flight subprocess
// has already been SIGKILLed via inFlightCancel (the dispatch path
// triggers it before exiting). Future SendMessage on the same project
// will spin up a fresh goroutine.
//
// Closing s.done on return is what unblocks Shutdown's wait — the
// pair guarantees a deterministic teardown order during process exit.
func (s *projectSession) run() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case item, ok := <-s.queue:
			if !ok {
				return
			}
			s.dispatch(item)
		}
	}
}

// availableSources resolves which event sources this org can reach, deciding
// which verb families the turn's envelope documents. A turn in an org with no
// Jira has no reason to be handed the Jira verbs.
//
// nil on any failure, which renderToolsReference reads as core's two — the
// superset, and the safe direction: the credential funnel refuses an
// unreachable verb with an error that names the reason, while a missing verb
// just makes the agent improvise.
func (s *projectSession) availableSources(item queueItem) []string {
	stores := s.curator.stores
	if stores.Tx == nil {
		return nil
	}
	var kinds []string
	if err := stores.Tx.SyntheticClaimsWithTx(s.ctx, item.orgID, item.creatorUserID, func(tx db.TxStores) error {
		var e error
		kinds, e = eventsource.AvailableKinds(s.ctx, tx, item.orgID)
		return e
	}); err != nil {
		curatorLog.Warn("resolve event-source availability for the curator envelope failed; documenting the core verb families",
			"org", item.orgID, "error", err)
		return nil
	}
	return kinds
}

// dispatch drives one ALREADY-CLAIMED turn under the requesting user's
// identity (item.orgID, item.creatorUserID). The claim loop minted
// item.claimID before handing the turn over, so this owns the engagement
// from delivery to release, never the claim's creation. Message writes wrap
// in stores.Tx.SyntheticClaimsWithTx so multi-mode RLS gates the rows
// through the conversation's private-visibility arm; claim writes ride the
// admin-pool System doors (tf_app cannot write claims). Each transition is
// broadcast so the Projects page updates without re-fetching.
//
// Cancel ordering: msgCtx and inFlightCancel are registered before any work
// that a cancel would want to interrupt, so a cancel arriving the instant
// the turn becomes observable has a handle to fire. The released_at IS NULL
// filter on the release is belt-and-suspenders (first terminal writer wins),
// but registering early closes the race window in the first place.
func (s *projectSession) dispatch(item queueItem) {
	if item.done != nil {
		defer close(item.done)
	}
	requestID := item.requestID
	if err := s.ctx.Err(); err != nil {
		// Shutdown raced ahead of the dequeue. The turn is claimed but
		// undelivered, so releasing the claim leaves it exactly as it
		// arrived — queued, unowned, and re-claimable by whoever is alive
		// next. That is the restart-survival property, not a cancellation.
		s.releaseForRedrive(item, "curator shut down before pickup")
		return
	}

	// Dead-letter gate: a turn whose prior pickups keep failing at BeginTurn
	// is poisoned input — every re-feed would just mint another doomed claim
	// (and leak it released-failed), so at the cap the turn retires
	// terminally instead of dispatching. Checked before admission so a
	// poisoned turn never waits on (or occupies) a capacity slot. A counter
	// read error fails open into a normal dispatch — worst case one more
	// ordinary attempt, never a stuck turn.
	if maxAttempts := s.curator.getTurnMaxAttempts(); maxAttempts > 0 {
		gateCtx := context.WithoutCancel(s.ctx)
		prior, lastErr, cntErr := s.curator.stores.Curator.FailedTurnAttemptsSystem(gateCtx, item.orgID, item.conversationID, item.messageID)
		if cntErr != nil {
			curatorLog.Warn("count failed turn attempts failed; dispatching anyway", "request", requestID, "error", cntErr)
		} else if prior >= maxAttempts {
			errMsg := fmt.Sprintf("curator turn failed %d times, giving up", prior)
			if lastErr != "" {
				errMsg += ": " + lastErr
			}
			matched, dlErr := s.curator.stores.Curator.DeadLetterTurnSystem(gateCtx, item.orgID, item.conversationID, item.messageID, errMsg)
			if dlErr != nil {
				curatorLog.Warn("dead-letter turn failed", "request", requestID, "error", dlErr)
				return
			}
			if matched {
				// Only the writer that actually retired the row announces —
				// a raced cancel already broadcast its own terminal state.
				curatorLog.Warn("dead-lettered poisoned curator turn", "request", requestID, "attempts", prior)
				s.curator.broadcastRequestUpdate(item.orgID, s.projectID, requestID, "failed")
			}
			return
		}
	}

	// Per-message ctx is a child of the session ctx. SIGKILL of the
	// in-flight subprocess goes through this; cancelInFlight fires
	// it from outside the goroutine.
	msgCtx, msgCancel := context.WithCancel(s.ctx)
	s.inFlightMu.Lock()
	s.inFlightCancel = msgCancel
	s.inFlightRequestID = requestID
	s.inFlightConversationID = item.conversationID
	s.inFlightMu.Unlock()

	defer func() {
		s.inFlightMu.Lock()
		s.inFlightCancel = nil
		s.inFlightRequestID = ""
		s.inFlightConversationID = ""
		s.inFlightMu.Unlock()
		msgCancel()
	}()

	// No admission gate and no claim mint: the claim loop did both before it
	// handed this turn over — it holds a concurrency slot for the whole
	// call, which is the same memory guardrail and semaphore a delegated run
	// passes through, and the claim it minted is the engagement this
	// dispatch drives.
	claimCtx := context.WithoutCancel(msgCtx)
	claimID := item.claimID

	// Deliver the turn: flip the user message delivered, consume every
	// pending injection:context row, and snapshot project + resume handle in
	// ONE tx — the diff below compares each consumed baseline against the
	// project state read here, so a PATCH landing mid-dispatch can't be
	// consumed while the note (built from an older read) suppresses it.
	var start *db.CuratorTurnStart
	wrapErr := s.curator.stores.Tx.SyntheticClaimsWithTx(claimCtx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		got, err := ts.Curator.BeginTurn(claimCtx, item.orgID, s.projectID, item.conversationID, item.messageID)
		if err != nil {
			return err
		}
		start = got
		return nil
	})
	if wrapErr != nil {
		if errors.Is(wrapErr, sql.ErrNoRows) {
			// The queued row is gone (user cancelled before pickup) or already
			// delivered (duplicate feed). Release the just-minted claim
			// quietly — the canceller already broadcast, and a duplicate feed
			// has nothing to run.
			_, _ = s.curator.stores.Curator.ReleaseActiveTurnSystem(claimCtx, item.orgID, item.conversationID, "cancelled", "queued turn withdrawn before pickup", 0, 0, 0)
			return
		}
		curatorLog.Warn("begin turn failed", "request", requestID, "error", wrapErr)
		s.releaseFailed(item, fmt.Sprintf("begin turn: %v", wrapErr))
		// A BeginTurn failure rolls its tx back, so the message is still
		// undelivered — and releasing the claim above is all it takes to
		// make the conversation claimable again, so the one claim loop is
		// the retry driver. The dead-letter gate at the top of dispatch
		// bounds that cycle.
		return
	}
	if start.Project == nil {
		s.releaseFailed(item, "project missing")
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}
	project := start.Project
	s.curator.broadcastRequestUpdate(item.orgID, s.projectID, requestID, "running")

	// Cancel could have fired during the claim/begin DB calls. Check
	// before doing any further work so we don't pointlessly load
	// the project / spawn claude on a cancelled turn.
	if msgCtx.Err() != nil {
		s.releaseCancelled(item, "user cancelled")
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}

	cwd, err := ensureKnowledgeDir(item.orgID, s.projectID)
	if err != nil {
		s.releaseFailed(item, fmt.Sprintf("knowledge dir: %v", err))
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}

	// Knowledge-base blob sync brackets (multi mode). fsnotify can drop
	// events, so every turn is bracketed: pull the store→disk diff before the
	// agent runs so it sees panel uploads and any writes another executor
	// made, and reconcile disk→store after runAgent returns (all exit paths)
	// so anything the executor's watcher missed still lands durably. Local
	// mode's KB on disk is already the truth — no blob store in the path — so
	// both are skipped and behavior is byte-identical.
	if kb := s.curator.getKB(); kb != nil && runmode.Current() == runmode.ModeMulti {
		kbDir := filepath.Join(cwd, "knowledge-base")
		if mErr := materializeProjectKB(msgCtx, kb, item.orgID, s.projectID, kbDir); mErr != nil {
			curatorLog.Warn("kb turn-start materialize failed", "project", s.projectID, "error", mErr)
		}
		defer func() {
			// WithoutCancel(s.ctx): the reconcile must complete even on a
			// cancelled turn or a shutting-down session — it is the durability
			// backstop. Registered after the materialize so it fires on every
			// exit path below, runAgent included.
			if rErr := reconcileProjectKB(context.WithoutCancel(s.ctx), kb, item.orgID, s.projectID, kbDir); rErr != nil {
				curatorLog.Warn("kb turn-end reconcile failed", "project", s.projectID, "error", rErr)
			}
		}()
	}

	// Executor path: stand the turn's network + credential sidecar up
	// BEFORE the pinned-repo materialize, so the host-side fetch routes through
	// the sidecar's git proxy while the orchestrator holds no credential. The
	// seam is wired only on a multi-mode executor pod (buildCuratorRuntime gated
	// on the executor runtime); control/all/local leave it nil (bringUp nil →
	// turnSidecar nil) and keep the in-process path below byte-identical.
	// Keyed by the conversation id — the claim-credentials channel's key, and
	// unique per active turn (one active claim per conversation).
	var turnSidecar TurnSidecar
	if bringUp := s.curator.getTurnSidecar(); bringUp != nil {
		sb, err := bringUp(msgCtx, item.orgID, item.conversationID, item.creatorUserID, project.TeamID, project.PinnedRepos)
		if err != nil {
			s.releaseFailed(item, fmt.Sprintf("bring up turn sandbox: %v", err))
			s.revertTurnContext(item, start.Consumed, 0)
			return
		}
		if sb != nil {
			turnSidecar = sb
			// Deferred immediately after a successful bring-up. releaseRepos
			// (deferred below, so registered LATER) runs BEFORE this Close under
			// LIFO — safe: both fire only after runAgent returns (the jail has
			// exited), and the shared-worktree refcount is independent of the
			// network/sidecar teardown.
			defer sb.Close()
		}
	}

	// Refresh pinned-repo worktrees before spawning the agent so its
	// view of the world matches upstream HEAD on the user-configured
	// branch (profile.BaseBranch || profile.DefaultBranch). Per-repo
	// failures are non-fatal: the agent still gets the project's
	// knowledge files plus whatever subset of repos materialized.
	//
	// Multi-mode (jailed) reads each pinned repo from a SHARED read-only
	// worktree per (org, repo) bind-mounted into the sandbox, so N concurrent
	// member sessions on the same repo share one on-disk checkout instead of N
	// (TFAC-61). The shared tree is refreshed host-side only when no jail is
	// reading it, and a reader is held for the life of the dispatch (released
	// on return, after agentproc.Run). Local mode keeps the per-project
	// worktree under <cwd>/repos (N=1, no jail, no sharing) on its existing path.
	//
	// The jail mounts only the worktree files (the bare is intentionally NOT
	// mounted — sharing one bare across orgs is a tenancy boundary), so the
	// agent inspects repos with its file tools (Read/Grep/Glob), and the
	// curator never depends on in-jail commit/push/fetch: pinned-repo fetches
	// run host-side here, and in-jail git history is out of scope (TFAC-71).
	// authFor builds the host-side fetch credential for a pinned repo. On the
	// executor the turn sandbox routes it through the sidecar's git proxy (no
	// orchestrator-held token); on all/local it injects the App token
	// from the live store (the prior path). turnSidecar is nil on all/local, so
	// the closure resolves to CloneAuthFor there exactly as before.
	authFor := func(ctx context.Context, owner, cloneURL string) worktree.CloneAuth {
		if turnSidecar != nil {
			return turnSidecar.GitCloneAuth(cloneURL)
		}
		return worktree.CloneAuthFor(cloneURL, s.curator.cloneTokenFor(ctx, item.orgID, owner))
	}
	var roRepoMounts []agentproc.ReadOnlyRepoMount
	if agentproc.WillSandbox() {
		var releaseRepos func()
		roRepoMounts, releaseRepos = materializeSharedPinnedRepos(msgCtx, s.curator.stores.Repos,
			authFor, item.orgID, s.projectID, project.PinnedRepos)
		defer releaseRepos()
	} else {
		materializePinnedRepos(msgCtx, s.curator.stores.Repos,
			authFor, item.orgID, s.projectID, cwd, project.PinnedRepos)
	}
	if msgCtx.Err() != nil {
		// Cancel fired during repo refresh (one big bare clone can
		// take seconds on a fresh fetch). Don't waste cycles spawning
		// claude only to immediately cancel it.
		s.releaseCancelled(item, "user cancelled")
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}

	// Per-(org, team) default model. project.TeamID scopes the
	// model to the project's owning team so a multi-team org honors each
	// team's choice; project is loaded + nil-checked just above. WithoutCancel
	// so a cancel mid-dispatch doesn't break the model read; the resolver has
	// its own fallback-on-error.
	model := s.curator.resolveModel(context.WithoutCancel(msgCtx), item.orgID, project.TeamID)

	// Pre-flight model check before we spawn claude. The Curator
	// constructor takes "" until config loads (mirroring Spawner),
	// and a SendMessage that lands during that window would
	// otherwise reach agentproc.Run and emit a confusing
	// "missing --model" error from claude itself. Fail the turn up
	// front so the user sees a clear message.
	if model == "" {
		s.releaseFailed(item, "curator AI model is not configured")
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}

	// And whether the org can authenticate anything at all, on the same
	// pre-flight footing and for the same reason: a turn that reached
	// agentproc.Run here would hand the subprocess an empty environment and let
	// the SDK pick up whatever the operator's shell holds, billing this org's
	// conversation to a credential nobody configured. The delegated-run and
	// background-job gates refuse it; a curator turn is the org's spend too.
	if orgSet, err := s.curator.stores.Orgs.GetSettingsSystem(
		context.WithoutCancel(msgCtx), item.orgID,
	); err != nil {
		// An unreadable settings row is a fault, not a configuration state —
		// the same distinction the model read above draws, and the reason this
		// does not quietly proceed.
		s.releaseFailed(item, "could not read organization settings")
		s.revertTurnContext(item, start.Consumed, 0)
		return
	} else if err := modelaccess.Ready(orgSet, runmode.Current() == runmode.ModeMulti); err != nil {
		s.releaseFailed(item, err.Error())
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}

	// Resolve selfBin so the allowlist's `Bash(<selfBin> exec *)`
	// pattern matches the same absolute path the agent will invoke
	// for the "ticket as a spec" skill. Falling back to a
	// hard fail rather than running with a broken allowlist —
	// `os.Executable()` errors are vanishingly rare, but if one
	// happens we'd silently disable curator tooling.
	selfBin, err := os.Executable()
	if err != nil {
		s.releaseFailed(item, fmt.Sprintf("resolve own binary path: %v", err))
		s.revertTurnContext(item, start.Consumed, 0)
		return
	}

	envelope := envelopeInputs{
		AvailableSources:   s.availableSources(item),
		ProjectName:        project.Name,
		ProjectDescription: project.Description,
		PinnedRepos:        project.PinnedRepos,
		JiraProjectKey:     project.JiraProjectKey,
		LinearProjectKey:   project.LinearProjectKey,
		// Sandbox-visible binary path: the envelope's {{BINARY_PATH}} feeds the
		// system prompt, which agentproc does NOT rewrite for the jail (only the
		// AllowedTools selfBin pattern is, via rewriteAllowedToolsForSandbox).
		// In multi mode the host binary is bind-mounted at sandboxTFBinary, so
		// telling the agent the host selfBin would ENOENT inside the jail and
		// every `exec gh|jira` would fail. AgentVisibleBinary returns the
		// in-jail path when sandboxed, the host path otherwise — mirrors the
		// spawner (internal/delegate/run.go).
		BinaryPath: agentproc.AgentVisibleBinary(selfBin),
	}
	systemPrompt := renderEnvelope(envelope)

	message := start.UserInput
	var auditMessageID int64
	contextNote := pendingChangesNote(start.Consumed, envelope)
	if contextNote != "" {
		message = contextNote + "\n\n" + message
		// Persist the rendered note as a system audit row on the
		// conversation, stamped with this turn's claim. The frontend filters
		// subtype `context_change` out of rendered chat, but the row makes
		// the history reproducible: replay shows exactly what the agent saw.
		// Its id is remembered so the failure/cancel revert can delete it —
		// history must not show a "context noted" entry for a turn that
		// never delivered the deltas. Best-effort — failing to write the
		// audit row should not abort the dispatch.
		auditMsg := &domain.Message{
			ConversationID: item.conversationID,
			UserID:         item.creatorUserID,
			ClaimID:        claimID,
			Role:           "system",
			Subtype:        "context_change",
			Content:        contextNote,
		}
		auditCtx := context.WithoutCancel(msgCtx)
		id, auditErr := s.curator.stores.Conversations.InsertMessageForClaimSystem(auditCtx, item.orgID, claimID, auditMsg)
		switch {
		case errors.Is(auditErr, db.ErrClaimReleased):
			// This turn's claim was released between the mint above and here,
			// so a successor engagement owns the conversation. Abandon the
			// turn without releasing or reverting anything — both would act
			// on the successor's turn, not this one.
			curatorLog.Error("claim fence refused this turn's first write — a successor owns the conversation; abandoning the turn without writing",
				"request", requestID, "conversation", item.conversationID, "claim_id", claimID, "error", auditErr)
			return
		case auditErr != nil:
			curatorLog.Warn("insert context_change audit row failed", "request", requestID, "error", auditErr)
		default:
			auditMessageID = id
		}
	}

	// Allow the rm guard (and other path-scoped tool checks) to reach
	// the knowledge-base + repos subtrees explicitly. By default Claude
	// Code's rm policy treats the cwd as the sole allowed dir; without
	// these the agent can read/write files via Edit/Write but cannot
	// delete obsolete knowledge notes.
	//
	// `repos` is listed in both modes for a uniform path model. In local mode
	// it holds the per-project worktrees (writable, reset each turn). In
	// sandbox mode the pinned repos are read-only bind mounts, so this add-dir
	// is inert for them — the ro mount, not the rm policy, is the write
	// boundary — and reads work regardless (the paths are under cwd).
	addDirs := []string{
		filepath.Join(cwd, "knowledge-base"),
		filepath.Join(cwd, "repos"),
	}

	// Materialize the project's spec-authorship prompt as a Claude Code
	// skill at <cwd>/.claude/skills/ticket-spec/SKILL.md. Written fresh
	// on every dispatch so prompt edits + per-project re-targeting both
	// take effect on the next turn without a session reset.
	// Failure is non-fatal: the user's chat turn should still answer
	// even if skill writing hits a permission glitch.
	skillCtx := context.WithoutCancel(msgCtx)
	if err := materializeSpecSkill(skillCtx, s.curator.stores, item.orgID, item.creatorUserID, project, cwd); err != nil {
		curatorLog.Warn("materialize spec skill failed", "project", s.projectID, "error", err)
	}
	if err := materializeJiraFormattingSkill(skillCtx, s.curator.stores, item.orgID, item.creatorUserID, project, cwd); err != nil {
		curatorLog.Warn("materialize jira formatting skill failed", "project", s.projectID, "error", err)
	}

	// In-sandbox exec daemon (TFAC-61). The curator envelope advertises a real
	// `triagefactory exec gh|jira` surface, so in multi mode the jailed agent
	// needs the per-turn agenthost socket to answer those calls host-side —
	// without it, exec would fail in the jail (no DB, no keychain). Mirrors the
	// spawner (internal/delegate/run.go). The closure is only invoked in the
	// sandbox branch (multi+linux); local/non-sandbox runs never call it.
	// ConversationID is the conversation id (unique per active turn → unique socket);
	// identity is the requesting user's (org, user). TeamID is the curated
	// project's owning team (this surface is project-scoped, not run-backed,
	// so project.TeamID is the canonical team here). IsEventTriggered is
	// false — every curator turn is user-driven.
	startAgentHost := func() (sandbox.Mount, io.Closer, error) {
		if turnSidecar != nil {
			// Executor path: the credential sidecar HOSTS the exec-verb
			// socket server (created at bring-up over the sealed bundle), so the
			// orchestrator only supplies the bind mount — it runs no in-process
			// daemon and keeps the hostile-input parser out of its own address
			// space, exactly like the delegated executor branch
			// (internal/delegate/run.go). Never reads the disabled secret store.
			return agenthost.SocketMountFor(item.conversationID), noopCloser{}, nil
		}
		// all/local: the orchestrator IS the credential holder, so it hosts the
		// socket server in-process over its live stores. PinnedRepos authorizes
		// the agent's exec-gh verbs against the project's pinned set — a curator
		// turn creates no conversation_worktrees rows the run path's gate would key on.
		hd, mount, err := agenthost.Start(s.curator.stores, agenthost.ConversationInfo{
			OrgID:            item.orgID,
			UserID:           item.creatorUserID,
			ConversationID:   item.conversationID,
			TeamID:           project.TeamID,
			IsEventTriggered: false,
			PinnedRepos:      project.PinnedRepos,
		}, nil)
		if err != nil {
			return sandbox.Mount{}, nil, err
		}
		return mount, hd, nil
	}

	// Executor path: launch the agent into the prebuilt sandbox network + proxy
	// env the turn sidecar produced (nil-safe accessors), so agentproc skips its
	// in-process credential resolution entirely — the disabled secret store is
	// never touched and the sealed bundle drives the sidecar's proxies. nil on
	// all/local, where agentproc allocates the network and resolves in-process.
	var prebuiltNet *sandbox.RunNetwork
	var prebuiltProxyEnv []string
	var ghChannel *agentproc.GHChannelParams
	if turnSidecar != nil {
		prebuiltNet = turnSidecar.Network()
		prebuiltProxyEnv = turnSidecar.JailEnv()
		ghChannel = turnSidecar.GHChannel(item.conversationID)
	}

	// msgCancel is the sink's kill switch for a fence trip: a turn whose
	// writes are being refused has to stop producing output, and cancelling
	// the message ctx is the same SIGKILL path a user cancel takes.
	sink := newTurnSink(s.curator, s.projectID, item.conversationID, requestID, claimID, item.orgID, item.creatorUserID, msgCancel)

	outcome, runErr := s.curator.runAgent(msgCtx, agentproc.RunOptions{
		Cwd:          cwd,
		Model:        model,
		SessionID:    start.SDKSessionID,
		Message:      message,
		SystemPrompt: systemPrompt,
		AllowedTools: agentproc.BuildAllowedTools(selfBin),
		AddDirs:      addDirs,
		ExtraEnv: []string{
			"TRIAGE_FACTORY_CURATOR_PROJECT_ID=" + s.projectID,
			"TRIAGE_FACTORY_CURATOR_REQUEST_ID=" + requestID,
		},
		TraceID:     requestID,
		OrgID:       item.orgID,
		Secrets:     s.curator.getSecrets(),
		LLMResolver: s.curator.getLLMResolver(),
		// A curator turn is an executor engagement like any delegated run, so
		// its jail's measured cost lands on its own claim at teardown — the
		// same channel, keyed by the id minted above rather than by looking
		// for the conversation's active claim (by then this one is released).
		ClaimID:              claimID,
		RecordSandboxActuals: s.curator.recordSandboxActuals,
		// PrebuiltNetwork/PrebuiltProxyEnv are set only on the executor path;
		// agentproc.Run skips in-process credential resolution whenever
		// PrebuiltNetwork is non-nil, so Secrets/LLMResolver above are inert
		// there (the sidecar's proxies serve the sealed bundle instead) and the
		// diff stays minimal — no branch on the RunOptions themselves.
		PrebuiltNetwork:    prebuiltNet,
		PrebuiltProxyEnv:   prebuiltProxyEnv,
		GHChannel:          ghChannel,
		StartAgentHost:     startAgentHost,
		ReadOnlyRepoMounts: roRepoMounts,
	}, sink)

	// Fenced out mid-turn: the claim was released and a successor engagement
	// is driving the conversation. Checked before the cancellation branch
	// because the fence is what fired that cancellation — falling through
	// would release the successor's claim as "user cancelled" and revert the
	// context rows its turn is working from. Nothing is written here, by
	// design; the successor owns the turn's disposition now.
	if sink.fenceTripped() {
		return
	}

	// Cancellation observed → release the claim cancelled. Distinguish
	// between turn-level cancellation and broader session/project
	// shutdown so the recorded terminal reason is accurate. Consumed
	// injection rows are reverted so the next user message picks them up
	// again.
	if msgCtx.Err() != nil {
		cancelReason := "user cancelled"
		if s.ctx.Err() != nil {
			cancelReason = "session cancelled"
		}
		s.releaseCancelled(item, cancelReason)
		s.revertTurnContext(item, start.Consumed, auditMessageID)
		return
	}

	if runErr != nil && (outcome == nil || outcome.Result == nil) {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		s.releaseFailed(item, fmt.Sprintf("%v\nstderr: %s", runErr, stderr))
		s.revertTurnContext(item, start.Consumed, auditMessageID)
		return
	}

	if outcome == nil || outcome.Result == nil {
		s.releaseFailed(item, "claude exited without producing a result event")
		s.revertTurnContext(item, start.Consumed, auditMessageID)
		return
	}

	claimOutcome := "completed"
	status := "done"
	errMsg := ""
	if outcome.Result.IsError {
		claimOutcome = "failed"
		status = "failed"
		errMsg = outcome.Result.Result
	}
	completeCtx := context.WithoutCancel(msgCtx)
	flipped, completeErr := s.curator.stores.Curator.ReleaseActiveTurnSystem(
		completeCtx, item.orgID, item.conversationID, claimOutcome, errMsg,
		outcome.Result.CostUSD, outcome.Result.DurationMs, outcome.Result.NumTurns,
	)
	if completeErr != nil {
		curatorLog.Warn("release turn failed", "request", requestID, "error", completeErr)
		// We don't know whether the claim landed released. Revert the
		// consumed rows on the conservative assumption that the agent
		// did not see them — if the turn turns out to be done after
		// all, the worst case is the user gets a duplicate diff on
		// their next message, which is far better than silently
		// losing the deltas.
		s.revertTurnContext(item, start.Consumed, auditMessageID)
		return
	}
	if !flipped {
		// The claim was already released — most likely a user cancel
		// landed during agentproc.Run and the handler beat us to the
		// DB. Don't broadcast a status that doesn't match; the cancel
		// path already broadcast cancelled. Revert here too as a
		// belt-and-suspenders for the "released before our completion
		// write" race.
		curatorLog.Warn("turn already released, skipping completion broadcast", "request", requestID, "intended_status", status)
		s.revertTurnContext(item, start.Consumed, auditMessageID)
		return
	}
	if status != "done" {
		// Terminal failed from agentproc's IsError result: the agent
		// emitted a result event marking the turn as a failure. Treat
		// the same as a process-level failure for injection-row
		// purposes — user retry should re-see the deltas. A done turn
		// needs no finalize: the consumed rows stay delivered, plain
		// transcript history.
		s.revertTurnContext(item, start.Consumed, auditMessageID)
	}
	s.curator.broadcastRequestUpdate(item.orgID, s.projectID, requestID, status)
}

// releaseForRedrive releases a claimed-but-undelivered turn's engagement
// without a user-visible terminal: the message stays queued, so the
// conversation is claimable again the instant the claim is gone and whoever
// is alive next drives it. This is how a turn survives a restart that lands
// between the claim and the delivery.
func (s *projectSession) releaseForRedrive(item queueItem, reason string) {
	ctx := context.WithoutCancel(s.ctx)
	if _, err := s.curator.stores.Curator.ReleaseActiveTurnSystem(
		ctx, item.orgID, item.conversationID, "requeued", reason, 0, 0, 0,
	); err != nil {
		curatorLog.Warn("release turn for re-drive failed", "request", item.requestID, "error", err)
	}
}

// releaseCancelled releases the conversation's active claim as cancelled,
// broadcasting only when this call won the terminal write (a racing
// handler-side cancel already broadcast).
func (s *projectSession) releaseCancelled(item queueItem, reason string) {
	ctx := context.WithoutCancel(s.ctx)
	flipped, err := s.curator.stores.Curator.ReleaseActiveTurnSystem(ctx, item.orgID, item.conversationID, "cancelled", reason, 0, 0, 0)
	if err != nil {
		curatorLog.Warn("cancel turn failed", "request", item.requestID, "error", err)
		return
	}
	if flipped {
		s.curator.broadcastRequestUpdate(item.orgID, s.projectID, item.requestID, "cancelled")
	}
}

// releaseFailed releases the conversation's active claim as failed. A
// no-flip means a racing cancel won — cancelled stands, and the canceller
// already broadcast.
func (s *projectSession) releaseFailed(item queueItem, errMsg string) {
	ctx := context.WithoutCancel(s.ctx)
	flipped, err := s.curator.stores.Curator.ReleaseActiveTurnSystem(ctx, item.orgID, item.conversationID, "failed", errMsg, 0, 0, 0)
	if err != nil {
		curatorLog.Warn("fail turn failed", "request", item.requestID, "error", err)
		return
	}
	if !flipped {
		return
	}
	s.curator.broadcastRequestUpdate(item.orgID, s.projectID, item.requestID, "failed")
}

// revertTurnContext un-consumes the injection rows this turn consumed and
// deletes its context_change audit row, so a retry re-sees the deltas and
// history doesn't show a note the agent never absorbed. Best-effort logging
// — a revert failure leaves consumed rows the next turn simply won't
// re-render, and must not poison the chat or block other dispatches.
func (s *projectSession) revertTurnContext(item queueItem, consumed []domain.CuratorContextChange, auditMessageID int64) {
	if len(consumed) == 0 && auditMessageID == 0 {
		return
	}
	ctx := context.WithoutCancel(s.ctx)
	if err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		return ts.Curator.RevertTurnContext(ctx, item.orgID, item.conversationID, consumed, auditMessageID)
	}); err != nil {
		curatorLog.Warn("revert turn context failed", "request", item.requestID, "error", err)
	}
}

// hasInFlight reports whether a curator turn is currently running on this
// session (a live subprocess). Used by Curator.InFlightProjectCount to count the
// sessions a team archive will force-stop (TFAC-448). inFlightRequestID is set
// before the message ctx is registered and cleared on the deferred teardown, so
// it tracks exactly the window where cancelInFlight has something to kill.
func (s *projectSession) hasInFlight() bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	return s.inFlightRequestID != ""
}

// cancelInFlight fires the active message's ctx if one exists.
// Called from Curator.Cancel (user click) and Curator.CancelProject
// (project delete). The goroutine observes msgCtx.Err() in its
// agentproc.Run return and releases the claim cancelled itself —
// cancelInFlight only sends the signal.
func (s *projectSession) cancelInFlight() {
	s.inFlightMu.Lock()
	cancel := s.inFlightCancel
	s.inFlightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// shutdown cancels the session ctx (kills any in-flight subprocess),
// stops the goroutine, and waits for it to fully drain before
// returning. Reason becomes the error on any in-flight claim's
// terminal release.
//
// Blocking on the goroutine's exit matters for graceful shutdown:
// Shutdown is called as part of process teardown, and the goroutine's
// terminal writes must happen before the DB closes underneath it. The
// wait is bounded by the agentproc subprocess honoring ctx.Done()
// promptly (it does — exec.CommandContext SIGKILLs the process group)
// so callers don't have to time out.
func (s *projectSession) shutdown(reason string) {
	// Capture the in-flight turn before the goroutine has a chance to
	// clear it on its own ctx.Err observation, so the terminal release
	// carries the shutdown reason rather than the goroutine's default.
	s.inFlightMu.Lock()
	inFlightID := s.inFlightRequestID
	inFlightConv := s.inFlightConversationID
	s.inFlightMu.Unlock()

	s.stopAll()

	// If a turn was in flight, release its claim explicitly with the
	// reason. The goroutine's own ctx.Err handler may also release;
	// the released_at IS NULL filter makes the second write a no-op.
	// Admin-pool System door — shutdown fires outside any request/JWT
	// context, and claim writes have no app-pool door anyway.
	if inFlightID != "" && inFlightConv != "" {
		if flipped, err := s.curator.stores.Curator.ReleaseActiveTurnSystem(context.Background(), s.orgID, inFlightConv, "cancelled", reason, 0, 0, 0); err == nil && flipped {
			s.curator.broadcastRequestUpdate(s.orgID, s.projectID, inFlightID, "cancelled")
		}
	}

	<-s.done
}

// noopCloser is an io.Closer that closes nothing — returned by the executor
// path's startAgentHost, where the credential sidecar owns the
// socket's lifecycle (torn down by the turn sandbox's Close, not this closer).
// Mirrors the delegate spawner's noopCloser.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }
