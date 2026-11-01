package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// enqueueForbidden is an event queue whose Enqueue fails the test: the
// pre-enqueued path must reach the bus without writing a second queue row.
type enqueueForbidden struct {
	db.EventQueueStore
	t *testing.T
}

func (q enqueueForbidden) Enqueue(context.Context, string, domain.Event, string) (string, error) {
	q.t.Error("PublishPreEnqueued enqueued an event the caller already committed")
	return "", nil
}

// TestExtensionAPI_PublishPreEnqueued_FansOutWithoutEnqueue pins that an
// event an extension committed in its own transaction reaches the bus with
// the id its enqueue minted, and is not enqueued again.
func TestExtensionAPI_PublishPreEnqueued_FansOutWithoutEnqueue(t *testing.T) {
	bus := eventbus.New()
	got := make(chan domain.Event, 1)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "capture",
		Handle: func(e domain.Event) { got <- e },
	})

	woke := false
	srv := &Server{}
	srv.SetIngestor(ingest.New(bus, enqueueForbidden{t: t}, func() { woke = true }))
	api := serverExtensionAPI{srv}

	api.PublishPreEnqueued(context.Background(), domain.Event{ID: "evt-1", EventType: domain.EventSlackMessage, OrgID: "org-1"})

	select {
	case e := <-got:
		if e.ID != "evt-1" {
			t.Errorf("bus event id = %q, want evt-1 (the id the caller's enqueue minted)", e.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the ingestor to forward the event onto the bus")
	}
	if !woke {
		t.Error("drain worker not nudged for a committed queue row")
	}
}

// TestExtensionAPI_PublishPreEnqueued_NilIngestor_Logs: unwired, the fan-out
// is skipped with a log line naming the queued event.
func TestExtensionAPI_PublishPreEnqueued_NilIngestor_Logs(t *testing.T) {
	var logbuf bytes.Buffer
	restore := logging.SetOutput(&logbuf)
	defer restore()

	api := serverExtensionAPI{&Server{}}
	api.PublishPreEnqueued(context.Background(), domain.Event{ID: "evt-1", EventType: domain.EventSlackMessage, OrgID: "org-1"})

	if !strings.Contains(logbuf.String(), "ingestor not wired") || !strings.Contains(logbuf.String(), "evt-1") {
		t.Errorf("expected an 'ingestor not wired' log naming evt-1 on a nil ingestor; got:\n%s", logbuf.String())
	}
}

// TestExtensionAPI_OnReady_NilHookPanics: a nil hook panics at registration,
// like the other core registries, rather than crashing a goroutine later when
// StartExtensionWorkers calls it.
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

// TestExtensionAPI_OnReady_DoesNotFireUntilBrainStarts: an OnReady hook does not
// run on registration or from the replica-safe StartExtensionWorkers, only from
// StartBrainExtensionWorkers.
func TestExtensionAPI_OnReady_DoesNotFireUntilBrainStarts(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	fired := make(chan struct{}, 1)
	api.OnReady(func(ctx context.Context) { fired <- struct{}{} })

	select {
	case <-fired:
		t.Fatal("hook fired on OnReady registration; it must wait for StartBrainExtensionWorkers")
	case <-time.After(100 * time.Millisecond):
	}

	srv.StartExtensionWorkers(context.Background())
	select {
	case <-fired:
		t.Fatal("a brain-gated OnReady hook must not fire from StartExtensionWorkers (the replica-safe path)")
	case <-time.After(100 * time.Millisecond):
	}

	srv.StartBrainExtensionWorkers(context.Background())
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StartBrainExtensionWorkers to fire the hook")
	}
}

// TestExtensionAPI_OnReadyReplicaSafe_NilHookPanics mirrors OnReady's
// fail-fast-at-registration contract for the replica-safe hatch.
func TestExtensionAPI_OnReadyReplicaSafe_NilHookPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("OnReadyReplicaSafe(nil) did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "OnReadyReplicaSafe") {
			t.Errorf("panic value = %v, want a message naming OnReadyReplicaSafe", r)
		}
	}()

	api := serverExtensionAPI{&Server{}}
	api.OnReadyReplicaSafe(nil)
}

// TestExtensionAPI_OnReadyReplicaSafe_FiresFromStartExtensionWorkers: a
// replica-safe hook fires from StartExtensionWorkers and not from
// StartBrainExtensionWorkers.
func TestExtensionAPI_OnReadyReplicaSafe_FiresFromStartExtensionWorkers(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	fired := make(chan struct{}, 1)
	api.OnReadyReplicaSafe(func(ctx context.Context) { fired <- struct{}{} })

	srv.StartBrainExtensionWorkers(context.Background())
	select {
	case <-fired:
		t.Fatal("a replica-safe hook must not fire from StartBrainExtensionWorkers")
	case <-time.After(100 * time.Millisecond):
	}

	srv.StartExtensionWorkers(context.Background())
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StartExtensionWorkers to fire the replica-safe hook")
	}
}

// TestServer_StartBrainExtensionWorkers_HookSeesWiredBus: Bus() is nil before
// SetEventBus, and inside a hook it returns the bus wired before the brain
// started.
func TestServer_StartBrainExtensionWorkers_HookSeesWiredBus(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}
	if got := api.Bus(); got != nil {
		t.Errorf("Bus() = %v, want nil before SetEventBus", got)
	}

	bus := eventbus.New()
	srv.SetEventBus(bus)

	got := make(chan *eventbus.Bus, 1)
	api.OnReady(func(ctx context.Context) { got <- api.Bus() })

	srv.StartBrainExtensionWorkers(context.Background())

	select {
	case b := <-got:
		if b != bus {
			t.Errorf("Bus() inside hook = %v, want the bus wired before the brain started (%v)", b, bus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hook to observe Bus()")
	}
}

// TestServer_StartBrainExtensionWorkers_CtxCancelObservableInHook: a hook runs on
// the context passed to StartBrainExtensionWorkers, which internal/app cancels on
// shutdown or lease demotion.
func TestServer_StartBrainExtensionWorkers_CtxCancelObservableInHook(t *testing.T) {
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
	srv.StartBrainExtensionWorkers(ctx)

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

// TestServer_StartBrainExtensionWorkers_RepeatCallsRestartHooks: a second call,
// as on lease re-acquisition, fires the hooks again. Unlike StartExtensionWorkers
// it has no once-guard; internal/app's startBrain prevents a double start.
func TestServer_StartBrainExtensionWorkers_RepeatCallsRestartHooks(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	calls := make(chan struct{}, 10)
	api.OnReady(func(ctx context.Context) { calls <- struct{}{} })

	srv.StartBrainExtensionWorkers(context.Background())
	srv.StartBrainExtensionWorkers(context.Background())

	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for call %d", i+1)
		}
	}
}

// TestServer_StartExtensionWorkers_SecondCallIsNoOp pins the idempotence
// guard on the replica-safe path: a second StartExtensionWorkers call
// (e.g. a test or caller double-invoking Run) must not fire hooks again.
func TestServer_StartExtensionWorkers_SecondCallIsNoOp(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	calls := make(chan struct{}, 10)
	api.OnReadyReplicaSafe(func(ctx context.Context) { calls <- struct{}{} })

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

// TestServer_StartBrainExtensionWorkers_MultipleHooksAllFire pins that
// every registered hook fires — the shape of two ee/ extensions each
// registering their own OnReady worker off the same install pass.
func TestServer_StartBrainExtensionWorkers_MultipleHooksAllFire(t *testing.T) {
	srv := &Server{}
	api := serverExtensionAPI{srv}

	fired1 := make(chan struct{}, 1)
	fired2 := make(chan struct{}, 1)
	api.OnReady(func(ctx context.Context) { fired1 <- struct{}{} })
	api.OnReady(func(ctx context.Context) { fired2 <- struct{}{} })

	srv.StartBrainExtensionWorkers(context.Background())

	for _, ch := range []chan struct{}{fired1, fired2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a registered hook to fire")
		}
	}
}
