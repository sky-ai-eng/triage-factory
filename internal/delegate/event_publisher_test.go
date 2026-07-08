package delegate

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// fakeEventPublisher captures every published event so tests can assert
// the spawner's bus-mirror choke points (TFAC-592) without a real
// eventbus.Bus.
type fakeEventPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (f *fakeEventPublisher) Publish(evt domain.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
}

func (f *fakeEventPublisher) eventsCopy() []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Event, len(f.events))
	copy(out, f.events)
	return out
}

func decodeRunStatus(t *testing.T, raw string) events.SystemRunStatusMetadata {
	t.Helper()
	var meta events.SystemRunStatusMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode SystemRunStatusMetadata: %v", err)
	}
	return meta
}

func decodeRunActivity(t *testing.T, raw string) events.SystemRunActivityMetadata {
	t.Helper()
	var meta events.SystemRunActivityMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode SystemRunActivityMetadata: %v", err)
	}
	return meta
}

// TestBroadcastRunUpdate_PublishesRunStatus pins the status-flip choke
// point: broadcastRunUpdate mirrors onto the bus alongside (not gated by)
// the websocket broadcast — wsHub is nil here, matching most of this
// package's test fixtures, and the publish must still fire.
func TestBroadcastRunUpdate_PublishesRunStatus(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastRunUpdate("org-a", "run-1", "running")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	evt := got[0]
	if evt.EventType != domain.EventSystemRunStatus {
		t.Errorf("event_type = %q, want %q", evt.EventType, domain.EventSystemRunStatus)
	}
	if evt.OrgID != "org-a" {
		t.Errorf("org_id = %q, want org-a", evt.OrgID)
	}
	meta := decodeRunStatus(t, evt.MetadataJSON)
	if meta.RunID != "run-1" || meta.Status != "running" {
		t.Errorf("metadata = %+v, want RunID=run-1 Status=running", meta)
	}
	if meta.FailureKind != "" {
		t.Errorf("FailureKind = %q, want empty on a non-failure flip", meta.FailureKind)
	}
}

// TestBroadcastRunFailed_PublishesFailureKind pins broadcastRunFailed's
// failure arm: the published metadata carries FailureKind when the kind
// is classified, and omits it (empty) for the unclassified default.
func TestBroadcastRunFailed_PublishesFailureKind(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastRunFailed("org-a", "run-1", domain.RunFailureMemoryLimit)

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	meta := decodeRunStatus(t, got[0].MetadataJSON)
	if meta.Status != "failed" {
		t.Errorf("Status = %q, want failed", meta.Status)
	}
	if meta.FailureKind != string(domain.RunFailureMemoryLimit) {
		t.Errorf("FailureKind = %q, want %q", meta.FailureKind, domain.RunFailureMemoryLimit)
	}
}

// TestBroadcastRunFailed_UnclassifiedOmitsFailureKind mirrors the
// websocket arm's "key omitted, not sent empty" contract on the bus side.
func TestBroadcastRunFailed_UnclassifiedOmitsFailureKind(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastRunFailed("org-a", "run-1", domain.RunFailureUnclassified)

	got := pub.eventsCopy()
	meta := decodeRunStatus(t, got[0].MetadataJSON)
	if meta.FailureKind != "" {
		t.Errorf("FailureKind = %q, want empty for unclassified", meta.FailureKind)
	}
}

// TestBroadcastMessage_ToolUsePublishesActivity pins the volume-bounding
// rule: only Subtype=="tool_use" publishes system:run:activity, and the
// tool's Description resolves from Input["description"] when present.
func TestBroadcastMessage_ToolUsePublishesActivity(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastMessage("org-a", "run-1", &domain.AgentMessage{
		Subtype: "tool_use",
		ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "Bash", Input: map[string]any{"description": "run tests", "command": "go test ./..."}},
			{ID: "2", Name: "Read", Input: map[string]any{"file_path": "/x"}},
		},
	})

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	evt := got[0]
	if evt.EventType != domain.EventSystemRunActivity {
		t.Errorf("event_type = %q, want %q", evt.EventType, domain.EventSystemRunActivity)
	}
	meta := decodeRunActivity(t, evt.MetadataJSON)
	if meta.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", meta.RunID)
	}
	if len(meta.Tools) != 2 {
		t.Fatalf("Tools = %+v, want 2 entries", meta.Tools)
	}
	if meta.Tools[0].Name != "Bash" || meta.Tools[0].Description != "run tests" {
		t.Errorf("Tools[0] = %+v, want Name=Bash Description=\"run tests\"", meta.Tools[0])
	}
	if meta.Tools[1].Name != "Read" || meta.Tools[1].Description != "" {
		t.Errorf("Tools[1] = %+v, want Name=Read Description=\"\" (no description input)", meta.Tools[1])
	}
}

// TestBroadcastMessage_NonToolUseSubtypesPublishNothing pins the noise
// filter: text/thinking messages never reach the bus.
func TestBroadcastMessage_NonToolUseSubtypesPublishNothing(t *testing.T) {
	for _, subtype := range []string{"text", "thinking", "tool"} {
		t.Run(subtype, func(t *testing.T) {
			s := NewSpawner(nil, db.Stores{}, nil, nil, "")
			pub := &fakeEventPublisher{}
			s.SetEventPublisher(pub)

			s.broadcastMessage("org-a", "run-1", &domain.AgentMessage{Subtype: subtype})

			if got := pub.eventsCopy(); len(got) != 0 {
				t.Fatalf("subtype %q published %d events, want 0", subtype, len(got))
			}
		})
	}
}

// TestBroadcast_NilPublisher_NoPanic pins the nil-guard: with no
// publisher wired (the default), every broadcast helper is a pure
// websocket no-op — no panic, nothing published.
func TestBroadcast_NilPublisher_NoPanic(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	s.broadcastRunUpdate("org-a", "run-1", "running")
	s.broadcastRunFailed("org-a", "run-1", domain.RunFailureMemoryLimit)
	s.broadcastMessage("org-a", "run-1", &domain.AgentMessage{
		Subtype:   "tool_use",
		ToolCalls: []domain.ToolCall{{ID: "1", Name: "Bash"}},
	})
}
