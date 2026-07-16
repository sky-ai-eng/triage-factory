package repoprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// hubTestClient wraps a dialed connection with a background read loop that
// feeds every inbound frame onto a channel. Mirrors internal/app's helper of
// the same name/shape (not exported from there, so copied rather than
// imported) — coder/websocket closes the connection the moment a Read's
// context is cancelled/times out, so per-assertion Read(ctx-with-timeout)
// calls would kill the socket after the first "expect nothing" check.
// Reading continuously against one long-lived context and asserting via
// channel + time.After sidesteps that.
type hubTestClient struct {
	conn   *ws.Conn
	msgs   chan []byte
	cancel context.CancelFunc
}

func dialHubTestClient(t *testing.T, hub *websocket.Hub) *hubTestClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWS(w, r, "", "", "")
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
// HandleWS completing registration.
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
