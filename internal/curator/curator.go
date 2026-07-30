package curator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Curator owns the per-project Claude Code chat session lifecycle.
// One instance per process; each project gets its own goroutine on
// first SendMessage. Cross-project messages run in parallel; same-
// project messages queue serially so the conversation history stays
// coherent.
//
// The per-project goroutine processes each turn under the requesting
// user's identity (the conversation's creator), wrapping every message
// write in stores.Tx.SyntheticClaimsWithTx so multi-mode RLS gates the
// rows through the conversation's private-visibility arm; claim writes
// ride the admin-pool System doors.
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
	// id, used only to compute forward (home != self).
	homer  *Homer
	selfID string

	// executorID/bootEpoch are this process's persistent instance-registry
	// identity (SetExecutorIdentity), stamped onto every claims row a
	// dispatch mints so the engagement is attributed to the process that ran
	// it and the ownership-scoped boot sweep can find its own strays. Empty
	// in tests — claims then carry the '' sentinel.
	executorID string
	bootEpoch  int64

	// doorbell publishes a cross-pod tf_ctl notification (SetDoorbell). Wired on
	// control pods to nudge the home executor's claim loop ("curator_new") and
	// to route a cross-pod cancel ("curator_cancel"). nil elsewhere — the
	// executor claim loop's backstop poll and the DB-level cancel flip cover a
	// missed doorbell, so this is a latency optimization, never a correctness
	// dependency.
	doorbell func(kind, orgID, projectID string)

	// bringUpTurnSandbox stands up a homed turn's executor sandbox (network +
	// credential sidecar) before the turn runs — the delegation spawner's
	// BringUpCuratorSandbox, wired via SetTurnSandbox on a multi-mode executor
	// pod only. nil on control/all/local keeps the in-process
	// agenthost.Start path byte-identical. Defined as a curator-local func type
	// (not a delegate import) to avoid a dependency cycle, exactly how admitTurn
	// avoids importing delegate.
	bringUpTurnSandbox BringUpTurnSandboxFunc

	// turnRetryDelay paces the session's own re-feed after a BeginTurn
	// failure — the one failure class that leaves the turn queued with
	// nothing else driving it (the executor claim loop's handed-off dedup
	// only clears once a pickup mints a claim, and local mode has no loop
	// at all). Small enough to feel responsive, large enough that a hard
	// DB outage doesn't spin the mint/fail cycle.
	turnRetryDelay time.Duration

	// turnMaxAttempts caps how many failed pickups (claim minted, BeginTurn
	// failed, claim released 'failed' with the message still undelivered) a
	// queued turn may accumulate before dispatch dead-letters it instead of
	// feeding it into another doomed cycle — without the cap, a permanent
	// BeginTurn failure (e.g. a corrupt project row) loops forever, leaking
	// one zero-message failed claim per scan. Defaults to
	// DefaultTurnMaxAttempts; SetTurnMaxAttempts overrides from
	// TF_CURATOR_TURN_MAX_ATTEMPTS at app wiring.
	turnMaxAttempts int

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

	// kb is the multi-mode knowledge-base blob store. On an
	// executor the session brackets materialize the KB store→disk at turn
	// start and reconcile disk→store at turn end through it, and the
	// kb_changed doorbell materializes a panel upload into a live session's
	// dir. nil in local mode and in tests, where the brackets branch on
	// runmode and never touch it.
	kb *kbstore.Store

	// closed is set during Shutdown; SendMessage rejects after this.
	closed bool
}

// New constructs a Curator. Run the boot recovery sweep
// (stores.Curator.CancelOrphanedTurnsSystem, or the ownership-scoped
// CancelStrandedTurnsForHomeSystem on multi split roles) before
// constructing — see the app wiring.
//
// stores carries the Tx runner (for SyntheticClaimsWithTx wraps), the
// CuratorStore (per-turn message writes plus the admin-pool …System claim
// doors), the ConversationStore (transcript inserts), the PromptStore (skill
// materialization), and the RepoStore (pinned-repo materialization). Every
// row read/write the curator issues goes through a claims-bound tx or an
// admin-pool door, so no raw *sql.DB handle is retained.
func New(stores db.Stores, wsHub *websocket.Hub, model string) *Curator {
	return &Curator{
		stores:          stores,
		wsHub:           wsHub,
		model:           model,
		sessions:        make(map[string]*projectSession),
		runAgent:        agentproc.Run,
		turnMaxAttempts: DefaultTurnMaxAttempts,
		turnRetryDelay:  DefaultTurnRetryDelay,
	}
}

// DefaultTurnMaxAttempts is the failed-pickup budget a queued turn gets
// before dispatch dead-letters it. Distinct from the delegation side's
// TF_RUN_MAX_ATTEMPTS: this caps repeated BeginTurn failures on one turn's
// message, not executor-loss re-claims.
const DefaultTurnMaxAttempts = 3

// DefaultTurnRetryDelay paces the self-retry after a failed BeginTurn.
const DefaultTurnRetryDelay = 5 * time.Second

// ParseTurnMaxAttempts parses TF_CURATOR_TURN_MAX_ATTEMPTS. Empty maps to
// DefaultTurnMaxAttempts; anything else must parse as a positive integer.
// Mirrors reaper.ParseMaxAttempts (TF_RUN_MAX_ATTEMPTS).
func ParseTurnMaxAttempts(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultTurnMaxAttempts, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid TF_CURATOR_TURN_MAX_ATTEMPTS=%q (want a positive integer)", raw)
	}
	return n, nil
}

// SetTurnMaxAttempts overrides the dead-letter cap. Called once at app
// wiring with the ParseTurnMaxAttempts result.
func (c *Curator) SetTurnMaxAttempts(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turnMaxAttempts = n
}

// getTurnMaxAttempts returns the cap under the lock, matching getSecrets'
// race-free accessor shape.
func (c *Curator) getTurnMaxAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnMaxAttempts
}

// SetTurnRetryDelay overrides the BeginTurn-failure re-feed pacing (tests
// shrink it to keep the retry cycle fast).
func (c *Curator) SetTurnRetryDelay(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turnRetryDelay = d
}

func (c *Curator) getTurnRetryDelay() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnRetryDelay
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

// SetKBStore wires the knowledge-base blob store so the session
// brackets and the kb_changed doorbell can materialize/reconcile the KB
// against the object store in multi mode. Set once at startup on pods that
// build a curator; nil in local mode leaves the brackets on their no-op
// branch (local KB is plain on-disk files, already the truth).
func (c *Curator) SetKBStore(kb *kbstore.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kb = kb
}

// getKB returns the wired KB store under the lock, matching getSecrets' race-
// free accessor shape.
func (c *Curator) getKB() *kbstore.Store {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kb
}

// HasLiveSession reports whether this pod currently runs a session goroutine
// for the project — the in-memory self-filter the kb_changed doorbell uses to
// decide whether a panel upload must be materialized into a live turn's dir
// (the home-but-idle case is covered by the next turn's start-materialize, so
// it needs no per-executor DB read here).
func (c *Curator) HasLiveSession(projectID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sessions[projectID]
	return ok
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

// SetExecutorIdentity wires this process's persistent instance-registry
// identity, stamped onto every claims row a dispatch mints. Called once at
// startup on every pod that builds a curator.
func (c *Curator) SetExecutorIdentity(id string, bootEpoch int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executorID = id
	c.bootEpoch = bootEpoch
}

// getExecutorIdentity returns the wired identity under the lock, matching
// getSecrets' race-free accessor shape.
func (c *Curator) getExecutorIdentity() (string, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executorID, c.bootEpoch
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

// TurnSandbox is the executor-side network + credential sidecar a homed curator
// turn runs under. Returned by the SetTurnSandbox seam (the
// delegation spawner's BringUpCuratorSandbox); *delegate.executorSandbox
// satisfies it. Curator defines the interface rather than importing delegate to
// avoid a dependency cycle, mirroring how admitTurn avoids it.
type TurnSandbox interface {
	// Network is the prebuilt run network agentproc launches the jail into
	// (RunOptions.PrebuiltNetwork).
	Network() *sandbox.RunNetwork
	// ProxyEnv is the non-secret sandbox proxy env (RunOptions.PrebuiltProxyEnv).
	ProxyEnv() []string
	// GHChannel is the real-gh channel params (RunOptions.GHChannel) when the
	// turn's sidecar bound the injector; nil otherwise. runID is the
	// conversation id (the cert path key).
	GHChannel(runID string) *agentproc.GHChannelParams
	// GitCloneAuth routes a host-side fetch of cloneURL through the turn's git
	// proxy so the orchestrator holds no token (empty when the sandbox has no
	// git proxy).
	GitCloneAuth(cloneURL string) worktree.CloneAuth
	// Close tears the sandbox down (network + sidecar). Deferred by the dispatch
	// after a successful bring-up.
	Close()
}

// BringUpTurnSandboxFunc stands up a homed curator turn's sandbox before the
// turn runs, keyed by the conversation id (the claim-credentials channel's
// key — one active turn per conversation, so it also names the per-turn
// network/socket uniquely). Its shape matches Spawner.BringUpCuratorSandbox,
// which returns (nil, nil) on every non-executor role and unwired fixture —
// the caller then keeps the in-process path.
type BringUpTurnSandboxFunc func(ctx context.Context, orgID, conversationID, userID, teamID string, pinnedRepos []string) (TurnSandbox, error)

// SetTurnSandbox wires the executor-side turn-sandbox bring-up — the
// delegation spawner's BringUpCuratorSandbox. Called once at startup on a
// multi-mode executor pod only (gated on the executor runtime in
// buildCuratorRuntime); left nil on control/all/local, where dispatch keeps the
// in-process agenthost.Start path unchanged.
func (c *Curator) SetTurnSandbox(fn BringUpTurnSandboxFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bringUpTurnSandbox = fn
}

// getTurnSandbox returns the wired bring-up seam under the lock, matching
// getAdmitTurn's race-free accessor shape. nil on every pod but a multi-mode
// executor.
func (c *Curator) getTurnSandbox() BringUpTurnSandboxFunc {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bringUpTurnSandbox
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

// recordSandboxActuals is the RunOptions.RecordSandboxActuals recorder for a
// curator turn — the same write the delegate spawner does, since a turn is
// an executor engagement with its own claim. agentproc calls it at teardown
// on a detached context and swallows the error, so a lost stamp costs this
// turn's accounting and nothing else.
func (c *Curator) recordSandboxActuals(ctx context.Context, orgID, claimID string, actuals sandbox.RunActuals) error {
	return c.stores.Conversations.RecordClaimSandboxStatsSystem(ctx, orgID, claimID, actuals.PeakMemMB, actuals.CPUUsec)
}

// queueItem carries everything the per-project goroutine needs to
// dispatch a turn under the requesting user's identity. orgID +
// creatorUserID are captured at enqueue time (SendMessage's handler
// context, or the claim loop's scan projection) so the goroutine doesn't
// have to read rows again just to figure out who to bill the writes to —
// that read would itself need claims set under Postgres RLS, creating a
// chicken-and-egg problem. requestID is messageID rendered decimal — the
// turn id on the wire.
type queueItem struct {
	conversationID string
	messageID      int64
	requestID      string
	orgID          string
	creatorUserID  string
}

// turnRequestID renders a turn's user-message id as the wire request id.
func turnRequestID(messageID int64) string {
	return strconv.FormatInt(messageID, 10)
}

// SendMessage records the user's input as an undelivered user message on the
// sender's live curator conversation (minting the conversation on first
// contact), hands the turn to the project's goroutine, and returns the turn
// id — the message id as a decimal string. The HTTP handler returns 202 +
// this id; the per-project goroutine claims the turn on pickup and releases
// the claim on completion.
//
// orgID + creatorUserID identify the requesting user — every message write
// the goroutine produces for this turn runs inside
// Stores.Tx.SyntheticClaimsWithTx with these claims set so multi-mode RLS
// attributes the rows correctly (claim writes ride the admin-pool System
// doors instead — tf_app cannot write claims).
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
// in which case the queued message is deleted before we return (it never
// entered context). Either way, no message reaches a non-running goroutine.
func (c *Curator) SendMessage(ctx context.Context, projectID, orgID, creatorUserID, content string) (string, error) {
	// Resolve where this turn executes before minting the row (curator
	// homing, spec §6.3) — the Homer upserts the durable curator_homes
	// mapping the executor claim loop scans against. local / role=all /
	// no-homer runs in-process; a capless control pod with no eligible
	// executor fails fast rather than queueing a turn that can never run.
	_, mode := c.resolveHome(ctx, orgID, projectID)
	if mode == homeUnavailable {
		return "", ErrNoCuratorExecutor
	}

	var (
		conversationID string
		messageID      int64
	)
	if err := c.stores.Tx.SyntheticClaimsWithTx(ctx, orgID, creatorUserID, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(ctx, orgID, projectID, creatorUserID)
		if err != nil {
			return err
		}
		if conv == nil {
			return errors.New("conversation mint returned nothing")
		}
		conversationID = conv.ID
		id, err := ts.Curator.EnqueueUserMessage(ctx, orgID, conv.ID, creatorUserID, content)
		if err != nil {
			return err
		}
		messageID = id
		return nil
	}); err != nil {
		return "", fmt.Errorf("enqueue curator turn: %w", err)
	}
	requestID := turnRequestID(messageID)

	if mode == homeForward {
		// Homed to a remote executor — do NOT run a session on this control
		// pod. The durable undelivered message IS the delivery; the doorbell
		// just nudges the home's claim loop so it need not wait for its
		// backstop poll. Output streams back to this browser over the WS
		// backplane from wherever the turn runs, so nothing else is needed
		// here.
		c.broadcastRequestUpdate(orgID, projectID, requestID, "queued")
		c.ringDoorbell("curator_new", orgID, projectID)
		return requestID, nil
	}

	item := queueItem{
		conversationID: conversationID,
		messageID:      messageID,
		requestID:      requestID,
		orgID:          orgID,
		creatorUserID:  creatorUserID,
	}

	session := c.getOrStartSession(orgID, projectID)
	if session == nil {
		// A queued turn that will never run must not linger as a phantom
		// undelivered row — delete it (it never entered context), exactly the
		// queued-cancel semantic. WithoutCancel so a canceled/timed-out
		// request ctx can't skip the cleanup.
		c.deleteQueuedTurn(context.WithoutCancel(ctx), item)
		return "", errors.New("curator is shut down")
	}

	select {
	case session.queue <- item:
		c.broadcastRequestUpdate(orgID, projectID, requestID, "queued")
		return requestID, nil
	default:
		// Queue is full — should not happen at the per-project depth we
		// configure, but if it ever does, drop the row up-front rather than
		// blocking the HTTP handler (or leaving an undelivered message the
		// in-process path would never revisit).
		c.deleteQueuedTurn(context.WithoutCancel(ctx), item)
		c.broadcastRequestUpdate(orgID, projectID, requestID, "failed")
		return "", errors.New("curator queue is full")
	}
}

// deleteQueuedTurn best-effort removes a queued turn's undelivered user
// message under the requesting user's claims — the shared fallback for the
// shutdown / queue-full paths where the turn can never run.
func (c *Curator) deleteQueuedTurn(ctx context.Context, item queueItem) {
	if err := c.stores.Tx.SyntheticClaimsWithTx(ctx, item.orgID, item.creatorUserID, func(ts db.TxStores) error {
		_, err := ts.Curator.DeleteQueuedTurn(ctx, item.orgID, item.conversationID, item.messageID)
		return err
	}); err != nil {
		curatorLog.Warn("delete queued turn failed", "request", item.requestID, "error", err)
	}
}

// EnqueueClaimed feeds a claimable turn (scanned off the home executor's
// claim loop, spec §6.3) into its per-project session goroutine — the
// executor-side counterpart of SendMessage's in-process enqueue, minus the
// row creation (the control pod already recorded the undelivered message).
// Returns true when the turn was handed to a session; false when the curator
// is shut down or the per-project queue is momentarily full, so the claim
// loop leaves the turn queued and retries on its next scan.
//
// Idempotency: feeding the same turn twice is safe — the second dispatch's
// claim mint skips (the first engagement is still live, or already
// delivered the message) and returns without re-running the agent — so a
// duplicated doorbell or a scan race never double-executes a turn.
func (c *Curator) EnqueueClaimed(orgID, projectID, conversationID string, messageID int64, creatorUserID string) bool {
	session := c.getOrStartSession(orgID, projectID)
	if session == nil {
		return false // curator shut down
	}
	item := queueItem{
		conversationID: conversationID,
		messageID:      messageID,
		requestID:      turnRequestID(messageID),
		orgID:          orgID,
		creatorUserID:  creatorUserID,
	}
	select {
	case session.queue <- item:
		c.broadcastRequestUpdate(orgID, projectID, item.requestID, "queued")
		return true
	default:
		return false // queue full — leave the turn queued for the next scan
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

// CancelProject is the project-teardown hook: cancel any in-flight turn and
// stop the goroutine so nothing runs after the teardown.
//
// reason is recorded on the released claim's error and surfaced in the
// cancellation broadcast — "project deleted" for the project-delete path,
// "team archived" for the team-archive force-stop (TFAC-448) — so the audit
// trail distinguishes the two.
//
// Called BEFORE the projects DELETE (delete path) so the FK cascade doesn't
// race a still-running goroutine. Delivered history removal is the cascade's
// job once the project row drops (projects → conversations →
// messages/claims), but queued turns are drained HERE rather than left to
// it: the team-archive force-stop keeps the project row, and an undrained
// queued turn stays claimable — the executor claim loop or a pending retry
// timer would resurrect work for a project that was just force-stopped.
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

	// The force-stop drain, after the session teardown so the dispatch can't
	// re-enqueue behind it. Background ctx: teardown fires outside any
	// request/JWT context, and the drain spans every creator's conversation,
	// which only the admin-pool System door reaches.
	if n, err := c.stores.Curator.DeleteQueuedTurnsForProjectSystem(context.Background(), orgID, projectID); err != nil {
		curatorLog.Warn("drain queued curator turns on project teardown failed", "org", orgID, "project", projectID, "error", err)
	} else if n > 0 {
		curatorLog.Info("drained queued curator turns on project teardown", "org", orgID, "project", projectID, "count", n)
	}

	// Curator homing (spec §6.3): when the live session lives on a remote
	// executor, the local session teardown above is a no-op — route a cross-pod
	// cancel so the home executor SIGKILLs its subprocess promptly rather than
	// running the turn to completion against rows the FK cascade is about to
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
// In-flight claims release as cancelled with reason "process shutting
// down"; still-queued turns stay durable undelivered messages — the boot
// sweep releases only stranded claims, and a queued turn is re-claimed
// (multi) or cancellable by the user (local) after restart.
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
