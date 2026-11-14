package wsbackplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/conversationframe"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// These tests exercise the dispatch/self-origin logic directly (no
// Postgres involved — b.db is never touched by handleWSNotification's
// inline path, HandleCtlKick, or handleBusNotification), which
// is where the "no echo loops, by construction" invariant actually
// lives. The Postgres transport itself (LISTEN/NOTIFY round-tripping a
// payload) is covered separately by the pgtest-backed integration tests.

// dialTestClient stands up a real websocket connection against hub —
// zero DB involved, pkg/websocket is pure in-memory.
func dialTestClient(t *testing.T, hub *websocket.Hub, userID, orgID, sid string) *ws.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.HandleWS(w, r, websocket.ConnIdentity{UserID: userID, OrgID: orgID, SessionID: sid})
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

func waitClientCount(t *testing.T, hub *websocket.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hub.ClientCount() never reached %d", want)
}

// readOrTimeout reads one frame within timeout, or reports whether none
// arrived (used to assert absence, not just presence).
func readOrTimeout(t *testing.T, conn *ws.Conn, timeout time.Duration) (msg []byte, ok bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, false
	}
	return data, true
}

// TestHandleWSNotification_SkipsSelfOrigin pins the origin pod's half of
// the no-echo-loop invariant: an envelope this same instance published
// must never be re-delivered when it comes back around on the LISTEN
// loop (the origin pod already delivered it locally, synchronously,
// inside Broadcast).
func TestHandleWSNotification_SkipsSelfOrigin(t *testing.T) {
	hub := websocket.NewHub()
	conn := dialTestClient(t, hub, "user-1", "org-1", "sid-1")
	waitClientCount(t, hub, 1)

	b := &Backplane{originID: "pod-a", hub: hub}
	we, err := newWireEvent(websocket.Event{Type: "task_updated", OrgID: "org-1"})
	if err != nil {
		t.Fatalf("newWireEvent: %v", err)
	}
	env := wsEnvelope{OriginInstanceID: "pod-a", Event: &we}
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	b.handleWSNotification(string(payload))

	if _, ok := readOrTimeout(t, conn, 200*time.Millisecond); ok {
		t.Fatal("self-origin envelope was delivered locally — this is the echo loop TFAC-584 forbids")
	}
}

// TestHandleWSNotification_DeliversNonSelfOriginInline is the receiving
// pod's half: a REMOTE pod's inline envelope must be delivered to local
// sockets, scoped exactly like Broadcast would have.
func TestHandleWSNotification_DeliversNonSelfOriginInline(t *testing.T) {
	hub := websocket.NewHub()
	conn := dialTestClient(t, hub, "user-1", "org-1", "sid-1")
	waitClientCount(t, hub, 1)

	b := &Backplane{originID: "pod-b", hub: hub}
	we, err := newWireEvent(websocket.Event{Type: "task_updated", OrgID: "org-1", Data: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatalf("newWireEvent: %v", err)
	}
	env := wsEnvelope{OriginInstanceID: "pod-a", Event: &we} // different origin: "remote"
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	b.handleWSNotification(string(payload))

	data, ok := readOrTimeout(t, conn, 500*time.Millisecond)
	if !ok {
		t.Fatal("non-self-origin envelope was not delivered locally")
	}
	var got websocket.Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode delivered frame: %v", err)
	}
	if got.Type != "task_updated" {
		t.Fatalf("delivered event Type = %q, want task_updated", got.Type)
	}
}

// TestHandleWSNotification_ScopesToMatchingOrgOnly asserts the delivered
// event still respects per-connection org scoping — a remote envelope
// isn't a bare broadcast-to-everyone.
func TestHandleWSNotification_ScopesToMatchingOrgOnly(t *testing.T) {
	hub := websocket.NewHub()
	inOrg := dialTestClient(t, hub, "user-1", "org-A", "sid-1")
	otherOrg := dialTestClient(t, hub, "user-2", "org-B", "sid-2")
	waitClientCount(t, hub, 2)

	b := &Backplane{originID: "pod-b", hub: hub}
	we, _ := newWireEvent(websocket.Event{Type: "task_updated", OrgID: "org-A"})
	env := wsEnvelope{OriginInstanceID: "pod-a", Event: &we}
	payload, _ := json.Marshal(env)

	b.handleWSNotification(string(payload))

	if _, ok := readOrTimeout(t, inOrg, 500*time.Millisecond); !ok {
		t.Fatal("org-A client should have received the event")
	}
	if _, ok := readOrTimeout(t, otherOrg, 200*time.Millisecond); ok {
		t.Fatal("org-B client received an org-A-scoped event — cross-tenant leak")
	}
}

// TestHandleWSNotification_AppliesRecipientSet asserts the receiving pod
// delivers a remote event addressed to a recipient set only to the
// connections in it: the set crossed the hop, and DeliverRemote applies it
// there as Broadcast does on the publishing pod.
func TestHandleWSNotification_AppliesRecipientSet(t *testing.T) {
	hub := websocket.NewHub()
	inSet := dialTestClient(t, hub, "user-1", "org-A", "sid-1")
	sameOrg := dialTestClient(t, hub, "user-2", "org-A", "sid-2")
	waitClientCount(t, hub, 2)

	b := &Backplane{originID: "pod-b", hub: hub}
	we, _ := newWireEvent(websocket.Event{Type: "message", OrgID: "org-A", UserIDs: []string{"user-1"}})
	env := wsEnvelope{OriginInstanceID: "pod-a", Event: &we}
	payload, _ := json.Marshal(env)

	b.handleWSNotification(string(payload))

	if _, ok := readOrTimeout(t, inSet, 500*time.Millisecond); !ok {
		t.Fatal("the connection in the recipient set should have received the event")
	}
	if _, ok := readOrTimeout(t, sameOrg, 200*time.Millisecond); ok {
		t.Fatal("a same-org connection outside the recipient set received the event")
	}
}

// TestHandleWSNotification_MalformedPayloadDropsSilently asserts a
// corrupt NOTIFY payload never panics — it's dropped with a warning.
func TestHandleWSNotification_MalformedPayloadDropsSilently(t *testing.T) {
	hub := websocket.NewHub()
	b := &Backplane{originID: "pod-a", hub: hub}
	b.handleWSNotification("{not valid json")
}

// TestHandleCtlKick_SkipsSelfOrigin mirrors the WS case for the
// kick envelope: this pod's own published kick, received back via the
// app's tf_ctl dispatcher, must not re-trigger a local close — the origin
// call site already closed matching local sockets directly before
// publishing.
func TestHandleCtlKick_SkipsSelfOrigin(t *testing.T) {
	hub := websocket.NewHub()
	conn := dialTestClient(t, hub, "user-1", "org-1", "sid-1")
	waitClientCount(t, hub, 1)

	b := &Backplane{originID: "pod-a", hub: hub}
	env := kickEnvelope{Kind: "kick", OriginInstanceID: "pod-a", UserID: "user-1", Code: int(websocket.CloseSessionRevoked), Reason: "session revoked"}
	payload, _ := json.Marshal(env)

	b.HandleCtlKick(string(payload))

	// The connection must stay open — prove it by sending it a broadcast
	// and reading it back, rather than a bare timeout (which coder/websocket
	// treats as a close on its own).
	hub.Broadcast(websocket.Event{Type: "__alive", OrgID: "org-1"})
	if _, ok := readOrTimeout(t, conn, 500*time.Millisecond); !ok {
		t.Fatal("connection should have stayed open (self-origin kick must not re-close locally)")
	}
}

// TestHandleCtlKick_ClosesNonSelfOriginMatch is the receiving
// pod's half: a remote pod's kick must close this pod's matching local
// connection.
func TestHandleCtlKick_ClosesNonSelfOriginMatch(t *testing.T) {
	hub := websocket.NewHub()
	conn := dialTestClient(t, hub, "user-1", "org-1", "sid-1")
	waitClientCount(t, hub, 1)

	b := &Backplane{originID: "pod-b", hub: hub}
	env := kickEnvelope{Kind: "kick", OriginInstanceID: "pod-a", UserID: "user-1", Code: int(websocket.CloseSessionRevoked), Reason: "session revoked"}
	payload, _ := json.Marshal(env)

	b.HandleCtlKick(string(payload))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("expected the connection to close")
	} else if got := ws.CloseStatus(err); got != websocket.CloseSessionRevoked {
		t.Fatalf("close code = %d, want %d", got, websocket.CloseSessionRevoked)
	}
}

// TestHandleBusNotification_SkipsSelfOrigin mirrors the same rule for
// tf_bus: at TF_ROLE=all the sentinel is already on the only bus there
// is, so a self-origin envelope must never re-publish locally.
func TestHandleBusNotification_SkipsSelfOrigin(t *testing.T) {
	b := &Backplane{originID: "pod-a"}
	env := busEnvelope{OriginInstanceID: "pod-a", Event: domain.Event{EventType: "system:conversation:status", OrgID: "org-1"}}
	payload, _ := json.Marshal(env)

	called := false
	b.handleBusNotification(string(payload), func(domain.Event) { called = true })

	if called {
		t.Fatal("self-origin sentinel was republished onto the local bus — this is the echo loop TFAC-584 forbids")
	}
}

// TestHandleBusNotification_RepublishesNonSelfOrigin is the brain's
// half: an executor's sentinel must land on the brain's local bus.
func TestHandleBusNotification_RepublishesNonSelfOrigin(t *testing.T) {
	b := &Backplane{originID: "brain-pod"}
	want := domain.Event{EventType: "system:conversation:activity", OrgID: "org-1", MetadataJSON: `{"tool":"bash"}`}
	env := busEnvelope{OriginInstanceID: "executor-pod", Event: want}
	payload, _ := json.Marshal(env)

	var got domain.Event
	called := false
	b.handleBusNotification(string(payload), func(evt domain.Event) { called = true; got = evt })

	if !called {
		t.Fatal("non-self-origin sentinel was not republished onto the local bus")
	}
	if got.EventType != want.EventType || got.OrgID != want.OrgID || got.MetadataJSON != want.MetadataJSON {
		t.Fatalf("republished event = %+v, want %+v", got, want)
	}
}

// readersEnvelope is a remote pod's tf_ws payload for an event marked for
// conversationID's readers in orgID, the shape a deferred notifier on an
// executor publishes.
func readersEnvelope(t *testing.T, orgID, conversationID string) string {
	t.Helper()
	we, err := newWireEvent(websocket.Event{Type: "message", OrgID: orgID, ConversationID: conversationID, ReadersOfConversation: true})
	if err != nil {
		t.Fatalf("newWireEvent: %v", err)
	}
	payload, err := json.Marshal(wsEnvelope{OriginInstanceID: "executor-pod", Event: &we})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(payload)
}

// TestHandleWSNotification_ReadersOfConversationDecideDelivery pins how the
// delivering pod applies the answer it reads for a marked event: the readers
// alone for a narrower audience, every member for an org-visible
// conversation, and nobody when it cannot resolve the readers, the read
// fails, or it names nobody.
func TestHandleWSNotification_ReadersOfConversationDecideDelivery(t *testing.T) {
	cases := []struct {
		name          string
		readers       conversationframe.AudienceFunc
		wantReader    bool
		wantOutsider  bool
		wantReadCount int
	}{
		{"readers", func(context.Context, string, string) (bool, []string, error) {
			return false, []string{"reader"}, nil
		}, true, false, 1},
		{"org-wide", func(context.Context, string, string) (bool, []string, error) {
			return true, nil, nil
		}, true, true, 1},
		{"nobody", func(context.Context, string, string) (bool, []string, error) {
			return false, []string{}, nil
		}, false, false, 1},
		{"read failed", func(context.Context, string, string) (bool, []string, error) {
			return false, nil, errors.New("db down")
		}, false, false, 1},
		{"no resolver", nil, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := websocket.NewHub()
			reader := dialTestClient(t, hub, "reader", "org-1", "sid-1")
			outsider := dialTestClient(t, hub, "outsider", "org-1", "sid-2")
			waitClientCount(t, hub, 2)

			b := &Backplane{originID: "control-pod", hub: hub}
			reads := 0
			if tc.readers != nil {
				b.SetConversationReaders(func(ctx context.Context, orgID, conversationID string) (bool, []string, error) {
					reads++
					if orgID != "org-1" || conversationID != "conv-1" {
						t.Errorf("readers asked about (%q, %q), want (org-1, conv-1)", orgID, conversationID)
					}
					return tc.readers(ctx, orgID, conversationID)
				})
			}

			b.handleWSNotification(readersEnvelope(t, "org-1", "conv-1"))

			if reads != tc.wantReadCount {
				t.Errorf("read the readers %d times, want %d", reads, tc.wantReadCount)
			}
			if _, got := readOrTimeout(t, reader, 300*time.Millisecond); got != tc.wantReader {
				t.Errorf("reader received the frame = %v, want %v", got, tc.wantReader)
			}
			if _, got := readOrTimeout(t, outsider, 200*time.Millisecond); got != tc.wantOutsider {
				t.Errorf("outsider received the frame = %v, want %v", got, tc.wantOutsider)
			}
		})
	}
}

// TestHandleWSNotification_ReadersOfConversationNoConnectionInOrgReadsNothing
// pins that a pod with no socket in the frame's org drops it without reading
// the audience: every pod that holds sockets receives every tf_ws event, so a
// read on each would multiply every frame's cost by the fleet's size. Once a
// connection in the org exists, the same frame is read for.
func TestHandleWSNotification_ReadersOfConversationNoConnectionInOrgReadsNothing(t *testing.T) {
	hub := websocket.NewHub()
	dialTestClient(t, hub, "member", "org-other", "sid-1")
	waitClientCount(t, hub, 1)

	b := &Backplane{originID: "control-pod", hub: hub}
	reads := 0
	b.SetConversationReaders(func(context.Context, string, string) (bool, []string, error) {
		reads++
		return true, nil, nil
	})

	b.handleWSNotification(readersEnvelope(t, "org-1", "conv-1"))
	if reads != 0 {
		t.Fatalf("read the readers %d times with no connection in the org, want 0", reads)
	}

	dialTestClient(t, hub, "member", "org-1", "sid-2")
	waitClientCount(t, hub, 2)
	b.handleWSNotification(readersEnvelope(t, "org-1", "conv-1"))
	if reads != 1 {
		t.Fatalf("read the readers %d times with a connection in the org, want 1", reads)
	}
}
