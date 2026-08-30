package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ws "github.com/coder/websocket"
)

// dialHubClient stands up an httptest server that registers the dialing
// connection with the hub under the given (userID, orgID, sid), and
// returns the live client conn. Each call gets its own server, but they
// all register against the same Hub — so a test can dial several
// connections for one or more users and then exercise the active-close
// path (TFAC-75). Server + conn are torn down via t.Cleanup.
func dialHubClient(t *testing.T, h *Hub, userID, orgID, sid string) *ws.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleWS(w, r, ConnIdentity{UserID: userID, OrgID: orgID, SessionID: sid})
	}))
	t.Cleanup(srv.Close)
	url := strings.Replace(srv.URL, "http://", "ws://", 1)
	conn, _, err := ws.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ws.StatusNormalClosure, "") })
	return conn
}

// userConnCount reads the per-user index size under the hub lock. Test
// helper (same package) for asserting register/deregister bookkeeping.
func userConnCount(h *Hub, userID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byUser[userID])
}

// waitUserConnCount polls until byUser[userID] reaches want, absorbing
// the async gap between ws.Dial returning and HandleWS registering (or
// between an active close and the readPump cleanup deindexing).
func waitUserConnCount(t *testing.T, h *Hub, userID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if userConnCount(h, userID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("byUser[%s] count never became %d (got %d)", userID, want, userConnCount(h, userID))
}

// expectCloseCode asserts the conn's next read fails with a websocket
// close carrying want, within 1s (the ticket's latency bound).
func expectCloseCode(t *testing.T, conn *ws.Conn, want ws.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatalf("expected close code %d, got a message instead", want)
	} else if got := ws.CloseStatus(err); got != want {
		t.Fatalf("close code = %d, want %d (err=%v)", got, want, err)
	}
}

// expectAlive asserts the conn is still open and streaming: it
// broadcasts a sentinel to the conn's org and reads it back. We can't
// assert "stays open" with a bare timeout-read — coder/websocket closes
// the connection when a Read's context expires — so we read a frame that
// is guaranteed to arrive instead. A CloseError here means the conn was
// wrongly kicked.
func expectAlive(t *testing.T, h *Hub, conn *ws.Conn, orgID string) {
	t.Helper()
	h.Broadcast(Event{Type: "__alive", OrgID: orgID})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("connection should have stayed open and received a broadcast, got err=%v (close=%d)",
			err, ws.CloseStatus(err))
	}
}

// TestCloseUserConnections_RevokeClosesAllUserSockets is the core revoke
// path: closing a user with no sid/org filter (the logout-all shape)
// kicks every one of that user's connections with the session-revoked
// code, leaves a different user's connection untouched, and deregisters
// the closed ones.
func TestCloseUserConnections_RevokeClosesAllUserSockets(t *testing.T) {
	h := NewHub()
	const orgA = "org-a"
	a1 := dialHubClient(t, h, "userA", orgA, "sid-1")
	a2 := dialHubClient(t, h, "userA", orgA, "sid-2")
	b1 := dialHubClient(t, h, "userB", orgA, "sid-3")
	waitUserConnCount(t, h, "userA", 2)
	waitUserConnCount(t, h, "userB", 1)

	if n := h.CloseUserConnections("userA", "", "", CloseSessionRevoked, "session revoked"); n != 2 {
		t.Fatalf("CloseUserConnections matched %d, want 2", n)
	}

	expectCloseCode(t, a1, CloseSessionRevoked)
	expectCloseCode(t, a2, CloseSessionRevoked)
	expectAlive(t, h, b1, orgA)

	waitUserConnCount(t, h, "userA", 0)
	if got := userConnCount(h, "userB"); got != 1 {
		t.Fatalf("userB conn count = %d, want 1 (untouched)", got)
	}
}

// TestCloseUserConnections_SidFilter pins the single-device logout
// guarantee: revoking ONE session closes only that session's sockets,
// leaving the same user's other sessions (other devices) connected.
func TestCloseUserConnections_SidFilter(t *testing.T) {
	h := NewHub()
	const orgA = "org-a"
	s1 := dialHubClient(t, h, "userA", orgA, "sid-1")
	s2 := dialHubClient(t, h, "userA", orgA, "sid-2")
	waitUserConnCount(t, h, "userA", 2)

	if n := h.CloseUserConnections("userA", "sid-1", "", CloseSessionRevoked, "session revoked"); n != 1 {
		t.Fatalf("CloseUserConnections matched %d, want 1", n)
	}

	expectCloseCode(t, s1, CloseSessionRevoked)
	waitUserConnCount(t, h, "userA", 1)
	expectAlive(t, h, s2, orgA)
}

// TestCloseUserConnections_OrgFilter pins the membership-removal
// guarantee: closing a user narrowed to one org kicks only the
// connections scoped to that org (with the membership-changed code),
// leaving the same user's connections to other orgs alone.
func TestCloseUserConnections_OrgFilter(t *testing.T) {
	h := NewHub()
	onA := dialHubClient(t, h, "userA", "org-a", "sid-1")
	onB := dialHubClient(t, h, "userA", "org-b", "sid-2")
	waitUserConnCount(t, h, "userA", 2)

	if n := h.CloseUserConnections("userA", "", "org-b", CloseMembershipChanged, "membership changed"); n != 1 {
		t.Fatalf("CloseUserConnections matched %d, want 1", n)
	}

	expectCloseCode(t, onB, CloseMembershipChanged)
	waitUserConnCount(t, h, "userA", 1)
	expectAlive(t, h, onA, "org-a")
}

// TestCloseUserConnections_NoTargets covers the no-op paths: a nil hub,
// an unknown user, and an empty userID all return 0 without panicking —
// matching Broadcast's nil-safety so conditionally-wired callers don't
// have to guard.
func TestCloseUserConnections_NoTargets(t *testing.T) {
	var nilHub *Hub
	if n := nilHub.CloseUserConnections("userA", "", "", CloseSessionRevoked, "x"); n != 0 {
		t.Errorf("nil hub returned %d, want 0", n)
	}
	h := NewHub()
	if n := h.CloseUserConnections("nobody", "", "", CloseSessionRevoked, "x"); n != 0 {
		t.Errorf("unknown user returned %d, want 0", n)
	}
	if n := h.CloseUserConnections("", "", "", CloseSessionRevoked, "x"); n != 0 {
		t.Errorf("empty userID returned %d, want 0", n)
	}
}

// TestCloseUserConnections_Concurrency exercises the registry under
// concurrent registration and concurrent close — the acceptance
// "covered by a concurrency test" item. With -race it proves the
// byUser index and the close fan-out are correctly serialized under the
// hub mutex; functionally it proves every connection is kicked and
// deregistered.
func TestCloseUserConnections_Concurrency(t *testing.T) {
	h := NewHub()
	const (
		users   = 5
		perUser = 4
	)
	userID := func(i int) string { return "user-" + string(rune('A'+i)) }

	// Dial users*perUser connections concurrently.
	var mu sync.Mutex
	conns := make(map[string][]*ws.Conn)
	var dialWg sync.WaitGroup
	for u := 0; u < users; u++ {
		for c := 0; c < perUser; c++ {
			dialWg.Add(1)
			go func(uid, sid string) {
				defer dialWg.Done()
				conn := dialHubClient(t, h, uid, "org-a", sid)
				mu.Lock()
				conns[uid] = append(conns[uid], conn)
				mu.Unlock()
			}(userID(u), userID(u)+"-sid-"+string(rune('0'+c)))
		}
	}
	dialWg.Wait()
	for u := 0; u < users; u++ {
		waitUserConnCount(t, h, userID(u), perUser)
	}

	// Close every user concurrently.
	var closeWg sync.WaitGroup
	for u := 0; u < users; u++ {
		closeWg.Add(1)
		go func(uid string) {
			defer closeWg.Done()
			if n := h.CloseUserConnections(uid, "", "", CloseSessionRevoked, "session revoked"); n != perUser {
				t.Errorf("user %s matched %d, want %d", uid, n, perUser)
			}
		}(userID(u))
	}
	closeWg.Wait()

	// Every connection saw the close code, and the index drained empty.
	for u := 0; u < users; u++ {
		uid := userID(u)
		for _, conn := range conns[uid] {
			expectCloseCode(t, conn, CloseSessionRevoked)
		}
		waitUserConnCount(t, h, uid, 0)
	}
}
