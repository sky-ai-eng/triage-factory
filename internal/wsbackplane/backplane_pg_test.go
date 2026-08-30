package wsbackplane_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
	"github.com/sky-ai-eng/triage-factory/internal/wsbackplane"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// These tests exercise the REAL Postgres LISTEN/NOTIFY transport
// (dispatch/self-origin logic is covered without Docker in
// dispatch_test.go's white-box tests). One migrated instance is shared
// across the whole file's subtests to amortize the container boot cost;
// each subtest uses distinct org/user ids so they don't interfere with
// each other's assertions even though ws_outbox/ws_presence rows persist
// across subtests within the shared instance.
//
// NOTIFY is never queued for a session that hasn't issued LISTEN yet —
// an earlier publish before a listener is ready is simply lost, not
// delivered late. That makes retryPublish below both necessary (to
// absorb the listener-goroutine startup race) and safe (a publish that
// lands before readiness costs nothing; only the first one to land
// AFTER readiness is ever observed).
func TestBackplane_Postgres(t *testing.T) {
	adminDB, dsn := pgtest.NewInstance(t)
	if err := db.Migrate(adminDB, "postgres"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	t.Run("WSFanout_CrossPodDeliveryAndOriginNoEcho", func(t *testing.T) {
		testWSFanoutCrossPod(t, adminDB, dsn)
	})
	t.Run("WSFanout_OutboxRefForLargePayload", func(t *testing.T) {
		testWSFanoutOutboxRef(t, adminDB, dsn)
	})
	t.Run("OutboxReaper_RemovesStaleRows", func(t *testing.T) {
		testOutboxReaper(t, adminDB)
	})
	t.Run("PresenceReaper_RemovesStaleRowsWithoutTouchingLiveOnes", func(t *testing.T) {
		testPresenceReaper(t, adminDB)
	})
	t.Run("Kick_CrossPodClosesRemoteConnection", func(t *testing.T) {
		testKickCrossPod(t, adminDB, dsn)
	})
	t.Run("Presence_HeartbeatWriteIsReadableCrossPod", func(t *testing.T) {
		testPresenceCrossPod(t, adminDB)
	})
	t.Run("Sentinel_BrainListenerRepublishesNonSelfOrigin", func(t *testing.T) {
		testSentinelBrainRelay(t, adminDB, dsn)
	})
}

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
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hub.ClientCount() never reached %d", want)
}

// retryPublish calls publish repeatedly (bounded by timeout) until check
// reports success. Safe against the LISTEN-readiness race: an earlier
// publish that lands before the listener is ready is simply dropped by
// Postgres (never queued), so this never over-delivers once readiness is
// reached — see the parent test's doc comment.
func retryPublish(t *testing.T, timeout time.Duration, publish func(), check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		publish()
		time.Sleep(75 * time.Millisecond)
		if check() {
			return
		}
	}
	t.Fatalf("condition not observed within %s", timeout)
}

// msgReader background-reads a connection into a channel so callers can
// poll for a message with a timeout WITHOUT calling conn.Read directly
// under a short-deadline context — coder/websocket closes the underlying
// connection when a Read's context expires (see this package's
// close_test.go precedent in pkg/websocket), so a bounded Read used
// purely as a non-blocking "is anything here yet" probe would kill the
// connection on its very first miss. Reading via one long-lived
// background goroutine avoids that: only ITS Read call ever runs against
// the live conn, and callers just drain the channel it feeds.
type msgReader struct {
	msgs        chan []byte
	closeStatus ws.StatusCode
}

func newMsgReader(conn *ws.Conn) *msgReader {
	r := &msgReader{msgs: make(chan []byte, 64), closeStatus: -1}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				// Written before the channel close, which the channel-close
				// happens-before edge makes visible to any goroutine that
				// observes msgs as closed (Go memory model).
				r.closeStatus = ws.CloseStatus(err)
				close(r.msgs)
				return
			}
			r.msgs <- data
		}
	}()
	return r
}

// next waits up to timeout for the next message. ok is false on timeout
// OR if the connection closed (the channel closing yields a zero-value,
// false receive) — callers that need to distinguish "closed with a
// specific code" from "no message yet" use closedWith instead.
func (r *msgReader) next(timeout time.Duration) (msg []byte, ok bool) {
	select {
	case data, open := <-r.msgs:
		return data, open
	case <-time.After(timeout):
		return nil, false
	}
}

// closedWith polls (non-blocking, timeout-free) whether the connection
// has already closed with the given status code. Deliberately does NOT
// itself block on a timeout — it's meant to be called from inside a
// retryPublish check closure, which supplies its own poll cadence; a
// blocking wait here would double up the two loops' timing.
//
// Only safe to use on a connection that isn't also expected to receive
// ordinary messages before the close under test — a buffered message
// would be silently consumed here and reported as "not yet closed"
// rather than handed to a next() caller.
func (r *msgReader) closedWith(want ws.StatusCode) bool {
	select {
	case _, open := <-r.msgs:
		return !open && r.closeStatus == want
	default:
		return false
	}
}

// drainUntilQuiet discards messages until none arrives for a full
// quietFor window — used to clear warm-up copies before a real
// single-shot assertion. A single non-blocking drain isn't enough here:
// a warm-up round that retried several times can still have echoes in
// flight over the loopback connection when the retry loop returns (each
// publish/deliver/network hop isn't synchronous with the caller), so
// this keeps resetting the quiet timer on every message actually seen.
func (r *msgReader) drainUntilQuiet(quietFor time.Duration) {
	for {
		if _, ok := r.next(quietFor); !ok {
			return
		}
	}
}

func testWSFanoutCrossPod(t *testing.T, adminDB *sql.DB, dsn string) {
	hubA := websocket.NewHub()
	hubB := websocket.NewHub()
	bpA := wsbackplane.New(adminDB, dsn, "pod-a-wsfanout", hubA)
	bpB := wsbackplane.New(adminDB, dsn, "pod-b-wsfanout", hubB)
	hubA.SetBackplane(bpA)
	hubB.SetBackplane(bpB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bpA.RunPublicListener(ctx)
	go bpB.RunPublicListener(ctx)

	clientA := dialTestClient(t, hubA, "user-1", "org-wsfanout", "sid-a")
	clientB := dialTestClient(t, hubB, "user-1", "org-wsfanout", "sid-b")
	waitClientCount(t, hubA, 1)
	waitClientCount(t, hubB, 1)
	readerA := newMsgReader(clientA)
	readerB := newMsgReader(clientB)

	// Warm-up round: retry-publish a throwaway type until pod B observes
	// it, proving both pods' LISTEN loops are up (they started together).
	retryPublish(t, 10*time.Second, func() {
		hubA.Broadcast(websocket.Event{Type: "warmup", OrgID: "org-wsfanout"})
	}, func() bool {
		_, ok := readerB.next(150 * time.Millisecond)
		return ok
	})
	// Drain every warm-up copy on both readers — see drainUntilQuiet's
	// doc comment for why a single non-blocking drain isn't sufficient.
	readerA.drainUntilQuiet(300 * time.Millisecond)
	readerB.drainUntilQuiet(300 * time.Millisecond)

	// The real assertion: publish exactly once now that both listeners
	// are confirmed ready. B must see exactly one copy (cross-pod
	// delivery works). A must ALSO see exactly one copy — Broadcast's
	// synchronous local delivery (DeliverRemote) unconditionally reaches
	// hubA's own subscribed clients, same as before this backplane
	// existed, so clientA legitimately gets one — but not a SECOND one,
	// which is what pod A's own tf_ws LISTEN loop failing to skip its own
	// origin would produce.
	hubA.Broadcast(websocket.Event{Type: "task_updated", OrgID: "org-wsfanout"})

	if _, ok := readerB.next(3 * time.Second); !ok {
		t.Fatal("pod B never received the cross-pod event")
	}
	if _, ok := readerB.next(300 * time.Millisecond); ok {
		t.Fatal("pod B received a duplicate copy")
	}
	if _, ok := readerA.next(2 * time.Second); !ok {
		t.Fatal("pod A's own client never received Broadcast's normal local delivery")
	}
	if _, ok := readerA.next(500 * time.Millisecond); ok {
		t.Fatal("pod A received a SECOND copy of its own event — echo loop (self-origin skip failed)")
	}
}

func testWSFanoutOutboxRef(t *testing.T, adminDB *sql.DB, dsn string) {
	hubA := websocket.NewHub()
	hubB := websocket.NewHub()
	bpA := wsbackplane.New(adminDB, dsn, "pod-a-outbox", hubA)
	bpB := wsbackplane.New(adminDB, dsn, "pod-b-outbox", hubB)
	hubA.SetBackplane(bpA)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bpB.RunPublicListener(ctx)

	clientB := dialTestClient(t, hubB, "user-1", "org-outbox", "sid-b")
	waitClientCount(t, hubB, 1)
	readerB := newMsgReader(clientB)

	// A payload comfortably over the default 6KB inline threshold forces
	// the ws_outbox path.
	big := strings.Repeat("x", 8*1024)
	retryPublish(t, 10*time.Second, func() {
		hubA.Broadcast(websocket.Event{Type: "message", OrgID: "org-outbox", Data: map[string]any{"text": big}})
	}, func() bool {
		_, ok := readerB.next(150 * time.Millisecond)
		return ok
	})

	var n int
	if err := adminDB.QueryRow(`SELECT count(*) FROM ws_outbox`).Scan(&n); err != nil {
		t.Fatalf("count ws_outbox: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one ws_outbox row for the oversized payload")
	}
}

func testOutboxReaper(t *testing.T, adminDB *sql.DB) {
	if _, err := adminDB.Exec(`INSERT INTO ws_outbox (payload, created_at) VALUES ('{}'::jsonb, now() - interval '10 minutes')`); err != nil {
		t.Fatalf("seed stale outbox row: %v", err)
	}
	hub := websocket.NewHub()
	bp := wsbackplane.New(adminDB, "", "pod-reaper", hub)

	ctx, cancel := context.WithCancel(context.Background())
	go bp.RunOutboxReaper(ctx, 50*time.Millisecond, 1*time.Second)
	t.Cleanup(cancel)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := adminDB.QueryRow(`SELECT count(*) FROM ws_outbox WHERE created_at < now() - interval '5 minutes'`).Scan(&n); err != nil {
			t.Fatalf("count stale ws_outbox rows: %v", err)
		}
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stale ws_outbox row was never reaped")
}

// testPresenceReaper pins the storage-bounding half of ws_presence's
// lifecycle (distinct from PresentFor's read-time live-window filter,
// which testPresenceCrossPod already covers): a row past the reaper's
// TTL gets physically deleted, while a fresh row — well inside both the
// live window AND the reap TTL — survives untouched. Without this, every
// reconnect (a new conn_id nothing ever overwrites) would accumulate
// forever.
func testPresenceReaper(t *testing.T, adminDB *sql.DB) {
	if _, err := adminDB.Exec(`
		INSERT INTO ws_presence (user_id, conn_id, org_id, instance_id, viewing, visible, last_seen)
		VALUES ('user-stale', 'conn-stale', 'org-presence-reaper', 'pod-x', 'board', true, now() - interval '10 minutes')
	`); err != nil {
		t.Fatalf("seed stale presence row: %v", err)
	}
	if _, err := adminDB.Exec(`
		INSERT INTO ws_presence (user_id, conn_id, org_id, instance_id, viewing, visible, last_seen)
		VALUES ('user-fresh', 'conn-fresh', 'org-presence-reaper', 'pod-x', 'board', true, now())
	`); err != nil {
		t.Fatalf("seed fresh presence row: %v", err)
	}

	hub := websocket.NewHub()
	bp := wsbackplane.New(adminDB, "", "pod-reaper", hub)

	ctx, cancel := context.WithCancel(context.Background())
	go bp.RunPresenceReaper(ctx, 50*time.Millisecond, 1*time.Second)
	t.Cleanup(cancel)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var stale bool
		if err := adminDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM ws_presence WHERE user_id = 'user-stale')`).Scan(&stale); err != nil {
			t.Fatalf("check stale row: %v", err)
		}
		if !stale {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var stale bool
	if err := adminDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM ws_presence WHERE user_id = 'user-stale')`).Scan(&stale); err != nil {
		t.Fatalf("check stale row: %v", err)
	}
	if stale {
		t.Fatal("stale ws_presence row was never reaped")
	}

	var fresh bool
	if err := adminDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM ws_presence WHERE user_id = 'user-fresh')`).Scan(&fresh); err != nil {
		t.Fatalf("check fresh row: %v", err)
	}
	if !fresh {
		t.Fatal("reaper deleted a fresh row it should never have touched")
	}
}

func testKickCrossPod(t *testing.T, adminDB *sql.DB, dsn string) {
	hubA := websocket.NewHub()
	hubB := websocket.NewHub()
	bpA := wsbackplane.New(adminDB, dsn, "pod-a-kick", hubA)
	bpB := wsbackplane.New(adminDB, dsn, "pod-b-kick", hubB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Pod B's tf_ctl consumer: since the tf_ctl consolidation, kicks no
	// longer ride RunPublicListener (tf_ws-only) — in production the
	// app's unified tf_ctl dispatcher (internal/app/ctl.go) LISTENs via a
	// pgnotify.Listener and routes kind:"kick" payloads to HandleCtlKick.
	// This listener is that dispatcher's kick branch, minimally.
	kickListener := pgnotify.NewListener(dsn, wsbackplane.ChannelCtl, func(payload string) {
		var probe struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Kind == "kick" {
			bpB.HandleCtlKick(payload)
		}
	})
	go kickListener.Run(ctx)

	connB := dialTestClient(t, hubB, "user-kick", "org-kick", "sid-b")
	waitClientCount(t, hubB, 1)
	readerB := newMsgReader(connB)

	// Pod A closes its own (zero) local matches, then publishes — mirrors
	// the real call sites in internal/server (local close, then publish).
	retryPublish(t, 10*time.Second, func() {
		hubA.CloseUserConnections("user-kick", "", "org-kick", websocket.CloseSessionRevoked, "session revoked")
		bpA.PublishKick(context.Background(), "user-kick", "", "org-kick", int(websocket.CloseSessionRevoked), "session revoked")
	}, func() bool {
		return readerB.closedWith(websocket.CloseSessionRevoked)
	})
}

func testPresenceCrossPod(t *testing.T, adminDB *sql.DB) {
	hubA := websocket.NewHub()
	hubB := websocket.NewHub()
	bpA := wsbackplane.New(adminDB, "", "pod-a-presence", hubA)
	bpB := wsbackplane.New(adminDB, "", "pod-b-presence", hubB)

	// A viewer connects to pod A and reports presence on the board.
	connA := dialTestClient(t, hubA, "user-presence", "org-presence", "sid-a")
	waitClientCount(t, hubA, 1)
	if err := connA.Write(context.Background(), ws.MessageText, []byte(`{"type":"presence","viewing":"board","visible":true}`)); err != nil {
		t.Fatalf("send presence frame: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hubA.PresentFor("org-presence", "some-run") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !hubA.PresentFor("org-presence", "some-run") {
		t.Fatal("pod A's own hub never observed the presence frame")
	}

	// Pod A's heartbeat mirrors that connection into ws_presence.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bpA.RunPresenceHeartbeat(ctx, 30*time.Millisecond)

	// Pod B — which has NO local socket for this user at all — must see
	// the viewer as present via the ws_presence fallback.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if bpB.PresentFor(context.Background(), "org-presence", "some-run") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pod B never observed pod A's presence via ws_presence")
}

func testSentinelBrainRelay(t *testing.T, adminDB *sql.DB, dsn string) {
	executor := wsbackplane.New(adminDB, dsn, "executor-1-sentinel", nil)
	brain := wsbackplane.New(adminDB, dsn, "brain-1-sentinel", nil)

	received := make(chan domain.Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go brain.RunBusListener(ctx, func(evt domain.Event) { received <- evt })

	want := domain.Event{EventType: "system:conversation:activity", OrgID: "org-sentinel", MetadataJSON: `{"tool":"bash"}`}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		executor.PublishSentinel(want)
		select {
		case got := <-received:
			if got.EventType != want.EventType || got.OrgID != want.OrgID {
				t.Fatalf("relayed sentinel = %+v, want %+v", got, want)
			}
			return
		case <-time.After(75 * time.Millisecond):
		}
	}
	t.Fatal("brain never received the executor's relayed sentinel")
}
