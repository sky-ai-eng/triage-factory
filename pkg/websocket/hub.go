package websocket

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	ws "github.com/coder/websocket"
)

// Event is a message sent to connected clients over the websocket.
type Event struct {
	Type  string `json:"type"` // "agent_message" | "agent_run_update" | "tasks_updated" | "scoring_started" | "scoring_completed"
	RunID string `json:"run_id,omitempty"`
	Data  any    `json:"data"`
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
}

// NewHub creates a new websocket hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
	}
}

// HandleWS is the HTTP handler for websocket upgrade requests.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Accept(w, r, &ws.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("[ws] accept error: %v", err)
		return
	}

	c := &client{
		conn:   conn,
		send:   make(chan []byte, 64),
		closed: make(chan struct{}),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	log.Printf("[ws] client connected (%d total)", h.clientCount())

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

	log.Printf("[ws] client disconnected (%d total)", h.clientCount())
}

// Broadcast sends an event to all connected clients.
func (h *Hub) Broadcast(evt Event) {
	data, err := json.Marshal(evt)
	if err != nil {
		log.Printf("[ws] marshal error: %v", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			log.Println("[ws] dropping message for slow client")
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
