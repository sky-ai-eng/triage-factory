package curator

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Curator owns the per-project Claude Code chat session lifecycle.
// One instance per process; each project gets its own goroutine on
// first SendMessage. Cross-project messages run in parallel; same-
// project messages queue serially so the conversation history stays
// coherent.
//
// The per-project goroutine processes each turn under the requesting
// user's identity (curator_requests.creator_user_id), wrapping every
// store call in stores.Tx.SyntheticClaimsWithTx so multi-mode RLS
// policies on (org_id, creator_user_id) gate the writes.
type Curator struct {
	stores db.Stores
	wsHub  *websocket.Hub

	mu    sync.Mutex
	model string
	// Per-org run-credential seam, wired once at startup via
	// SetRunCredentialResolvers. modelFor supersedes the process-global
	// model above; secrets feeds RunOptions.Secrets. Tests leave both nil
	// and fall back to model / the ambient-subscription path.
	//
	// ghResolver is the per-(org, owner) GitHub credential source used to
	// authenticate the host-side pinned-repo worktree refresh
	// (materializePinnedRepos → EnsureCuratorWorktree) for private repos in
	// multi mode. Nil in tests / when unset — the refresh then runs with no
	// injected credential (the unauthenticated path).
	secrets    agentproc.SecretsReader
	llmResolve func(context.Context, string) (map[string]string, error) // brain-side role-aware LLM resolver (nil in local/tests).
	modelFor   func(context.Context, string, string) string
	ghResolver ghclient.Resolver
	sessions   map[string]*projectSession // projectID → goroutine handle

	// homer resolves the home executor for a project and whether to forward
	// (curator homing, spec §6.3). Wired ONLY on multi-mode control pods
	// (SetHoming); nil on local / role=all / executor, where SendMessage runs
	// in-process on this pod exactly as before. selfID is this pod's instance
	// id — the home stamped when a turn runs in-process (so the ownership-scoped
	// boot sweep can find it).
	homer  *Homer
	selfID string

	// doorbell publishes a cross-pod tf_ctl notification (SetDoorbell). Wired on
	// control pods to nudge the home executor's claim loop ("curator_new") and
	// to route a cross-pod cancel ("curator_cancel"). nil elsewhere — the
	// executor claim loop's backstop poll and the DB-level cancel flip cover a
	// missed doorbell, so this is a latency optimization, never a correctness
	// dependency.
	doorbell func(kind, orgID, projectID string)

	// admitTurn gates one turn's execution through the host's shared agent
	// capacity — in production the delegation spawner's AcquireTurnSlot, so a
	// curator turn waits out the same memory guardrail and occupies the same
	// concurrency slot a delegated run would (which also puts it in the
	// instance heartbeat's occupancy snapshot). The wait happens with the
	// turn's row still 'queued' and its cancel handle armed, so a cancel
	// during the wait lands normally. nil (tests) admits immediately.
	admitTurn func(ctx context.Context) (release func(), err error)

	// runAgent dispatches one agent turn. Defaults to agentproc.Run in
	// New; the multi-mode capstone pgtest (TFAC-65) overrides it to drive
	// SendMessage → dispatch → terminal without spawning the claude
	// subprocess or booting the gVisor jail. Production never reassigns it,
	// so the live path is byte-for-byte the direct agentproc.Run call.
	runAgent func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error)

	// closed is set during Shutdown; SendMessage rejects after this.
	closed bool
}

// New constructs a Curator. Call
// stores.Curator.CancelOrphanedNonTerminalRequests at startup
// before constructing — see main.go wiring.
//
// stores carries the Tx runner (for SyntheticClaimsWithTx wraps), the
// CuratorStore (per-turn writes plus the admin-pool …System cancel
// variants the system-driven cleanup paths use), the ProjectStore
// (session-id bookkeeping), the PromptStore (skill materialization),
// and the RepoStore (pinned-repo materialization). Every row read/write
// the curator issues now goes through a claims-bound tx or an
// admin-pool door, so no raw *sql.DB handle is retained (TFAC-64).
func New(stores db.Stores, wsHub *websocket.Hub, model string) *Curator {
	return &Curator{
		stores:   stores,
		wsHub:    wsHub,
		model:    model,
		sessions: make(map[string]*projectSession),
		runAgent: agentproc.Run,
	}
}

// SetRunCredentialResolvers wires the per-org run-credential seam: the GitHub
// credential resolver (used to authenticate private pinned-repo refreshes
// host-side), the per-org LLM-credential reader (nil in
// local → ambient subscription; system-door reader in multi), and the
// per-(org, team) default-model resolver. Both modes resolve through these so
// credential resolution stops branching on mode. Set once at startup,
// post-New. Any may be nil; resolveModel falls back to the constructor model,
// a nil secrets reader yields the ambient-subscription fallback, and a nil
// resolver makes the pinned-repo refresh credential-free (the prior
// path). Signature mirrors Spawner.SetRunCredentialResolvers for symmetry.
func (c *Curator) SetRunCredentialResolvers(resolver ghclient.Resolver, secrets agentproc.SecretsReader, modelFor func(context.Context, string, string) string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ghResolver = resolver
	c.secrets = secrets
	c.modelFor = modelFor
}

// SetLLMResolver wires the shared brain-side LLM-credential resolver
// (internal/llmcred, TFAC-616) so a role-mode Bedrock org's curator turns
// mint short-lived STS session creds — a brain-bound mint (control-process
// egress, no network condition). nil in local mode (ambient) and in tests.
func (c *Curator) SetLLMResolver(fn func(context.Context, string) (map[string]string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.llmResolve = fn
}

// getLLMResolver returns the wired resolver func under the lock, matching
// getSecrets' race-free accessor shape.
func (c *Curator) getLLMResolver() func(context.Context, string) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.llmResolve
}

// SetHoming wires the curator homing seam (spec §6.3): the Homer that resolves
// a project's home executor and this pod's instance id. Called once at startup
// on a multi-mode control pod only — where a chat POST lands but the turn must
// execute on the home executor. Left unset on local / role=all (in-process,
// unchanged) and on executors (which run the claim loop, not SendMessage).
func (c *Curator) SetHoming(homer *Homer, selfID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.homer = homer
	c.selfID = selfID
}

// SetDoorbell wires the cross-pod tf_ctl publisher used to nudge the home
// executor's claim loop and route cross-pod cancels. Control pods only; nil
// elsewhere degrades to backstop-poll latency, never lost work.
func (c *Curator) SetDoorbell(fn func(kind, orgID, projectID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.doorbell = fn
}

// SetAdmission wires the shared turn-admission gate — the delegation
// spawner's AcquireTurnSlot in production. Set once at startup on every pod
// that can execute a turn in-process; without it a burst of concurrent
// project turns fans into sandboxes the host's capacity accounting never
// sees.
func (c *Curator) SetAdmission(fn func(ctx context.Context) (release func(), err error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.admitTurn = fn
}

// getAdmitTurn returns the wired admission gate under the lock, matching
// getSecrets' race-free accessor shape.
func (c *Curator) getAdmitTurn() func(context.Context) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.admitTurn
}

// homeMode is the routing decision for one turn.
type homeMode int

const (
	// homeLocal runs the turn in-process on this pod. No homer is wired: local
	// mode or role=all, where the curator sandboxes here as it always has.
	homeLocal homeMode = iota
	// homeForward routes the turn to a remote home executor (control pods).
	homeForward
	// homeUnavailable means a homer is wired (a capless control pod) but no
	// executor is eligible to run the turn, so it cannot execute anywhere — the
	// caller fails it fast and legibly.
	homeUnavailable
)

// ErrNoCuratorExecutor is returned by SendMessage on a control pod when no
// executor is eligible to run the turn. Control is capless (it cannot sandbox a
// curator turn itself), so this is a hard, legible failure rather than a silent
// queue — the operator must add or restore an executor.
var ErrNoCuratorExecutor = errors.New("no curator executor is available to run this turn; add or restore an executor")

// resolveHome decides where this turn runs. No homer (local / role=all) →
// homeLocal, home "" (in-process, home_instance_id NULL — the untouched path).
// A wired homer (control) → homeForward with the executor id, or homeUnavailable
// when the execution plane is down.
func (c *Curator) resolveHome(ctx context.Context, orgID, projectID string) (homeInstanceID string, mode homeMode) {
	c.mu.Lock()
	homer := c.homer
	c.mu.Unlock()
	if homer == nil {
		return "", homeLocal
	}
	home, forward, ok := homer.Resolve(ctx, orgID, projectID)
	if !ok {
		return "", homeUnavailable
	}
	if forward {
		return home, homeForward
	}
	// forward=false with ok=true means home == self; unreachable on a control
	// pod (it is never an eligible executor), so treat it as in-process.
	return home, homeLocal
}

// ringDoorbell publishes a cross-pod tf_ctl notification if a publisher is
// wired. Best-effort — a nil publisher or a publish error only costs backstop-
// poll latency.
func (c *Curator) ringDoorbell(kind, orgID, projectID string) {
	c.mu.Lock()
	fn := c.doorbell
	c.mu.Unlock()
	if fn != nil {
		fn(kind, orgID, projectID)
	}
}

// cloneTokenFor resolves the App installation token for a host-side fetch of
// a pinned repo owned by owner, via the GitHub resolver. Multi-mode only, to
// match the spawner (Spawner.resolveCloneToken) and keep local pinned-repo
// refreshes on their existing path (operator SSH key / anonymous HTTPS) —
// local behavior is unchanged by this ticket. Returns "" when local, when no
// resolver is wired, or when resolution fails; the refresh then runs with no
// injected credential. Logged-not-fatal: the refresh is already best-effort
// per repo.
func (c *Curator) cloneTokenFor(ctx context.Context, orgID, owner string) string {
	if runmode.Current() == runmode.ModeLocal {
		return ""
	}
	c.mu.Lock()
	resolver := c.ghResolver
	c.mu.Unlock()
	if resolver == nil {
		return ""
	}
	tok, err := resolver.TokenFor(ctx, orgID, owner)
	if err != nil {
		curatorLog.Warn("resolve clone token failed, refresh proceeds unauthenticated", "org", orgID, "owner", owner, "error", err)
		return ""
	}
	return tok.Value
}

// resolveModel resolves the project-owning team's default model via the
// resolver, falling back to the constructor-supplied model when no
// resolver is wired (tests) or the resolver returns empty. teamID is the
// project's owning team (a project belongs to exactly one team), so a
// multi-team org honors each team's model choice; empty falls back to the
// org default team inside the resolver.
func (c *Curator) resolveModel(ctx context.Context, orgID, teamID string) string {
	c.mu.Lock()
	fn := c.modelFor
	fallback := c.model
	c.mu.Unlock()
	if fn != nil {
		if m := fn(ctx, orgID, teamID); m != "" {
			return m
		}
	}
	return fallback
}

// getSecrets returns the per-org LLM-credential reader threaded into
// RunOptions.Secrets: nil in local (ambient subscription), the system-door
// reader in multi.
func (c *Curator) getSecrets() agentproc.SecretsReader {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.secrets
}

// queueItem carries everything the per-project goroutine needs to
// dispatch a turn under the requesting user's identity. orgID +
// creatorUserID are captured at enqueue time (SendMessage's handler
// context) so the goroutine doesn't have to read the curator_requests
// row again just to figure out who to bill the writes to — that read
// would itself need claims set under Postgres RLS, creating a chicken-
// and-egg problem.
type queueItem struct {
	requestID     string
	orgID         string
	creatorUserID string
}

// SendMessage records the user's input as a queued curator_request,
// hands it to the project's goroutine, and returns the request id.
// The HTTP handler returns 202 + this id; the per-project goroutine
// flips status to running on pickup and to terminal on completion.
//
// orgID + creatorUserID identify the requesting user — every write
// the goroutine produces for this turn (running flip, agent stream
// messages, pending-context consume/finalize/revert, terminal status)
// runs inside Stores.Tx.SyntheticClaimsWithTx with these claims set
// so multi-mode RLS attributes the rows correctly. In local mode the
// handler passes runmode.LocalDefaultOrgID + LocalDefaultUserID; the
// D9 sweep will replace those with values from request
// context.
//
// The user's content is required (empty/whitespace-only is rejected
// at the handler before reaching us); the project must exist
// (handler checks). This function does not validate either —
// callers are trusted to pre-check.
//
// Shutdown safety: getOrStartSession holds c.mu and refuses to
// hand back a session once c.closed flips, so a SendMessage that
// races Shutdown either (a) wins the lock first and gets a session
// that Shutdown then tears down — the session ctx kills the dispatch
// before it spawns claude — or (b) loses the lock and gets nil back,
// in which case the persisted row is flipped to cancelled before we
// return. Either way, no message reaches a non-running goroutine.
func (c *Curator) SendMessage(ctx context.Context, projectID, orgID, creatorUserID, content string) (string, error) {
	// Resolve where this turn executes before minting the row so the home is
	// stamped atomically at creation (curator homing, spec §6.3). local /
	// role=all / no-homer leaves it "" (the untouched in-process path); a
	// capless control pod with no eligible executor fails fast rather than
	// creating a row that can never run.
	homeInstanceID, mode := c.resolveHome(ctx, orgID, projectID)
	if mode == homeUnavailable {
		return "", ErrNoCuratorExecutor
	}

	var requestID string
	if err := c.stores.Tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, creatorUserID, homeInstanceID, content)
		if err != nil {
			return err
		}
		requestID = id
		return nil
	}); err != nil {
		return "", fmt.Errorf("create curator request: %w", err)
	}

	if mode == homeForward {
		// Homed to a remote executor — do NOT run a session on this control
		// pod. The durable queued row (home_instance_id = the executor) IS the
		// delivery; the doorbell just nudges the home's claim loop so it need
		// not wait for its backstop poll. Output streams back to this browser
		// over the WS backplane from wherever the turn runs, so nothing else is
		// needed here.
		c.broadcastRequestUpdate(orgID, projectID, requestID, "queued")
		c.ringDoorbell("curator_new", orgID, projectID)
		return requestID, nil
	}

	session := c.getOrStartSession(orgID, projectID)
	if session == nil {
		// Best-effort cancel via the admin-pool …System door — the
		// "curator is shut down" path runs from the handler goroutine
		// (not the per-project goroutine) with no synthetic-claims tx in
		// scope, so the RLS-gated app pool would reject the UPDATE and
		// leave the freshly created row dangling. WithoutCancel so a
		// canceled/timed-out request ctx can't skip the terminal
		// write — that would re-introduce the dangling row. TFAC-64.
		_, _ = c.stores.Curator.MarkRequestCancelledIfActiveSystem(context.WithoutCancel(ctx), orgID, requestID, "curator is shut down")
		return "", errors.New("curator is shut down")
	}

	item := queueItem{requestID: requestID, orgID: orgID, creatorUserID: creatorUserID}
	select {
	case session.queue <- item:
		c.broadcastRequestUpdate(orgID, projectID, requestID, "queued")
		return requestID, nil
	default:
		// Queue is full — should not happen at the per-project depth
		// we configure, but if it ever does, fail the row up-front
		// rather than blocking the HTTP handler. Admin-pool …System door
		// for the same no-claims reason as the shutdown path above;
		// WithoutCancel so a canceled request ctx can't skip the
		// terminal write and strand the row in `queued`. TFAC-64.
		_, _ = c.stores.Curator.CompleteRequestSystem(context.WithoutCancel(ctx), orgID, requestID, "failed", "curator queue full", 0, 0, 0)
		c.broadcastRequestUpdate(orgID, projectID, requestID, "failed")
		return "", errors.New("curator queue is full")
	}
}

// EnqueueClaimed feeds an already-created curator_requests row (claimed off the
// home executor's claim loop, spec §6.3) into its per-project session goroutine
// — the executor-side counterpart of SendMessage's in-process enqueue, minus
// the row creation (the control pod already minted the row and stamped the
// home). Returns true when the turn was handed to a session; false when the
// curator is shut down or the per-project queue is momentarily full, so the
// claim loop leaves the row queued and retries on its next scan.
//
// Idempotency: feeding the same request twice is safe — the second dispatch's
// MarkRequestRunning sees a non-queued row (sql.ErrNoRows) and returns without
// re-running the agent — so a duplicated doorbell or a scan/mark race never
// double-executes a turn.
func (c *Curator) EnqueueClaimed(orgID, projectID, requestID, creatorUserID string) bool {
	session := c.getOrStartSession(orgID, projectID)
	if session == nil {
		return false // curator shut down
	}
	item := queueItem{requestID: requestID, orgID: orgID, creatorUserID: creatorUserID}
	select {
	case session.queue <- item:
		c.broadcastRequestUpdate(orgID, projectID, requestID, "queued")
		return true
	default:
		return false // queue full — leave the row queued for the next scan
	}
}

// Cancel fires the per-project cancel func, terminating the in-flight
// agentproc.Run. The goroutine flips the row to cancelled when it
// observes ctx.Err(). Returns nil even if no in-flight goroutine
// exists — the typical race between user click and goroutine
// scheduling means "nothing to cancel" is a routine outcome rather
// than an error. Caller decides whether to surface it as 404 by
// checking InFlightCuratorRequestForProject first.
//
// Cross-pod (curator homing, spec §6.3): on a control pod the live session runs
// on the home executor, not here, so the local cancelInFlight is usually a
// no-op. The "curator_cancel" doorbell reaches the home executor's own Cancel
// (broadcast + self-filter: only the pod holding the project's session has
// something to kill), which SIGKILLs the subprocess promptly. The handler's
// DB-level MarkRequestCancelledIfActive is the backstop if the doorbell is
// dropped — the turn then runs to completion and its terminal write is a no-op
// (cancelled wins). A local session (role=all, or the control in-process
// fallback) is cancelled directly, so both paths always fire.
func (c *Curator) Cancel(orgID, projectID string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	c.mu.Unlock()
	if ok {
		session.cancelInFlight()
	}
	// Route to the home executor too (no-op when no doorbell is wired).
	c.ringDoorbell("curator_cancel", orgID, projectID)
}

// CancelLocal fires only the in-process session cancel, without ringing the
// cross-pod doorbell — the entry point the tf_ctl "curator_cancel" dispatch
// calls on an executor so a delivered cancel doesn't echo back onto the bus.
func (c *Curator) CancelLocal(projectID string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	c.mu.Unlock()
	if ok {
		session.cancelInFlight()
	}
}

// InFlightProjectCount returns how many of projectIDs have a curator request
// currently running (a live subprocess). The team-archive preview + cascade
// (TFAC-448) report it as the number of curator sessions the archive will
// force-stop. Counts only running work, not idle sessions or queued-but-unstarted
// rows — the "active work" the modal warns about. Safe to call with a nil/empty
// slice (returns 0).
func (c *Curator) InFlightProjectCount(projectIDs []string) int {
	if len(projectIDs) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, pid := range projectIDs {
		if s, ok := c.sessions[pid]; ok && s.hasInFlight() {
			n++
		}
	}
	return n
}

// CancelProject is the project-teardown hook: cancel any in-flight
// request, drain queued requests to cancelled (so the project doesn't
// have ghost queued rows), and stop the goroutine so nothing runs after
// the teardown.
//
// reason is recorded on the cancelled rows' error_msg and surfaced in
// the cancellation broadcast — "project deleted" for the project-delete
// path, "team archived" for the team-archive force-stop (TFAC-448) — so
// the audit trail distinguishes the two.
//
// Called BEFORE the projects DELETE (delete path) so the FK cascade
// doesn't race a still-running goroutine. The DB cascade (curator_requests
// → curator_messages) takes care of row removal once the project row is
// dropped.
//
// orgID is the project's owning tenant, threaded through to
// broadcast events so the cancellation toast/update only reaches
// connections authed against that org.
func (c *Curator) CancelProject(orgID, projectID, reason string) {
	c.mu.Lock()
	session, ok := c.sessions[projectID]
	if ok {
		delete(c.sessions, projectID)
	}
	c.mu.Unlock()

	if ok {
		session.shutdown(reason)
	}
	// Drain queued rows at the DB level so the FK cascade on project delete
	// doesn't leave behind status confusion (catches rows homed to executors
	// too — QueuedRequestsForProjectSystem is org-scoped, not home-scoped).
	c.cancelQueuedRows(orgID, projectID, reason)

	// Curator homing (spec §6.3): when the live session lives on a remote
	// executor, the local session teardown above is a no-op — route a cross-pod
	// cancel so the home executor SIGKILLs its subprocess promptly rather than
	// running the turn to completion against a row the FK cascade is about to
	// delete. Then drop the home mapping so it can't outlive the project (no FK
	// to projects — curator_homes is placement coordination).
	c.ringDoorbell("curator_cancel", orgID, projectID)
	if err := c.stores.CuratorHomes.Clear(context.Background(), orgID, projectID); err != nil {
		curatorLog.Warn("clear curator home on project teardown failed", "org", orgID, "project", projectID, "error", err)
	}
}

// Shutdown stops every per-project goroutine and rejects further
// SendMessage calls. Called from main.go on graceful shutdown so
// in-flight CC subprocesses are SIGKILLed before the process exits.
// In-flight rows land as cancelled with reason "process shutting
// down"; queued rows are not resumed by the next process and will
// instead be cancelled on restart by orphaned-request cleanup.
func (c *Curator) Shutdown() {
	c.mu.Lock()
	c.closed = true
	sessions := c.sessions
	c.sessions = make(map[string]*projectSession)
	c.mu.Unlock()

	for _, s := range sessions {
		s.shutdown("process shutting down")
	}
}

// cancelQueuedRows flips never-picked-up queued curator_requests for a
// project to cancelled. Called from CancelProject (handler-side) and
// from the fallback path in SendMessage when the curator is shut down.
//
// Only `queued` rows are drained — `running` rows are covered by two
// orthogonal paths that both complete before this runs: the startup
// sweep (CancelOrphanedNonTerminalRequests) retires a previous
// process's in-flight rows, and CancelProject's session.shutdown waits
// (<-s.done) for the goroutine to mark the current in-flight row
// terminal before this drain fires. So no `running` row is observable
// here.
//
// System-driven cancel: there is no live request/JWT context here (the
// rows may have been enqueued by any user, possibly in a previous
// process), so both the list and the per-row cancel route through the
// admin-pool …System doors. Under multi-mode RLS the app pool would
// hide the queued rows from the SELECT and reject the UPDATE, leaving
// them dangling past the project FK cascade. The row already records
// its creator (creator_user_id) for audit; a system cancel isn't a user
// action, so it doesn't reconstruct per-user claims. orgID scopes the
// admin-pool query. TFAC-64.
func (c *Curator) cancelQueuedRows(orgID, projectID, reason string) {
	ctx := context.Background()
	queued, err := c.stores.Curator.QueuedRequestsForProjectSystem(ctx, orgID, projectID)
	if err != nil {
		curatorLog.Warn("list queued requests failed", "project", projectID, "error", err)
		return
	}
	for _, req := range queued {
		flipped, err := c.stores.Curator.MarkRequestCancelledIfActiveSystem(ctx, orgID, req.ID, reason)
		if err != nil {
			curatorLog.Warn("cancel queued request failed", "request", req.ID, "error", err)
			continue
		}
		if flipped {
			c.broadcastRequestUpdate(orgID, projectID, req.ID, "cancelled")
		}
	}
}

// getOrStartSession returns the per-project session, starting a new
// goroutine if needed. Holding c.mu across the start prevents two
// concurrent SendMessage calls for the same project from spawning
// two goroutines, and folds the closed-check into the same critical
// section as the map mutation so a racing Shutdown can't observe a
// "no sessions to stop" snapshot while a fresh session is being
// inserted. Returns nil iff the curator has been shut down — caller
// flips the persisted row to cancelled so it doesn't dangle.
func (c *Curator) getOrStartSession(orgID, projectID string) *projectSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if existing, ok := c.sessions[projectID]; ok {
		return existing
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &projectSession{
		curator:   c,
		projectID: projectID,
		orgID:     orgID,
		queue:     make(chan queueItem, sessionQueueDepth),
		ctx:       ctx,
		stopAll:   cancel,
		done:      make(chan struct{}),
	}
	c.sessions[projectID] = session
	go session.run()
	return session
}

// sessionQueueDepth bounds how many user messages can be queued for
// one project ahead of the active one. Set generously for human-
// driven chat — a person can't reasonably backlog more than a
// handful of follow-ups before the answer to the first arrives.
const sessionQueueDepth = 64
