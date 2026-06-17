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
	"io/fs"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/projectclassify"
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

	// Per-org run-credential seam shared by every AI feature.
	ghResolver ghclient.Resolver
	runSecrets agentproc.SecretsReader
	modelFor   func(ctx context.Context, orgID, teamID string) string

	// Subsystems.
	scorer     *ai.Manager
	classifier *projectclassify.Runner
	ingestor   *ingest.Ingestor
	eventWake  chan struct{}
	pollerMgr  *poller.Manager
	spawner    *delegate.Spawner
	curator    *curator.Curator
	router     *routing.Router
	srv        *server.Server

	// Runtime helpers.
	reloader *reloader
	announce *announcer
}

// New builds the entire server graph in dependency order and returns it
// ready to Run. Each step is one named method; the local/multi-mode forks
// live inside the step that owns them, so this sequence reads the same in
// both modes.
func New(ctx context.Context, cfg Config, static fs.FS) (*App, error) {
	a := &App{cfg: cfg, announce: newAnnouncer()}

	if err := a.openStores(ctx); err != nil { // DB pool(s) + store bundle
		return nil, err
	}
	if err := a.buildServer(ctx, static); err != nil { // HTTP handlers + auth + static; owns the websocket hub
		return nil, err
	}
	a.buildInfra()                             // event bus
	a.buildRunCredentials()                    // ghResolver / runSecrets / modelFor
	a.buildAI()                                // scorer + project classifier
	if err := a.buildExecution(); err != nil { // delegation spawner + curator runtime
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
	a.startPolling(ctx)
	return a.srv.ListenAndServeContext(ctx, a.cfg.Addr)
}

// Close releases the database pools. Safe to call on a partially-built
// App (nil pools are skipped), so `defer a.Close()` right after New is
// always correct.
func (a *App) Close() error {
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
