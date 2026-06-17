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

// Hub manages websocket connections and broadcasts events to all clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
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
}

// NewHub creates a new websocket hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
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
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request, userID, orgID string) {
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
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	wsLog.Info("client connected", "total", h.clientCount())

	// Start write pump in background
	go h.writePump(c)

	// Read pump (blocks until disconnect)
	h.readPump(c)

	// Cleanup: remove from map first (under write lock so Broadcast can't
	// see this client), then signal writePump to exit via closed channel.
	// We never close c.send — writePump drains it naturally.
	h.mu.Lock()
	delete(h.clients, c)
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

func (h *Hub) readPump(c *client) {
	for {
		_, _, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
	}
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
