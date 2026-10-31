package wsbackplane

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestWireEvent_RoundTripPreservesOrgAndUser is the whole reason
// wireEvent exists: websocket.Event hides OrgID/UserID from its own
// JSON tags (browser-facing wire shape), but the backplane needs them
// preserved across the Postgres hop so the receiving pod's DeliverRemote
// can reapply the same per-connection scope filter Broadcast applied
// locally.
func TestWireEvent_RoundTripPreservesOrgAndUser(t *testing.T) {
	orig := websocket.Event{
		Type:           "message",
		ConversationID: "run-1",
		OrgID:          "org-1",
		UserID:         "user-1",
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
	if got.Type != orig.Type || got.ConversationID != orig.ConversationID || got.OrgID != orig.OrgID || got.UserID != orig.UserID {
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
