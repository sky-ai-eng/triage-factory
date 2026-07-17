// Socket Mode: the outbound-websocket ingest transport (TFAC-534), feeding
// the exact same pipeline the Events API webhook does (webhook.go's
// handleEventCallback / ingest.go's ingestPipeline). This file owns the
// level-triggered reconciler; socket_conn.go owns the per-app connection
// state machine it starts and stops.
//
// Connections are per-app, not per-workspace: Slack load-balances an app's
// envelopes across up to 10 open sockets regardless of which workspace(s)
// installed it, so the desired state is "one connection per api_app_id that
// has >=1 socket-transport row in an entitled org" — see desiredApps.
package slack

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// socketConfigChanged is the package-level signal notifyConfigChanged
// (workspaces.go) writes to and the production socket manager's run loop
// selects on, so a connect/disconnect triggers an immediate reconcile pass
// instead of waiting up to socketReconcileInterval for the next tick.
// Buffered 1 + non-blocking send: several config changes between reconcile
// passes collapse into a single wakeup. A package-level var (rather than a
// method on the manager) because notifyConfigChanged's call sites
// (handleConnect/handleDelete) have no reference to the manager instance —
// mirrors the existing no-op stub it replaces. Tests build their own
// manager with their own channel (see newSocketManager) so they never touch
// this shared var.
var socketConfigChanged = make(chan struct{}, 1)

// Tunable timings, as package vars (not consts) so tests can shrink them —
// mirrors slackAPIBase/slackHTTPClient's test-override convention.
var (
	socketReconcileInterval = 60 * time.Second
	socketBackoffBase       = 1 * time.Second
	socketBackoffCap        = 5 * time.Minute
	socketBackoffResetAfter = 60 * time.Second
	socketDialTimeout       = 15 * time.Second
	socketHelloTimeout      = 10 * time.Second
	// Liveness is an active ping, not a read deadline: an idle Socket Mode
	// connection legitimately carries no data frames for long stretches (Slack
	// keeps it warm with WebSocket ping control frames the library auto-pongs,
	// which never surface through Read), so a data-frame read deadline would
	// tear down a healthy connection. The pinger probes the peer every
	// socketPingInterval, bounded by socketPingTimeout; a failed probe is what
	// declares the connection dead.
	socketPingInterval = 30 * time.Second
	socketPingTimeout  = 10 * time.Second
	socketAckTimeout   = 5 * time.Second
	socketNow          = time.Now
)

// Per-connection lifecycle states (dialing -> open -> draining -> dead,
// never repaired) plus the two states that describe why a connection isn't
// open right now. These are also the wire values GET /api/slack/workspaces
// exposes for the frontend's status chip.
const (
	stateDialing    = "dialing"
	stateOpen       = "open"
	stateDraining   = "draining"
	stateBackingOff = "backing_off"
	stateAuthFailed = "auth_failed"
)

// connStatus is the observable state of one per-app connection — held by
// its appConnection (mutex-guarded there) and returned as a value snapshot
// by StatusFor / appConnection.snapshot for GET /api/slack/workspaces.
// ConnectedAt / LastEventAt are *time.Time, not time.Time: encoding/json's
// omitempty is a no-op on a plain struct field (time.Time is a struct) — it
// only elides false/0/nil-pointer/empty-string/empty-slice-or-map, so a
// zero-value time.Time here would serialize as
// "connected_at":"0001-01-01T00:00:00Z" instead of being omitted, for every
// connection that hasn't reached open or hasn't seen an event yet. A nil
// pointer, by contrast, omitempty genuinely elides — the convention every
// other optional timestamp in this codebase already follows (ee/sso's
// domains, invites_handler, event_queue, curator, ...).
type connStatus struct {
	State               string         `json:"state"`
	ConnectedAt         *time.Time     `json:"connected_at,omitempty"`
	LastEventAt         *time.Time     `json:"last_event_at,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	LastError           string         `json:"last_error,omitempty"`
	Sightings           []sightingView `json:"sightings,omitempty"`
}

// sightingView is one entry of an unconnected-workspace sighting: a
// team_id this app's connection saw an envelope for with no matching row.
// In-memory only (reset on process restart) — a hint for TFAC-511's
// settings surface, never a durable record.
type sightingView struct {
	TeamID   string    `json:"team_id"`
	Count    int       `json:"count"`
	LastSeen time.Time `json:"last_seen"`
}

// socketManager is the per-process Socket Mode connection reconciler:
// level-triggered, computing desired state fresh on every pass and diffing
// it against the running-connection map. No connection exists outside
// conns — a missed signal heals within one tick (or immediately on the next
// configChanged wakeup). Constructed once at install (see install.go);
// api.OnReady(mgr.run) starts it once app wiring is complete.
type socketManager struct {
	stores        db.Stores
	pipeline      *ingestPipeline
	httpClient    *http.Client
	configChanged <-chan struct{}

	mu    sync.Mutex
	conns map[string]*appConnection // keyed by api_app_id
}

// newSocketManager builds a manager wired to configChanged for its
// notify-now signal. Production passes the package-level socketConfigChanged;
// tests pass a private channel so parallel/sequential test runs can't wake
// each other's reconcilers.
func newSocketManager(stores db.Stores, pipeline *ingestPipeline, client *http.Client, configChanged <-chan struct{}) *socketManager {
	return &socketManager{
		stores:        stores,
		pipeline:      pipeline,
		httpClient:    client,
		configChanged: configChanged,
		conns:         map[string]*appConnection{},
	}
}

// StatusFor returns the live status of appID's socket connection, if the
// manager currently has one running (started by a reconcile pass, not yet
// torn down by a later one). ok=false covers every "no connection" case
// alike — webhook-only app, unentitled org, not yet reconciled — GET
// /api/slack/workspaces treats them identically (omits the field).
func (m *socketManager) StatusFor(appID string) (connStatus, bool) {
	m.mu.Lock()
	conn, ok := m.conns[appID]
	m.mu.Unlock()
	if !ok {
		return connStatus{}, false
	}
	return conn.snapshot(), true
}

// run is the reconciler's main loop — the function api.OnReady hands to
// StartExtensionWorkers. Local mode never has a connected Slack workspace
// (the store is Postgres-only, see ee/slack/store/lite), so it returns
// immediately rather than logging a ListAllSystem failure every tick.
func (m *socketManager) run(ctx context.Context) {
	if runmode.Current() == runmode.ModeLocal {
		return
	}

	slackLog.Info("slack socket mode: connection manager starting")
	m.reconcile(ctx)

	ticker := time.NewTicker(socketReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-ticker.C:
			m.reconcile(ctx)
		case <-m.configChanged:
			m.reconcile(ctx)
		}
	}
}

// desiredApp is one entry of the reconciler's desired state: an api_app_id
// that should have a live connection, plus what a fresh connection for it
// needs to dial.
type desiredApp struct {
	appID       string
	orgID       string
	appTokenRef string
}

// desiredApps computes the full desired connection set: every api_app_id
// with at least one transport=socket row in an entitled org. app->one-org
// (TFAC-533) means every row sharing an api_app_id carries the same org_id,
// so entitlement and the app-token secret ref are well-defined regardless
// of which matching row supplies them — "credentials come off any of its
// rows, same secret set" (see the ticket's architecture note). A nil Slack
// store bundle (extension not linked) and the Postgres-only store's
// ErrNotApplicableInLocal both yield an empty desired set with a nil error —
// run() already gates the reconciler out of local mode entirely, so this is
// a defensive backstop, not a path exercised in production; the point is a
// misconfigured/direct call here logs nothing instead of an ERROR line every
// tick.
func (m *socketManager) desiredApps(ctx context.Context) (map[string]desiredApp, error) {
	bundle := slackstore.FromStores(m.stores)
	if bundle == nil {
		return nil, nil
	}
	rows, err := bundle.Workspaces.ListAllSystem(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotApplicableInLocal) {
			return nil, nil
		}
		return nil, err
	}

	desired := make(map[string]desiredApp)
	for _, row := range rows {
		if row.Transport != transportSocket {
			continue
		}
		if !entitlements.For(row.OrgID).Has(entitlements.FeatureSlack) {
			continue
		}
		if row.AppTokenRef == "" {
			slackLog.Error("slack socket mode: socket-transport row has no app token ref, skipping",
				"app", row.APIAppID, "workspace", row.WorkspaceID)
			continue
		}
		if _, exists := desired[row.APIAppID]; !exists {
			desired[row.APIAppID] = desiredApp{appID: row.APIAppID, orgID: row.OrgID, appTokenRef: row.AppTokenRef}
		}
	}
	return desired, nil
}

// reconcile runs one full pass: compute desired state, diff against the
// running map, tear down what's no longer desired, start what's newly
// desired. Stops run concurrently but reconcile waits for them all to
// finish before starting anything new, so a caller that reconciles again
// right after (or inspects m.conns) sees a settled map — the shape the
// "row deleted -> torn down" and "two rows one app -> exactly one
// connection" tests rely on.
func (m *socketManager) reconcile(ctx context.Context) {
	desired, err := m.desiredApps(ctx)
	if err != nil {
		slackLog.Error("slack socket mode: reconcile failed to list workspaces", "error", err)
		return
	}

	m.mu.Lock()
	var stop []*appConnection
	for appID, conn := range m.conns {
		if _, ok := desired[appID]; !ok {
			stop = append(stop, conn)
			delete(m.conns, appID)
		}
	}
	var start []desiredApp
	for appID, d := range desired {
		if _, ok := m.conns[appID]; !ok {
			start = append(start, d)
		}
	}
	m.mu.Unlock()

	if len(stop) > 0 {
		var wg sync.WaitGroup
		for _, conn := range stop {
			wg.Add(1)
			go func(c *appConnection) {
				defer wg.Done()
				c.stop()
			}(conn)
		}
		wg.Wait()
	}

	for _, d := range start {
		m.startConn(ctx, d)
	}
}

// startConn launches conn's connection loop in its own goroutine under a
// context derived from ctx, so a later per-app stop() (reconcile removing
// just this app) doesn't require cancelling every other connection.
func (m *socketManager) startConn(ctx context.Context, d desiredApp) {
	connCtx, cancel := context.WithCancel(ctx)
	conn := &appConnection{
		appID:       d.appID,
		orgID:       d.orgID,
		appTokenRef: d.appTokenRef,
		cancel:      cancel,
		done:        make(chan struct{}),
		sightings:   map[string]*sightingView{},
		status:      connStatus{State: stateDialing},
	}

	m.mu.Lock()
	m.conns[d.appID] = conn
	m.mu.Unlock()

	slackLog.Info("slack socket mode: starting connection", "app", d.appID, "org", d.orgID)
	go func() {
		defer close(conn.done)
		conn.run(connCtx, m.stores, m.pipeline, m.httpClient)
	}()
}

// stopAll tears down every running connection — called once, from run's
// ctx.Done() branch. Each connCtx already inherits ctx's cancellation, so
// the connection loops are already unwinding by the time this runs; it
// exists to actually wait for them (via stop()'s <-c.done) so shutdown
// doesn't return while connections are still mid-close.
func (m *socketManager) stopAll() {
	m.mu.Lock()
	conns := make([]*appConnection, 0, len(m.conns))
	for appID, c := range m.conns {
		conns = append(conns, c)
		delete(m.conns, appID)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *appConnection) {
			defer wg.Done()
			c.stop()
		}(c)
	}
	wg.Wait()
}
