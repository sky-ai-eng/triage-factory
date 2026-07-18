package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var healthzLog = logging.Component("healthz")

// DefaultExecutorHealthzPort is the localhost port the executor healthz
// listener binds when TF_HEALTHZ_PORT is unset. The container HEALTHCHECK
// targets it; it carries no tenant data and takes no inbound traffic
// beyond the local health probe, so it binds loopback only and has no auth.
const DefaultExecutorHealthzPort = "3001"

// executorHealthzResponse is GET /healthz's body on an executor pod
// (spec §8.3). Every field is a liveness/introspection primitive — no
// tenant data — so the endpoint stays auth-free on a loopback bind.
type executorHealthzResponse struct {
	DispatcherAlive       bool  `json:"dispatcher_alive"`
	LastHeartbeatWriteAge int64 `json:"last_heartbeat_write_age_sec"`
	HeartbeatEverWritten  bool  `json:"heartbeat_ever_written"`
	BrokerOK              bool  `json:"broker_ok"`
	ActiveRuns            int   `json:"active_runs"`
	Draining              bool  `json:"draining"`
	Fenced                bool  `json:"fenced"`
}

// runExecutorHealthz serves the executor's localhost-only healthz endpoint
// and blocks until ctx is cancelled (the executor's Run has no user HTTP
// server to block on instead). The dispatcher, heartbeat, and reapers are
// already running as background workers by the time this is reached.
func (a *App) runExecutorHealthz(ctx context.Context) error {
	addr := net.JoinHostPort("127.0.0.1", executorHealthzPort())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleExecutorHealthz)

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// Loopback-only shrinks the audience to local processes, but the
		// auth-free listener still shouldn't hand out held-open connections
		// for free — same bounded posture as the /metrics listener
		// (internal/telemetry): a health probe is one small GET, so the
		// whole exchange is bounded, not just the header read.
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverExited := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
		case <-serverExited:
		}
	}()

	healthzLog.Info("executor healthz listening", "addr", addr)
	err := httpSrv.ListenAndServe()
	close(serverExited)
	<-shutdownDone

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// handleExecutorHealthz builds the liveness body and picks the status code.
//
// 200 requires the dispatcher loop to be live AND the last heartbeat write
// to have landed within 3× the heartbeat interval AND the cap-broker to
// answer a Ping (an executor that can't launch sandboxes is useless, so a
// dead broker is a hard 503) — 503 otherwise.
//
// draining and fenced are INFORMATIONAL and never flip the code (spec /
// #624): a fenced executor stays 200-but-fenced:true until TFAC-586's exit
// lands (the fence latch already stopped it claiming), so the HEALTHCHECK
// must not kill it prematurely. Because a fence also stops the heartbeat
// loop — its last write goes stale — the fenced case is forced 200 so that
// staleness doesn't read as a crash.
func (a *App) handleExecutorHealthz(w http.ResponseWriter, r *http.Request) {
	resp := executorHealthzResponse{
		DispatcherAlive: a.spawner.DispatcherAlive(),
		BrokerOK:        a.checkBrokerOK(r.Context()),
		ActiveRuns:      a.spawner.ActiveRuns(),
		Draining:        a.spawner.Draining(),
		Fenced:          a.spawner.IdentityFenced(),
	}

	age, everWritten := a.spawner.LastHeartbeatWriteAge()
	resp.HeartbeatEverWritten = everWritten
	if everWritten {
		resp.LastHeartbeatWriteAge = int64(age.Seconds())
	}

	// Fresh means a heartbeat has landed AND within 3× the interval.
	staleThreshold := 3 * delegate.DefaultInstanceHeartbeatInterval
	heartbeatFresh := everWritten && age <= staleThreshold

	code := http.StatusOK
	if !executorHealthy(resp.DispatcherAlive, heartbeatFresh, resp.BrokerOK, resp.Fenced) {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// executorHealthy is the 200/503 decision for the executor healthz, split
// out pure for testing. The hard checks are dispatcher liveness, heartbeat
// freshness, and broker reachability. A fenced executor is forced healthy:
// the fence is informational (#624) — the latch already stopped it claiming
// and stopped its heartbeat loop (so heartbeatFresh would be false), and
// TFAC-586 owns the actual exit; the HEALTHCHECK must not kill it early.
func executorHealthy(dispatcherAlive, heartbeatFresh, brokerOK, fenced bool) bool {
	if fenced {
		return true
	}
	return dispatcherAlive && heartbeatFresh && brokerOK
}

// checkBrokerOK round-trips a Ping against the cap-broker socket. When this
// host runs no broker (a.brokerPing nil — a non-sandboxing multi host, or
// the broker was never expected), there is nothing to verify, so it reports
// ok. On a real Linux executor the broker is the sole sandbox launch path,
// so a Ping failure means the executor can no longer do its job.
func (a *App) checkBrokerOK(ctx context.Context) bool {
	if a.brokerPing == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return a.brokerPing(ctx) == nil
}

// executorHealthzPort resolves the healthz bind port: TF_HEALTHZ_PORT when
// set to a valid port, else DefaultExecutorHealthzPort.
func executorHealthzPort() string {
	raw := strings.TrimSpace(os.Getenv("TF_HEALTHZ_PORT"))
	if raw == "" {
		return DefaultExecutorHealthzPort
	}
	if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 65535 {
		healthzLog.Warn("invalid TF_HEALTHZ_PORT; using default", "value", raw, "default", DefaultExecutorHealthzPort)
		return DefaultExecutorHealthzPort
	}
	return raw
}
