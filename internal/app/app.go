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
	"fmt"
	"io/fs"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/instance"
	"github.com/sky-ai-eng/triage-factory/internal/marketplacestats"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/projectclassify"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// App is the fully-wired server: every long-lived subsystem plus the
// boot-time state they were built from. Construct it with New, then call
// Run. The struct is the dependency graph written down in one place.
type App struct {
	cfg Config

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
	database   *sql.DB
	appDB      *sql.DB
	stores     db.Stores
	storedPort int

	// Infra.
	bus   *eventbus.Bus
	wsHub *websocket.Hub

	// deployPublicURL is the deployment's externally-visible base URL —
	// the same value handed to Server.SetDeployConfig (local: a.cfg.BrowserURL;
	// multi: TF_PUBLIC_URL), captured in buildServer for wire() to hand to
	// the spawner once it exists (buildExecution runs after buildServer, so
	// the spawner can't be wired directly at SetDeployConfig time).
	deployPublicURL string

	// Per-org run-credential seam shared by every AI feature.
	ghResolver ghclient.Resolver
	runSecrets agentproc.SecretsReader
	modelFor   func(ctx context.Context, orgID, teamID string) string

	// Subsystems.
	scorer     *ai.Manager
	profiler   *repoprofile.Manager
	classifier *projectclassify.Manager
	reconciler *reconcile.Manager
	// marketplaceStats is nil in local mode (TFAC-540): the marketplace is
	// multi-mode only, so there's nothing to aggregate — see buildAI and
	// registerSubscribers, which both branch on this being nil rather than
	// re-checking runmode.Current() a second time.
	marketplaceStats *marketplacestats.Manager
	ingestor         *ingest.Ingestor
	eventWake        chan struct{}
	pollerMgr        *poller.Manager
	spawner          *delegate.Spawner
	curator          *curator.Curator
	router           *routing.Router
	srv              *server.Server

	// Runtime helpers.
	reloader *reloader
	announce *announcer
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
	a := &App{cfg: cfg, announce: newAnnouncer()}

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
	if err = a.buildServer(ctx, static); err != nil { // HTTP handlers + auth + static; owns the websocket hub
		return nil, err
	}
	// TFAC-573: openStores' db.Migrate already ran to completion above (New
	// would have returned its error otherwise) — record that for GET
	// /readyz's "migrations" check now that the server exists to hold it.
	a.srv.SetMigrationsOK(true)
	a.buildInfra()                            // event bus
	a.buildRunCredentials()                   // ghResolver / runSecrets / modelFor
	a.buildAI()                               // scorer + project classifier
	if err = a.buildExecution(); err != nil { // delegation spawner + curator runtime
		return nil, err
	}
	a.buildRouting()        // ingestor + poller manager + event router
	a.wire()                // connect the cycles (spawner↔router, config-change hooks)
	a.registerSubscribers() // event-bus subscribers
	return a, nil
}

// Run performs one-time boot side effects, starts the background workers,
// kicks the first poll cycle, and serves HTTP until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	a.runStartupTasks(ctx)
	a.startWorkers(ctx)
	a.srv.StartExtensionWorkers(ctx)
	a.startPolling()
	return a.srv.ListenAndServeContext(ctx, a.cfg.Addr)
}

// Close releases the database pools and the instance identity lock. Safe
// to call on a partially-built App (nil fields are skipped), so
// `defer a.Close()` right after New is always correct.
func (a *App) Close() error {
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
	// spawner.Delegate ← router (construction arg); router.DrainEntity ←
	// spawner (post-construction). The latter closes the cycle.
	a.spawner.SetQueueDrainer(a.router)
	// {{RUN_URL}} prompt placeholder (TFAC-591) — the value captured in
	// buildServer from whichever branch ran (local a.cfg.BrowserURL / multi
	// TF_PUBLIC_URL), same as SetDeployConfig received.
	a.spawner.SetPublicURL(a.deployPublicURL)

	a.reloader = newReloader(a)
	a.srv.SetOnGitHubChanged(a.reloader.onGitHubChanged)
	a.srv.SetOnJiraChanged(a.reloader.onJiraChanged)
}

// local reports whether the binary is running in single-tenant local mode.
func (a *App) local() bool { return runmode.Current() == runmode.ModeLocal }

// announcer tracks the per-source "announce the next poll completion as a
// toast" flag. Set when a config change restarts a poller; cleared after
// the first post-restart completion fires, so users get one confirmation
// toast instead of every-cycle spam. Shared between the reloader (which
// sets it) and the poll-tracker subscriber (which consumes it).
type announcer struct {
	mu      sync.Mutex
	pending map[string]bool
}

func newAnnouncer() *announcer { return &announcer{pending: map[string]bool{}} }

func (a *announcer) setPending(source string) {
	a.mu.Lock()
	a.pending[source] = true
	a.mu.Unlock()
}

func (a *announcer) shouldAnnounce(source string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending[source] {
		a.pending[source] = false
		return true
	}
	return false
}
