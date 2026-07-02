package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// TestExtensionAPI_PublishEvent_DelegatesToIngestor pins that
// ExtensionAPI.PublishEvent forwards through the wired Ingestor (durable
// enqueue + bus fan-out) rather than touching the bus directly — the ee/
// ingest surface this adds. queue=nil degrades Publish to bus-only, which is
// enough to prove the delegation without a DB.
func TestExtensionAPI_PublishEvent_DelegatesToIngestor(t *testing.T) {
	bus := eventbus.New()
	got := make(chan domain.Event, 1)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "capture",
		Handle: func(e domain.Event) { got <- e },
	})

	srv := &Server{}
	srv.SetIngestor(ingest.New(bus, nil, nil))
	api := serverExtensionAPI{srv}

	api.PublishEvent(domain.Event{EventType: "fake:thing", OrgID: "org-1"})

	select {
	case e := <-got:
		if e.EventType != "fake:thing" {
			t.Errorf("event_type = %q, want fake:thing", e.EventType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the ingestor to forward the event onto the bus")
	}
}

// TestExtensionAPI_PublishEvent_NilIngestor_DropsWithLog pins the
// drop-loudly contract: before app wiring completes (ingestor nil),
// PublishEvent must NOT fall back to a bare bus publish — that would
// silently skip the durable outbox — so it drops and logs at ERROR instead.
func TestExtensionAPI_PublishEvent_NilIngestor_DropsWithLog(t *testing.T) {
	var logbuf bytes.Buffer
	restore := logging.SetOutput(&logbuf)
	defer restore()

	api := serverExtensionAPI{&Server{}}
	api.PublishEvent(domain.Event{EventType: "fake:thing", OrgID: "org-1"})

	if !strings.Contains(logbuf.String(), "ingestor not wired") {
		t.Errorf("expected an ERROR 'ingestor not wired' log on a nil ingestor; got:\n%s", logbuf.String())
	}
}

// TestExtensionAPI_Bus_ReadsThroughToSetEventBus pins Bus() as a plain
// read-through accessor to whatever SetEventBus wired — nil before wiring,
// the same bus instance after.
func TestExtensionAPI_Bus_ReadsThroughToSetEventBus(t *testing.T) {
	srv := &Server{}
	if got := (serverExtensionAPI{srv}).Bus(); got != nil {
		t.Errorf("Bus() = %v, want nil before SetEventBus", got)
	}

	bus := eventbus.New()
	srv.SetEventBus(bus)
	if got := (serverExtensionAPI{srv}).Bus(); got != bus {
		t.Errorf("Bus() = %v, want the bus SetEventBus wired (%v)", got, bus)
	}
}

// TestExtensionAPI_OnReady_NilHookPanics pins the fail-fast-at-registration
// contract shared with the other core registries (routing.RegisterSource,
// events.Register): a nil hook panics immediately during install, rather
// than being silently collected and later crashing a goroutine — with a much
// less useful stack trace — when StartExtensionWorkers calls it.
func TestExtensionAPI_OnReady_NilHookPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("OnReady(nil) did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "OnReady") {
			t.Errorf("panic value = %v, want a message naming OnReady", r)
		}
	}()

	api := serverExtensionAPI{&Server{}}
	api.OnReady(nil)
}

// TestExtensionAPI_OnReady_DoesNotFireUntilStarted pins that registering a
// hook (the install-time action) is inert on its own — it must not run until
// StartExtensionWorkers fires it, mirroring the real boot order where
// OnReady is called during routes()/installExtensions, well before app
// wiring (and therefore StartExtensionWorkers) completes.
func TestExtensionAPI_OnReady_DoesNotFireUntilStarted(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	fired := make(chan struct{}, 1)
	api.OnReady(func(ctx context.Context) { fired <- struct{}{} })

	select {
	case <-fired:
		t.Fatal("hook fired on OnReady registration; it must wait for StartExtensionWorkers")
	case <-time.After(100 * time.Millisecond):
	}

	srv.StartExtensionWorkers(context.Background())

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StartExtensionWorkers to fire the hook")
	}
}

// TestServer_StartExtensionWorkers_HookSeesWiredBus pins the seam's core
// ordering guarantee: by the time a hook fires, read-through accessors like
// Bus() resolve to whatever app wiring set before StartExtensionWorkers ran.
func TestServer_StartExtensionWorkers_HookSeesWiredBus(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	bus := eventbus.New()
	srv.SetEventBus(bus)

	got := make(chan *eventbus.Bus, 1)
	api.OnReady(func(ctx context.Context) { got <- api.Bus() })

	srv.StartExtensionWorkers(context.Background())

	select {
	case b := <-got:
		if b != bus {
			t.Errorf("Bus() inside hook = %v, want the bus wired before StartExtensionWorkers (%v)", b, bus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hook to observe Bus()")
	}
}

// TestServer_StartExtensionWorkers_CtxCancelObservableInHook pins that the
// context handed to a hook is the shutdown-cancelling context passed to
// StartExtensionWorkers, not some detached background context.
func TestServer_StartExtensionWorkers_CtxCancelObservableInHook(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	started := make(chan struct{})
	cancelled := make(chan struct{})
	api.OnReady(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})

	ctx, cancel := context.WithCancel(context.Background())
	srv.StartExtensionWorkers(ctx)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hook to start")
	}

	cancel()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ctx cancellation to be observed inside the hook")
	}
}

// TestServer_StartExtensionWorkers_SecondCallIsNoOp pins the idempotence
// guard: a second StartExtensionWorkers call (e.g. a test or caller
// double-invoking Run) must not fire hooks again.
func TestServer_StartExtensionWorkers_SecondCallIsNoOp(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	calls := make(chan struct{}, 10)
	api.OnReady(func(ctx context.Context) { calls <- struct{}{} })

	ctx := context.Background()
	srv.StartExtensionWorkers(ctx)
	srv.StartExtensionWorkers(ctx)

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hook to fire once")
	}

	select {
	case <-calls:
		t.Fatal("hook fired a second time; StartExtensionWorkers must be idempotent")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestServer_StartExtensionWorkers_MultipleHooksAllFire pins that every
// registered hook fires — the shape of two ee/ extensions each registering
// their own OnReady worker off the same install pass.
func TestServer_StartExtensionWorkers_MultipleHooksAllFire(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	fired1 := make(chan struct{}, 1)
	fired2 := make(chan struct{}, 1)
	api.OnReady(func(ctx context.Context) { fired1 <- struct{}{} })
	api.OnReady(func(ctx context.Context) { fired2 <- struct{}{} })

	srv.StartExtensionWorkers(context.Background())

	for _, ch := range []chan struct{}{fired1, fired2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a registered hook to fire")
		}
	}
}
