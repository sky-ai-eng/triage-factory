// Package app is the composition root for the Triage Factory server.
//
// main() stays a thin shell: it parses argv, dispatches CLI subcommands,
// and hands server mode to this package. Everything else — opening the
// database, wiring the dependency graph, registering bus subscribers, and
// running the HTTP server + background workers — lives here, behind a
// small, ordered set of build steps so a first-time reader can follow how
// the binary boots.
//
// The shape is deliberately plain Go (no DI framework): App holds the
// wired graph, New builds it in dependency order, and Run drives the
// runtime. The handful of cyclic references the constructors can't express
// alone (spawner ↔ router, server config-change hooks) are connected in a
// single explicit wire() step.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/credprovision"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/grantmirror"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/instance"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/lease"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/marketplacestats"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/modelprobe"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// App is the fully-wired server: every long-lived subsystem plus the
// boot-time state they were built from. Construct it with New, then call
// Run. The struct is the dependency graph written down in one place.
type App struct {
	cfg Config

	// plan is the per-role subsystem inventory (runmode.Role), computed once
	// in New. Every build/worker branch reads it instead of re-deriving role
	// predicates; the executor exclusion test asserts against it.
	plan subsystemPlan

	// identity is this process's persistent instance identity — resolved
	// first thing in New, held for the process lifetime via an
	// exclusive file lock, released in Close. bootEpoch is the fleet
	// registry's monotonic per-boot counter for that id, minted by
	// registerInstance once stores is live.
	identity  *instance.Identity
	bootEpoch int64

	// Persistence. database is the primary pool (SQLite in local mode,
	// the admin Postgres pool in multi mode); appDB is the multi-mode
	// app/RLS pool, nil in local. Both are closed by Close.
	database *sql.DB
	appDB    *sql.DB
	stores   db.Stores

	// capBroker is the spawned cap-broker subprocess, non-nil only on a host
	// that sandboxes runs (multi mode + Linux). nil otherwise — local mode
	// and non-Linux never sandbox, so there's nothing for a broker to
	// protect. Closed in Close().
	capBroker capBrokerHandle
	// brokerPing round-trips a Ping against the cap-broker's IPC socket,
	// non-nil whenever capBroker is (Linux). The executor healthz's
	// broker_ok check calls it: a broker that stops answering flips an
	// executor's probe to 503, since it can no longer launch sandboxes —
	// its whole job. nil when this host runs no broker (the check then
	// reports ok, there being nothing to verify).
	brokerPing func(context.Context) error

	// Infra.
	bus   *eventbus.Bus
	wsHub *websocket.Hub
	// wsBackplane is the multi-mode cross-pod fan-out for wsHub, the
	// spawner's presence check, and the server's session kick (TFAC-584).
	// nil in local mode — every consumer treats that as "behave exactly
	// as before this existed" (see buildWSBackplane's doc comment).
	wsBackplane *wsbackplane.Backplane

	// deployPublicURL is the deployment's externally-visible base URL —
	// the same value handed to Server.SetDeployConfig (local: a.cfg.BrowserURL;
	// multi: TF_PUBLIC_URL), captured in buildServer for wire() to hand to
	// the spawner once it exists (buildExecution runs after buildServer, so
	// the spawner can't be wired directly at SetDeployConfig time).
	deployPublicURL string

	// Per-org run-credential seam shared by every AI feature.
	ghResolver ghclient.Resolver
	runSecrets agentproc.SecretsReader
	modelFor   func(ctx context.Context, orgID, teamID string) (domain.TeamModels, error)

	// llmResolver is the shared LLM-credential resolver (internal/llmcred,
	// TFAC-616): every Bedrock/Anthropic resolution the brain does flows
	// through it, minting short-lived STS session creds for role-mode orgs
	// and passing stored material through otherwise. Non-nil only in multi
	// mode (built in buildRunCredentials alongside runSecrets); nil in local,
	// where resolution stays on the ambient path exactly as before.
	llmResolver *llmcred.Resolver
	// llmRecorder lands one system_llm_runs row per call TF makes on its own
	// behalf — the three background jobs and the availability probe. One per
	// process, because it also carries the shared provider circuit breaker the
	// background jobs coordinate through.
	llmRecorder *systemllm.Recorder

	// Subsystems.
	scorer     *ai.Manager
	profiler   *repoprofile.Manager
	reconciler *reconcile.Manager
	// reachCache refreshes the reachable-repo mirror the repository picker and
	// the team-repos write gate read. grantReconciler is the App-installation
	// grant reconcile it shares with the poller — one instance, two cadences
	// (the poller's per-cycle pass, and this manager's TTL-gated/forceable one).
	reachCache      *reachcache.Manager
	grantReconciler *grantmirror.Reconciler
	// marketplaceStats is nil in local mode (TFAC-540): the marketplace is
	// multi-mode only, so there's nothing to aggregate — see buildAI and
	// registerSubscribers, which both branch on this being nil rather than
	// re-checking runmode.Current() a second time.
	marketplaceStats *marketplacestats.Manager
	ingestor         *ingest.Ingestor
	eventWake        chan struct{}
	pollerMgr        *poller.Manager
	spawner          *delegate.Spawner
	router           *routing.Router
	srv              *server.Server

	// blobStore is the process-wide durable blob seam (storage.New): the
	// blueprint workspace snapshots. Built once in buildExecution and
	// shared — the spawner gets it via SetStorage.
	blobStore storage.Storage
	// teamKB is the process-wide team knowledge-base seam (kbstore.New): the
	// blob store above in multi, plain files under the state root in local.
	// Both the HTTP surface and the delegation spawner read through it.
	teamKB kbstore.KB

	// placementResolver computes the capacity-weighted rendezvous placement
	// (TFAC-587): the (org, repo) affinity stamp the spawner writes at enqueue
	// and the two-tier claim config the dispatcher passes to ClaimNextConversation.
	// Also backs the GET /api/fleet/placement explainer. Built in
	// buildPlacement; a disabled config (local mode, or TF_PLACEMENT=off)
	// still constructs a resolver whose Enabled() is false — a uniform no-op
	// seam rather than a nil to guard everywhere.
	placementResolver *placement.Resolver

	// leaseElector drives the background-brain lease election (TFAC-583).
	// Non-nil only at role=control in multi mode — role=all/local never
	// elects (see startBrain's direct call in Run and isBrainHolder).
	leaseElector *lease.Manager

	// reaperStore is the fleet reaper's Postgres store (TFAC-586, spec
	// §4.3) — non-nil only for brain-capable roles in multi mode (buildReaper).
	// startBrain/stopBrain start/stop RunReaper + RunRegistryGC against it,
	// nil-checked the same way a.wsBackplane is. reaperStaleThreshold /
	// reaperMaxAttempts are the resolved TF_REAPER_STALE_SEC /
	// TF_MAX_CLAIM_ATTEMPTS knobs those loops run with.
	reaperStore          reaper.Store
	reaperStaleThreshold time.Duration
	reaperMaxAttempts    int

	// credProvisioner is the brain-side sealed-credential-bundle
	// provisioner (TFAC-614, spec's "channel") — resolves a run's LLM/
	// GitHub/Jira credentials, seals them to the claiming executor's
	// published pubkey, and writes claim_credentials. Non-nil only for
	// brain-capable roles in multi mode (buildCredProvisioner), started/
	// stopped alongside the rest of the brain in startBrain/stopBrain,
	// nil-checked the same way a.reaperStore is.
	credProvisioner *credprovision.Manager

	// metricsAddr is the resolved /metrics bind address telemetry.Init
	// returned at boot ("" = metrics disabled) — kept so Run can start the
	// listener after the workers are up. Role-independent: an executor
	// serves metrics exactly like a control pod (its run/dispatch counters
	// are the ones a future ticket instruments), on its own port, never on
	// the user-facing server.
	metricsAddr string

	// runCtx is the ctx Run(ctx) was called with, captured so startBrain
	// (invoked later, from a lease-manager callback or directly at
	// role=all) can derive a cancellable brain-lifetime context from it
	// without threading ctx through the lease callback signature.
	runCtx context.Context

	// brainMu guards brainRunning/brainCancel — the background brain's
	// start/stop state (TFAC-583, brain.go). Transitions happen from the
	// lease manager's own goroutine (Run's tick loop) or, at role=all,
	// once synchronously from Run; the mutex makes concurrent
	// start/stop calls (a demote racing a fresh acquire) safe regardless.
	brainMu      sync.Mutex
	brainRunning bool
	// brainCancel cancels the brain-lifetime context handed to every
	// lease-gated background-brain goroutine (the event-queue drain
	// worker + sweeper, the tf_ctl relay listener, brain-gated EE OnReady
	// workers) — stopBrain's actual stop mechanism. nil when the brain
	// isn't running.
	brainCancel context.CancelFunc

	// Runtime helpers.
	reloader *reloader
}

// New builds the entire server graph in dependency order and returns it
// ready to Run. Each step is one named method; the local/multi-mode forks
// live inside the step that owns them, so this sequence reads the same in
// both modes.
//
// Named returns + the deferred cleanup below: ensureIdentity acquires (and
// holds) the instance identity file's exclusive lock for the process
// lifetime, but a failure on any LATER step must still release it — an
// error return here isn't process exit (retries, or a test calling New
// more than once against the same state root, would otherwise find the
// lock already held and fail confusingly). Every `if err = ...` below
// assigns the named return so the deferred check sees it.
func New(ctx context.Context, cfg Config, static fs.FS) (_ *App, err error) {
	a := &App{cfg: cfg, plan: planForRole(runmode.Role())}
	appLog.Info("boot role", "role", a.plan.role, "mode", runmode.Current())

	// Cheapest check in the file — pure embedded data, no I/O — so it runs
	// before anything acquires a lock or opens a pool.
	if err = checkModelCatalog(cfg.Version, errors.Join(modelcatalog.JoinError(), modelcatalog.SDKLoadError())); err != nil {
		return nil, err
	}

	// Install the OTel meter provider before anything downstream constructs
	// instruments, so every subsystem records against the real SDK from its
	// first sample (disabled → the global stays a no-op and instruments cost
	// nothing). The listener itself starts in Run, once workers are up.
	a.metricsAddr = telemetry.Init(cfg.Version)

	// Resolve this process's persistent instance identity before anything
	// else touches the state root: the identity file's exclusive
	// lock is the two-process-on-one-state-root guard, so it's most
	// valuable held as early in boot as possible — even before the local
	// bind guardrail below.
	if err = a.ensureIdentity(); err != nil {
		return nil, fmt.Errorf("instance identity: %w", err)
	}
	defer func() {
		if err != nil {
			if cerr := a.identity.Close(); cerr != nil {
				appLog.Error("release instance identity lock failed", "error", cerr)
			}
		}
	}()

	if a.local() {
		// Local-mode public-exposure guardrail (TFAC-409 item 1): local mode
		// runs with zero auth, so refuse to boot if it would bind a
		// non-loopback address (or a non-local TF_PUBLIC_URL is set) unless the
		// operator explicitly acknowledges via TF_ALLOW_PUBLIC_LOCAL. Checked
		// first, before any store/secret wiring, so a misconfigured public bind
		// fails fast and cheap.
		if err = a.checkLocalBind(); err != nil {
			return nil, err
		}

		// Local agent-run isolation (TF_LOCAL_SANDBOX). runmode resolved the
		// posture at the top of main; this is where "is it actually usable on
		// this host" gets asked, and a no refuses the boot rather than
		// degrading to unsandboxed runs. It lives in the server path only —
		// an operator running `triagefactory uninstall` on a host without
		// bubblewrap should not be blocked by an agent-run concern.
		if err = a.checkLocalSandbox(); err != nil {
			return nil, err
		}

		// Resolve (and, for the headless encrypted-file backend, construct +
		// validate) the local secret backend up front, so a missing
		// TF_SECRET_ENCRYPTION_KEY or an undecryptable secrets file fails the
		// server at boot rather than on the first credential read. Multi mode
		// uses the Postgres secret store and never touches internal/auth.
		if err = auth.InitLocalSecretBackend(); err != nil {
			return nil, fmt.Errorf("secret backend: %w", err)
		}
	}

	if err = a.openStores(ctx); err != nil { // DB pool(s) + store bundle
		return nil, err
	}
	if err = a.registerInstance(ctx); err != nil { // fleet registry boot-registration upsert
		return nil, err
	}
	// HTTP server + websocket hub — control/all only. An executor serves no
	// user routes; it exposes a localhost-only healthz (see Run) and owns a
	// standalone hub so the spawner's broadcasts are safe no-ops.
	if a.plan.serveHTTP {
		if err = a.buildServer(ctx, static); err != nil { // HTTP handlers + auth + static; owns the websocket hub
			return nil, err
		}
		// TFAC-573: openStores' db.Migrate already ran to completion above (New
		// would have returned its error otherwise) — record that for GET
		// /readyz's "migrations" check now that the server exists to hold it.
		a.srv.SetMigrationsOK(true)
	} else {
		a.buildExecutorRuntime() // standalone hub + public URL; no HTTP listener
	}
	a.buildInfra()                                 // event bus (SetEventBus only when a server exists)
	a.buildWSBackplane()                           // multi-mode cross-pod fan-out for wsHub (TFAC-584); no-op in local mode
	if err = a.buildRunCredentials(); err != nil { // ghResolver / runSecrets / modelFor / llmResolver
		return nil, err
	}
	// Bedrock role-mode setup + connect-probe resolver (TFAC-616) — the
	// server's live AWS calls (GetCallerIdentity / AssumeRole). Multi mode +
	// a serving role only; nil in local (no ambient AWS SDK) leaves the
	// role-setup endpoint reporting the method is control-service only.
	if a.srv != nil && a.llmResolver != nil {
		a.srv.SetBedrockRoleResolver(a.llmResolver)
	}
	if a.srv != nil {
		// Model-availability probes (the settings surface's "test connection"
		// and the eager pass after a credential bind). Wired on every serving
		// process, because a probe resolves credentials exactly as a run does
		// and every serving process has runs to answer for. The seams it takes
		// are therefore the ones a run takes here: the role-aware resolver in
		// multi, and in local the same two nils, which send the probe through
		// the agent runtime against the host's own environment.
		// SystemEnvResolver answers nil for a nil resolver, so local needs no
		// branch of its own.
		a.srv.SetModelProber(modelprobe.New(a.runSecrets, llmcred.SystemEnvResolver(a.llmResolver, "tf-model-probe"), a.llmRecorder))
	}
	if a.plan.brain {
		a.buildAI() // scorer + profiler + reconciler
	}
	if err = a.buildExecution(); err != nil { // delegation spawner
		return nil, err
	}
	// Fleet reaper knobs + (brain roles, multi mode) the reaper Store
	// (TFAC-586). Runs after buildExecution: it wires the spawner's
	// partition self-fence deadline and supersession exit hook, both of
	// which need a.spawner to exist.
	if err = a.buildReaper(); err != nil {
		return nil, err
	}
	// Placement affinity (TFAC-587): the rendezvous resolver + the spawner's
	// enqueue stamp / two-tier claim config + the explainer's server handle.
	// After buildExecution (needs a.spawner) and buildServer (a.srv, when a
	// serving role — nil-checked inside).
	if err = a.buildPlacement(); err != nil {
		return nil, err
	}
	// Brain-side sealed-credential-bundle provisioner (TFAC-614) — same
	// brain-capable-roles-in-multi-mode gate as buildReaper, and must run
	// after it exists so the conversation-signal/instance/conversation-queue
	// stores it reads (a.stores) are already the real bundle.
	if err = a.buildCredProvisioner(); err != nil {
		return nil, err
	}
	// The background-brain lease elector (TFAC-583) — control role in
	// multi mode only. Local (role=all) never elects (startBrain runs
	// directly, unconditionally, from Run); an executor is never
	// brain-capable.
	// Built after buildExecution so the spawner's identity-fence check
	// (IdentityFenced) exists to wire in.
	if a.plan.role == runmode.RoleControl {
		if err = a.buildLease(); err != nil {
			return nil, err
		}
	}
	if a.plan.brain {
		a.buildRouting() // ingestor + poller manager + event router
	}
	a.wire()                  // connect the cycles (spawner↔router, config-change hooks)
	a.registerSubscribers()   // event-bus subscribers (brain only)
	a.registerSentinelRelay() // tf_bus run-sentinel relay (executor only, multi mode)
	return a, nil
}

// Run performs one-time boot side effects, starts the background workers,
// starts (or elects) the background brain, and blocks until ctx is
// cancelled — serving user HTTP on control/all, or the localhost-only
// healthz on an executor.
func (a *App) Run(ctx context.Context) error {
	// Captured for startBrain, invoked later from a lease-manager callback
	// or (role=all) directly below — see the runCtx field doc.
	a.runCtx = ctx

	a.runStartupTasks(ctx)
	a.startWorkers(ctx)
	if a.metricsAddr != "" {
		// Fire-and-forget on every role: Serve logs its own bind failure and
		// a dead metrics listener must never take the process with it.
		go func() { _ = telemetry.Serve(ctx, a.metricsAddr) }()
	}
	if a.plan.serveHTTP {
		a.srv.StartExtensionWorkers(ctx) // replica-safe EE hooks only; brain-gated ones start with the brain below
	}

	switch {
	case a.plan.role == runmode.RoleControl:
		// The lease manager's Run loop drives startBrain/stopBrain via its
		// OnAcquire/OnDemote callbacks (buildLease) as this pod wins,
		// loses, and re-wins the "background-brain" lease across its
		// lifetime. Backgrounded — Run blocks on the HTTP listener below.
		go a.leaseElector.Run(ctx)
	case a.plan.brain: // role == all — local mode only (multi never plans all)
		// Single process, always self-holds — zero lease I/O, brain starts
		// once, unconditionally, exactly like every release before this
		// ticket.
		a.startBrain(1)
	}

	if a.plan.serveHTTP {
		return a.srv.ListenAndServeContext(ctx, a.cfg.Addr)
	}
	// Executor: no user HTTP. Serve the localhost healthz and block until
	// shutdown; the dispatcher + heartbeat + reapers run as workers.
	return a.runExecutorHealthz(ctx)
}

// Close flushes telemetry and releases the database pools and the instance
// identity lock. Safe to call on a partially-built App (nil fields are
// skipped), so `defer a.Close()` right after New is always correct.
func (a *App) Close() error {
	// Spans first, before the pools go: a finished span sitting in the
	// batch processor is lost outright if the process exits without
	// flushing, and the shutdown path is exactly the window whose traces
	// are most worth having. Its own deadline, not the run context's —
	// Close runs after that context has been cancelled by SIGINT/SIGTERM,
	// so an inherited one would abort the flush it is meant to bound. No
	// error return: a dropped batch of diagnostics must not become the
	// process's exit status.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := telemetry.ShutdownTraces(shutdownCtx); err != nil {
		appLog.Error("flush traces failed", "error", err)
	}

	if a.capBroker != nil {
		if err := a.capBroker.Close(); err != nil {
			appLog.Error("close cap-broker failed", "error", err)
		}
	}
	if err := a.identity.Close(); err != nil {
		appLog.Error("release instance identity lock failed", "error", err)
	}
	if a.appDB != nil {
		if err := a.appDB.Close(); err != nil {
			appLog.Error("close app db pool failed", "error", err)
		}
	}
	if a.database != nil {
		return a.database.Close()
	}
	return nil
}

// wire connects the back-edges the constructors can't express on their
// own: the spawner needs the router (built after it), and the server's
// credential-change callbacks need the whole graph. Keeping these in one
// place is what lets buildRouting/buildExecution stay acyclic.
func (a *App) wire() {
	// {{RUN_URL}} prompt placeholder (TFAC-591) — the value captured in
	// buildServer (local a.cfg.BrowserURL / multi TF_PUBLIC_URL) or, on an
	// executor, buildExecutorRuntime (TF_PUBLIC_URL). Every role's spawner
	// renders run deep links, so this is unconditional.
	a.spawner.SetPublicURL(a.deployPublicURL)

	// Fleet-wide presence check (TFAC-584) — every role's spawner runs
	// permission prompts (an executor most of all, post-split), so this is
	// unconditional too. nil in local mode / before buildWSBackplane runs;
	// SetPresenceChecker's nil branch then leaves presentFor reading
	// wsHub.PresentFor directly, unchanged.
	if a.wsBackplane != nil {
		a.spawner.SetPresenceChecker(a.wsBackplane)
	}

	// The router and reloader are brain components. spawner.Delegate ←
	// router (construction arg); router.DrainTask ← spawner
	// (post-construction) — the latter closes the cycle. An executor has
	// neither: its spawner's queue drainer stays nil (nil-safe), and it
	// runs no poller for the reloader to nudge.
	if a.plan.brain {
		a.spawner.SetQueueDrainer(a.router)
		a.reloader = newReloader(a)
	}

	// Config-change callbacks land on the server, which only exists on
	// serveHTTP roles (which, today, are exactly the brain roles — so the
	// reloader wired above is non-nil here).
	if a.plan.serveHTTP {
		a.srv.SetOnGitHubChanged(a.reloader.onGitHubChanged)
		a.srv.SetOnJiraChanged(a.reloader.onJiraChanged)
		a.srv.SetOnSourcesChanged(a.sourcesChanged)
	}
}

// local reports whether the binary is running in single-tenant local mode.
func (a *App) local() bool { return runmode.Current() == runmode.ModeLocal }

// unreleasedVersion is the value main.Version carries when the linker did not
// stamp a release tag. Config.Version is empty on a build path that never
// reaches main (tests constructing a Config directly), which is unreleased for
// the same reason.
const unreleasedVersion = "dev"

// checkModelCatalog decides what a broken model registry costs. It covers both:
// the native registry's join against the pricing datasheet, and the per-SDK
// lists, which have no join and can only be malformed.
//
// Every file involved is compiled in, so a failure is settled when the binary is
// linked: whoever builds it can see it, and an operator running it cannot fix
// it. So an unreleased build refuses to boot — the author is right there, and a
// model TF names but cannot serve is a defect to fix, not to ship — while a
// released build logs the dropped rows and serves the rest. A key retired
// upstream costs a stale binary one picker row; it must never cost it the
// server.
func checkModelCatalog(version string, err error) error {
	if err == nil {
		return nil
	}
	if version == "" || version == unreleasedVersion {
		return fmt.Errorf("model catalog: %w", err)
	}
	appLog.Error("model catalog entries dropped", "error", err)
	return nil
}
