package websocket

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// drain receives every queued frame on the client's send chan, decoded
// back into an Event. We need the decode because Hub.Broadcast
// serialises to JSON before fanout; clients only ever see []byte.
//
// withTimeout is a short ceiling so a regression that fails to deliver
// to the client surfaces as "expected N, got M" rather than wedging
// the test forever.
func drain(t *testing.T, c *client, withTimeout time.Duration) []Event {
	t.Helper()
	deadline := time.After(withTimeout)
	var out []Event
	for {
		select {
		case msg := <-c.send:
			var evt Event
			if err := json.Unmarshal(msg, &evt); err != nil {
				t.Fatalf("decode broadcast frame: %v (raw=%q)", err, msg)
			}
			out = append(out, evt)
		case <-deadline:
			return out
		}
	}
}

// addBareClient mints a *client with identity but no live connection.
// HandleWS does the upgrade-and-register dance in production; tests
// for the Broadcast filter only care about the in-memory routing
// surface (client map + send chan), so we synthesize a registered
// client directly. send is buffered large enough that fanout to a
// handful of clients never trips the slow-client drop path.
func addBareClient(h *Hub, userID, orgID string) *client {
	c := &client{
		// Big buffer keeps the concurrent fanout test from tripping
		// Broadcast's "dropping message for slow client" path before
		// the test has had a chance to drain — production buffers
		// are 64; we use more to bound test sizes without flaking.
		send:   make(chan []byte, 1024),
		closed: make(chan struct{}),
		userID: userID,
		orgID:  orgID,
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// TestBroadcast_OrgScoping is the core multi-tenancy guarantee: an
// event stamped with orgA's UUID must reach orgA's clients and ONLY
// orgA's clients. Without the per-connection filter, the old hub
// fanned every event to every connection, which leaked PR titles /
// run statuses / entity metadata across tenants.
func TestBroadcast_OrgScoping(t *testing.T) {
	h := NewHub()
	const (
		orgA = "00000000-0000-0000-0000-0000000000aa"
		orgB = "00000000-0000-0000-0000-0000000000bb"
	)
	clientA := addBareClient(h, "userA", orgA)
	clientB := addBareClient(h, "userB", orgB)

	h.Broadcast(Event{Type: "task_updated", OrgID: orgA, Data: map[string]string{"task_id": "t1"}})

	gotA := drain(t, clientA, 50*time.Millisecond)
	gotB := drain(t, clientB, 50*time.Millisecond)

	if len(gotA) != 1 {
		t.Fatalf("orgA expected 1 event, got %d", len(gotA))
	}
	if gotA[0].Type != "task_updated" {
		t.Errorf("orgA event type = %q, want task_updated", gotA[0].Type)
	}
	if gotA[0].OrgID != "" {
		// json:"-" must keep OrgID out of the wire shape: leaking the
		// routing-side org to the client would couple the FE to
		// server-side identity it doesn't need.
		t.Errorf("decoded event carried OrgID %q on the wire — must be tagged json:\"-\"", gotA[0].OrgID)
	}
	if len(gotB) != 0 {
		t.Errorf("orgB unexpectedly received %d events for an orgA broadcast", len(gotB))
	}
}

// TestBroadcast_EmptyOrgDeliversEverywhere covers the system-wide
// event path: main.go's worktree clone-status broadcast and
// historically the ws-broadcast subscriber both fired with an empty
// OrgID. Empty event OrgID is the explicit "system-wide" contract;
// every connected client must still receive it.
func TestBroadcast_EmptyOrgDeliversEverywhere(t *testing.T) {
	h := NewHub()
	clientA := addBareClient(h, "userA", "orgA")
	clientB := addBareClient(h, "userB", "orgB")
	unscoped := addBareClient(h, "", "")

	h.Broadcast(Event{Type: "system_announcement", Data: "deploy in 5 min"})

	for label, c := range map[string]*client{"orgA": clientA, "orgB": clientB, "unscoped": unscoped} {
		got := drain(t, c, 50*time.Millisecond)
		if len(got) != 1 {
			t.Errorf("%s expected 1 system-wide event, got %d", label, len(got))
		}
	}
}

// TestBroadcast_UnscopedClientReceivesEverything covers the pre-auth
// / test-harness case where a client connects without identity
// (HandleWS tolerates empty userID/orgID for tests that hit the hub
// without the full server pipeline). The filter must not drop events
// targeted at any specific org for an unscoped client — that would
// regress every existing test that wires a Hub directly.
func TestBroadcast_UnscopedClientReceivesEverything(t *testing.T) {
	h := NewHub()
	unscoped := addBareClient(h, "", "")

	h.Broadcast(Event{Type: "first", OrgID: "orgA"})
	h.Broadcast(Event{Type: "second", OrgID: "orgB"})
	h.Broadcast(Event{Type: "third"}) // system-wide

	got := drain(t, unscoped, 50*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("unscoped client expected 3 events, got %d", len(got))
	}
}

// TestBroadcast_UserScoping covers the per-user filter for events that
// should reach exactly one user inside an org (today: reserved for
// future "another user took over your run" notifications). Two
// clients in the same org with different userIDs: an event with
// UserID = userA must skip userB.
func TestBroadcast_UserScoping(t *testing.T) {
	h := NewHub()
	const orgA = "orgA"
	clientA := addBareClient(h, "userA", orgA)
	clientB := addBareClient(h, "userB", orgA)

	h.Broadcast(Event{
		Type:   "personal_ping",
		OrgID:  orgA,
		UserID: "userA",
		Data:   "for you only",
	})

	gotA := drain(t, clientA, 50*time.Millisecond)
	gotB := drain(t, clientB, 50*time.Millisecond)

	if len(gotA) != 1 {
		t.Errorf("userA expected 1 event, got %d", len(gotA))
	}
	if len(gotB) != 0 {
		t.Errorf("userB unexpectedly received a per-user event for userA: %+v", gotB)
	}
}

// TestBroadcast_RecipientSet pins the set axis: an event carrying UserIDs
// reaches only the connections whose user id is in the set. Unlike the
// UserID filter it is strict, so a connection with no user id (which
// UserID lets through) receives nothing.
func TestBroadcast_RecipientSet(t *testing.T) {
	h := NewHub()
	const org = "orgA"
	inA := addBareClient(h, "userA", org)
	inB := addBareClient(h, "userB", org)
	outside := addBareClient(h, "userC", org)
	unscoped := addBareClient(h, "", "")

	h.Broadcast(Event{Type: "message", OrgID: org, UserIDs: []string{"userA", "userB"}, Data: "x"})

	for label, c := range map[string]*client{"userA": inA, "userB": inB} {
		if got := drain(t, c, 50*time.Millisecond); len(got) != 1 {
			t.Errorf("%s expected 1 event, got %d", label, len(got))
		}
	}
	for label, c := range map[string]*client{"userC": outside, "unscoped": unscoped} {
		if got := drain(t, c, 50*time.Millisecond); len(got) != 0 {
			t.Errorf("%s received %d events addressed to a set it is not in", label, len(got))
		}
	}
}

// TestBroadcast_MalformedRecipientSetDeliversAndPublishesNothing pins the
// two shapes Broadcast refuses: an empty non-nil set, and a set alongside
// UserID. Neither reaches any connection, the unscoped one included, and
// neither is published to the backplane.
func TestBroadcast_MalformedRecipientSetDeliversAndPublishesNothing(t *testing.T) {
	cases := map[string]Event{
		"empty set":        {Type: "message", OrgID: "orgA", UserIDs: []string{}},
		"set and user too": {Type: "message", OrgID: "orgA", UserID: "userA", UserIDs: []string{"userA"}},
	}
	for name, evt := range cases {
		t.Run(name, func(t *testing.T) {
			h := NewHub()
			bp := &fakeBackplane{}
			h.SetBackplane(bp)
			scoped := addBareClient(h, "userA", "orgA")
			unscoped := addBareClient(h, "", "")

			h.Broadcast(evt)

			for label, c := range map[string]*client{"userA": scoped, "unscoped": unscoped} {
				if got := drain(t, c, 50*time.Millisecond); len(got) != 0 {
					t.Errorf("%s received %d events", label, len(got))
				}
			}
			if got := bp.count(); got != 0 {
				t.Errorf("backplane saw %d published events, want 0", got)
			}
		})
	}
}

// TestBroadcast_NilHub matches the documented "nil-receiver-safe"
// contract: a nil *Hub silently drops the event so callers that
// conditionally have a hub (tests, pre-wired packages) don't have to
// guard every call site.
func TestBroadcast_NilHub(t *testing.T) {
	var h *Hub
	h.Broadcast(Event{Type: "should_not_panic"}) // no panic = pass
}

// TestBroadcast_OrgMismatchUnderConcurrency exercises the filter under
// the kind of fanout the production hub sees — N goroutines each
// firing events targeted at one of M orgs, with one client per org.
// Confirms the RLock-only fanout doesn't admit cross-talk and that
// no per-org client ever sees a sibling org's event.
func TestBroadcast_OrgMismatchUnderConcurrency(t *testing.T) {
	h := NewHub()
	const (
		orgA = "orgA"
		orgB = "orgB"
		fan  = 50 // events per org
	)
	clientA := addBareClient(h, "userA", orgA)
	clientB := addBareClient(h, "userB", orgB)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < fan; i++ {
			h.Broadcast(Event{Type: "a_only", OrgID: orgA})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < fan; i++ {
			h.Broadcast(Event{Type: "b_only", OrgID: orgB})
		}
	}()
	wg.Wait()

	// 200ms is generous for 100 in-process Broadcasts to land on the
	// buffered send chans; raises the noise floor for "did everything
	// arrive?" without inviting flakes.
	gotA := drain(t, clientA, 200*time.Millisecond)
	gotB := drain(t, clientB, 200*time.Millisecond)

	if len(gotA) != fan {
		t.Errorf("orgA expected %d events, got %d", fan, len(gotA))
	}
	for _, e := range gotA {
		if e.Type != "a_only" {
			t.Errorf("orgA leaked event for %q", e.Type)
		}
	}
	if len(gotB) != fan {
		t.Errorf("orgB expected %d events, got %d", fan, len(gotB))
	}
	for _, e := range gotB {
		if e.Type != "b_only" {
			t.Errorf("orgB leaked event for %q", e.Type)
		}
	}
}

// TestEvent_WireShapeHidesIdentity guards the json:"-" tags on
// OrgID/UserID. Adding them would couple the FE to server-side
// identity fields it has no business with, and re-introduce a leak
// path for "who-owns-what" metadata via the WS stream. If a future
// refactor accidentally drops the json:"-" tag, this test catches
// it loudly.
func TestEvent_WireShapeHidesIdentity(t *testing.T) {
	evt := Event{
		Type:   "task_claimed",
		OrgID:  "should-not-leak",
		UserID: "also-should-not-leak",
		Data:   map[string]string{"task_id": "t1"},
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded := string(raw)
	if strings.Contains(encoded, "should-not-leak") {
		t.Errorf("OrgID leaked into wire format: %s", encoded)
	}
	if strings.Contains(encoded, "also-should-not-leak") {
		t.Errorf("UserID leaked into wire format: %s", encoded)
	}
}
