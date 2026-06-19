package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	ws "github.com/coder/websocket"
)

// Event is a message sent to connected clients over the websocket.
//
// RunID and ProjectID are optional discriminators frontend listeners
// filter on. RunID identifies events from a single delegated run
// (agent_message / agent_run_update); ProjectID identifies events
// from a project's Curator session (curator_message /
// curator_request_update). Events that broadcast to the whole UI
// (tasks_updated, scoring_*) leave both empty.
//
// OrgID and UserID are server-side routing fields used by the hub's
// per-connection scoping. They are intentionally NOT serialised on the
// wire (json:"-"): the frontend filters by RunID/ProjectID, and
// coupling it to server-side identity would leak who-owns-what to
// other tabs/extensions parsing the WS stream. Empty OrgID means
// "system event, deliver to every connection"; empty UserID means
// "not user-specific". The hub's filter only kicks in when both
// event-side and client-side values are set — see Broadcast.
type Event struct {
	Type      string `json:"type"`
	RunID     string `json:"run_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	OrgID     string `json:"-"`
	UserID    string `json:"-"`
	Data      any    `json:"data"`
}

// Close codes the hub sends when it actively kicks a connection
// (TFAC-75). They live in the private application range (4000-4999) so
// the browser surfaces them verbatim on the CloseEvent, letting the
// client distinguish a deliberate kick from a network drop.
//
//   - CloseSessionRevoked: the session backing this connection was
//     revoked (logout / logout-all). The client should clear local auth
//     state and route to /login WITHOUT reconnecting.
//   - CloseMembershipChanged: the user lost membership in the org this
//     connection was scoped to. The client should refresh its session
//     view (active org may be gone) and re-handshake, but stay logged
//     in — it may still be a member of other orgs.
const (
	CloseSessionRevoked    ws.StatusCode = 4001
	CloseMembershipChanged ws.StatusCode = 4002
)

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
	// viewing + visible are the client's self-reported presence (TFAC-392),
	// updated from inbound "presence" control frames in readPump. viewing is
	// "board", "run:<runID>", or "other" (the latter for any non-answer-capable
	// surface); visible mirrors the Page Visibility / focus state of the tab.
	// Both are guarded by the hub mutex (written in readPump under h.mu, read in
	// PresentFor under the RLock). Zero value ("", false) is "not on an
	// answer-capable surface / not focused" — a fresh connection counts as absent
	// until it reports otherwise.
	viewing string
	visible bool
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

// HandleWS is the HTTP handler for websocket upgrade requests. The
// caller is responsible for extracting identity from r.Context() and
// passing it in via userID/orgID — this keeps pkg/websocket free of
// any import on internal/server (which would be the wrong direction
// architecturally). The wrapper handler that mounts this lives in
// internal/server and pulls ClaimsFrom + OrgIDFrom before invoking us.
//
// Empty values are tolerated: tests that hit the hub directly without
// the server pipeline get an "unscoped" client that receives every
// event (matching pre-D9b behavior).
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request, userID, orgID, sid string) {
	conn, err := ws.Accept(w, r, &ws.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		wsLog.Warn("accept failed", "error", err)
		return
	}

	c := &client{
		conn:   conn,
		send:   make(chan []byte, 64),
		closed: make(chan struct{}),
		userID: userID,
		orgID:  orgID,
		sid:    sid,
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.indexLocked(c)
	h.mu.Unlock()

	wsLog.Info("client connected", "total", h.clientCount())

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

	wsLog.Info("client disconnected", "total", h.clientCount())
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
func (h *Hub) Broadcast(evt Event) {
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
// raised by runID in orgID (TFAC-392): an answer-capable, focused tab. A client
// qualifies iff it is in the run's org (or unscoped, matching Broadcast's
// tolerance for pre-auth / test clients) AND its tab is visible/focused AND it
// is viewing the board or this run's own detail page. A backgrounded tab, or a
// tab on Settings (viewing "other"), never qualifies.
//
// Nil-receiver-safe (returns false) so the hub-less test spawner and any
// conditionally-wired caller read as "nobody present" without guarding the call
// site, consistent with Broadcast.
func (h *Hub) PresentFor(orgID, runID string) bool {
	if h == nil {
		return false
	}
	runView := "run:" + runID
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

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
