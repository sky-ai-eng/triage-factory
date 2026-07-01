package server

import (
	"bytes"
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
