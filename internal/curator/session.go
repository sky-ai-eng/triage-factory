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
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
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
	// terminal cancelled status BEFORE we let the database close out
	// from under it. Without the wait, a graceful shutdown would
	// race with the goroutine's last DB write and log spurious
	// "database is closed" errors.
	done chan struct{}

	// inFlightMu guards inFlightCancel and inFlightRequestID — the
	// per-message ctx is recreated for each agentproc.Run invocation
	// and the cancel button reads it from outside the goroutine.
	inFlightMu        sync.Mutex
	inFlightCancel    context.CancelFunc
	inFlightRequestID string
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

// dispatch processes one queued request under the requesting user's
// identity (item.orgID, item.creatorUserID). Each per-turn write is
// wrapped in stores.Tx.SyntheticClaimsWithTx so multi-mode RLS gates
// the row on the same (org_id, creator_user_id) pair the schema
// columns carry. Owns the row's lifecycle from queued → running →
// terminal; broadcasts each transition so the Projects page can
// update without re-fetching.
//
// Cancel ordering: msgCtx and inFlightCancel are registered BEFORE
// MarkRequestRunning so that by the time any external observer can
// see the row in `running` state, the cancel handle is already armed.
// Without this, a cancel that landed in the window between "row is
// running" and "inFlightCancel registered" would see a nil cancel
// handle and be a no-op — the goroutine would then run agentproc to
// completion, and even though the cancel handler also flips the row
// at the DB level, the goroutine's terminal write could clobber it.
// The SQL filter on CompleteRequest belt-and-suspenders that, but
// registering early closes the race window in the first place.
func (s *projectSession) dispatch(item queueItem) {
	requestID := item.requestID
	if err := s.ctx.Err(); err != nil {
		// Shutdown raced ahead of the dequeue — flip the row to
		// cancelled so it doesn't sit forever in queued.
		s.markCancelled(item, "process shutting down")
		return
	}

	// Per-message ctx is a child of the session ctx. SIGKILL of the
	// in-flight subprocess goes through this; cancelInFlight fires
	// it from outside the goroutine.
	msgCtx, msgCancel := context.WithCancel(s.ctx)
	s.inFlightMu.Lock()
	s.inFlightCancel = msgCancel
	s.inFlightRequestID = requestID
	s.inFlightMu.Unlock()

	defer func() {
		s.inFlightMu.Lock()
		s.inFlightCancel = nil
		s.inFlightRequestID = ""
		s.inFlightMu.Unlock()
		msgCancel()
	}()

	// MarkRequestRunning + the immediate GetRequest read happen in
	// one short tx — same identity, single round-trip on Postgres.
	// Cancellation that lands during this tx is observed via msgCtx
	// after the wrap returns; the SQL filter on the UPDATE makes a
	// double-flip safe.
	markCtx := context.WithoutCancel(msgCtx)
	var req *domain.CuratorRequest
	wrapErr := s.curator.stores.Tx.SyntheticClaimsWithTx(markCtx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		if err := ts.Curator.MarkRequestRunning(markCtx, item.orgID, requestID); err != nil {
			return err
		}
		got, err := ts.Curator.GetRequest(markCtx, item.orgID, requestID)
		if err != nil {
			return err
		}
		req = got
		return nil
	})
	if wrapErr != nil {
		if errors.Is(wrapErr, sql.ErrNoRows) {
			// Already terminal — usually because Cancel raced before
			// pickup. Skip; the canceller already wrote the row.
			return
		}
		curatorLog.Warn("mark request running failed", "request", requestID, "error", wrapErr)
		s.failRequest(item, fmt.Sprintf("mark running: %v", wrapErr))
		return
	}
	if req == nil {
		s.failRequest(item, "request not found")
		return
	}
	s.curator.broadcastRequestUpdate(item.orgID, s.projectID, requestID, "running")

	// Cancel could have fired during MarkRunning's DB call. Check
	// before doing any further work so we don't pointlessly load
	// the project / spawn claude on a cancelled request.
	if msgCtx.Err() != nil {
		s.markCancelled(item, "user cancelled")
		return
	}

	cwd, err := ensureKnowledgeDir(item.orgID, s.projectID)
	if err != nil {
		s.failRequest(item, fmt.Sprintf("knowledge dir: %v", err))
		return
	}

	// Consume pending context-change rows AND read the project state in
	// one transaction. This is intentional: the diff at the bottom of
	// this function compares each pending row's baseline against the
	// project's current value, and if those two reads were independent
	// a PATCH that landed between them could be claimed here while the
	// envelope (built from the older read) showed values matching the
	// baseline — the diff would suppress the note, finalize on `done`,
	// and the user's delta would be lost. ConsumePendingContext returns
	// the project state alongside the claimed rows so every downstream
	// step (materialize, envelope render, diff) sees the same snapshot.
	//
	// Two-phase consume: rows are *claimed* here (consumed_at +
	// consumed_by_request_id stamped) but not deleted. On terminal
	// `done` we finalize (purge); on `cancelled` or `failed` we
	// revert (un-consume) so a transient agentproc failure doesn't
	// silently lose the user's deltas. The merge logic in
	// RevertPendingContext handles the case where a NEW PATCH lands
	// during dispatch.
	var (
		project *domain.Project
		pending []domain.CuratorPendingContext
	)
	consumeCtx := context.WithoutCancel(msgCtx)
	if err := s.curator.stores.Tx.SyntheticClaimsWithTx(consumeCtx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		p, ps, err := ts.Curator.ConsumePendingContext(consumeCtx, item.orgID, s.projectID, requestID)
		if err != nil {
			return err
		}
		project = p
		pending = ps
		return nil
	}); err != nil {
		s.failRequest(item, fmt.Sprintf("consume pending context: %v", err))
		return
	}
	if project == nil {
		s.failRequest(item, "project missing")
		return
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
	cloneTokenFor := func(ctx context.Context, owner string) string {
		return s.curator.cloneTokenFor(ctx, item.orgID, owner)
	}
	var roRepoMounts []agentproc.ReadOnlyRepoMount
	if agentproc.WillSandbox() {
		var releaseRepos func()
		roRepoMounts, releaseRepos = materializeSharedPinnedRepos(msgCtx, s.curator.stores.Repos,
			cloneTokenFor, item.orgID, s.projectID, project.PinnedRepos)
		defer releaseRepos()
	} else {
		materializePinnedRepos(msgCtx, s.curator.stores.Repos,
			cloneTokenFor, item.orgID, s.projectID, cwd, project.PinnedRepos)
	}
	if msgCtx.Err() != nil {
		// Cancel fired during repo refresh (one big bare clone can
		// take seconds on a fresh fetch). Don't waste cycles spawning
		// claude only to immediately cancel it.
		s.markCancelled(item, "user cancelled")
		s.revertPendingFor(item)
		return
	}

	// Per-(org, team) default model (SKY-389). project.TeamID scopes the
	// model to the project's owning team so a multi-team org honors each
	// team's choice; project is loaded + nil-checked just above. WithoutCancel
	// so a cancel mid-dispatch doesn't break the model read; the resolver has
	// its own fallback-on-error.
	model := s.curator.resolveModel(context.WithoutCancel(msgCtx), item.orgID, project.TeamID)

	// Pre-flight model check before we spawn claude. The Curator
	// constructor takes "" until config loads (mirroring Spawner),
	// and a SendMessage that lands during that window would
	// otherwise reach agentproc.Run and emit a confusing
	// "missing --model" error from claude itself. Fail the row up
	// front so the user sees a clear message.
	if model == "" {
		s.failRequest(item, "curator AI model is not configured")
		s.revertPendingFor(item)
		return
	}

	// Resolve selfBin so the allowlist's `Bash(<selfBin> exec *)`
	// pattern matches the same absolute path the agent will invoke
	// for SKY-221's "ticket as a spec" skill. Falling back to a
	// hard fail rather than running with a broken allowlist —
	// `os.Executable()` errors are vanishingly rare, but if one
	// happens we'd silently disable curator tooling.
	selfBin, err := os.Executable()
	if err != nil {
		s.failRequest(item, fmt.Sprintf("resolve own binary path: %v", err))
		s.revertPendingFor(item)
		return
	}

	envelope := envelopeInputs{
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

	message := req.UserInput
	contextNote := pendingChangesNote(pending, envelope)
	if contextNote != "" {
		message = contextNote + "\n\n" + message
		// Persist the rendered note as a curator_messages audit row
		// keyed to the consuming request. Frontend filters subtype
		// `context_change` out of rendered chat (SKY-226), but having
		// the row keyed to request_id makes the chat history
		// reproducible: replay shows exactly what the agent saw.
		// Best-effort — failing to write the audit row should not
		// abort the dispatch.
		auditMsg := &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "system",
			Subtype:   "context_change",
			Content:   contextNote,
		}
		auditCtx := context.WithoutCancel(msgCtx)
		if auditErr := s.curator.stores.Tx.SyntheticClaimsWithTx(auditCtx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
			_, err := ts.Curator.InsertMessage(auditCtx, item.orgID, auditMsg)
			return err
		}); auditErr != nil {
			curatorLog.Warn("insert context_change audit row failed", "request", requestID, "error", auditErr)
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
	// take effect on the next turn without a session reset (SKY-221).
	// Failure is non-fatal: the user's chat turn should still answer
	// even if skill writing hits a permission glitch.
	skillCtx := context.WithoutCancel(msgCtx)
	if err := materializeSpecSkill(skillCtx, s.curator.stores, item.orgID, item.creatorUserID, project, cwd); err != nil {
		curatorLog.Warn("materialize spec skill failed", "project", s.projectID, "error", err)
	}
	if err := materializeJiraFormattingSkill(cwd); err != nil {
		curatorLog.Warn("materialize jira formatting skill failed", "project", s.projectID, "error", err)
	}

	// In-sandbox exec daemon (TFAC-61). The curator envelope advertises a real
	// `triagefactory exec gh|jira` surface, so in multi mode the jailed agent
	// needs the per-run agenthost socket to answer those calls host-side —
	// without it, exec would fail in the jail (no DB, no keychain). Mirrors the
	// spawner (internal/delegate/run.go). The closure is only invoked in the
	// sandbox branch (multi+linux); local/non-sandbox runs never call it.
	// RunID is the curator request id (unique per turn → unique socket);
	// identity is the requesting user's (org, user). IsEventTriggered is false
	// — every curator turn is user-driven.
	startAgentHost := func() (sandbox.Mount, io.Closer, error) {
		hd, mount, err := agenthost.Start(s.curator.stores, agenthost.RunInfo{
			OrgID:            item.orgID,
			UserID:           item.creatorUserID,
			RunID:            requestID,
			IsEventTriggered: false,
		})
		if err != nil {
			return sandbox.Mount{}, nil, err
		}
		return mount, hd, nil
	}

	outcome, runErr := s.curator.runAgent(msgCtx, agentproc.RunOptions{
		Cwd:          cwd,
		Model:        model,
		SessionID:    project.CuratorSessionID,
		Message:      message,
		SystemPrompt: systemPrompt,
		AllowedTools: agentproc.BuildAllowedTools(selfBin),
		AddDirs:      addDirs,
		ExtraEnv: []string{
			"TRIAGE_FACTORY_CURATOR_PROJECT_ID=" + s.projectID,
			"TRIAGE_FACTORY_CURATOR_REQUEST_ID=" + requestID,
		},
		TraceID:            requestID,
		OrgID:              item.orgID,
		Secrets:            s.curator.getSecrets(),
		StartAgentHost:     startAgentHost,
		ReadOnlyRepoMounts: roRepoMounts,
	}, newRequestSink(s.curator, s.projectID, requestID, item.orgID, item.creatorUserID))

	// Cancellation observed → terminal cancelled status. Distinguish
	// between request-level cancellation and broader session/project
	// shutdown so the recorded terminal reason is accurate. Pending
	// rows are reverted so the next user message picks them up again.
	if msgCtx.Err() != nil {
		cancelReason := "user cancelled"
		if s.ctx.Err() != nil {
			cancelReason = "session cancelled"
		}
		s.markCancelled(item, cancelReason)
		s.revertPendingFor(item)
		return
	}

	if runErr != nil && (outcome == nil || outcome.Result == nil) {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		s.failRequest(item, fmt.Sprintf("%v\nstderr: %s", runErr, stderr))
		s.revertPendingFor(item)
		return
	}

	if outcome == nil || outcome.Result == nil {
		s.failRequest(item, "claude exited without producing a result event")
		s.revertPendingFor(item)
		return
	}

	status := "done"
	errMsg := ""
	if outcome.Result.IsError {
		status = "failed"
		errMsg = outcome.Result.Result
	}
	completeCtx := context.WithoutCancel(msgCtx)
	var flipped bool
	completeErr := s.curator.stores.Tx.SyntheticClaimsWithTx(completeCtx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.CompleteRequest(
			completeCtx, item.orgID, requestID, status, errMsg,
			outcome.Result.CostUSD, outcome.Result.DurationMs, outcome.Result.NumTurns,
		)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	})
	if completeErr != nil {
		curatorLog.Warn("complete request failed", "request", requestID, "error", completeErr)
		// We don't know whether the row landed terminal. Revert the
		// pending rows on the conservative assumption that the agent
		// did not see them — if the row turns out to be `done` after
		// all, the worst case is the user gets a duplicate diff on
		// their next message, which is far better than silently
		// losing the deltas.
		s.revertPendingFor(item)
		return
	}
	if !flipped {
		// The row was already terminal — most likely a user cancel
		// landed during agentproc.Run and the handler beat us to the
		// DB. Don't broadcast a status change that doesn't match the
		// row's actual state; the cancel handler already broadcast
		// cancelled. Pending rows: the cancel path will revert them
		// when it observes msgCtx.Err() above, but we may have
		// reached this branch from a successful agentproc with the
		// cancel landing concurrently — revert here too as a
		// belt-and-suspenders for the "row was already cancelled
		// before our completion write" race.
		curatorLog.Warn("request already terminal, skipping completion broadcast", "request", requestID, "intended_status", status)
		s.revertPendingFor(item)
		return
	}
	if status == "done" {
		s.finalizePendingFor(item)
	} else {
		// Terminal `failed` from agentproc's IsError result: the agent
		// emitted a result event marking the turn as a failure. Treat
		// the same as a process-level failure for pending-row
		// purposes — user retry should re-see the deltas.
		s.revertPendingFor(item)
	}
	s.curator.broadcastRequestUpdate(item.orgID, s.projectID, requestID, status)
}

// finalizePendingFor purges every pending-context row consumed by this
// request. Best-effort logging — finalization failure leaves stale
// rows that the next user message will skip (they are already marked
// consumed) but does not poison the chat or block other dispatches.
func (s *projectSession) finalizePendingFor(item queueItem) {
	ctx := context.WithoutCancel(s.ctx)
	if err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		return ts.Curator.FinalizePendingContext(ctx, item.orgID, item.requestID)
	}); err != nil {
		curatorLog.Warn("finalize pending context failed", "request", item.requestID, "error", err)
	}
}

// revertPendingFor un-consumes every pending-context row claimed by
// this request so the next user message picks them up again. Also
// removes the curator_messages audit row keyed to this request so the
// chat history doesn't show a phantom "context noted" entry for a
// turn that never delivered the deltas.
func (s *projectSession) revertPendingFor(item queueItem) {
	ctx := context.WithoutCancel(s.ctx)
	if err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		if err := ts.Curator.RevertPendingContext(ctx, item.orgID, item.requestID); err != nil {
			return fmt.Errorf("revert pending: %w", err)
		}
		if err := ts.Curator.DeleteMessagesBySubtype(ctx, item.orgID, item.requestID, "context_change"); err != nil {
			return fmt.Errorf("delete audit: %w", err)
		}
		return nil
	}); err != nil {
		curatorLog.Warn("revert/delete pending context failed", "request", item.requestID, "error", err)
	}
}

// hasInFlight reports whether a curator request is currently running on this
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
// agentproc.Run return and writes the cancelled terminal status
// itself — cancelInFlight only sends the signal.
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
// returning. Reason becomes the error message on any in-flight row's
// terminal cancellation.
//
// Blocking on the goroutine's exit matters for graceful shutdown:
// Shutdown is called as part of process teardown, and the goroutine's
// terminal write to curator_requests must happen before the DB closes
// underneath it. The wait is bounded by the agentproc subprocess
// honoring ctx.Done() promptly (it does — exec.CommandContext
// SIGKILLs the process group) so callers don't have to time out.
func (s *projectSession) shutdown(reason string) {
	// Capture the in-flight request id before the goroutine has a
	// chance to clear it on its own ctx.Err observation, so the
	// terminal status carries the shutdown reason rather than the
	// goroutine's default.
	s.inFlightMu.Lock()
	inFlightID := s.inFlightRequestID
	s.inFlightMu.Unlock()

	s.stopAll()

	// If a request was in flight, flip it explicitly with the
	// reason. The goroutine's own ctx.Err handler may also flip
	// it; the status filter makes the second write a no-op.
	//
	// System-driven cancel via the admin-pool …System door: shutdown
	// fires outside any request/JWT context (the in-flight request's
	// identity was captured for the goroutine's claims-bound writes but
	// shutdown runs from the caller's goroutine, and the row may not
	// have been dequeued yet), so the RLS-gated app pool would reject
	// the UPDATE and strand the row in `running`. TFAC-64.
	if inFlightID != "" {
		if flipped, err := s.curator.stores.Curator.MarkRequestCancelledIfActiveSystem(context.Background(), s.orgID, inFlightID, reason); err == nil && flipped {
			s.curator.broadcastRequestUpdate(s.orgID, s.projectID, inFlightID, "cancelled")
		}
	}

	<-s.done
}

func (s *projectSession) markCancelled(item queueItem, reason string) {
	ctx := context.WithoutCancel(s.ctx)
	var flipped bool
	err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.MarkRequestCancelledIfActive(ctx, item.orgID, item.requestID, reason)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	})
	if err != nil {
		curatorLog.Warn("cancel request failed", "request", item.requestID, "error", err)
		return
	}
	if flipped {
		s.curator.broadcastRequestUpdate(item.orgID, s.projectID, item.requestID, "cancelled")
	}
}

func (s *projectSession) failRequest(item queueItem, errMsg string) {
	ctx := context.WithoutCancel(s.ctx)
	var flipped bool
	err := s.curator.stores.Tx.SyntheticClaimsWithTx(ctx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		f, err := ts.Curator.CompleteRequest(ctx, item.orgID, item.requestID, "failed", errMsg, 0, 0, 0)
		if err != nil {
			return err
		}
		flipped = f
		return nil
	})
	if err != nil {
		curatorLog.Warn("fail request failed", "request", item.requestID, "error", err)
		return
	}
	if !flipped {
		// Cancel raced ahead of the failure write. Cancelled wins;
		// the handler already broadcast that.
		return
	}
	s.curator.broadcastRequestUpdate(item.orgID, s.projectID, item.requestID, "failed")
}
