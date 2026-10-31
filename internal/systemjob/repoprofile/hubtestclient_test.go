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
	return dialHubTestClientAs(t, hub, "", "")
}

// dialHubTestClientAs registers the connection under a (userID, orgID)
// identity, engaging the hub's per-connection Broadcast filter — the
// scoped sibling of dialHubTestClient's unscoped (receive-everything)
// client.
func dialHubTestClientAs(t *testing.T, hub *websocket.Hub, userID, orgID string) *hubTestClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWS(w, r, websocket.ConnIdentity{UserID: userID, OrgID: orgID})
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
	waitForHubClients(t, hub, 1)
}

// waitForHubClients is the n-client form, for tests that dial several
// scoped connections and must not Broadcast before the last registers.
func waitForHubClients(t *testing.T, hub *websocket.Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hub never registered %d dialing client(s)", n)
}
