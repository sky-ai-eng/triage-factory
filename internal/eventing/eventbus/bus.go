// Package eventbus is the in-process pub/sub the binary uses to wire
// pollers, the router, the scorer trigger, and the WS broadcaster
// together without direct callbacks. Publishers emit domain.Event
// values; subscribers register handlers with an optional event-type
// prefix filter and an optional org dimension.
//
// Org-aware delivery:
//
//   - Each published event carries evt.OrgID — the tenant the event
//     belongs to. Publishers stamp this at the boundary they emit from
//     (per-org poller loops in multi-mode; LocalDefaultOrgID in local).
//
//   - Subscribers declare their profile via the constructor they use:
//
//   - Subscribe(sub) — unfiltered, system-service profile. The
//     handler sees every event regardless of org and is expected to
//     branch on evt.OrgID itself (router, scorer trigger, drainer,
//     poll-tracker, lifetime counter).
//
//   - SubscribeFor(orgID, sub) — org-scoped profile. The handler only
//     sees events whose evt.OrgID matches the subscriber's declared
//     org. Used by handlers reacting on behalf of a single tenant
//     (WebSocket fanout per (user, org), per-org request handlers).
//
//   - In local mode every event is stamped LocalDefaultOrgID and any
//     org-scoped subscriber subscribes for the same sentinel, so the
//     filter trivially matches and behavior is identical to the
//     pre-D9a single-org bus.
package eventbus

import (
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Handler is a function that processes an event. Handlers are called synchronously
// in a dedicated goroutine per subscriber, so they can block without affecting others.
type Handler func(domain.Event)

// Subscriber is a named event handler with optional filters.
//
// Filter narrows by event-type prefix; empty means "every type."
//
// OrgID narrows by tenant: empty (the system-service profile, set by
// Subscribe) delivers regardless of evt.OrgID; non-empty (the org-scoped
// profile, set by SubscribeFor) only delivers events whose OrgID matches.
// Subscribers should not set OrgID directly — go through SubscribeFor so
// the profile is explicit at the call site.
type Subscriber struct {
	Name   string   // for logging: "ws-broadcast", "scorer", etc.
	Filter []string // if non-empty, only deliver events matching these type prefixes
	OrgID  string   // if non-empty, only deliver events whose OrgID matches; empty = unfiltered (system-service)
	Handle Handler
}

// Bus is an in-process event bus. Pollers and system components publish events;
// subscribers (WS broadcaster, scorer, audit logger) consume them.
type Bus struct {
	mu          sync.RWMutex
	subscribers []subscriberEntry
	closed      bool
}

type subscriberEntry struct {
	sub Subscriber
	ch  chan domain.Event
}

// New creates a new event bus.
func New() *Bus {
	return &Bus{}
}

// Subscribe registers an unfiltered (system-service) handler. The
// handler sees every published event regardless of org and is expected
// to branch on evt.OrgID itself. Pass a non-empty sub.Filter to narrow
// by event-type prefix.
//
// For handlers that should only see events from a single tenant, use
// SubscribeFor instead.
//
// Starts a goroutine that drains events to the handler. Returns an
// unsubscribe function.
func (b *Bus) Subscribe(sub Subscriber) func() {
	return b.subscribe(sub)
}

// SubscribeFor registers an org-scoped handler. The handler only sees
// events whose evt.OrgID matches orgID; events from other tenants are
// dropped at the bus surface. The event-type prefix filter (sub.Filter)
// is composed with the org filter — both must match.
//
// Local mode passes runmode.LocalDefaultOrgID; every published event
// carries the same value so the filter trivially matches.
//
// Panics if orgID is empty — the org-scoped profile is meaningless
// without a tenant to scope to. Use Subscribe for the unfiltered
// system-service profile.
func (b *Bus) SubscribeFor(orgID string, sub Subscriber) func() {
	if orgID == "" {
		panic("eventbus: SubscribeFor requires a non-empty orgID; use Subscribe for the unfiltered system-service profile")
	}
	sub.OrgID = orgID
	return b.subscribe(sub)
}

func (b *Bus) subscribe(sub Subscriber) func() {
	ch := make(chan domain.Event, 256)
	entry := subscriberEntry{sub: sub, ch: ch}

	b.mu.Lock()
	b.subscribers = append(b.subscribers, entry)
	b.mu.Unlock()

	// Drain goroutine
	go func() {
		for evt := range ch {
			sub.Handle(evt)
		}
	}()

	// Return unsubscribe function
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, e := range b.subscribers {
			if e.ch == ch {
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

// Publish sends an event to all matching subscribers. Non-blocking — if a subscriber's
// buffer is full, the event is dropped with a warning.
//
// Matching is the conjunction of:
//   - event-type prefix filter (empty = match all),
//   - org filter (empty subscriber OrgID = match all; non-empty = match
//     evt.OrgID exactly).
func (b *Bus) Publish(evt domain.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, entry := range b.subscribers {
		if !matchesFilter(evt.EventType, entry.sub.Filter) {
			continue
		}
		if !matchesOrg(evt.OrgID, entry.sub.OrgID) {
			continue
		}
		select {
		case entry.ch <- evt:
		default:
			eventbusLog.Warn("dropping event for slow subscriber", "event_type", evt.EventType, "subscriber", entry.sub.Name)
		}
	}
}

// Close shuts down all subscriber channels.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, entry := range b.subscribers {
		close(entry.ch)
	}
	b.subscribers = nil
}

// matchesFilter returns true if the event type matches any of the filter prefixes,
// or if the filter is empty (match all).
func matchesFilter(eventType string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, prefix := range filter {
		if len(eventType) >= len(prefix) && eventType[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// matchesOrg returns true if the subscriber should receive an event
// from evtOrgID. An empty subOrgID is the system-service profile and
// matches every event; a non-empty subOrgID requires an exact match
// against the event's OrgID.
func matchesOrg(evtOrgID, subOrgID string) bool {
	if subOrgID == "" {
		return true
	}
	return evtOrgID == subOrgID
}
