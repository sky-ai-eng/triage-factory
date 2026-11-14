package wsbackplane

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestWireEvent_RoundTripPreservesRouting is the whole reason wireEvent
// exists: websocket.Event hides OrgID, UserID and UserIDs from its own
// JSON tags (browser-facing wire shape), but the backplane needs them
// preserved across the Postgres hop so the receiving pod's DeliverRemote
// can reapply the same per-connection scope filter Broadcast applied
// locally. The event carries both a user and a set only to check each
// field; Broadcast never publishes that shape.
func TestWireEvent_RoundTripPreservesRouting(t *testing.T) {
	orig := websocket.Event{
		Type:           "message",
		ConversationID: "run-1",
		OrgID:          "org-1",
		UserID:         "user-1",
		UserIDs:        []string{"user-2", "user-3"},
		Data:           map[string]any{"text": "hello"},
	}
	we, err := newWireEvent(orig)
	if err != nil {
		t.Fatalf("newWireEvent: %v", err)
	}
	raw, err := json.Marshal(we)
	if err != nil {
		t.Fatalf("marshal wireEvent: %v", err)
	}
	var decoded wireEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal wireEvent: %v", err)
	}
	got := decoded.toEvent()
	if got.Type != orig.Type || got.ConversationID != orig.ConversationID || got.OrgID != orig.OrgID || got.UserID != orig.UserID ||
		!slices.Equal(got.UserIDs, orig.UserIDs) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, orig)
	}
	data, ok := got.Data.(map[string]any)
	if !ok || data["text"] != "hello" {
		t.Fatalf("Data not preserved across round trip: %+v", got.Data)
	}
}

// TestWireEvent_MalformedDataDegradesGracefully asserts a corrupt Data
// payload doesn't sink the whole event — Type/OrgID/ConversationID still route.
func TestWireEvent_MalformedDataDegradesGracefully(t *testing.T) {
	we := wireEvent{Type: "task_updated", OrgID: "org-1", Data: json.RawMessage(`{not valid json`)}
	got := we.toEvent()
	if got.Type != "task_updated" || got.OrgID != "org-1" {
		t.Fatalf("malformed Data corrupted the rest of the event: %+v", got)
	}
	if got.Data != nil {
		t.Fatalf("Data should degrade to nil on unmarshal failure, got %v", got.Data)
	}
}

// TestEnvelope_ReadersOfConversationUnderKeysOfItsOwn pins that an event
// marked for its conversation's readers sets neither key a pod without the
// marker reads, inline or by outbox reference, so such a pod drops it
// rather than delivering it to the whole org. An unmarked event still uses
// those keys.
func TestEnvelope_ReadersOfConversationUnderKeysOfItsOwn(t *testing.T) {
	// The tf_ws envelope as a pod without the marker decodes it.
	type markerlessEnvelope struct {
		Event    *json.RawMessage `json:"event"`
		OutboxID int64            `json:"outbox_id"`
	}
	decode := func(t *testing.T, env wsEnvelope) markerlessEnvelope {
		t.Helper()
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		var got markerlessEnvelope
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		return got
	}
	we := &wireEvent{Type: "message", OrgID: "org-1", ConversationID: "conv-1", Data: json.RawMessage(`{}`)}

	if got := decode(t, inlineEnvelope("executor-pod", we, true)); got.Event != nil || got.OutboxID != 0 {
		t.Errorf("marked inline envelope = %+v; want neither event nor outbox_id", got)
	}
	if got := decode(t, outboxEnvelope("executor-pod", 7, true)); got.Event != nil || got.OutboxID != 0 {
		t.Errorf("marked outbox envelope = %+v; want neither event nor outbox_id", got)
	}
	if got := decode(t, inlineEnvelope("control-pod", we, false)); got.Event == nil {
		t.Error("unmarked inline envelope has no event")
	}
	if got := decode(t, outboxEnvelope("control-pod", 7, false)); got.OutboxID != 7 {
		t.Errorf("unmarked outbox envelope outbox_id = %d, want 7", got.OutboxID)
	}
}
