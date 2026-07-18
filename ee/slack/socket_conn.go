package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"

	slackws "github.com/coder/websocket"
	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// socketEnvelope is Slack's Socket Mode envelope shape: hello carries only
// type; events_api carries envelope_id + payload (the same event_callback
// JSON the webhook receives, unmarshaled below into slackEventEnvelope);
// disconnect carries reason. Aliased ws import (slackws) avoids colliding
// with this package's pervasive use of "ws" as a slackstore.Workspace
// variable name.
type socketEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason,omitempty"`
	// RetryAttempt/RetryReason are Socket Mode's counterpart of the Events
	// API's X-Slack-Retry-Num/-Reason headers: attempt > 0 means Slack is
	// redelivering an envelope an earlier connection (or a slow ack) let
	// lapse. No disable cascade on this transport — Slack just keeps
	// redelivering — but sustained retries are the same "acks aren't keeping
	// up with volume" warning.
	RetryAttempt int    `json:"retry_attempt"`
	RetryReason  string `json:"retry_reason"`
}

// appConnection is one running Socket Mode connection: a goroutine (run,
// started by socketManager.startConn) plus the mutex-guarded status/sightings
// the reconciler's diff and GET /api/slack/workspaces read as a snapshot.
// Disposable, never repaired — run's loop tears the whole connection down on
// any error and dials fresh; there is no in-place recovery.
type appConnection struct {
	appID       string
	orgID       string // the app's org (app->one-org, TFAC-533) — captured once at start
	appTokenRef string // org_secrets key for the app-level (xapp-) token

	cancel context.CancelFunc // stops just this connection, independent of its siblings
	done   chan struct{}      // closed when run returns

	// stats records retry-marked envelopes (stats.go) — set by
	// socketManager.startConn from the manager's shared instance; nil-safe
	// for tests that construct appConnection directly.
	stats *ingestStats

	mu        sync.Mutex
	status    connStatus
	sightings map[string]*sightingView // keyed by team_id
}

// snapshot returns a value copy of the connection's current status,
// including a stable-ordered sightings slice — safe to hand to a caller
// outside the mutex (StatusFor).
func (c *appConnection) snapshot() connStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.status
	if len(c.sightings) > 0 {
		out.Sightings = make([]sightingView, 0, len(c.sightings))
		for teamID, s := range c.sightings {
			out.Sightings = append(out.Sightings, sightingView{TeamID: teamID, Count: s.Count, LastSeen: s.LastSeen})
		}
		sort.Slice(out.Sightings, func(i, j int) bool { return out.Sightings[i].TeamID < out.Sightings[j].TeamID })
	}
	return out
}

func (c *appConnection) setState(state string) {
	c.mu.Lock()
	c.status.State = state
	c.mu.Unlock()
}

func (c *appConnection) setConnected(t time.Time) {
	c.mu.Lock()
	c.status.State = stateOpen
	c.status.ConnectedAt = &t
	c.status.ConsecutiveFailures = 0
	c.status.LastError = ""
	c.mu.Unlock()
}

func (c *appConnection) setLastEvent(t time.Time) {
	c.mu.Lock()
	c.status.LastEventAt = &t
	c.mu.Unlock()
}

func (c *appConnection) recordFailure(state string, err error) {
	c.mu.Lock()
	c.status.State = state
	c.status.LastError = err.Error()
	c.status.ConsecutiveFailures++
	c.mu.Unlock()
}

// recordSighting bumps the in-memory count for an unconnected workspace's
// team_id seen via this app's connection — a hint for TFAC-511's settings
// surface, not a durable record (reset on restart, never written to a table).
func (c *appConnection) recordSighting(teamID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sightings[teamID]
	if !ok {
		s = &sightingView{TeamID: teamID}
		c.sightings[teamID] = s
	}
	s.Count++
	s.LastSeen = socketNow()
}

// stop cancels the connection's context and waits for its goroutine to
// exit. Idempotent-safe to call once per connection (socketManager never
// calls it twice for the same *appConnection).
func (c *appConnection) stop() {
	c.cancel()
	<-c.done
}

// run is the per-app dial loop: connect, serve until the connection ends
// (error, a graceful disconnect frame, or ctx cancellation), then pace the
// next attempt through backoffDuration before trying again. Returns only
// when ctx is done.
func (c *appConnection) run(ctx context.Context, stores db.Stores, pipeline *ingestPipeline, client *http.Client) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		c.setState(stateDialing)
		connectedAt, err := c.serveOnce(ctx, stores, pipeline, client)
		if ctx.Err() != nil {
			return
		}

		// A connection that survived socketBackoffResetAfter is steady
		// state, regardless of why it just ended — the next attempt starts
		// the backoff schedule over from the base delay.
		if !connectedAt.IsZero() && socketNow().Sub(connectedAt) >= socketBackoffResetAfter {
			attempt = 0
		}

		var wait time.Duration
		switch {
		case err != nil && isSlackAuthError(err):
			// A revoked/invalid token will not start working by retrying
			// faster — jump straight to the cap and report a distinct
			// status so this doesn't read as an ordinary transient outage.
			c.recordFailure(stateAuthFailed, err)
			wait = socketBackoffCap
		case err != nil:
			// Logged here (not just recorded onto status): an error-driven
			// reconnect is otherwise invisible in the logs, which show only the
			// next "connection open" with no reason.
			c.recordFailure(stateBackingOff, err)
			wait = backoffDuration(attempt)
			slackLog.Warn("slack socket mode: connection ended, reconnecting",
				"app", c.appID, "error", err, "attempt", attempt, "backoff", wait.String())
			attempt++
		default:
			// A graceful `disconnect` frame — Slack's documented steady-state
			// reconnection, not a failure. attempt was just reset to 0 above
			// (disconnects only fire on a connection that was actually open,
			// which by construction survived past socketBackoffResetAfter),
			// so this redials on the base delay, not the failure schedule.
			c.setState(stateBackingOff)
			wait = backoffDuration(attempt)
			attempt++
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// serveOnce dials one fresh Socket Mode connection and serves it until it
// ends. connectedAt is zero if the connection never reached "open" (dial or
// hello failure); err is nil only for a graceful `disconnect` frame — every
// other exit (dial failure, hello failure, read/ack error, ctx cancellation)
// returns a non-nil err (ctx cancellation errors are discarded by the caller,
// which checks ctx.Err() itself).
func (c *appConnection) serveOnce(ctx context.Context, stores db.Stores, pipeline *ingestPipeline, client *http.Client) (connectedAt time.Time, err error) {
	appToken, err := stores.Secrets.GetSystem(ctx, c.orgID, c.appTokenRef)
	if err != nil {
		return time.Time{}, fmt.Errorf("read app token: %w", err)
	}
	if appToken == "" {
		return time.Time{}, errors.New("app token not configured")
	}

	url, err := slackConnectionsOpen(ctx, client, appToken)
	if err != nil {
		return time.Time{}, fmt.Errorf("apps.connections.open: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, socketDialTimeout)
	conn, _, err := slackws.Dial(dialCtx, url, nil)
	cancel()
	if err != nil {
		return time.Time{}, fmt.Errorf("websocket dial: %w", err)
	}
	// Best-effort: this connection is being torn down regardless (returning
	// from serveOnce always means dialing fresh next), so a close failure
	// here (already-dead socket, peer gone) is not actionable.
	defer func() { _ = conn.CloseNow() }()

	typ, data, err := readEnvelopeFrame(ctx, conn, socketHelloTimeout)
	if err != nil {
		return time.Time{}, fmt.Errorf("read hello: %w", err)
	}
	if typ != slackws.MessageText {
		return time.Time{}, fmt.Errorf("expected a text hello frame, got message type %v", typ)
	}
	var hello socketEnvelope
	if err := json.Unmarshal(data, &hello); err != nil {
		return time.Time{}, fmt.Errorf("unmarshal hello envelope: %w", err)
	}
	if hello.Type != "hello" {
		return time.Time{}, fmt.Errorf("expected hello envelope, got %q", hello.Type)
	}

	connectedAt = socketNow()
	c.setConnected(connectedAt)
	slackLog.Info("slack socket mode: connection open", "app", c.appID)

	// Liveness pinger. The read loop below blocks on serveCtx with no data-frame
	// deadline (an idle connection is not a dead one); the pinger is what detects
	// a genuinely dead peer. On a failed probe it cancels serveCtx, which unblocks
	// Read and ends the connection so run() reconnects. coder/websocket's Ping
	// requires a concurrent Reader to read the pong — the read loop is exactly
	// that. serveCtx is cancelled on any return (deferred), stopping the pinger
	// before conn.CloseNow so it never probes a closing socket.
	serveCtx, cancel := context.WithCancel(ctx)
	pingerDone := make(chan struct{})
	defer func() {
		cancel()
		<-pingerDone
	}()
	go c.pingLoop(serveCtx, conn, cancel, pingerDone)

	for {
		typ, data, err := conn.Read(serveCtx)
		if err != nil {
			return connectedAt, fmt.Errorf("read: %w", err)
		}
		if typ != slackws.MessageText {
			continue
		}
		var env socketEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			slackLog.Warn("slack socket mode: malformed envelope, dropping", "app", c.appID, "error", err)
			continue
		}

		switch env.Type {
		case "hello":
			// Slack sends exactly one at connect time (already consumed
			// above); tolerate a stray duplicate rather than treating it as
			// unknown.
		case "disconnect":
			slackLog.Info("slack socket mode: disconnect requested, redialing", "app", c.appID, "reason", env.Reason)
			c.setState(stateDraining)
			return connectedAt, nil
		case "events_api":
			if c.processEventsAPIEnvelope(ctx, stores, pipeline, env) {
				if err := ackEnvelope(ctx, conn, env.EnvelopeID); err != nil {
					return connectedAt, fmt.Errorf("ack: %w", err)
				}
			}
			// ack=false means the pipeline itself failed — deliberately no
			// ack, so Slack redelivers (at-least-once); never ack-then-process.
		default:
			// Envelope types this leaf doesn't subscribe to (slash_commands,
			// interactive, ...) never arrive today — the manifest requests
			// none of them — but Slack's protocol acks by envelope_id alone,
			// not by type: any envelope carrying one must still be acked or
			// Slack redelivers it indefinitely. Ack blindly and drop the
			// payload; a bare `hello`/heartbeat-style envelope carries no
			// envelope_id and is correctly skipped.
			if env.EnvelopeID != "" {
				if err := ackEnvelope(ctx, conn, env.EnvelopeID); err != nil {
					return connectedAt, fmt.Errorf("ack: %w", err)
				}
			}
		}
	}
}

// pingLoop probes the peer every socketPingInterval, bounded by
// socketPingTimeout, until ctx ends or a probe fails. A failed probe means the
// connection is dead (or wedged past the timeout), so it cancels the
// connection — unblocking serveOnce's Read and driving a reconnect. This is
// the connection's only liveness mechanism now that the read loop no longer
// imposes a data-frame deadline; coder/websocket delivers the pong through the
// concurrent Read in serveOnce, so this must run alongside that loop.
//
// Running from its own goroutine is safe against serveOnce's concurrent
// ack Writes: coder/websocket funnels every frame — control (this ping) and
// data (the ack) — through one internal write mutex, so the two can't
// interleave bytes on the wire or race the shared write buffer.
func (c *appConnection) pingLoop(ctx context.Context, conn *slackws.Conn, onDead context.CancelFunc, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(socketPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, socketPingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return // teardown already in progress; not a ping failure
				}
				slackLog.Warn("slack socket mode: ping failed, tearing down connection",
					"app", c.appID, "error", err)
				onDead()
				return
			}
		}
	}
}

// readEnvelopeFrame reads one frame off conn, bounded by timeout against
// ctx. Cancelling ctx (a stop()) unblocks this immediately — coder/websocket
// closes the connection and returns an error when the context passed to
// Read is done, so shutdown never waits on a blocking Read. Used for the
// bounded hello read; the steady-state loop reads on the connection ctx with
// no deadline (see serveOnce).
func readEnvelopeFrame(ctx context.Context, conn *slackws.Conn, timeout time.Duration) (slackws.MessageType, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Read(rctx)
}

// ackEnvelope sends the {"envelope_id": "..."} acknowledgement Slack
// requires within ~3s of an events_api envelope, or it redelivers.
func ackEnvelope(ctx context.Context, conn *slackws.Conn, envelopeID string) error {
	data, err := json.Marshal(map[string]string{"envelope_id": envelopeID})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, socketAckTimeout)
	defer cancel()
	return conn.Write(wctx, slackws.MessageText, data)
}

// processEventsAPIEnvelope is the boundary both transports share, mirroring
// webhook.go's handleWebhook/handleEventCallback step for step: resolve the
// (team_id, api_app_id) row, gate on entitlement, run the shared pipeline.
// Returns whether the caller should ack — true for every "nothing more to
// do here" case (malformed/mismatched envelope, unconnected workspace,
// cross-org anomaly, lapsed entitlement) exactly like the webhook's
// ackEmpty, and only for a successful pipeline run; false is reserved for a
// genuine processing failure, so Slack's redelivery gives it another shot.
func (c *appConnection) processEventsAPIEnvelope(ctx context.Context, stores db.Stores, pipeline *ingestPipeline, env socketEnvelope) (ack bool) {
	// Recorded before any payload parsing: the retry marker sits on the
	// envelope and is meaningful even when the payload turns out to be
	// nothing this leaf processes — it measures the ack loop, not the event.
	if env.RetryAttempt > 0 {
		slackLog.Warn("slack socket mode: envelope is a Slack retry — an earlier delivery went unacked",
			"app", c.appID, "retry_attempt", env.RetryAttempt, "retry_reason", env.RetryReason)
		c.stats.recordRetry(ctx, c.appID, transportSocket)
	}

	var payload slackEventEnvelope
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		slackLog.Error("slack socket mode: malformed events_api payload", "app", c.appID, "error", err)
		return true
	}
	if payload.APIAppID != c.appID {
		// Should be impossible — Slack only ever routes an app's own
		// envelopes onto that app's connection. If this fires, Slack broke
		// its own model; drop and ack rather than risk redelivering forever.
		slackLog.Error("slack socket mode: envelope api_app_id mismatch",
			"connection_app", c.appID, "payload_app_id", payload.APIAppID)
		return true
	}
	if payload.Type != "event_callback" {
		// url_verification and friends are Events-API-only; Socket Mode
		// never carries them.
		return true
	}

	bundle := slackstore.FromStores(stores)
	ws, err := bundle.Workspaces.GetByWorkspaceAppSystem(ctx, payload.TeamID, payload.APIAppID)
	if err != nil {
		slackLog.Error("slack socket mode: resolve workspace failed", "app", c.appID, "error", err)
		return false // transient — worth a redelivery retry
	}
	if ws == nil {
		// The Grid unconnected-workspace case: this app is installed
		// somewhere TF has no row for. Never respond from ingest (TFAC-511
		// owns any write-back); just record the hint.
		slackLog.Warn("slack socket mode: unconnected workspace", "app", c.appID, "team", payload.TeamID)
		c.recordSighting(payload.TeamID)
		return true
	}
	if ws.OrgID != c.orgID {
		// Structurally guaranteed impossible by the app->one-org invariant
		// (TFAC-533); defensive assert, same posture as the api_app_id check.
		slackLog.Error("slack socket mode: workspace org mismatch",
			"app", c.appID, "workspace_org", ws.OrgID, "connection_org", c.orgID)
		return true
	}
	if !entitlements.For(ws.OrgID).Has(entitlements.FeatureSlack) {
		// Dormancy: an entitlement lapse mid-connection race. The reconciler
		// takes the whole connection down within a tick; until then, drop
		// silently — same as the webhook's ackEmpty for a lapsed org.
		return true
	}

	var inner slackInnerEvent
	if err := json.Unmarshal(payload.Event, &inner); err != nil {
		slackLog.Error("slack socket mode: malformed inner event", "app", c.appID, "error", err)
		return true
	}
	ev := inboundMention{
		Type:     inner.Type,
		Subtype:  inner.Subtype,
		EventID:  payload.EventID,
		Channel:  inner.Channel,
		User:     inner.User,
		BotID:    inner.BotID,
		Text:     inner.Text,
		TS:       inner.TS,
		ThreadTS: inner.ThreadTS,
	}
	if err := pipeline.handleEventCallback(ctx, *ws, ev); err != nil {
		slackLog.Error("slack socket mode: pipeline error", "app", c.appID, "error", err)
		return false
	}
	c.setLastEvent(socketNow())
	return true
}

// backoffDuration computes the full-jitter exponential backoff for the
// given (zero-based) attempt count: a uniform random draw from
// [0, min(socketBackoffCap, socketBackoffBase*2^attempt)]. Full jitter (as
// opposed to capped-exponential-without-jitter) so many connections
// recovering from a shared outage don't all redial in lockstep.
func backoffDuration(attempt int) time.Duration {
	exp := socketBackoffCap
	if attempt < 32 { // guard the shift against overflow well before it matters
		if e := socketBackoffBase * time.Duration(uint64(1)<<uint(attempt)); e > 0 && e < socketBackoffCap {
			exp = e
		}
	}
	if exp <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(exp) + 1))
}
