// The Spawner type — central coordinator for delegated agent runs — and
// the small cross-cutting helpers (status broadcasts, status updates,
// drainer/classification wiring) every other file in this package
// reaches for. The lifecycle methods (Delegate, Stop, SendMessage)
// live in their own files; this one is the type definition + the bits
// that don't belong anywhere else.

package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/pushpolicy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// worktreePushTargetBranch reads the REMOTE branch a bare `git push` from a
// worktree's live checkout would update — the current branch mapped through its
// configured push refspec ("" when detached / unreadable). A package var so the
// push-authorization tests can inject a deterministic path→target mapping
// without standing up real git worktrees on disk.
var worktreePushTargetBranch = worktree.PushTargetBranch

// shortConversationID truncates a run UUID to 8 chars for toast messages — full UUIDs
// are noisy in a notification. Kept consistent so users can cross-reference
// the runs page listing.
func shortConversationID(conversationID string) string {
	if len(conversationID) < 8 {
		return conversationID
	}
	return conversationID[:8]
}

// QueueDrainer is the interface the spawner uses to notify the per-task
// firing queue that an auto run has reached a terminal state and the
// task may be ready to drain its next pending firing. Implemented by
// the routing.Router. Manual runs do not call this — manual is fully
// decoupled from the queue by design. orgID scopes the
// drain to the run's tenant so multi-mode lookups hit the right
// pending_firings rows.
type QueueDrainer interface {
	DrainTask(orgID, taskID string)
}

// EventPublisher is the bus-publish seam the spawner uses to mirror run
// lifecycle onto the event bus (TFAC-592), so an EE subscriber
// (ExtensionAPI.Bus()) can observe run status/activity alongside the
// websocket broadcast. Mirrors internal/tracker's Publisher shape; the
// plain *eventbus.Bus satisfies this interface directly.
type EventPublisher interface {
	Publish(evt domain.Event)
}

// PresenceChecker is the multi-mode fleet-wide presence check (TFAC-584):
// the plain *wsbackplane.Backplane satisfies this directly, combining
// this pod's local Hub state with the fleet-wide ws_presence table so a
// reviewer connected to a different pod than the one running a
// permission check still counts as present. See presentFor.
type PresenceChecker interface {
	PresentFor(ctx context.Context, orgID, conversationID string) bool
}

// Spawner manages delegated agent runs.
type Spawner struct {
	database          *sql.DB
	prompts           db.PromptStore
	agents            db.AgentStore // resolves actor for run.actor_agent_id stamping
	blueprints        db.BlueprintStore
	conversationQueue db.ConversationQueueStore // the run queue the dispatcher drains: enqueue a step, claim it, run it, react
	// claimCredentials is the sealed per-run credential bundle channel
	// (TFAC-614) — the executor's awaiting-credentials wait reads it;
	// nil-safe (local resolves credentials directly and never gates on it).
	claimCredentials db.ClaimCredentialsStore
	// awaitingCredentialsTimeout overrides awaitingCredentialsTimeout (the
	// package default) when > 0 — tests inject a short value via
	// SetAwaitingCredentialsTimeout, mirroring idleHibernateTimeout.
	awaitingCredentialsTimeoutOverride time.Duration
	// awaitingCredentialsPollInterval overrides the package default poll
	// cadence when > 0, same override shape.
	awaitingCredentialsPollIntervalOverride time.Duration
	tasks                                   db.TaskStore         // re-read tasks for run lifecycle handlers
	conversations                           db.ConversationStore // run lifecycle + transcript
	entities                                db.EntityStore       // entity reads for project lookup + resume context
	artifacts                               db.ArtifactStore     // review + draft-PR artifact lookup on processCompletion park check
	// stagedInjections is the durable, producer-agnostic "stage for next
	// resume" agent-injection queue. The generic staged-injection API
	// (stageOrDeliverInjection / stagedInjectionsForResume) appends here
	// when a target run has no warm process and flushes on the next resume.
	// Admin-pool System methods only — both the producer (an eventbus
	// subscriber) and the consumer (a resume goroutine) run without JWT
	// claims. Nil-safe (tests passing a partial db.Stores{}).
	stagedInjections db.StagedInjectionStore
	events           db.EventStore // admin-pool GetMetadataSystem for post-run prompt building
	// taskMemory routes the post-completion UpsertAgentMemorySystem
	// and the run-start GetMemoriesForEntitySystem through the dual-
	// pool store. Both fire inside the runAgent goroutine, which has
	// no JWT-claims context, so they hit the admin pool in Postgres.
	taskMemory db.TaskMemoryStore
	// conversationWorktrees serves the spawner's per-run cleanup defers (Jira
	// runs accumulate lazy worktrees via the agent's `workspace add`
	// CLI; the defer iterates and removes them). Goroutine-internal
	// callers, all routed through the admin-pool System variants.
	conversationWorktrees db.ConversationWorktreeStore
	// orgs reads per-org settings (GitHub clone protocol, the TFAC-477 daily
	// spend cap) from org_settings during run setup. Post-internal/config
	// deletion; every per-org read goes through OrgsStore.GetSettingsSystem
	// (no JWT claims context on the run goroutine).
	orgs db.OrgsStore
	// spend reads the org's settled LLM spend for the TFAC-477 daily-cost-cap
	// admission check at Delegate entry. Read via SpendByCategorySystem — the
	// admin-pool variant — because Delegate runs under context.Background()
	// with no JWT claims, so an app-pool/RLS read would see nothing and the cap
	// would never trip. Read-only; a plain store ref like s.orgs.
	spend db.SpendStore
	// jiraRules reads the team's per-project Jira status rules under the
	// admin pool (ListForTeamSystem) for the TFAC-300 board→Jira mirror's
	// system-context rule lookup. Populated from the Stores bundle in
	// NewSpawner — a plain store ref like s.tasks, set once and never
	// mutated, so it needs no mu guard (its sibling seam jiraResolver does).
	jiraRules db.JiraStatusRulesStore
	// externalActions records the TFAC-300 board→Jira mirror's writes into the
	// append-only external-action audit log (TFAC-483) under the admin pool
	// (RecordSystem — the detached mirror has no JWT-claims context). A plain
	// store ref like s.jiraRules; nil-safe (a partial test Stores skips recording).
	externalActions db.ExternalActionStore
	// repos reads repositories under the admin pool (GetSystem) for the one
	// thing a workspace rehydrate needs and cannot derive: the repo's upstream
	// clone URL. The first claim gets that URL from the PR object it already
	// fetched; a later step / a resume fetches no PR, so the profile — written
	// in the org's configured protocol — is the URL's home. A plain store ref
	// like s.orgs; nil-safe (a partial test Stores yields no URL, and the
	// rehydrate degrades to the bare-must-already-exist path).
	repos db.RepositoryStore
	// teams reads per-team settings under the admin pool (GetSettingsSystem)
	// at spawn time — currently the TFAC-392 presence-gated absent-auto-deny
	// knobs (grace window + on/off toggle). Resolved once per run when the
	// permission handler is built, not per prompt. A plain store ref like
	// s.tasks/s.jiraRules; nil-safe (the helper falls back to defaults).
	teams db.TeamsStore
	// instances is the fleet membership registry RunInstanceHeartbeat
	// renews on a timer. A plain store ref like s.jiraRules; nil-safe (a nil
	// store makes the heartbeat loop a logged no-op, same shape as ConversationQueue on
	// RunDispatcher). Also the liveness read the cross-pod signal seam
	// (TFAC-585) uses to decide whether a run's executor is still around.
	instances db.InstanceStore
	// workspaceSnapshots is the durable per-snapshot-key lifecycle record the
	// teardown writes around its blob write: 'pending' before the capture,
	// 'written'/'failed' after, CAS'd on this engagement's claim so a late
	// upload from a displaced engagement never overwrites a successor's newer
	// blob. A plain store ref like s.instances; nil-safe (a partial test
	// Stores still writes the blob, with no lifecycle recorded against it).
	workspaceSnapshots db.WorkspaceSnapshotStore
	// instanceStats is the 1-minute fleet telemetry sink RunInstanceStatSampler
	// writes (TFAC-589). Nil-safe: a nil store makes the sampler a logged no-op,
	// same shape as instances on the heartbeat loop.
	instanceStats db.InstanceStatStore
	// sandboxStats is the per-sandbox resource series the same sampler tick
	// appends a row to per live jail. Nil-safe: a nil store makes the
	// extension a silent no-op, and the instance sample on that tick is
	// unaffected either way.
	sandboxStats db.SandboxStatStore
	// pendingInput is the durable half of resume-by-enqueue (TFAC-585): the
	// message recorded before a parked run's continuation is re-queued as
	// ordinary claimable work. Wired unconditionally in NewSpawner — both
	// dialects support it (local mode's dispatcher claims its own resumed
	// runs the same way). Nil-safe (a partial test Stores{} skips it).
	pendingInput db.ConversationPendingInputStore
	// pendingFirings lets the owner's signal-apply loop compensate a cross-
	// pod `inject` signal whose target run turned out dead by the time it
	// was applied: it enqueues the pending_firing itself so the additive
	// event's intent survives (TFAC-585's "gone" ack path). Wired
	// unconditionally — both dialects support pending_firings.
	pendingFirings db.PendingFiringsStore
	// conversationSignals is the cross-pod run-control outbox (TFAC-585, Postgres
	// only). Nil except when SetConversationSignals wires it — always nil in local
	// mode and in every test that doesn't opt in, which is what keeps
	// s.controller as the plain inProcessController and every cross-pod
	// code path (crossPodController, the apply loop, StageOrDeliverAdditiveEvent's
	// remote branch) a no-op/never-reached by construction. Guarded by mu
	// like the other post-construction seams.
	conversationSignals db.ConversationSignalStore
	// signalNotifyDB is the admin-pool *sql.DB SetConversationSignals wires
	// alongside conversationSignals — NOTIFY needs no session affinity (unlike
	// LISTEN), so it rides whatever pooled connection is at hand. Nil
	// exactly when conversationSignals is nil.
	signalNotifyDB *sql.DB
	// ackWaiters holds one wake channel per in-flight signal a control
	// request on this pod is waiting to see acked, keyed by conversation_signals.id.
	// The shared tf_ctl Listener's onNotify dispatch sends on the matching
	// channel (non-blocking, 1-buffered) when it observes {"kind":"ack"};
	// the waiter treats it as "check now", not as the data itself (the
	// authoritative read is always AckStatus). Guarded by mu.
	ackWaiters map[int64]chan struct{}
	// signalApplyWake is the best-effort latency nudge for the owner's
	// signal-apply loop, mirroring dispatchWake. Non-blocking send from the
	// shared Listener's onNotify dispatch on {"kind":"new"}.
	signalApplyWake chan struct{}
	// signalApplied tracks signal ids (int64) delivered by this process but
	// whose ack write hasn't landed yet (id -> ack result string). A re-scan
	// re-acks from here instead of re-delivering, giving exactly-once delivery
	// per process for the non-idempotent kinds (steer, inject). Pruned on a
	// successful ack. See applySignal.
	signalApplied sync.Map
	// signalAckTimeout overrides the interrupt/steer reply-leg timeout
	// (TF_SIGNAL_ACK_TIMEOUT). Zero means use DefaultSignalAckTimeout.
	signalAckTimeout time.Duration
	// tx runs synthetic-claims write batches for manual runs (the
	// run's creator_user_id is the synthetic claim subject, so RLS
	// policies on the writes pass under tf_app). Event-triggered runs
	// don't construct a tx — their writes go through `...System`
	// admin-pool methods directly. Routing is inline at each call
	// site: `if triggerType == "manual" { s.tx.SyntheticClaimsWithTx
	// (..., creatorUserID, fn) } else { s.x.MethodSystem(...) }`.
	tx    db.TxRunner
	wsHub *websocket.Hub
	// presence is the optional multi-mode presence checker (TFAC-584),
	// wired via SetPresenceChecker. When set, presentFor consults it
	// (local Hub state OR the fleet-wide ws_presence table) instead of
	// wsHub.PresentFor directly, so a reviewer connected to a different
	// pod than the one running this run still counts as present. Nil in
	// local mode and at TF_ROLE=all before wiring — presentFor falls back
	// to wsHub.PresentFor unchanged in that case.
	presence PresenceChecker

	mu       sync.Mutex
	ghClient *ghclient.Client
	model    string
	// publicURL is the deployment's externally-visible base URL, used to
	// compute the {{RUN_URL}} prompt placeholder (TFAC-591) — the "view
	// this run in TF" deep link. Wired post-construction via SetPublicURL
	// with the same value handed to Server.SetDeployConfig (internal/app/
	// httpserver.go, both call sites). Empty disables the placeholder
	// (renders "") rather than fabricating a localhost link in multi mode.
	publicURL string
	// Per-org run-credential seam, wired once at startup via
	// SetRunCredentialResolvers. When set (both modes in production) these
	// supersede the process-global ghClient/model above; tests leave them
	// nil and the resolver helpers fall back to ghClient/model.
	ghResolver ghclient.Resolver                                                // per-(org, owner) GitHub client source (App token in multi, keychain PAT in local)
	runSecrets agentproc.SecretsReader                                          // per-org LLM-credential reader (nil in local → ambient subscription; system-door reader in multi)
	modelFor   func(context.Context, string, string) (domain.TeamModels, error) // per-(org, team) model resolver: the default a step inherits plus the set every model is held to
	// llmResolver is the shared LLM-credential resolver (internal/llmcred,
	// TFAC-616) — role-mode Bedrock orgs mint short-lived STS session creds
	// through it. Used only where a run resolves its own credentials in
	// process (local): the RunOptions.LLMResolver it feeds is never consulted
	// on an executor, whose agent launches into a prebuilt cell whose
	// credentials the sidecar unsealed. nil in local mode (ambient) and in
	// tests. Wired post-construction via SetLLMResolver.
	llmResolver bundleLLMResolver
	// jiraResolver routes the TFAC-300 board→Jira mirror under the org's
	// system/bot credential (ForSystem). Wired post-construction via
	// SetJiraResolver (the resolver is built in the app composition, not handed
	// to NewSpawner); nil disables the mirror (tests, local without Jira).
	// mu-guarded like the credential seam above it — creds hot-swap on config
	// change, so a client is resolved fresh per write and never cached here.
	jiraResolver jira.Resolver
	// jiraMirrorLocks serializes the TFAC-300 board→Jira mirror per ticket so a
	// slow in-progress mirror and the done mirror can't interleave or reorder
	// their writes against the same issue (which could drag a Done ticket back
	// to In Progress). Its own keyed lock, independent of mu; zero value ready.
	jiraMirrorLocks keyedMutex
	// workspaceLocks serializes mutation of one snapshot key's workspace tree
	// within this process. Two things in an executor touch a parked tree: an
	// engagement materializing it (ensureWorkspace — warm stat or cold
	// rehydrate) and the eviction sweep deleting it. They run in the same
	// process, so a bare "is anyone claimed on this key" read is a check
	// against a fact that can change a line later; holding this across the
	// decision is what makes the answer still true at the removal. Keyed by
	// (org, blueprint_run_id) — the key a tree belongs to, not a conversation.
	// Its own keyed lock, independent of mu; zero value ready.
	workspaceLocks keyedMutex

	// blobs is the durable blob/object store handle for the blueprint
	// workspace seam: local mode → an on-disk store under the state root,
	// multi → an S3-compatible object store. Wired once at startup via
	// SetStorage, mirroring SetRunCredentialResolvers above; read through
	// Storage() by the snapshot/rehydrate consumer (a follow-up). Guarded
	// by mu like the credential seam it sits beside.
	blobs storage.Storage

	// teamKB is the team knowledge-base seam a claim stages a run's knowledge
	// from — the task team's whole KB plus every other team's published root.
	// Wired once at startup via SetTeamKB, beside SetStorage. nil is tolerated
	// (a fixture that never stages knowledge), and a run then stages none.
	teamKB kbstore.KB

	cancels map[string]context.CancelFunc // conversationID → cancel the entire run
	// engagements holds the live claim attempt's trace root for each run
	// currently dispatching, keyed by run id — the seam between the setup
	// span (which ends at agent-live) and the punctual spans that link back
	// to it for the rest of the run. A conversation with no entry is the
	// ordinary untraced case, and every reader is nil-safe. Guarded by mu
	// like cancels beside it.
	engagements map[string]*engagement

	dispatchWake   chan struct{}  // best-effort latency nudge for the conversation-queue dispatcher; non-blocking send on enqueue, buffered depth 1 so a missed wake only defers to the next scan tick
	drainer        QueueDrainer   // nil-safe; set post-construction via SetQueueDrainer
	eventPublisher EventPublisher // nil-safe; set post-construction via SetEventPublisher — mirrors run status/activity onto the bus (TFAC-592)

	// procs holds the live agent process handle for each run currently
	// executing as a LiveRun, keyed by run id. It survives across HTTP
	// turns because the spawner is the startup singleton, so a control op
	// (interrupt/steer/cancel) arriving on a later request can still
	// reach the process a delegation goroutine spawned. Distinct from
	// cancels (the hard-kill ctx) — a live run holds both a procs and a
	// cancels entry at once. Guarded by s.mu; see the
	// register/get/deregister accessors in process_registry.go.
	procs map[string]*liveRunHandle
	// controller routes the live-process control ops (interrupt, steer,
	// cancel-kill) to wherever the run's process lives. At N=1 it's the
	// in-process impl that resolves the handle from procs/cancels; the
	// seam horizontal scaling swaps for a DB-signal to the owning
	// executor. Set once in NewSpawner.
	controller RunController
	// permPending brokers the browser tool-permission round-trip: each
	// in-flight canUseTool prompt registers a pending entry here keyed by the
	// tool_use id of the call being gated; the WS POST resolves it and the
	// parked handler goroutine receives the decision (or a bounded timeout
	// denies it). In-memory only; guarded by s.mu. This is still the whole
	// mechanism — the durable rows below are a record kept alongside it, never
	// the transport, and nothing drives the agent off them.
	permPending map[string]*pendingPermission
	// permissions is the durable record of every prompt the broker above
	// surfaces: one row per gated call, resolved by whichever path answered it
	// (a human, the full-window timeout, the presence-gated absent deny). It
	// gives a live pending prompt an address — a refresh, a second tab, or a
	// cold load reconstructs it from here — and gives every decision an audit
	// row, which a fire-once websocket frame could do neither of. Nil-safe:
	// every write logs and moves on, because failing to record a prompt must
	// never be a reason to fail to ASK it.
	permissions db.PermissionStore
	// executorID is this spawner instance's executor identity, stamped onto
	// claims.executor_id at claim and resume. Empty at construction —
	// production wires the persistent instance-registry id via
	// SetExecutorID once main resolves it, alongside bootEpoch (the pair
	// the heartbeat loop's fenced renewal keys on), and the empty default
	// is what makes a missed wiring observable: the heartbeat loop
	// refuses to start (logged) and claim stamps bind NULL rather than a
	// plausible-looking-but-unregistered random uuid. At N=1 there is one
	// executor per process; on restart the persistent id re-stamps
	// re-claimed runs. The run→executor ownership hook horizontal scaling
	// builds the lease layer on. Guarded by s.mu like the other
	// startup-set seams (SetStores, SetStorage) — read through
	// executorIdentity().
	executorID string
	bootEpoch  int64
	// placementResolver computes the (org, repo) rendezvous stamp at enqueue
	// and, via placementClaim, the two-tier claim config (TFAC-587). Both nil/
	// zero when placement is off (local N=1, or the layer feature-flagged
	// off): enqueue then stamps nothing and the claim runs the global-oldest
	// path — the whole layer is advisory. Guarded by s.mu like the other
	// startup-set seams; wired once via SetPlacement before the dispatcher
	// starts.
	placementResolver placementResolver
	placementClaim    db.ClaimPlacement
	// runSem bounds how many runs execute off the dispatcher at once — a
	// process-wide cap so a burst of queued steps doesn't fan into an
	// unbounded number of agent subprocesses. Sized in NewSpawner
	// (DefaultMaxConcurrentClaims) and replaceable via SetMaxConcurrentClaims
	// before the dispatcher starts. Each drain acquires a slot before
	// claiming and the run goroutine releases it on terminal.
	runSem chan struct{}
	// idleHibernateTimeout is how long a live run may go quiet (no stream
	// activity) before it hibernates to a durable resume. Zero means use
	// DefaultIdleHibernateTimeout; tests inject a short value via
	// SetIdleHibernateTimeout. Read through idleTimeout().
	idleHibernateTimeout time.Duration
	// permPresencePoll is how often the TFAC-392 presence-gated permission wait
	// re-checks for an answer-capable, focused tab. Zero means use
	// defaultPresencePollInterval; tests inject a short value via
	// SetPresencePollInterval. Read through presencePollInterval().
	permPresencePoll time.Duration
	// snapshotRetentionTTL bounds how long a parked/aborted run's durable
	// workspace snapshot is kept before the retention reaper discards it. Zero
	// means use DefaultSnapshotRetentionTTL; tests inject a short value via
	// SetSnapshotRetentionTTL. Read through snapshotRetention().
	snapshotRetentionTTL time.Duration
	// snapshotWaitTimeout bounds how long a cold resume waits on a workspace
	// snapshot whose record says a persist is in flight — the hung-writer
	// backstop, not the expected duration. Zero means DefaultSnapshotWait; set
	// once at startup via SetSnapshotWaitTimeout (TF_SNAPSHOT_WAIT_SEC) and
	// read through snapshotWait().
	snapshotWaitTimeout time.Duration
	// snapshotWaitPollInterval is how often that wait re-reads the record and
	// the blob store. Zero means snapshotWaitPoll; tests shrink it so a wait
	// with a real ladder behind it still runs in milliseconds.
	snapshotWaitPollInterval time.Duration
	// memFloorMB is the dispatch memory guardrail: when available memory
	// (hostmem.AvailableMB — cgroup-scoped when confined)
	// drops below this, drainConversationQueue defers claims (runs stay queued)
	// until memory recovers. Zero disables. Set once at startup via
	// SetDispatchMemFloor; the probe is injectable for tests.
	memFloorMB int
	// memAvailMB probes host available memory (MiB). Defaults to
	// hostmem.AvailableMB in NewSpawner; tests swap in a fake.
	memAvailMB func() int
	// memGated tracks the guardrail's last observed state so the
	// transition (and only the transition) is logged — a gated host
	// would otherwise emit a line every scan tick.
	memGated atomic.Bool
	// capSaturated tracks whether the dispatcher last found every runSem
	// slot occupied with runs queued behind it, so the saturation episode
	// is logged on its transitions only — same rationale as memGated.
	capSaturated atomic.Bool
	// identityFenced latches (sticky, restart to clear) when a heartbeat
	// renewal proves another process re-registered this instance id with
	// a newer boot_epoch — the split-identity case. Once set, the
	// dispatcher stops claiming and resumes are refused; see
	// fenceIdentity / IdentityFenced (instance_heartbeat.go).
	identityFenced atomic.Bool
	// partitionFenced latches (NOT sticky — clears on the next successful
	// heartbeat write) when this instance fails to WRITE its heartbeat for
	// longer than selfFenceDeadline: the reaper can't tell a partitioned
	// executor from a dead one, so the protocol makes them equivalent —
	// stop claiming and kill live sandboxes, same reaction as
	// identityFenced, but reversible (connectivity may return) and never
	// exits the process (a restart would lose warm worktrees for
	// nothing). See checkPartitionSelfFence / PartitionFenced
	// (instance_heartbeat.go, TFAC-586).
	partitionFenced atomic.Bool
	// selfFenceDeadline is TF_SELF_FENCE_SEC (default DefaultSelfFenceDeadline):
	// the own-monotonic-clock deadline since the last successful heartbeat
	// write past which this instance self-fences the partition case. Zero
	// (the NewSpawner default) falls back to DefaultSelfFenceDeadline —
	// mirrors memFloorMB's "zero means use the default" shape. Set once at
	// startup via SetSelfFenceDeadline; boot refuses a value >=
	// TF_REAPER_STALE_SEC (internal/app cross-validates both).
	selfFenceDeadline time.Duration
	// onSupersessionFence is invoked once, synchronously, right after
	// fenceIdentity kills this instance's live sandboxes on a supersession
	// (identityFenced) — the second half of fence completion (spec
	// §4.1(4)): the caller wires this to os.Exit with a distinct code
	// (internal/app), keeping process-lifecycle decisions out of this
	// package. nil is a safe no-op (tests, and any mode where supersession
	// is structurally unreachable).
	onSupersessionFence func()
	// lastGoodContactAt is the partition self-fence's elapsed-time
	// baseline (checkPartitionSelfFence, TFAC-586): this instance's last
	// known-good contact with the registry — a successful heartbeat
	// write, or RunInstanceHeartbeat's loop start when none has landed
	// yet. A plain time.Time under mu, deliberately NOT an atomic.Int64 of
	// UnixNano: time.Now() carries a monotonic-clock reading that
	// time.Since keeps using as long as the value never round-trips
	// through Unix/UnixNano — which an atomic.Int64 encoding would force.
	// Round-tripping falls back to wall-clock subtraction, vulnerable to
	// NTP steps / manual clock changes, defeating the "own monotonic
	// clock" guarantee the self-fence deadline depends on.
	lastGoodContactAt time.Time

	// --- executor healthz signals (TFAC-582) ---
	//
	// The localhost-only executor healthz listener (internal/app) reads
	// these through the accessors in health.go to build its liveness body.
	// dispatcherRunning latches true while RunDispatcher's loop is live
	// (set at loop entry, cleared on return); lastHeartbeatWriteNanos is
	// the UnixNano of the last successful instance-registry heartbeat write
	// (0 = never); draining latches when the drain verb (TFAC-586) asks
	// this executor to quiesce (informational until that ticket consumes
	// it).
	dispatcherRunning       atomic.Bool
	lastHeartbeatWriteNanos atomic.Int64
	draining                atomic.Bool
	// reportCapacity gates whether the heartbeat writes the capacity +
	// admission snapshot. True for executor/all (a real dispatcher backs
	// the numbers); a pure-control pod sets it false via SetReportCapacity
	// so its registry row leaves those fields NULL — they're "meaningful
	// only for executor-capable roles" (domain.Instance). Defaults true in
	// NewSpawner so existing all-role behavior is unchanged.
	reportCapacity atomic.Bool

	// claimCount accumulates successful run claims for the 1-minute fleet
	// telemetry sampler (TFAC-589). Incremented once per claimed run in the
	// dispatch loop; TakeClaims swaps it to 0 so each sample reports the
	// delta since the last. A monotonic counter read-and-reset, never a gate.
	claimCount atomic.Int64
	// spawnMu guards spawnDurationsMS — the per-run bring-up latencies
	// (claim → agent start) recorded since the last sample. TakeSpawnP50MS
	// computes the median and clears the slice. Bounded implicitly by the
	// 1-minute drain cadence (at most one entry per run started in the window).
	spawnMu          sync.Mutex
	spawnDurationsMS []int

	agentToolsOnce  sync.Once
	agentToolsCache string

	// stores is the full db.Stores bundle the per-run agenthost daemon
	// hands to its LocalClient at request dispatch. Set post-
	// construction via SetStores so we don't have to thread another
	// arg through every test fixture's NewSpawner call — the sandbox
	// branch is Linux+multi-mode only and unit tests never reach it.
	//
	// Pointer (rather than db.Stores value) so callers can branch on
	// `stores != nil` cleanly. The earlier `db.Stores{} != stores`
	// shape relied on every field being a comparable interface and
	// would runtime-panic the moment a future field landed with a
	// non-comparable concrete type (slice/map/func).
	stores *db.Stores
}

// NewSpawner constructs a Spawner from the per-resource store
// bundle plus the runtime knobs (GitHub client, WS hub, model). The
// Spawner keeps individual store fields rather than the full bundle
// so existing hot paths (s.tasks, s.entities, ...) stay put; New just
// unpacks once. Tests that only exercise a subset of stores can pass
// partial db.Stores{} — every field is a nil-safe interface.
func NewSpawner(database *sql.DB, stores db.Stores, ghClient *ghclient.Client, wsHub *websocket.Hub, model string) *Spawner {
	s := &Spawner{
		database:              database,
		prompts:               stores.Prompts,
		agents:                stores.Agents,
		blueprints:            stores.Blueprints,
		conversationQueue:     stores.ConversationQueue,
		claimCredentials:      stores.ClaimCredentials,
		tasks:                 stores.Tasks,
		conversations:         stores.Conversations,
		entities:              stores.Entities,
		artifacts:             stores.Artifacts,
		stagedInjections:      stores.StagedInjections,
		events:                stores.Events,
		taskMemory:            stores.TaskMemory,
		conversationWorktrees: stores.ConversationWorktrees,
		orgs:                  stores.Orgs,
		spend:                 stores.Spend,
		jiraRules:             stores.JiraStatusRules,
		externalActions:       stores.ExternalActions,
		repos:                 stores.Repos,
		teams:                 stores.Teams,
		instances:             stores.Instances,
		workspaceSnapshots:    stores.WorkspaceSnapshots,
		instanceStats:         stores.InstanceStats,
		sandboxStats:          stores.SandboxStats,
		pendingInput:          stores.ConversationPendingInput,
		pendingFirings:        stores.PendingFirings,
		permissions:           stores.Permissions,
		tx:                    stores.Tx,
		ghClient:              ghClient,
		wsHub:                 wsHub,
		model:                 model,
		cancels:               make(map[string]context.CancelFunc),
		engagements:           make(map[string]*engagement),
		dispatchWake:          make(chan struct{}, 1),
		procs:                 make(map[string]*liveRunHandle),
		permPending:           make(map[string]*pendingPermission),
		ackWaiters:            make(map[int64]chan struct{}),
		signalApplyWake:       make(chan struct{}, 1),
		runSem:                make(chan struct{}, DefaultMaxConcurrentClaims),
		memAvailMB:            hostmem.AvailableMB,
	}
	s.controller = inProcessController{s: s}
	// Report capacity by default (executor/all); a pure-control pod flips
	// this off via SetReportCapacity so its registry row carries no
	// misleading dispatcher-capacity numbers.
	s.reportCapacity.Store(true)
	return s
}

// useSSHCloneProtocol reports whether this run should clone over SSH. The
// ssh-vs-https decision is delegated to domain.EffectiveCloneProtocol so the
// "multi-mode is always HTTPS" invariant has a single home shared with the
// settings API view — an App installation token is an HTTPS bearer credential
// that can't be used over SSH, and the runtime container has no
// ssh-agent/key/known_hosts. orgs is nil-safe and any store failure logs +
// defaults to HTTPS, matching the prior config.Load() degrade path.
func (s *Spawner) useSSHCloneProtocol(ctx context.Context, orgID, conversationID string) bool {
	if s.orgs == nil {
		return false
	}
	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("load org settings to pick clone protocol failed; defaulting to HTTPS", "conversation", conversationID, "error", err)
		return false
	}
	return domain.EffectiveCloneProtocol(settings.GitHubCloneProtocol, runmode.Current() == runmode.ModeMulti) == "ssh"
}

// SetStores hands the per-run agenthost daemon's store bundle to the
// spawner. The bundle is consulted only inside the sandbox branch
// (multi-mode + Linux); local-mode spawning ignores it entirely. main
// calls this once at startup post-NewSpawner; tests that don't
// exercise the sandbox path can leave it unset.
func (s *Spawner) SetStores(stores db.Stores) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stores = &stores
}

// getStores returns the configured store bundle and a bool indicating
// whether it was set. Callers branch on the bool rather than
// comparing the value against db.Stores{} so a future non-comparable
// field on db.Stores can't turn the check into a runtime panic.
func (s *Spawner) getStores() (db.Stores, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stores == nil {
		return db.Stores{}, false
	}
	return *s.stores, true
}

// SetQueueDrainer wires the firing-queue drainer into the spawner. Done
// post-construction because the router (which implements QueueDrainer)
// holds a reference to the spawner, so the spawner can't take it as a
// constructor arg without a circular dependency. Same post-construction
// injection pattern as SetRunCredentialResolvers. Safe to call once at
// startup; nil drainer disables the drain hook (used in tests).
func (s *Spawner) SetQueueDrainer(d QueueDrainer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drainer = d
}

// SetPresenceChecker wires the multi-mode fleet-wide presence check
// (TFAC-584). Post-construction, same pattern as SetQueueDrainer/
// SetEventPublisher. Safe to call once at startup; nil (the default,
// always true in local mode) leaves presentFor reading wsHub.PresentFor
// directly, unchanged from before this existed.
func (s *Spawner) SetPresenceChecker(pc PresenceChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presence = pc
}

// SetEventPublisher wires the bus publisher the spawner mirrors run
// status/activity onto (TFAC-592). Post-construction, same pattern as
// SetQueueDrainer — the bus is built before the spawner in app
// composition, but the setter keeps internal/delegate decoupled from
// internal/eventbus's concrete type. Safe to call once at startup; nil
// publisher (the default) disables the bus mirror entirely (tests).
func (s *Spawner) SetEventPublisher(p EventPublisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventPublisher = p
}

// publishEvent nil-guards and marshal-failure-guards a bus publish so
// the run lifecycle mirror can never affect the websocket broadcast it
// follows. orgID is always stamped by the caller — every broadcast
// choke point has it in scope.
func (s *Spawner) publishEvent(orgID, eventType string, metadata any) {
	s.mu.Lock()
	pub := s.eventPublisher
	s.mu.Unlock()
	if pub == nil {
		return
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		delegateLog.Warn("marshal conversation lifecycle event metadata failed", "event_type", eventType, "error", err)
		return
	}
	pub.Publish(domain.Event{
		OrgID:        orgID,
		EventType:    eventType,
		MetadataJSON: string(raw),
		OccurredAt:   time.Now().UTC(),
	})
}

// SetPublicURL wires the deployment's externally-visible base URL, used by
// runURLFor to compute the {{RUN_URL}} prompt placeholder (TFAC-591). Call
// with the same value handed to Server.SetDeployConfig — both call sites in
// internal/app/httpserver.go. Nil-safe to leave unset: runURLFor then
// renders the placeholder empty, never a fabricated localhost link.
//
// Trailing slashes are trimmed, mirroring Server.SetDeployConfig
// (internal/server/auth_handlers.go) — without this, a TF_PUBLIC_URL (or
// BrowserURL) configured with a trailing "/" would produce "//orgs/..." /
// "//runs/..." in runURLFor's concatenation.
func (s *Spawner) SetPublicURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publicURL = strings.TrimRight(url, "/")
}

// runURLFor computes the {{RUN_URL}} placeholder value — a deep link back to
// this run in the TF UI. Empty publicURL degrades to "" (no wrong fallbacks:
// never fabricate a localhost link when the deployment has no configured
// public URL). Local mode's run route has no org segment
// (frontend/src/main.tsx "/runs/:conversationID"); multi mode nests every route under
// "/orgs/:org_id" ("/orgs/:org_id/runs/:conversationID").
func (s *Spawner) runURLFor(orgID, conversationID string) string {
	s.mu.Lock()
	publicURL := s.publicURL
	s.mu.Unlock()
	if publicURL == "" {
		return ""
	}
	if runmode.Current() == runmode.ModeMulti {
		return publicURL + "/orgs/" + orgID + "/runs/" + conversationID
	}
	return publicURL + "/runs/" + conversationID
}

// notifyDrainer fires the QueueDrainer hook for a task if a drainer is
// configured AND the run that just finished was an auto-fired one.
// Manual runs are fully decoupled from the queue by design — they
// neither participate in the gate nor trigger drains. Runs in goroutine
// to keep run-teardown latency unaffected.
func (s *Spawner) notifyDrainer(orgID, triggerType, taskID string) {
	if triggerType == "manual" || taskID == "" {
		return
	}
	s.mu.Lock()
	d := s.drainer
	s.mu.Unlock()
	if d == nil {
		return
	}
	go d.DrainTask(orgID, taskID)
}

// SetRunCredentialResolvers wires the per-org run-credential seam:
// both modes resolve a run's GitHub client + LLM key + default
// model through these, so credential resolution stops branching on mode.
//
//   - resolver: per-(org, owner) GitHub client — App-installation token in
//     multi, keychain PAT in local. Replaces the process-global ghClient
//     the retired UpdateCredentials used to hot-swap from main's local block.
//   - secrets: per-org LLM-credential reader. nil in local → the agent
//     inherits the host's ambient Claude subscription; the system-door
//     reader in multi.
//   - modelFor: per-(org, team) model configuration — the team's default plus
//     the enable-set it may pick from. The prompt's own Model still overrides
//     the default per delegation; nothing overrides the set.
//
// Set once at startup, post-NewSpawner. Any of the three may be nil; the
// resolver helpers fall back to the constructor-supplied ghClient/model
// (the test / no-seam path).
func (s *Spawner) SetRunCredentialResolvers(resolver ghclient.Resolver, secrets agentproc.SecretsReader, modelFor func(context.Context, string, string) (domain.TeamModels, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ghResolver = resolver
	s.runSecrets = secrets
	s.modelFor = modelFor
}

// SetLLMResolver wires the shared LLM-credential resolver (internal/llmcred,
// TFAC-616) so a role-mode Bedrock org mints short-lived STS session creds
// for its delegated runs on the all/local path. Set once at startup like
// SetRunCredentialResolvers; nil in local mode (ambient) and in tests.
func (s *Spawner) SetLLMResolver(r bundleLLMResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.llmResolver = r
}

// getLLMResolver returns the wired resolver under the lock, race-free against
// a startup-time SetLLMResolver.
func (s *Spawner) getLLMResolver() bundleLLMResolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.llmResolver
}

// SetJiraResolver wires the Jira write-actor resolver so the TFAC-300
// board→Jira mirror can resolve the org's system/bot credential at each
// transition. Set once at startup, post-NewSpawner (the resolver is built in
// the app composition, like the GitHub one). Nil-safe: an unset resolver
// disables the mirror — tests and local-mode-without-Jira just skip the Jira
// write. Same post-construction injection shape as SetRunCredentialResolvers.
func (s *Spawner) SetJiraResolver(r jira.Resolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jiraResolver = r
}

// getJiraResolver returns the wired Jira resolver, or nil when the mirror is
// disabled. The lock keeps it race-free against a startup-time SetJiraResolver,
// matching the credential-seam getters.
func (s *Spawner) getJiraResolver() jira.Resolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jiraResolver
}

// SetStorage wires the durable blob store into the spawner. Done
// post-construction (like SetRunCredentialResolvers and SetStores) so the
// constructor signature — and every test fixture's NewSpawner call — stays
// put. Safe to call once at startup; nil is tolerated (tests that never
// touch the workspace seam leave it unset, and Storage() returns nil).
func (s *Spawner) SetStorage(blobs storage.Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs = blobs
}

// SetTeamKB wires the team knowledge-base seam. Post-construction like
// SetStorage, and for the same reason — every fixture's NewSpawner call stays
// put. nil is tolerated: a run whose spawner has no KB stages no knowledge,
// which is the same tree a team with an empty KB produces.
func (s *Spawner) SetTeamKB(kb kbstore.KB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teamKB = kb
}

// TeamKB returns the wired knowledge-base seam, or nil if none was set. The
// lock keeps it race-free against a startup-time SetTeamKB.
func (s *Spawner) TeamKB() kbstore.KB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.teamKB
}

// Storage returns the wired blob store, or nil if none was set. It is the
// read side of the seam for the blueprint-workspace snapshot/rehydrate
// path; the lock keeps it race-free against a startup-time SetStorage.
func (s *Spawner) Storage() storage.Storage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blobs
}

// resolveRunCredentials resolves the per-(org, owner) GitHub client and the
// run's team model configuration. Both modes call this identically;
// mode-awareness lives inside the resolver (App token vs PAT) and the
// secrets reader (system door vs nil), not at the call site. owner is the
// GitHub account the run targets — empty for Jira runs, which don't
// pre-clone. repo disambiguates a bundle-backed lookup (TFAC-614) when the
// run's authorized set covers more than one repo under owner; empty when
// the caller has none in view. teamID is the task's owning team, so a
// multi-team org honors each team's model choice; empty falls back to the
// org default team.
//
// A model that cannot be resolved is an error rather than a substitution, so
// the caller refuses the delegation instead of buying a model nobody chose.
func (s *Spawner) resolveRunCredentials(ctx context.Context, orgID, owner, repo, teamID string) (*ghclient.Client, domain.TeamModels, error) {
	models, err := s.resolveModel(ctx, orgID, teamID)
	if err != nil {
		return nil, domain.TeamModels{}, err
	}
	return s.resolveGHClient(ctx, orgID, owner, repo), models, nil
}

// resolveGHClient resolves the per-(org, owner) GitHub client via the
// resolver, falling back to the constructor-supplied client when
// no resolver is wired (test fixtures). A resolve failure returns nil:
// setupGitHub surfaces "GitHub credentials not configured" for GitHub
// tasks, and Jira runs don't need a client. The error is logged so a real
// backend failure (e.g. vault outage) isn't silent.
//
// repo is the specific repo the call concerns, when the caller has one in
// view (every real caller does — see ownerRepoForTask). It disambiguates
// the TF_ROLE=executor bundle lookup below: unlike the live resolver's
// account-wide App installation token, a bundle's RepoTokens are scoped
// per-repo, so an owner alone is ambiguous whenever a run's authorized set
// covers more than one repo under the same account — passing "" there
// would let credbundle.ResolveRepoToken's map iteration pick an arbitrary sibling
// repo's token, which then 403s every call it's used for.
func (s *Spawner) resolveGHClient(ctx context.Context, orgID, owner, repo string) *ghclient.Client {
	// TF_ROLE=executor never reaches here for a run's GetPR — setupGitHub
	// builds its client against the credential sidecar's GitHub-REST proxy
	// (sidecar), so this resolver path serves only all/local.
	s.mu.Lock()
	resolver := s.ghResolver
	fallback := s.ghClient
	s.mu.Unlock()
	if resolver == nil {
		return fallback
	}
	client, err := resolver.ClientFor(ctx, orgID, owner)
	if err != nil {
		delegateLog.Warn("resolve GitHub client failed", "org", orgID, "target", owner, "error", err)
		return nil
	}
	return client
}

// gitAuthorizeDecision is the git proxy's live per-repo gate (Layer 2 + the
// Layer-3 ref allowlist): the run may touch a repo only if its team tracks it
// AND it appears in the run's conversation_worktrees ledger (the eagerly-cloned task
// repo — recorded at setup — or a workspace-add'd one). The allowed push ref is
// the worktree's LIVE current branch mapped through its configured push
// refspec — "you may push where a bare `git push` from your checkout lands"
// (TFAC-498, refspec-aware) — read fresh from disk per call rather than a
// branch prescribed when the worktree was reserved. The refspec mapping matters for PR
// worktrees: the checkout is the run-namespaced triagefactory/<rootKey>/pr-<n>
// while push tracking maps it to the PR's real head branch, and the
// receive-pack command block the ref gate inspects carries that REMOTE ref —
// comparing against the local name rejected every PR push with a 403
// (ref-not-allowed). A detached HEAD (the state a fresh default / --ref
// `workspace add` lands in) authorizes nothing until the agent creates its own
// branch; the repo's base / default branch is authorized only when the team's
// base-branch push policy (internal/pushpolicy) permits it for this run —
// under the default policy it never is, even when the mapping resolves to it.
// Reads live each call, so untracking a repo, removing
// a worktree, or switching branches propagates to the next request with no
// re-mint. Fails closed (deny) when a store it needs is absent or a lookup it
// depends on errors — a misconfigured or degraded gate must never allow-all,
// and in particular must never authorize a base/protected ref just because the
// profile that names them couldn't be read.
func gitAuthorizeDecision(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, owner, repo string) (gitproxy.Decision, error) {
	// Repos is required: it names the repo's protected refs. Without it we can't
	// honor the "base/protected refs are refused" guarantee, so a wiring missing
	// it must deny rather than fall through to authorizing whatever branch the
	// checkout is on (which could be the base branch).
	if stores.TeamGitHubRepos == nil || stores.ConversationWorktrees == nil || stores.Repos == nil {
		return gitproxy.Decision{Allowed: false}, nil
	}
	repoID := owner + "/" + repo
	// The three reads below each fail the decision closed, and the only thing
	// that reaches an operator is the proxy's one log line at the far end of a
	// relay. Name which read broke: the bare driver error they used to return
	// ("column X does not exist") says what went wrong without saying where,
	// and these three are answering quite different questions.
	tracks, err := stores.TeamGitHubRepos.TracksRepoSystem(ctx, info.TeamID, owner, repo)
	if err != nil {
		return gitproxy.Decision{}, fmt.Errorf("tracked-set read: %w", err)
	}
	if !tracks {
		return gitDenyNotTracked(repoID), nil
	}
	rows, err := stores.ConversationWorktrees.ListSystem(ctx, info.OrgID, info.ConversationID)
	if err != nil {
		return gitproxy.Decision{}, fmt.Errorf("worktree ledger read: %w", err)
	}

	// Base / protected refs are not pushable regardless of what the worktree is
	// checked out on, unless the team's base-branch push policy says otherwise
	// for this run (pushpolicy.ProtectedFor returns an empty set then). A
	// failure to resolve either half fails the whole decision closed (proxy
	// denies) rather than silently authorizing — being unable to tell whether
	// the live branch IS the base branch, or whether the team lifted the guard,
	// is exactly when we must not allow the push.
	protected, err := pushpolicy.ProtectedFor(ctx, stores, info.OrgID, info.TeamID, domain.RepoRef{Owner: owner, Repo: repo}, info.IsEventTriggered)
	if err != nil {
		return gitproxy.Decision{}, fmt.Errorf("push policy read: %w", err)
	}

	var allowedRefs []string
	found := false
	for _, w := range rows {
		if !strings.EqualFold(w.RepoID, repoID) {
			continue
		}
		found = true
		// A HEAD file read plus a few `git config --file` subprocesses per
		// matching row (the current branch comes from a plain .git/HEAD read,
		// no subprocess). conversation_worktrees is keyed (conversation_id, repo_id, ref), so
		// several rows can match; git ops per run are few enough that per-row
		// spawning stays fine.
		branch := worktreePushTargetBranch(w.Path)
		if branch == "" || protected[branch] {
			continue
		}
		allowedRefs = append(allowedRefs, "refs/heads/"+branch)
	}
	if !found {
		// No ledger row for this repo yet. The run's OWN task PR repo is the
		// bootstrap exception: its conversation_worktrees row is written only AFTER the
		// eager-PR setup clone materializes the worktree, yet that clone routes
		// through this same per-run proxy — so the very first fetch would be
		// denied against its own not-yet-written row. Authorize the task's own
		// repo for READ by construction (the run exists to work on that PR).
		// AllowedRefs stays empty, so a push attempted before the row lands is
		// still rejected by the receive-pack gate: read-only bootstrap, push
		// authority is earned once the checkout's branch resolves through a real
		// ledger row.
		if isTaskOwnRepo(ctx, stores, info, owner, repo) {
			return gitproxy.Decision{Allowed: true, ProtectedRefs: pushpolicy.Refs(protected)}, nil
		}
		return gitDenyNotMaterialized(repoID), nil
	}
	// ProtectedRefs rides along on the allow so a ref-level refusal downstream
	// can say WHY it refused: the receive-pack gate sees only the ref set, and
	// "that's the base branch" and "that isn't your worktree's branch" have
	// different remedies. Empty when the team's policy permits base-branch
	// pushes — there is then nothing protected to explain.
	return gitproxy.Decision{
		Allowed:       true,
		AllowedRefs:   allowedRefs,
		ProtectedRefs: pushpolicy.Refs(protected),
	}, nil
}

// The git-proxy deny builders below mirror the exec-gh repo gate
// (cmd/exec/agenthost/local.go authorizeRepo) so a clone/fetch and a gh verb
// that fail on the same repo tell the agent the same thing. The "gitproxy: "
// prefix matches the proxy's other 403 bodies; the agent reads these in git's
// remote output. Keep the wording in sync with authorizeRepo's messages.
func gitDenyNotTracked(repoID string) gitproxy.Decision {
	return gitproxy.Decision{
		Allowed:     false,
		DenyReason:  "repo-not-tracked",
		DenyMessage: fmt.Sprintf("gitproxy: repo %s is not tracked by this team; a team admin must add it as a tracked repo in Settings before it can be used", repoID),
	}
}

func gitDenyNotMaterialized(repoID string) gitproxy.Decision {
	return gitproxy.Decision{
		Allowed:     false,
		DenyReason:  "repo-not-materialized",
		DenyMessage: fmt.Sprintf("gitproxy: repo %s is tracked by this team but not yet materialized in this run; run 'workspace add %s' to persist it, then retry", repoID, repoID),
	}
}

// isTaskOwnRepo reports whether (owner, repo) is the GitHub repo of the run's
// own task — the repo the run was created to work on. It authorizes the initial
// setup clone for read before that repo's conversation_worktrees ledger row exists (the
// row is written post-clone). ConversationInfo carries no task id, so the run is resolved
// to its task here. Non-GitHub tasks and any resolution failure report false,
// falling back to the ledger gate (fail closed).
func isTaskOwnRepo(ctx context.Context, stores db.Stores, info agenthost.ConversationInfo, owner, repo string) bool {
	if stores.Conversations == nil || stores.Tasks == nil || info.ConversationID == "" {
		return false
	}
	conv, err := stores.Conversations.GetSystem(ctx, info.OrgID, info.ConversationID)
	if err != nil || conv == nil || conv.TaskID == "" {
		return false
	}
	task, err := stores.Tasks.GetSystem(ctx, info.OrgID, conv.TaskID)
	if err != nil || task == nil || task.EntitySource != "github" {
		return false
	}
	taskOwner, taskRepo, _ := parseGitHubTask(*task)
	return strings.EqualFold(taskOwner, owner) && strings.EqualFold(taskRepo, repo)
}

// gitPushRecorder builds the git proxy's RecordPush callback for one run. The
// proxy parses each non-delete ref out of a receive-pack body and hands it here
// with the upstream's final status; this is where the outcome splits:
//
//   - 2xx (the push landed): shape it into a branch artifact and upsert it
//     through the same host-side path the pre-push hook uses —
//     agenthost.NewLocal → UpsertArtifact — so the write routes to the right
//     pool (admin for an event-triggered run, the kicking-off user's synthetic
//     claims for a manual one) and lands the ActionBranchPushed audit row in
//     the same write. Same domain.NewBranchArtifact dedup_key as the hook, so
//     an (out-of-mode) hook twin still converges on one row.
//   - non-2xx (the upstream refused it — nothing landed): record ONLY the
//     branch_push_failed audit row. The audit log never omits an attempt;
//     the artifact ledger never gains a branch that doesn't exist.
//
// host is the run's shared audit client rather than one built here, so the
// credential the sidecar reports after bring-up reaches this recorder and the
// denial recorder beside it through a single object.
//
// On the managed Git path this observer is authoritative for push capture —
// every ordinary push transits the proxy (even `git push --no-verify`, which
// skips client hooks), and the pre-push hook stands down there
// (githooks.PushCaptureEnvVar) because it fires before the transfer and would
// record failed pushes as artifacts.
//
// Best-effort: a non-branch ref or a record failure is dropped (logged), never
// surfaced. By the time this runs the push has already completed upstream, so
// nothing it does can block, alter, or fail the push.
func gitPushRecorder(host *agenthost.LocalClient, info agenthost.ConversationInfo) func(context.Context, gitproxy.PushedRef) {
	return func(ctx context.Context, push gitproxy.PushedRef) {
		art, ok := domain.NewBranchArtifact(push.Repo, push.Ref, push.NewSHA, push.Created)
		if !ok {
			return // not a branch ref (tag/other) or an unparseable repo path
		}
		if !push.Succeeded() {
			host.RecordGitPushFailed(ctx, push.Repo, push.Ref, push.NewSHA, push.Created, push.Status)
			return
		}
		if _, err := host.UpsertArtifact(ctx, art); err != nil {
			delegateLog.Warn("git-proxy push capture: record branch artifact failed",
				"conversation", info.ConversationID, "repo", push.Repo, "ref", push.Ref, "error", err)
		}
	}
}

// resolveModel resolves the run's team model configuration via the resolver.
// teamID is the run's owning team; empty falls back to the org default team
// inside the resolver.
//
// With no resolver wired (test fixtures) it answers the constructor-supplied
// model under an unrestricted set — there are no stores to read an enable-set
// out of, and inventing an empty one would refuse every model instead of
// admitting that nothing was configured to narrow.
func (s *Spawner) resolveModel(ctx context.Context, orgID, teamID string) (domain.TeamModels, error) {
	s.mu.Lock()
	fn := s.modelFor
	fallback := s.model
	s.mu.Unlock()
	if fn == nil {
		return domain.NewTeamModels(fallback, domain.ModelSet{}), nil
	}
	return fn(ctx, orgID, teamID)
}

// getRunSecrets returns the per-org LLM-credential reader threaded into
// RunOptions.Secrets: nil in local (ambient subscription fallback), the
// system-door reader in multi.
func (s *Spawner) getRunSecrets() agentproc.SecretsReader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runSecrets
}

// updatePhase records the live engagement's setup sub-state on its own claim
// (phase "" clears it — the agent process is live) and broadcasts the display
// status: the phase itself, or "running" on a clear, so the wire sequence the
// frontend chips key on is unchanged. Goroutine-internal, no JWT claims in
// scope, so admin pool.
//
// claimID names the engagement reporting the progress; empty falls back to
// whichever claim is active on the conversation, for the paths with no
// claimed run in scope.
//
// Returns fenced: true when the claim-keyed write was refused because the
// claim is released — this executor no longer owns the conversation. Nothing
// landed, so the caller has nothing to undo; what it decides is whether to go
// on. The mid-setup phases carry on, because the next fenced write asks the
// same question and there is no runtime to spare yet. The pre-spawn phase is
// different: it is the last write before a sandbox or a node process comes
// up, and an engagement that launches one anyway spends it on a transcript
// whose first row will be refused. Those callers return the fenced
// disposition instead — which writes nothing, exactly as a fenced-out
// engagement must not — rather than spawning into a conversation somebody
// else owns.
func (s *Spawner) updatePhase(ctx context.Context, orgID, conversationID, claimID, phase string) (fenced bool) {
	// Clearing the phase IS agent-live — the one signal both runtimes share —
	// so it is where the engagement's trace root ends (standing decision 5).
	// Ahead of the write, because the span should measure bring-up rather than
	// the bookkeeping that announces it, and because a fence refusal below
	// still leaves the setup we just traced worth exporting.
	if phase == "" {
		s.endEngagement(conversationID, engagementLive)
	}
	// Detached from cancellation exactly as the former context.Background()
	// was — a shutdown mid-setup must not abort the progress write — but
	// WithoutCancel keeps the caller's values, so the write lands inside the
	// engagement's trace instead of as an orphan.
	ctx = context.WithoutCancel(ctx)
	var err error
	if claimID != "" {
		_, err = s.conversations.SetClaimPhaseSystem(ctx, orgID, conversationID, claimID, phase)
	} else {
		_, err = s.conversations.SetActiveClaimPhaseSystem(ctx, orgID, conversationID, phase)
	}
	if errors.Is(err, db.ErrClaimReleased) {
		// Not broadcast: the display status is the row's, and the row belongs
		// to whoever released the claim — announcing this engagement's phase
		// over theirs would show a stopped conversation as setting up.
		delegateLog.Error("claim fence refused a phase write — this executor no longer owns the conversation",
			"conversation", conversationID, "claim_id", claimID, "org_id", orgID, "phase", phase, "error", err)
		return true
	} else if err != nil {
		delegateLog.Warn("update phase for conversation failed", "conversation", conversationID, "error", err)
	}
	display := phase
	if display == "" {
		display = "running"
	}
	s.broadcastConversationUpdate(orgID, conversationID, display)
	// Board placement is not mirrored per-run from setup progress here:
	// the blueprint orchestrator drives the aggregate column via
	// recomputeTaskBoardColumn at its transition points (blueprint start, step
	// start, park, resume). updatePhase stays a pure claim-phase + WS helper.
	return false
}

// setWorktreePath records where a run's workspace resolved, routed exactly the
// way updatePhase routes a phase: an engagement names its own claim and meets
// the fence, a writer with no claim in scope goes through the unfenced door.
//
// Every production caller is an engagement — a source setup's first clone, a
// blueprint step's per-claim stamp, a cold rehydrate landing on a new path —
// and each of them is slow enough to outlive the claim that started it. The
// empty-claim arm is for a caller minting or enqueueing a conversation no
// executor can have picked up yet: no successor can exist, so there is nothing
// to assert ownership against.
//
// The fence refusal is logged HERE, once, and every call site then filters it
// out of its own warning. That split is deliberate: each site's warning names
// what a lost stamp costs it — a rejected follow-up, a repeat cold rehydrate —
// and none of those consequences is true of a refusal. The row still has a
// path; it is the successor's, put there by the engagement that owns the
// conversation now. A caller that logged the refusal under its own message
// would be filing a lost claim as a workspace problem.
//
// The error is still returned rather than swallowed, so a future caller that
// wants to change course on losing ownership can, and so the two outcomes stay
// distinguishable at the call site. No caller does today: a missing path costs
// a cold rehydrate on the next resume, not the run.
func (s *Spawner) setWorktreePath(ctx context.Context, orgID, conversationID, claimID, path string) error {
	var err error
	if claimID != "" {
		_, err = s.conversations.SetWorktreePathForClaimSystem(ctx, orgID, conversationID, claimID, path)
	} else {
		_, err = s.conversations.SetWorktreePathSystem(ctx, orgID, conversationID, path)
	}
	if errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Error("claim fence refused a worktree_path write — a successor owns this conversation; its own workspace stands",
			"conversation", conversationID, "claim_id", claimID, "org_id", orgID, "worktree_path", path, "error", err)
	}
	return err
}

// recomputeTaskBoardColumn is the blueprint-era board placement rule: a task's
// live column (tasks.status) is a recomputed aggregate over its active
// blueprint_run's step runs, never a mirror of one run. For a bot-claimed task
// with an active blueprint_run it sets in_review ("needs 👀") when the blueprint
// has an unresolved artifact (a draft PR / ready review — the derived approval
// signal, never a stored status) or any step
// run is parked open, else in_progress, writing tasks.status only when it
// changes and pushing a WS nudge so peer boards follow.
//
// Terminal columns are NOT owned here — terminateBlueprint closes the task
// (done) on a clean finish and leaves it open for attention on abort/fail. This
// helper deliberately no-ops when there is no active blueprint_run.
//
// The rule has no step-count gate: a 1-step and a multi-step blueprint move
// identically, bouncing in_progress ↔ in_review across however many
// human-interaction points the blueprint has, and the aggregate-over-runs shape
// is parallel-ready by construction (more concurrent runs just feed it). A user
// claim is column-neutral: it flips the claim to the user, so the bot-claim guard
// below short-circuits and the column doesn't move.
//
// All failures are logged-not-fatal — the run state is already persisted and
// broadcast; a failed board write leaves a recoverable state the next transition
// reconciles.
func (s *Spawner) recomputeTaskBoardColumn(orgID, taskID string) {
	if s.tasks == nil || s.blueprints == nil {
		return
	}
	ctx := context.Background()
	task, err := s.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil {
		return
	}
	// Only place bot-claimed tasks. A user takeover flips the claim to the user,
	// who owns the lifecycle from then on — leave their card alone.
	if task.ClaimedByAgentID == "" {
		return
	}
	// A closed/dismissed task is terminal — a late transition must not reopen it.
	if task.Status == "done" || task.Status == "dismissed" {
		return
	}
	// The active blueprint_run is the unit the board tracks. None → no live work
	// to place; the terminal column belongs to terminateBlueprint, so leave the
	// task as-is. When the task was re-delegated, the latest running blueprint_run
	// drives the column, so a stale older run's transitions resolve against the
	// newer run's state rather than clobbering it.
	br, err := s.blueprints.ActiveRunForTaskSystem(ctx, orgID, taskID)
	if err != nil || br == nil {
		return
	}
	convs, err := s.blueprints.ConversationsForBlueprintSystem(ctx, orgID, br.ID)
	if err != nil {
		return
	}
	target := "in_progress"
	for _, r := range convs {
		if r.Status == "open" {
			target = "in_review"
			break
		}
	}
	// An unresolved artifact (draft PR / ready review) is the derived approval
	// signal, and it is derived rather than stored: a step that completed
	// leaving one keeps the task in the approval column even though no run is open.
	// A parked-open run and an unresolved artifact both map to the same column
	// (in_review), so the two checks are unordered as far as the result goes; the
	// loop above runs first only as an optimization — the artifact reads are
	// skipped once a parked-open run has already forced in_review (and reuse the
	// convs slice already loaded above rather than re-fetching).
	if target != "in_review" && s.conversationsHaveUnresolvedArtifacts(ctx, orgID, convs) {
		target = "in_review"
	}
	// Idempotent: skip the write + WS broadcast when already at the target.
	if task.Status == target {
		return
	}
	if _, err := s.tasks.SetStatusSystem(ctx, orgID, taskID, target); err != nil {
		delegateLog.Warn("set board column for task failed", "column", target, "task", taskID, "error", err)
		return
	}
	s.broadcastTaskUpdate(orgID, taskID, target)

	// TFAC-300: mirror the board move back onto the Jira ticket under the org's
	// system/bot credential. Past the idempotency guard, so this fires only on a
	// real column change — and both in_progress and in_review collapse to the
	// same InProgress bucket, so bouncing across human-interaction points makes
	// at most one real Jira move (the rest no-op in the mirror's own membership
	// skip). task is bot-claimed here (guarded above), so the write is always
	// bot-attributed by construction.
	s.mirrorJiraInProgress(orgID, task)
}

// placeTaskInApprovalColumn lands a task in the derived approval column
// (in_review) and leaves it open — the terminal-time counterpart to
// recomputeTaskBoardColumn for a blueprint that COMPLETED with an unresolved
// artifact. recomputeTaskBoardColumn no-ops once the blueprint is
// terminal (no active run), so terminateBlueprint calls this instead to surface
// the still-open task for approval rather than closing it. Same ownership guards
// as recomputeTaskBoardColumn (bot-claimed, non-terminal, idempotent); the Jira
// in-progress re-assert is left to the caller (terminateBlueprint already mirrors
// it once for the completed terminal) so this never double-mirrors.
func (s *Spawner) placeTaskInApprovalColumn(ctx context.Context, orgID, taskID string) {
	if s.tasks == nil {
		return
	}
	task, err := s.tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil {
		return
	}
	// Only place bot-claimed tasks — a user takeover owns the lifecycle.
	if task.ClaimedByAgentID == "" {
		return
	}
	// A closed/dismissed task is terminal — never reopen it.
	if task.Status == "done" || task.Status == "dismissed" {
		return
	}
	if task.Status == "in_review" {
		return // idempotent
	}
	if _, err := s.tasks.SetStatusSystem(ctx, orgID, taskID, "in_review"); err != nil {
		delegateLog.Warn("place task in approval column failed", "task", taskID, "error", err)
		return
	}
	s.broadcastTaskUpdate(orgID, taskID, "in_review")
}

// broadcastTaskUpdate emits a task_updated WS event so the
// board can refetch / patch the card without polling. Payload
// matches the shared event shape (task_id + status) the other
// emitters use (broadcastTaskStatus on the task PATCH arms,
// finalizeRequeue), so the FE's typed WSEvent ('task_updated':
// {task_id, status}) holds across producers.
func (s *Spawner) broadcastTaskUpdate(orgID, taskID, status string) {
	if s.wsHub == nil {
		return
	}
	s.wsHub.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]string{"task_id": taskID, "status": status},
	})
}

// updateBreakerCounter is a no-op stub. The breaker is now query-based
// (see routing.Router + db.CountConsecutiveFailedConversations). Kept as a call site
// placeholder until all callers are cleaned up.
func (s *Spawner) updateBreakerCounter(taskID, triggerType, status string) {
	// Breaker is query-based now — no per-task counter to update.
	// See internal/routing/router.go and internal/db/tasks.go.
}

// broadcastConversationUpdate stamps the run's owning org on the event so the
// hub's per-connection scoping filter routes it only to clients
// authed against that tenant. Every caller is inside a goroutine
// that already has orgID in scope (the run's tenant, set at
// Delegate() entry and threaded through every helper).
func (s *Spawner) broadcastConversationUpdate(orgID, conversationID, status string) {
	if s.wsHub != nil {
		s.wsHub.Broadcast(websocket.Event{
			Type:           "conversation_update",
			OrgID:          orgID,
			ConversationID: conversationID,
			Data:           map[string]string{"status": status},
		})
	}
	s.publishEvent(orgID, domain.EventSystemConversationStatus, events.SystemConversationStatusMetadata{
		ConversationID: conversationID,
		Status:         status,
	})
}

// broadcastConversationResumable is broadcastConversationUpdate's late-workspace arm: the same
// conversation_update carrying the parked status the row already has, plus the
// one thing that changed — this conversation's workspace is accounted for, so
// a follow-up can land now.
//
// It closes one window and only that one. A cross-pod stop parks the row and
// announces `open` from control, before the executor holding the workspace has
// recorded that it owes a persist for it; between those moments the run read
// honestly answers "not resumable", and the browser would keep that answer
// until someone reloaded. The executor's teardown fires this the instant its
// record lands — when the answer changes, not when the blob appears. Every
// other park needs nothing of the sort: an engagement parking its own run
// opens the record before its own flip, so that flip's `open` is already
// resumable when it reaches a client.
//
// The status rides along rather than a new event type because resumability is
// an attribute of the already-announced park, not a new situation — the same
// shape broadcastConversationFailed uses for failure_kind, and it makes the field
// two-way: a retention sweep that collects the snapshot can announce
// `resumable: false` the same way, disabling an open composer live instead of
// failing on send. Consumers must therefore merge a repeated `open`
// idempotently; nothing may read this as a transition.
//
// The hub's cross-pod backplane is what puts it in front of a browser attached
// to some other pod, so the executor emitting it is enough.
func (s *Spawner) broadcastConversationResumable(orgID, conversationID string) {
	if s.wsHub != nil {
		s.wsHub.Broadcast(websocket.Event{
			Type:           "conversation_update",
			OrgID:          orgID,
			ConversationID: conversationID,
			Data:           map[string]any{"status": domain.StatusOpen, "resumable": true},
		})
	}
	resumable := true
	s.publishEvent(orgID, domain.EventSystemConversationStatus, events.SystemConversationStatusMetadata{
		ConversationID: conversationID,
		Status:         domain.StatusOpen,
		Resumable:      &resumable,
	})
}

// broadcastConversationFailed is broadcastConversationUpdate's failure arm: the same
// conversation_update event with the machine-readable failure kind
// alongside the status flip, so the frontend can render kind-specific
// failure copy without a refetch. The key is omitted (not sent empty)
// for an unclassified failure — consumers treat absence as "generic
// failed", which keeps the payload backward compatible.
func (s *Spawner) broadcastConversationFailed(orgID, conversationID string, kind domain.ConversationFailureKind) {
	data := map[string]string{"status": "failed"}
	if kind != domain.ConversationFailureUnclassified {
		data["failure_kind"] = string(kind)
	}
	if s.wsHub != nil {
		s.wsHub.Broadcast(websocket.Event{
			Type:           "conversation_update",
			OrgID:          orgID,
			ConversationID: conversationID,
			Data:           data,
		})
	}
	meta := events.SystemConversationStatusMetadata{ConversationID: conversationID, Status: "failed"}
	if kind != domain.ConversationFailureUnclassified {
		meta.FailureKind = string(kind)
	}
	s.publishEvent(orgID, domain.EventSystemConversationStatus, meta)
}

func (s *Spawner) broadcastMessage(orgID, conversationID string, msg *domain.Message) {
	if s.wsHub != nil {
		s.wsHub.Broadcast(websocket.Event{
			Type:           "message",
			OrgID:          orgID,
			ConversationID: conversationID,
			Data:           msg.ToDTO(),
		})
	}
	// Only a tool-calling assistant turn is published — a plain text or
	// reasoning turn is noise for bus consumers, and this bounds volume.
	//
	// The gate is the row's shape, never a subtype. A subtype marks a row
	// that deviates from ordinary role behavior, and a tool call is the most
	// ordinary thing an assistant row does: every producer writes these rows
	// with a blank one, so a subtype gate here matches nothing.
	if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
		return
	}
	tools := make([]events.ConversationActivityTool, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		tool := events.ConversationActivityTool{Name: tc.Name}
		if desc, ok := tc.Input["description"].(string); ok {
			tool.Description = desc
		}
		tools = append(tools, tool)
	}
	s.publishEvent(orgID, domain.EventSystemConversationActivity, events.SystemConversationActivityMetadata{
		ConversationID: conversationID,
		Tools:          tools,
	})
}
