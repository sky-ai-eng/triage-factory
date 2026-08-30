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

// fakeBackplane records every event Publish receives, standing in for
// internal/wsbackplane.Backplane in unit tests that don't want a real
// Postgres connection.
type fakeBackplane struct {
	mu        sync.Mutex
	published []Event
}

func (f *fakeBackplane) Publish(evt Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, evt)
}

func (f *fakeBackplane) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

// TestBroadcast_PublishesToBackplane pins the cross-pod fan-out hook: a
// wired Backplane sees every Broadcast call, in addition to (not instead
// of) the existing local delivery.
func TestBroadcast_PublishesToBackplane(t *testing.T) {
	h := NewHub()
	bp := &fakeBackplane{}
	h.SetBackplane(bp)
	c := addBareClient(h, "user-1", "org-1")

	h.Broadcast(Event{Type: "task_updated", OrgID: "org-1"})

	if got := bp.count(); got != 1 {
		t.Fatalf("backplane saw %d published events, want 1", got)
	}
	got := drain(t, c, 200*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("local client received %d events, want 1 (local delivery must still happen)", len(got))
	}
}

// TestBroadcast_NilBackplaneIsNoop covers local mode / before wiring: a
// Hub with no backplane set behaves exactly as it did before this field
// existed — no panic, and Broadcast is unaffected.
func TestBroadcast_NilBackplaneIsNoop(t *testing.T) {
	h := NewHub()
	c := addBareClient(h, "user-1", "org-1")
	h.Broadcast(Event{Type: "task_updated", OrgID: "org-1"})
	got := drain(t, c, 200*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("local client received %d events, want 1", len(got))
	}
}

// TestDeliverRemote_NeverPublishesToBackplane is the other half of the
// no-echo-loop invariant: applying a REMOTE pod's event via DeliverRemote
// must never re-publish it, or a multi-pod backplane would bounce every
// event around the fleet forever.
func TestDeliverRemote_NeverPublishesToBackplane(t *testing.T) {
	h := NewHub()
	bp := &fakeBackplane{}
	h.SetBackplane(bp)
	c := addBareClient(h, "user-1", "org-1")

	h.DeliverRemote(Event{Type: "task_updated", OrgID: "org-1"})

	if got := bp.count(); got != 0 {
		t.Fatalf("DeliverRemote published %d events to the backplane, want 0 (echo loop)", got)
	}
	got := drain(t, c, 200*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("local client received %d events, want 1 (DeliverRemote must still deliver locally)", len(got))
	}
}

// TestDeliverRemote_AppliesSameScopeFilterAsBroadcast asserts DeliverRemote
// isn't a bare fan-out-to-everyone — it must apply the identical
// per-connection (orgID, userID) scope Broadcast does, since a backplane
// dispatch relies on it to keep cross-tenant isolation on the receiving
// pod.
func TestDeliverRemote_AppliesSameScopeFilterAsBroadcast(t *testing.T) {
	h := NewHub()
	inOrg := addBareClient(h, "user-1", "org-A")
	otherOrg := addBareClient(h, "user-2", "org-B")

	h.DeliverRemote(Event{Type: "task_updated", OrgID: "org-A"})

	if got := drain(t, inOrg, 200*time.Millisecond); len(got) != 1 {
		t.Fatalf("org-A client received %d events, want 1", len(got))
	}
	if got := drain(t, otherOrg, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("org-B client received %d events, want 0 (cross-tenant leak)", len(got))
	}
}

// TestSnapshot_SkipsUnscopedClientsAndReturnsIdentifiedOnes pins the
// presence-heartbeat data source: only identified (non-empty userID)
// clients are worth a ws_presence row.
func TestSnapshot_SkipsUnscopedClientsAndReturnsIdentifiedOnes(t *testing.T) {
	h := NewHub()
	addBareClient(h, "", "org-1") // unscoped: pre-auth / test harness
	addBareClient(h, "user-1", "org-1")

	snap := h.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot returned %d entries, want 1 (unscoped client must be skipped)", len(snap))
	}
	if snap[0].UserID != "user-1" || snap[0].OrgID != "org-1" {
		t.Fatalf("Snapshot entry = %+v, want user-1/org-1", snap[0])
	}
}

// TestSnapshot_NilHub matches the rest of the type's nil-receiver
// convention.
func TestSnapshot_NilHub(t *testing.T) {
	var h *Hub
	if got := h.Snapshot(); got != nil {
		t.Fatalf("nil hub Snapshot() = %v, want nil", got)
	}
}

// TestHandleWS_MintsDistinctConnIDs asserts real connections (as opposed
// to addBareClient's synthetic ones) get distinct, non-empty connID
// values — the presence heartbeat keys ws_presence rows on this, so a
// collision between two live sockets would silently drop one's presence.
func TestHandleWS_MintsDistinctConnIDs(t *testing.T) {
	h := NewHub()
	dialHubClient(t, h, "user-1", "org-1", "sid-1")
	dialHubClient(t, h, "user-1", "org-1", "sid-2")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.ClientCount() == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	snap := h.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot returned %d entries, want 2", len(snap))
	}
	if snap[0].ConnID == "" || snap[1].ConnID == "" {
		t.Fatalf("Snapshot entries have empty ConnID: %+v", snap)
	}
	if snap[0].ConnID == snap[1].ConnID {
		t.Fatalf("two distinct connections minted the same ConnID: %q", snap[0].ConnID)
	}
}

// TestRevalidateSessions_ClosesInvalidAndSparesValid is the kick
// backstop's core contract: a connection whose session fails isValid
// gets closed with CloseSessionRevoked; a connection whose session is
// still valid is left alone.
func TestRevalidateSessions_ClosesInvalidAndSparesValid(t *testing.T) {
	h := NewHub()
	staleConn := dialHubClient(t, h, "user-1", "org-1", "sid-stale")
	freshConn := dialHubClient(t, h, "user-2", "org-1", "sid-fresh")
	waitUserConnCount(t, h, "user-1", 1)
	waitUserConnCount(t, h, "user-2", 1)

	n := h.RevalidateSessions(func(sid string) bool {
		return sid != "sid-stale"
	})
	if n != 1 {
		t.Fatalf("RevalidateSessions closed %d connections, want 1", n)
	}
	expectCloseCode(t, staleConn, CloseSessionRevoked)
	expectAlive(t, h, freshConn, "org-1")
}

// TestRevalidateSessions_CallsIsValidOncePerDistinctSid asserts the
// per-sid dedup: a session backing several connections (multiple tabs)
// costs one isValid call, not one per connection.
func TestRevalidateSessions_CallsIsValidOncePerDistinctSid(t *testing.T) {
	h := NewHub()
	dialHubClient(t, h, "user-1", "org-1", "sid-shared")
	dialHubClient(t, h, "user-1", "org-1", "sid-shared")
	waitUserConnCount(t, h, "user-1", 2)

	var mu sync.Mutex
	calls := map[string]int{}
	h.RevalidateSessions(func(sid string) bool {
		mu.Lock()
		calls[sid]++
		mu.Unlock()
		return true
	})
	if calls["sid-shared"] != 1 {
		t.Fatalf("isValid called %d times for sid-shared, want 1", calls["sid-shared"])
	}
}

// TestRevalidateSessions_SkipsUnscopedClients mirrors Snapshot's rule:
// an empty sid (pre-auth/local mode) has no session to validate.
func TestRevalidateSessions_SkipsUnscopedClients(t *testing.T) {
	h := NewHub()
	addBareClient(h, "", "")
	n := h.RevalidateSessions(func(string) bool {
		t.Fatal("isValid must not be called for an unscoped client")
		return true
	})
	if n != 0 {
		t.Fatalf("RevalidateSessions closed %d connections, want 0", n)
	}
}

// TestRevalidateSessions_NilHub matches the rest of the type's
// nil-receiver convention.
func TestRevalidateSessions_NilHub(t *testing.T) {
	var h *Hub
	if got := h.RevalidateSessions(func(string) bool { return false }); got != 0 {
		t.Fatalf("nil hub RevalidateSessions() = %d, want 0", got)
	}
}

// dialHubTokenClient is dialHubClient's Bearer twin: a connection whose
// credential is an API token rather than a session.
func dialHubTokenClient(t *testing.T, h *Hub, userID, orgID, tokenID string) *ws.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleWS(w, r, ConnIdentity{UserID: userID, OrgID: orgID, TokenID: tokenID})
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

// TestRevalidateTokens_ClosesInvalidAndSparesValid is RevalidateSessions'
// contract for the Bearer half: a connection whose token fails isValid closes,
// one whose token still lives is left streaming.
func TestRevalidateTokens_ClosesInvalidAndSparesValid(t *testing.T) {
	h := NewHub()
	revoked := dialHubTokenClient(t, h, "user-1", "org-1", "tok-revoked")
	live := dialHubTokenClient(t, h, "user-2", "org-1", "tok-live")
	waitUserConnCount(t, h, "user-1", 1)
	waitUserConnCount(t, h, "user-2", 1)

	n := h.RevalidateTokens(func(tokenID string) bool {
		return tokenID != "tok-revoked"
	})
	if n != 1 {
		t.Fatalf("RevalidateTokens closed %d connections, want 1", n)
	}
	expectCloseCode(t, revoked, CloseSessionRevoked)
	expectAlive(t, h, live, "org-1")
}

// TestRevalidateSweepsDoNotCrossCredentialKinds is what keeps the two sweeps
// independent: a session sweep must not see token-backed connections, and vice
// versa, or a store that can only answer for one kind would close the other's
// sockets by saying no.
func TestRevalidateSweepsDoNotCrossCredentialKinds(t *testing.T) {
	h := NewHub()
	dialHubClient(t, h, "user-1", "org-1", "sid-1")
	dialHubTokenClient(t, h, "user-2", "org-1", "tok-1")
	waitUserConnCount(t, h, "user-1", 1)
	waitUserConnCount(t, h, "user-2", 1)

	h.RevalidateSessions(func(sid string) bool {
		if sid != "sid-1" {
			t.Errorf("session sweep saw %q; it must only see session-backed conns", sid)
		}
		return true
	})
	h.RevalidateTokens(func(tokenID string) bool {
		if tokenID != "tok-1" {
			t.Errorf("token sweep saw %q; it must only see token-backed conns", tokenID)
		}
		return true
	})
}

// TestRevalidateTokens_NilHub matches the rest of the type's nil-receiver
// convention.
func TestRevalidateTokens_NilHub(t *testing.T) {
	var h *Hub
	if got := h.RevalidateTokens(func(string) bool { return false }); got != 0 {
		t.Fatalf("nil hub RevalidateTokens() = %d, want 0", got)
	}
}
