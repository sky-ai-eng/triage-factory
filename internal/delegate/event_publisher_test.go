package delegate

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
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

func decodeConversationStatus(t *testing.T, raw string) events.SystemConversationStatusMetadata {
	t.Helper()
	var meta events.SystemConversationStatusMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode SystemConversationStatusMetadata: %v", err)
	}
	return meta
}

func decodeConversationActivity(t *testing.T, raw string) events.SystemConversationActivityMetadata {
	t.Helper()
	var meta events.SystemConversationActivityMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode SystemConversationActivityMetadata: %v", err)
	}
	return meta
}

// TestBroadcastRunUpdate_PublishesRunStatus pins the status-flip choke
// point: broadcastConversationUpdate mirrors onto the bus alongside (not gated by)
// the websocket broadcast — wsHub is nil here, matching most of this
// package's test fixtures, and the publish must still fire.
func TestBroadcastRunUpdate_PublishesRunStatus(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastConversationUpdate("org-a", "run-1", "running")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	evt := got[0]
	if evt.EventType != domain.EventSystemConversationStatus {
		t.Errorf("event_type = %q, want %q", evt.EventType, domain.EventSystemConversationStatus)
	}
	if evt.OrgID != "org-a" {
		t.Errorf("org_id = %q, want org-a", evt.OrgID)
	}
	meta := decodeConversationStatus(t, evt.MetadataJSON)
	if meta.ConversationID != "run-1" || meta.Status != "running" {
		t.Errorf("metadata = %+v, want ConversationID=run-1 Status=running", meta)
	}
	if meta.FailureKind != "" {
		t.Errorf("FailureKind = %q, want empty on a non-failure flip", meta.FailureKind)
	}
}

// TestBroadcastRunFailed_PublishesFailureKind pins broadcastConversationFailed's
// failure arm: the published metadata carries FailureKind when the kind
// is classified, and omits it (empty) for the unclassified default.
func TestBroadcastRunFailed_PublishesFailureKind(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastConversationFailed("org-a", "run-1", domain.ConversationFailureMemoryLimit)

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	meta := decodeConversationStatus(t, got[0].MetadataJSON)
	if meta.Status != "failed" {
		t.Errorf("Status = %q, want failed", meta.Status)
	}
	if meta.FailureKind != string(domain.ConversationFailureMemoryLimit) {
		t.Errorf("FailureKind = %q, want %q", meta.FailureKind, domain.ConversationFailureMemoryLimit)
	}
}

// TestBroadcastRunFailed_UnclassifiedOmitsFailureKind mirrors the
// websocket arm's "key omitted, not sent empty" contract on the bus side.
func TestBroadcastRunFailed_UnclassifiedOmitsFailureKind(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastConversationFailed("org-a", "run-1", domain.ConversationFailureUnclassified)

	got := pub.eventsCopy()
	meta := decodeConversationStatus(t, got[0].MetadataJSON)
	if meta.FailureKind != "" {
		t.Errorf("FailureKind = %q, want empty for unclassified", meta.FailureKind)
	}
}

// TestBroadcastMessage_AssistantToolCallsPublishActivity pins the
// volume-bounding rule: a tool-calling assistant turn publishes
// system:conversation:activity, one event for the whole batch, and the tool's
// Description resolves from Input["description"] when present.
//
// The row is built the way a producer builds one — role and tool calls, blank
// subtype — which is the whole contract. A fixture that stamps a subtype the
// producers do not write would pass against a gate nothing can satisfy.
func TestBroadcastMessage_AssistantToolCallsPublishActivity(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	s.broadcastMessage("org-a", "run-1", &domain.Message{
		Role: "assistant",
		ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "Bash", Input: map[string]any{"description": "run tests", "command": "go test ./..."}},
			{ID: "2", Name: "Read", Input: map[string]any{"file_path": "/x"}},
		},
	})

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1 (one per assistant turn, not per call)", len(got))
	}
	evt := got[0]
	if evt.EventType != domain.EventSystemConversationActivity {
		t.Errorf("event_type = %q, want %q", evt.EventType, domain.EventSystemConversationActivity)
	}
	meta := decodeConversationActivity(t, evt.MetadataJSON)
	if meta.ConversationID != "run-1" {
		t.Errorf("ConversationID = %q, want run-1", meta.ConversationID)
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

// TestBroadcastMessage_NativeProducerRowPublishesActivity feeds the publisher
// exactly what the native runtime persists: inference.MessageToRow's output
// for a provider completion carrying a tool call. It is the guard the
// hand-built fixture above cannot be — it fails if the mapper ever stops
// producing a row this gate accepts, rather than agreeing with a fixture.
func TestBroadcastMessage_NativeProducerRowPublishesActivity(t *testing.T) {
	name, id, fnType := "bash", "call-1", "function"
	row, err := inference.MessageToRow(schemas.ChatMessage{
		Role: schemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &schemas.ChatAssistantMessage{
			ToolCalls: []schemas.ChatAssistantMessageToolCall{{
				Index: 0,
				Type:  &fnType,
				ID:    &id,
				Function: schemas.ChatAssistantMessageToolCallFunction{
					Name:      &name,
					Arguments: `{"command":"go test ./..."}`,
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("MessageToRow: %v", err)
	}

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)
	s.broadcastMessage("org-a", "run-1", &row)

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("a native tool-call row published %d events, want 1", len(got))
	}
	meta := decodeConversationActivity(t, got[0].MetadataJSON)
	if len(meta.Tools) != 1 || meta.Tools[0].Name != "bash" {
		t.Errorf("Tools = %+v, want one entry named bash", meta.Tools)
	}
}

// TestBroadcastMessage_SDKProducerRowPublishesActivity is the native guard's
// counterpart for the SDK runtime: a real tool_use stream through the parser,
// whose flushed row goes to the publisher untouched.
func TestBroadcastMessage_SDKProducerRowPublishesActivity(t *testing.T) {
	st := agentproc.NewStreamState()
	st.ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"s"}`), "t")
	st.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"description":"run tests","command":"go test ./..."}}]}}`), "t")
	flushed, _ := st.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[]}}`), "t")
	if len(flushed) != 1 {
		t.Fatalf("parser flushed %d rows, want 1", len(flushed))
	}

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)
	s.broadcastMessage("org-a", "run-1", flushed[0])

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("an SDK tool-call row published %d events, want 1", len(got))
	}
	meta := decodeConversationActivity(t, got[0].MetadataJSON)
	if len(meta.Tools) != 1 || meta.Tools[0].Name != "Bash" || meta.Tools[0].Description != "run tests" {
		t.Errorf("Tools = %+v, want one Bash entry described \"run tests\"", meta.Tools)
	}
}

// TestBroadcastMessage_NonToolRowsPublishNothing pins the noise filter from
// the other side: a turn that called no tool, and a row that is not an
// assistant turn at all, stay off the bus. The tool-result arm matters most —
// it carries the answer to a call that already published, and publishing it
// again would double every tool the indicator shows.
func TestBroadcastMessage_NonToolRowsPublishNothing(t *testing.T) {
	cases := []struct {
		name string
		msg  domain.Message
	}{
		{"assistant text only", domain.Message{Role: "assistant", Content: "here is what I found"}},
		{"assistant reasoning only", domain.Message{Role: "assistant", Reasoning: []domain.ReasoningDetail{{Text: "thinking"}}}},
		{"tool result", domain.Message{Role: "tool", ToolCallID: "call-1", Content: "ok"}},
		{"user message", domain.Message{Role: "user", Content: "please fix the flake"}},
		{"stop note", domain.Message{Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: "stopped"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSpawner(nil, db.Stores{}, nil, nil, "")
			pub := &fakeEventPublisher{}
			s.SetEventPublisher(pub)

			msg := tc.msg
			s.broadcastMessage("org-a", "run-1", &msg)

			if got := pub.eventsCopy(); len(got) != 0 {
				t.Fatalf("published %d events, want 0", len(got))
			}
		})
	}
}

// TestBroadcast_NilPublisher_NoPanic pins the nil-guard: with no
// publisher wired (the default), every broadcast helper is a pure
// websocket no-op — no panic, nothing published.
func TestBroadcast_NilPublisher_NoPanic(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	s.broadcastConversationUpdate("org-a", "run-1", "running")
	s.broadcastConversationFailed("org-a", "run-1", domain.ConversationFailureMemoryLimit)
	s.broadcastMessage("org-a", "run-1", &domain.Message{
		Role:      "assistant",
		ToolCalls: []domain.ToolCall{{ID: "1", Name: "Bash"}},
	})
}
