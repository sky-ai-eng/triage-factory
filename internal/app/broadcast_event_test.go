package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// hubTestClient wraps a dialed connection with a background read loop that
// feeds every inbound frame onto a channel. coder/websocket closes the
// connection the moment a Read's context is cancelled/times out (there's no
// other way for it to interrupt a blocked read), so per-assertion
// Read(ctx-with-timeout) calls would kill the socket after the first
// "expect nothing" check. Reading continuously against one long-lived
// context and asserting via channel + time.After sidesteps that.
type hubTestClient struct {
	conn   *ws.Conn
	msgs   chan []byte
	cancel context.CancelFunc
}

func dialHubTestClient(t *testing.T, hub *websocket.Hub) *hubTestClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWS(w, r, websocket.ConnIdentity{})
	}))
	t.Cleanup(srv.Close)
	url := strings.Replace(srv.URL, "http://", "ws://", 1)
	conn, _, err := ws.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &hubTestClient{conn: conn, msgs: make(chan []byte, 16), cancel: cancel}
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			c.msgs <- data
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close(ws.StatusNormalClosure, "")
	})
	return c
}

func (c *hubTestClient) expectNoMessage(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case data := <-c.msgs:
		t.Fatalf("expected no message, got: %s", data)
	case <-time.After(wait):
	}
}

func (c *hubTestClient) expectMessage(t *testing.T, wait time.Duration) []byte {
	t.Helper()
	select {
	case data := <-c.msgs:
		return data
	case <-time.After(wait):
		t.Fatal("expected a message, got none")
		return nil
	}
}

// waitForHubClient polls until the hub has registered the dialing
// connection, absorbing the async gap between ws.Dial returning and
// HandleWS completing registration (same race pkg/websocket's own tests
// guard against).
func waitForHubClient(t *testing.T, hub *websocket.Hub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("hub never registered the dialing client")
}

// TestBroadcastEvent_SkipsConversationLifecycleSentinels pins the TFAC-592 guard: the
// ws-broadcast subscriber must not re-forward system:conversation:* sentinels — the
// spawner's two choke points already emit the dedicated conversation_update /
// message websocket events, so forwarding the bus sentinel too would
// double every tool call on the wire in a second shape. A system:poll:completed
// event (existing behavior) must still go out.
func TestBroadcastEvent_SkipsConversationLifecycleSentinels(t *testing.T) {
	hub := websocket.NewHub()
	a := &App{wsHub: hub}
	client := dialHubTestClient(t, hub)
	waitForHubClient(t, hub)

	a.broadcastEvent(domain.Event{EventType: domain.EventSystemConversationStatus, OrgID: "org-a"})
	client.expectNoMessage(t, 200*time.Millisecond)

	a.broadcastEvent(domain.Event{EventType: domain.EventSystemConversationActivity, OrgID: "org-a"})
	client.expectNoMessage(t, 200*time.Millisecond)

	a.broadcastEvent(domain.Event{EventType: domain.EventSystemPollCompleted, OrgID: "org-a"})
	client.expectMessage(t, time.Second)
}
