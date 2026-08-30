package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	ws "github.com/coder/websocket"
)

// Event is a message sent to connected clients over the websocket.
//
// ConversationID is an optional discriminator frontend listeners filter on:
// it identifies events from a single conversation (message /
// conversation_update). Events that broadcast to the whole UI
// (tasks_updated, scoring_*) leave it empty.
//
// OrgID and UserID are server-side routing fields used by the hub's
// per-connection scoping. They are intentionally NOT serialised on the
// wire (json:"-"): the frontend filters by ConversationID, and
// coupling it to server-side identity would leak who-owns-what to
// other tabs/extensions parsing the WS stream. Empty OrgID means
// "system event, deliver to every connection"; empty UserID means
// "not user-specific". The hub's filter only kicks in when both
// event-side and client-side values are set — see Broadcast.
type Event struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id,omitempty"`
	OrgID          string `json:"-"`
	UserID         string `json:"-"`
	Data           any    `json:"data"`
}

// Close codes the hub sends when it actively kicks a connection
// (TFAC-75). They live in the private application range (4000-4999) so
// the browser surfaces them verbatim on the CloseEvent, letting the
// client distinguish a deliberate kick from a network drop.
//
//   - CloseSessionRevoked: the credential backing this connection was
//     revoked — the session (logout / logout-all), or the API token that
//     authorized the handshake. The client should clear local auth state
//     and route to /login WITHOUT reconnecting.
//   - CloseMembershipChanged: the user lost membership in the org this
//     connection was scoped to. The client should refresh its session
//     view (active org may be gone) and re-handshake, but stay logged
//     in — it may still be a member of other orgs.
const (
	CloseSessionRevoked    ws.StatusCode = 4001
	CloseMembershipChanged ws.StatusCode = 4002
)

// Backplane is the optional cross-pod publish hook a multi-mode deployment
// wires in (TFAC-584, docs/for-agents/specs/horizontal-scaling/README.md §5.1):
// Broadcast calls Publish after fanning to this pod's local sockets, so a
// Postgres LISTEN/NOTIFY relay can mirror the event onto every other
// control pod's Hub (via DeliverRemote there). A nil Backplane — the
// default, and always the case in local mode — makes Broadcast behave
// exactly as it always has: local-only fan-out, zero behavior change.
//
// Backplane implementations MUST NOT call Broadcast (or anything that
// reaches it) when applying a received remote event — that would
// re-publish it and echo it back onto the wire. Use DeliverRemote, which
// only fans to local sockets. This is what makes "no echo loops" true by
// construction rather than by a runtime origin check inside this package.
type Backplane interface {
	Publish(evt Event)
}

// Hub manages websocket connections and broadcasts events to all clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	// byUser indexes the live connections of each authenticated user so
	// a revoke / membership-removal can actively close them (TFAC-75)
	// without scanning every client. Keyed by userID; the inner set is
	// the user's connections (a user may hold several — tabs, devices).
	// Unscoped clients (empty userID — pre-auth / test harness) are not
	// indexed: they carry no identity to target. Guarded by mu, same as
	// clients.
	byUser map[string]map[*client]struct{}
	// backplane is the optional cross-pod publish hook (TFAC-584). Wired
	// via SetBackplane in multi mode only; nil everywhere else, including
	// every read of it in Broadcast is a plain nil-check.
	backplane Backplane
	// connSeq mints per-process-unique connection ids for Snapshot
	// (TFAC-584's presence heartbeat) — see client.connID's doc comment.
	connSeq uint64
}

type client struct {
	conn   *ws.Conn
	send   chan []byte
	closed chan struct{} // signals writePump to exit
	// userID + orgID are captured at handshake by HandleWS. Empty means
	// "unscoped client" (pre-auth, tests that hit the hub without the
	// full server pipeline); such a client receives every event,
	// matching the pre-D9b behavior. The filter in Broadcast only
	// kicks in when both event and client carry values.
	userID string
	orgID  string
	// sid is the opaque session id backing this connection (TFAC-75),
	// captured at handshake. It lets the revoke path close exactly the
	// connections of ONE session (single-device logout) without kicking
	// the same user's other still-valid sessions. Empty for unscoped /
	// local-mode clients (no session), which the revoke path never
	// targets.
	sid string
	// tokenID is the API token that authorized this handshake, when a
	// Bearer header did rather than a cookie. Exactly one of sid and
	// tokenID is ever set — a request authenticates one way — and the
	// revalidation sweep reads whichever it is, so a revoked token's socket
	// dies on the same cadence a revoked session's does.
	tokenID string
	// viewing + visible are the client's self-reported presence (TFAC-392),
	// updated from inbound "presence" control frames in readPump. viewing is
	// "board", "run:<conversationID>", or "other" (the latter for any non-answer-capable
	// surface); visible mirrors the Page Visibility / focus state of the tab.
	// Both are guarded by the hub mutex (written in readPump under h.mu, read in
	// PresentFor under the RLock). Zero value ("", false) is "not on an
	// answer-capable surface / not focused" — a fresh connection counts as absent
	// until it reports otherwise.
	viewing string
	visible bool
	// connID is a per-process-unique identifier minted at connect time
	// (TFAC-584), used only by the multi-mode presence heartbeat to key
	// ws_presence rows (qualified with this pod's instance id there, since
	// the sequence is only unique within one process). Never sent to the
	// browser, never compared for anything other than that upsert key.
	connID string
}

// presenceMsg is the inbound client→server control frame shape for presence
// reporting (TFAC-392). The frontend sends one on navigation and on
// visibility/focus change so the hub can answer PresentFor without polling the
// client. Unknown message types decode into this struct with Type != "presence"
// and are ignored (forward-compat), as are non-JSON frames (pings).
type presenceMsg struct {
	Type    string `json:"type"`
	Viewing string `json:"viewing"`
	Visible bool   `json:"visible"`
}

// NewHub creates a new websocket hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
		byUser:  make(map[string]map[*client]struct{}),
	}
}

// ConnIdentity is who a websocket connection belongs to and what authorized
// it, captured at handshake and fixed for the connection's life.
//
// UserID and OrgID drive Broadcast's scoping filter. SessionID and TokenID name
// the credential, so a revalidation sweep can close exactly the connections
// whose credential died; at most one is set, since a request authenticates one
// way. Every field empty is the "unscoped" client — pre-auth, local mode, a
// test hitting the hub without the server pipeline — which receives every event
// and no sweep targets.
type ConnIdentity struct {
	UserID    string
	OrgID     string
	SessionID string
	TokenID   string
}

// HandleWS is the HTTP handler for websocket upgrade requests. The
// caller is responsible for extracting identity from r.Context() and
// passing it in — this keeps pkg/websocket free of any import on
// internal/server (which would be the wrong direction architecturally).
// The wrapper handler that mounts this lives in internal/server and pulls
// ClaimsFrom + OrgIDFrom before invoking us.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request, id ConnIdentity) {
	conn, err := ws.Accept(w, r, &ws.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		wsLog.Warn("accept failed", "error", err)
		return
	}

	h.mu.Lock()
	h.connSeq++
	c := &client{
		conn:    conn,
		send:    make(chan []byte, 64),
		closed:  make(chan struct{}),
		userID:  id.UserID,
		orgID:   id.OrgID,
		sid:     id.SessionID,
		tokenID: id.TokenID,
		connID:  strconv.FormatUint(h.connSeq, 10),
	}
	h.clients[c] = struct{}{}
	h.indexLocked(c)
	h.mu.Unlock()

	wsLog.Debug("client connected", "total", h.ClientCount())

	// Start write pump in background
	go h.writePump(c)

	// Read pump (blocks until disconnect)
	h.readPump(c)

	// Cleanup: remove from maps first (under write lock so Broadcast and
	// CloseUserConnections can't see this client), then signal writePump
	// to exit via closed channel. We never close c.send — writePump
	// drains it naturally.
	h.mu.Lock()
	delete(h.clients, c)
	h.deindexLocked(c)
	h.mu.Unlock()
	close(c.closed)
	// Best-effort close; the client is already gone in most cases so
	// the error (broken pipe / already-closed) is not actionable.
	_ = conn.Close(ws.StatusNormalClosure, "")

	wsLog.Debug("client disconnected", "total", h.ClientCount())
}

// Broadcast sends an event to all connected clients, gated by the
// per-connection (orgID, userID) scope captured at handshake.
//
// Filter semantics:
//
//   - evt.OrgID == "" — system-wide event, delivers to every client
//     regardless of their org. Used for system:poll:* sentinels,
//     deployment-level toasts, etc.
//   - c.orgID == "" — unscoped client (pre-auth, test harness),
//     receives every event. Matches pre-D9b behavior.
//   - Both set + mismatch → skip. This is the multi-tenant case.
//
// The per-user filter has the same shape: evt.UserID != "" AND
// c.userID != "" AND they differ → skip. UserID is reserved for
// per-user events (e.g. "another user took over your run"); today
// every caller leaves it empty.
//
// Nil-receiver-safe: a nil *Hub silently drops the event so callers
// that conditionally have a hub (tests, pre-wired packages) don't
// have to guard every call site.
//
// Multi-mode cross-pod fan-out (TFAC-584): after delivering locally via
// DeliverRemote, Broadcast publishes evt to the optional Backplane so
// every other control pod's Hub mirrors it to its own local sockets. A
// nil backplane (local mode, or before SetBackplane runs) makes this a
// no-op — Broadcast is then exactly the local-only fan-out it always was.
func (h *Hub) Broadcast(evt Event) {
	if h == nil {
		return
	}
	h.DeliverRemote(evt)
	if h.backplane != nil {
		h.backplane.Publish(evt)
	}
}

// DeliverRemote fans evt to this pod's locally-connected sockets only,
// applying the same per-connection (orgID, userID) scope as Broadcast —
// see Broadcast's doc comment for the filter semantics. It never touches
// the backplane.
//
// This is the local half Broadcast always performed, split out so a
// multi-pod backplane's tf_ws LISTEN dispatcher can apply a REMOTE pod's
// event to this pod's sockets without re-publishing it (TFAC-584: "no
// echo loops, by construction" — a backplane must call this, never
// Broadcast, when relaying a received envelope, or the republish would
// bounce the event right back onto the wire).
func (h *Hub) DeliverRemote(evt Event) {
	if h == nil {
		return
	}
	data, err := json.Marshal(evt)
	if err != nil {
		wsLog.Error("marshal event failed", "error", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		if evt.OrgID != "" && c.orgID != "" && evt.OrgID != c.orgID {
			continue
		}
		if evt.UserID != "" && c.userID != "" && evt.UserID != c.userID {
			continue
		}
		select {
		case c.send <- data:
		default:
			wsLog.Warn("dropping message for slow client")
		}
	}
}

// SetBackplane wires the optional cross-pod publish hook (TFAC-584).
// Multi-mode wiring only; called once at boot before any traffic flows.
// Not safe to call concurrently with Broadcast — set it during startup,
// not at runtime.
func (h *Hub) SetBackplane(b Backplane) {
	if h == nil {
		return
	}
	h.backplane = b
}

// indexLocked adds c to the per-user index. Caller holds h.mu. Unscoped
// clients (empty userID) are not indexed — they carry no identity for
// the revoke path to target.
func (h *Hub) indexLocked(c *client) {
	if c.userID == "" {
		return
	}
	set := h.byUser[c.userID]
	if set == nil {
		set = make(map[*client]struct{})
		h.byUser[c.userID] = set
	}
	set[c] = struct{}{}
}

// deindexLocked removes c from the per-user index, dropping the user's
// bucket once empty so the map doesn't accumulate dead keys. Caller
// holds h.mu. A no-op for unscoped clients (never indexed).
func (h *Hub) deindexLocked(c *client) {
	if c.userID == "" {
		return
	}
	set := h.byUser[c.userID]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.byUser, c.userID)
	}
}

// CloseUserConnections actively closes a user's live websocket
// connections with an application close code (TFAC-75), so a revoke or
// membership removal severs the stream immediately rather than waiting
// for the connection to drop on its own.
//
// The match is narrowed by optional filters (empty == wildcard):
//
//   - sid != "" restricts to the connections of a single session. The
//     single-device logout path passes the revoked session's id so a
//     user's OTHER still-valid sessions keep their sockets.
//   - orgID != "" restricts to connections scoped to one org. The
//     membership-removal path passes the lost org so a multi-org user's
//     connections to other orgs are untouched.
//
// Returns the number of connections matched (and thus being closed).
// The close itself is performed per-connection in its own goroutine:
// coder/websocket's Close runs the close handshake with up to a 10s
// budget, which must not block the revoking request or hold h.mu. The
// handshake unblocks the connection's readPump, whose normal cleanup
// path then deindexes it — we deliberately don't touch the maps here.
//
// Nil-receiver-safe, matching Broadcast: a conditionally-wired hub
// (tests, local-mode paths) drops the call instead of panicking.
func (h *Hub) CloseUserConnections(userID, sid, orgID string, code ws.StatusCode, reason string) int {
	if h == nil || userID == "" {
		return 0
	}
	h.mu.RLock()
	var targets []*client
	for c := range h.byUser[userID] {
		if sid != "" && c.sid != sid {
			continue
		}
		if orgID != "" && c.orgID != orgID {
			continue
		}
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		go func(c *client) {
			// Best-effort: an already-closing connection (peer dropped,
			// self-disconnect racing us) returns an error we can't act
			// on — the deindex happens on its own readPump exit either
			// way. coder/websocket's Close is idempotent, so a later
			// normal-closure from that cleanup is a no-op and our code
			// is the one the client sees.
			_ = c.conn.Close(code, reason)
		}(c)
	}
	return len(targets)
}

func (h *Hub) readPump(c *client) {
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		// Inbound client frames are presence reports (TFAC-392). Decode
		// tolerantly: a non-JSON frame (ping/keepalive) or an unknown message
		// type is silently ignored so the client→server channel stays
		// forward-compatible. Only a well-formed "presence" frame mutates state.
		var msg presenceMsg
		if json.Unmarshal(data, &msg) != nil || msg.Type != "presence" {
			continue
		}
		h.mu.Lock()
		c.viewing = msg.Viewing
		c.visible = msg.Visible
		h.mu.Unlock()
	}
}

// PresentFor reports whether any connected client is "present" for a prompt
// raised by conversationID in orgID (TFAC-392): an answer-capable, focused tab. A client
// qualifies iff it is in the run's org (or unscoped, matching Broadcast's
// tolerance for pre-auth / test clients) AND its tab is visible/focused AND it
// is viewing the board or this run's own detail page. A backgrounded tab, or a
// tab on Settings (viewing "other"), never qualifies.
//
// Nil-receiver-safe (returns false) so the hub-less test spawner and any
// conditionally-wired caller read as "nobody present" without guarding the call
// site, consistent with Broadcast.
func (h *Hub) PresentFor(orgID, conversationID string) bool {
	if h == nil {
		return false
	}
	runView := "run:" + conversationID
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if orgID != "" && c.orgID != "" && c.orgID != orgID {
			continue
		}
		if !c.visible {
			continue
		}
		if c.viewing == "board" || c.viewing == runView {
			return true
		}
	}
	return false
}

func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.closed:
			return
		case msg := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.conn.Write(ctx, ws.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// ClientCount reports the number of currently registered connections. A
// client only counts once HandleWS has added it to the hub's maps — i.e. once
// it is guaranteed to receive subsequent Broadcasts. Tests use this to close
// the window between a client's dial returning (handshake done) and the
// server goroutine completing registration, during which a Broadcast is
// silently delivered to nobody.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ConnSnapshot is one connection's presence-relevant state, returned by
// Snapshot for the multi-mode presence heartbeat (TFAC-584).
type ConnSnapshot struct {
	ConnID  string
	UserID  string
	OrgID   string
	Viewing string
	Visible bool
}

// Snapshot returns one entry per currently-registered connection that
// carries a userID — unscoped clients (pre-auth, test harness) are
// skipped, since there's no identity to key a ws_presence row on.
//
// This is a deliberate pull: rather than instrumenting readPump's hot
// path with a DB write on every presence frame, the multi-mode presence
// heartbeat polls Snapshot on its own ~15s timer and upserts what it
// sees. Nil-receiver-safe (returns nil), matching the rest of this type.
func (h *Hub) Snapshot() []ConnSnapshot {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ConnSnapshot, 0, len(h.clients))
	for c := range h.clients {
		if c.userID == "" {
			continue
		}
		out = append(out, ConnSnapshot{
			ConnID:  c.connID,
			UserID:  c.userID,
			OrgID:   c.orgID,
			Viewing: c.viewing,
			Visible: c.visible,
		})
	}
	return out
}

// RevalidateSessions closes every locally-connected socket whose session
// fails isValid — the cross-pod kick backstop (TFAC-584): a kick
// travels over tf_ctl NOTIFY, which is lossy by contract, so this bounds
// a missed kick at one revalidation interval (default 60s) and
// independently covers sessions revoked while this pod's LISTEN
// connection was reconnecting. This is the guarantee; the NOTIFY is
// only the latency optimization.
//
// isValid is called once per DISTINCT sid, not once per connection — a
// session backing several tabs/devices costs one lookup, and every
// connection under an invalid sid closes together. Connections not backed
// by a session (token-authed, or unscoped — pre-auth, local mode, test
// harness) are skipped. Returns the number of connections closed.
//
// Nil-receiver-safe (returns 0), matching the rest of this type.
func (h *Hub) RevalidateSessions(isValid func(sid string) bool) int {
	return h.revalidateBy(func(c *client) string { return c.sid }, isValid, "session revoked")
}

// RevalidateTokens is RevalidateSessions for Bearer-authenticated
// connections: same cadence, same closure semantics, same guarantee that a
// revoked credential's socket dies within one sweep. It is a second method
// rather than a widened first because the two resolve against different
// tables, and a caller holding only one of those stores must not be able to
// close the other's connections by passing a predicate that says no.
//
// Nil-receiver-safe (returns 0), matching the rest of this type.
func (h *Hub) RevalidateTokens(isValid func(tokenID string) bool) int {
	return h.revalidateBy(func(c *client) string { return c.tokenID }, isValid, "api token revoked")
}

// revalidateBy is the shared sweep: group live connections by the credential
// credOf reads off each, ask isValid once per distinct value, and close every
// connection under one that fails. A client whose credOf is empty carries no
// credential of that kind and is skipped.
func (h *Hub) revalidateBy(credOf func(*client) string, isValid func(string) bool, reason string) int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	byCred := make(map[string][]*client)
	for c := range h.clients {
		id := credOf(c)
		if id == "" {
			continue
		}
		byCred[id] = append(byCred[id], c)
	}
	h.mu.RUnlock()

	var closed int
	for id, clients := range byCred {
		if isValid(id) {
			continue
		}
		for _, c := range clients {
			go func(c *client) {
				// Best-effort, same rationale as CloseUserConnections: the
				// connection's own readPump-exit cleanup deindexes it either
				// way, so an error here (already closing) isn't actionable.
				_ = c.conn.Close(CloseSessionRevoked, reason)
			}(c)
		}
		closed += len(clients)
	}
	return closed
}
