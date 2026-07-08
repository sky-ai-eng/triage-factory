package slack

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// TestLifecycleAdapter_Run_SubscribesFilteredAndUnsubscribesOnShutdown
// exercises run() itself (the pgtest suite in lifecycle_pg_test.go calls
// dispatch directly and never goes through the real bus): it subscribes with
// the "system:run:"/"system:routing:" prefix filter, and returns promptly
// once ctx is cancelled. Publishes a matching-but-undispatched event type
// (proves the subscription + prefix filter work) and a non-matching one
// (must never reach dispatch — stores is nil here, so a mis-routed event
// would panic).
func TestLifecycleAdapter_Run_SubscribesFilteredAndUnsubscribesOnShutdown(t *testing.T) {
	bus := eventbus.New()
	adapter := newLifecycleAdapter(db.Stores{}, slackHTTPClient, staticURL(""), func() *eventbus.Bus { return bus })

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		adapter.run(ctx)
		close(runDone)
	}()
	time.Sleep(20 * time.Millisecond) // let run() reach bus.Subscribe before publishing

	bus.Publish(domain.Event{OrgID: "org-1", EventType: "system:run:unused-for-test"})
	bus.Publish(domain.Event{OrgID: "org-1", EventType: "system:poll:completed"})
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return within 2s of ctx cancellation")
	}
}

// TestLifecycleAdapter_Run_NilBus_ReturnsImmediately covers the defensive
// guard for a bus accessor that resolves to nil (shouldn't happen in
// production post-OnReady, but run() must not block forever if it does).
func TestLifecycleAdapter_Run_NilBus_ReturnsImmediately(t *testing.T) {
	adapter := newLifecycleAdapter(db.Stores{}, slackHTTPClient, staticURL(""), func() *eventbus.Bus { return nil })
	done := make(chan struct{})
	go func() {
		adapter.run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run() with a nil bus should return immediately")
	}
}
