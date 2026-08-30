package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"
)

// setPresence stamps a bare client's self-reported presence the way an inbound
// "presence" frame would, under the hub mutex so PresentFor's RLock sees a
// consistent value.
func setPresence(h *Hub, c *client, viewing string, visible bool) {
	h.mu.Lock()
	c.viewing = viewing
	c.visible = visible
	h.mu.Unlock()
}

// TestPresentFor_TruthTable walks the (viewing × visible) grid for a client in
// the run's org: present iff visible AND viewing is the board or this run's
// detail page. A backgrounded tab or a tab on a non-answer-capable surface
// (viewing "other") never counts.
func TestPresentFor_TruthTable(t *testing.T) {
	const (
		org            = "orgA"
		conversationID = "run-1"
	)
	runView := "run:" + conversationID
	cases := []struct {
		viewing string
		visible bool
		want    bool
	}{
		{"board", true, true},
		{runView, true, true},
		{"run:other-run", true, false}, // a different run's detail page
		{"other", true, false},         // e.g. Settings
		{"", true, false},              // not yet reported a surface
		{"board", false, false},        // backgrounded board tab
		{runView, false, false},        // backgrounded run tab
		{"other", false, false},
	}
	for _, tc := range cases {
		h := NewHub()
		c := addBareClient(h, "userA", org)
		setPresence(h, c, tc.viewing, tc.visible)
		if got := h.PresentFor(org, conversationID); got != tc.want {
			t.Errorf("PresentFor(viewing=%q, visible=%v) = %v, want %v", tc.viewing, tc.visible, got, tc.want)
		}
	}
}

// TestPresentFor_OrgScoping pins the tenancy guarantee: a focused board tab in
// org B does not satisfy a prompt raised in org A. An unscoped client (orgID
// "") still counts for any org, matching Broadcast's tolerance for pre-auth /
// test connections.
func TestPresentFor_OrgScoping(t *testing.T) {
	const (
		orgA           = "orgA"
		orgB           = "orgB"
		conversationID = "run-1"
	)

	// A present client in org B must not satisfy org A.
	h := NewHub()
	b := addBareClient(h, "userB", orgB)
	setPresence(h, b, "board", true)
	if h.PresentFor(orgA, conversationID) {
		t.Error("a present board tab in org B satisfied a prompt in org A")
	}
	if !h.PresentFor(orgB, conversationID) {
		t.Error("org B's own present board tab should satisfy org B")
	}

	// An unscoped client (orgID "") counts for any org.
	h2 := NewHub()
	u := addBareClient(h2, "", "")
	setPresence(h2, u, "board", true)
	if !h2.PresentFor(orgA, conversationID) {
		t.Error("an unscoped present client should satisfy any org")
	}
}

// TestPresentFor_NilHub matches the nil-receiver-safe contract: the hub-less
// test spawner reads as "nobody present" without a guard at the call site.
func TestPresentFor_NilHub(t *testing.T) {
	var h *Hub
	if h.PresentFor("orgA", "run-1") {
		t.Error("nil hub PresentFor must return false")
	}
}

// dialPresenceClient stands up an httptest server registering the dialing
// connection with the hub under the given org, and returns the live conn so the
// test can write presence frames over the real wire (exercising readPump's
// inbound JSON decode).
func dialPresenceClient(t *testing.T, h *Hub, orgID string) (*ws.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleWS(w, r, ConnIdentity{UserID: "user", OrgID: orgID})
	}))
	url := strings.Replace(srv.URL, "http://", "ws://", 1)
	conn, _, err := ws.Dial(context.Background(), url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial hub: %v", err)
	}
	return conn, func() {
		_ = conn.Close(ws.StatusNormalClosure, "")
		srv.Close()
	}
}

// waitPresent polls PresentFor until it reaches want or a short deadline,
// absorbing the async gap between a frame hitting the wire and readPump applying
// it under the hub mutex.
func waitPresent(t *testing.T, h *Hub, orgID, conversationID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.PresentFor(orgID, conversationID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("PresentFor(%s, %s) never became %v", orgID, conversationID, want)
}

// TestReadPump_PresenceFrame drives the full inbound path: a presence frame over
// the wire updates the client's state, a visibility flip clears it, and
// malformed / unknown frames are ignored rather than corrupting state.
func TestReadPump_PresenceFrame(t *testing.T) {
	const (
		org            = "orgA"
		conversationID = "run-1"
	)
	h := NewHub()
	conn, cleanup := dialPresenceClient(t, h, org)
	defer cleanup()

	write := func(s string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := conn.Write(ctx, ws.MessageText, []byte(s)); err != nil {
			t.Fatalf("write frame %q: %v", s, err)
		}
	}

	// A focused board frame makes the client present.
	write(`{"type":"presence","viewing":"board","visible":true}`)
	waitPresent(t, h, org, conversationID, true)

	// Backgrounding the tab (visible:false) drops presence.
	write(`{"type":"presence","viewing":"board","visible":false}`)
	waitPresent(t, h, org, conversationID, false)

	// Re-focus on this run's detail page makes it present again.
	write(`{"type":"presence","viewing":"run:` + conversationID + `","visible":true}`)
	waitPresent(t, h, org, conversationID, true)

	// A non-JSON frame (ping/keepalive) and an unknown message type are both
	// ignored — state stays as last reported (present).
	write("not json at all")
	write(`{"type":"something-else","viewing":"other","visible":false}`)
	// Give readPump a beat to process (and prove it did NOT mutate state).
	time.Sleep(20 * time.Millisecond)
	if !h.PresentFor(org, conversationID) {
		t.Error("a non-presence / non-JSON frame must not clear presence")
	}
}
