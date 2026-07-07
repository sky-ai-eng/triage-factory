package eventbus

import (
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// drain waits for the subscriber goroutine to receive `want` events (or
// the deadline elapses). The Subscribe → Handle hop is asynchronous, so
// every test that asserts on received counts needs a short busy-wait.
func drain(t *testing.T, mu *sync.Mutex, got *[]domain.Event, want int) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*got)
		mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSubscribeFor_FiltersByOrg pins the org-aware delivery contract that
// org-scoped subscribers only see events whose evt.OrgID matches the
// subscriber's declared org. Events for a different tenant must not
// reach the org-A handler, and vice versa.
func TestSubscribeFor_FiltersByOrg(t *testing.T) {
	bus := New()
	defer bus.Close()

	const orgA = "00000000-0000-0000-0000-00000000000a"
	const orgB = "00000000-0000-0000-0000-00000000000b"

	var (
		muA       sync.Mutex
		receivedA []domain.Event
		muB       sync.Mutex
		receivedB []domain.Event
	)
	bus.SubscribeFor(orgA, Subscriber{
		Name: "org-a-handler",
		Handle: func(evt domain.Event) {
			muA.Lock()
			receivedA = append(receivedA, evt)
			muA.Unlock()
		},
	})
	bus.SubscribeFor(orgB, Subscriber{
		Name: "org-b-handler",
		Handle: func(evt domain.Event) {
			muB.Lock()
			receivedB = append(receivedB, evt)
			muB.Unlock()
		},
	})

	bus.Publish(domain.Event{OrgID: orgA, EventType: "github:pr:opened"})
	bus.Publish(domain.Event{OrgID: orgB, EventType: "github:pr:merged"})
	bus.Publish(domain.Event{OrgID: orgA, EventType: "jira:issue:assigned"})

	drain(t, &muA, &receivedA, 2)
	drain(t, &muB, &receivedB, 1)

	muA.Lock()
	defer muA.Unlock()
	muB.Lock()
	defer muB.Unlock()

	if len(receivedA) != 2 {
		t.Errorf("org-A subscriber: got %d events, want 2; received=%+v", len(receivedA), receivedA)
	}
	for _, e := range receivedA {
		if e.OrgID != orgA {
			t.Errorf("org-A subscriber received cross-org event: %+v", e)
		}
	}
	if len(receivedB) != 1 {
		t.Errorf("org-B subscriber: got %d events, want 1; received=%+v", len(receivedB), receivedB)
	}
	for _, e := range receivedB {
		if e.OrgID != orgB {
			t.Errorf("org-B subscriber received cross-org event: %+v", e)
		}
	}
}

// TestSubscribe_UnfilteredSeesAllOrgs pins that system-service
// subscribers registered via Subscribe (no orgID) see every event
// regardless of org — the profile router, scorer trigger, drainer,
// poll-tracker rely on.
func TestSubscribe_UnfilteredSeesAllOrgs(t *testing.T) {
	bus := New()
	defer bus.Close()

	var (
		mu       sync.Mutex
		received []domain.Event
	)
	bus.Subscribe(Subscriber{
		Name: "system-service",
		Handle: func(evt domain.Event) {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		},
	})

	bus.Publish(domain.Event{OrgID: "org-a", EventType: "github:pr:opened"})
	bus.Publish(domain.Event{OrgID: "org-b", EventType: "github:pr:merged"})
	bus.Publish(domain.Event{OrgID: "", EventType: "system:poll:completed"})

	drain(t, &mu, &received, 3)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Fatalf("unfiltered subscriber: got %d events, want 3; received=%+v", len(received), received)
	}
}

// TestSubscribeFor_DoesNotMatchEmptyOrgEvent pins that an event with no
// OrgID (a misconfigured publisher, or a legacy emission) does not leak
// into any org-scoped subscriber. The system-service profile is the
// right venue for OrgID-less events.
func TestSubscribeFor_DoesNotMatchEmptyOrgEvent(t *testing.T) {
	bus := New()
	defer bus.Close()

	var (
		mu       sync.Mutex
		received []domain.Event
	)
	bus.SubscribeFor("org-a", Subscriber{
		Name: "scoped",
		Handle: func(evt domain.Event) {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		},
	})

	bus.Publish(domain.Event{EventType: "system:poll:completed"}) // no OrgID

	// Give the bus a moment in case (incorrectly) it would deliver.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 0 {
		t.Errorf("org-scoped subscriber received OrgID-less event: %+v", received)
	}
}

// TestSubscribeFor_ComposesWithTypeFilter pins that the event-type
// prefix filter and the org filter compose as an AND — both must match
// for delivery.
func TestSubscribeFor_ComposesWithTypeFilter(t *testing.T) {
	bus := New()
	defer bus.Close()

	const orgA = "org-a"

	var (
		mu       sync.Mutex
		received []domain.Event
	)
	bus.SubscribeFor(orgA, Subscriber{
		Name:   "github-only",
		Filter: []string{"github:"},
		Handle: func(evt domain.Event) {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		},
	})

	bus.Publish(domain.Event{OrgID: orgA, EventType: "github:pr:opened"})    // org+type match
	bus.Publish(domain.Event{OrgID: orgA, EventType: "jira:issue:assigned"}) // type miss
	bus.Publish(domain.Event{OrgID: "org-b", EventType: "github:pr:merged"}) // org miss

	drain(t, &mu, &received, 1)
	time.Sleep(30 * time.Millisecond) // settle: prove nothing else arrives

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("got %d events, want 1; received=%+v", len(received), received)
	}
	if received[0].EventType != "github:pr:opened" {
		t.Errorf("delivered wrong event: %+v", received[0])
	}
}

// TestSubscribeFor_PanicsOnEmptyOrgID pins that the org-scoped surface
// refuses an empty orgID — empty is the system-service sentinel and
// silently treating SubscribeFor("") as Subscribe would mask wiring
// bugs.
func TestSubscribeFor_PanicsOnEmptyOrgID(t *testing.T) {
	bus := New()
	defer bus.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("SubscribeFor(\"\") did not panic")
		}
	}()
	bus.SubscribeFor("", Subscriber{Name: "bad", Handle: func(domain.Event) {}})
}

// TestUnsubscribe_DoesNotReceiveAfterCancel pins that the unsubscribe
// returned by SubscribeFor halts delivery — the same guarantee
// Subscribe already provides. This matters for org-scoped WebSocket
// fanout (D9b) where a client disconnect must unwire its handler.
func TestUnsubscribe_DoesNotReceiveAfterCancel(t *testing.T) {
	bus := New()
	defer bus.Close()

	const orgA = "org-a"

	var (
		mu       sync.Mutex
		received []domain.Event
	)
	cancel := bus.SubscribeFor(orgA, Subscriber{
		Name: "scoped",
		Handle: func(evt domain.Event) {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		},
	})

	bus.Publish(domain.Event{OrgID: orgA, EventType: "github:pr:opened"})
	drain(t, &mu, &received, 1)

	cancel()

	bus.Publish(domain.Event{OrgID: orgA, EventType: "github:pr:merged"})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Errorf("got %d events after cancel, want 1; received=%+v", len(received), received)
	}
}
