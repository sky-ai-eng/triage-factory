package domain

import (
	"encoding/json"
	"testing"
	"time"
)

// TestPendingPermissionDTOs_InputIsAlwaysAnObject pins the wire contract a
// client depends on structurally: it renders a prompt by indexing into `input`,
// so an omitted key or a null would be a render crash rather than a thinner
// prompt. A row whose input is unknown — nil because the stored blob was
// unreadable — must still serialize as an object.
func TestPendingPermissionDTOs_InputIsAlwaysAnObject(t *testing.T) {
	now := time.Now().UTC()
	dtos := PendingPermissionDTOs([]ConversationPermission{
		{ToolCallID: "toolu_known", ToolName: "Bash", Input: map[string]any{"command": "ls"}, RequestedAt: now},
		{ToolCallID: "toolu_unknown", ToolName: "Bash", Input: nil, RequestedAt: now},
	}, now)

	raw, err := json.Marshal(dtos)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(wire))
	}
	for i, w := range wire {
		input, ok := w["input"]
		if !ok {
			t.Fatalf("prompt %d omitted `input` — a client indexing it would throw", i)
		}
		if string(input) == "null" {
			t.Fatalf("prompt %d sent `input: null` — a client indexing it would throw", i)
		}
	}
	if string(wire[1]["input"]) != "{}" {
		t.Fatalf("unknown input = %s, want an empty object", wire[1]["input"])
	}
}

// TestPendingPermissionDTOs_TimeoutIsRemaining pins that the deadline on the
// wire is what's LEFT, not the window originally granted — a prompt picked up
// mid-window after a refresh must expire on time rather than get a fresh one.
func TestPendingPermissionDTOs_TimeoutIsRemaining(t *testing.T) {
	now := time.Now().UTC()
	in90s := now.Add(90 * time.Second)
	past := now.Add(-time.Second)

	dtos := PendingPermissionDTOs([]ConversationPermission{
		{ToolCallID: "toolu_live", ExpiresAt: &in90s},
		{ToolCallID: "toolu_lapsed", ExpiresAt: &past},
		{ToolCallID: "toolu_no_expiry"},
	}, now)

	if got := dtos[0].TimeoutMs; got < 89_000 || got > 90_000 {
		t.Fatalf("timeout_ms = %d, want ~90000 (the remaining window)", got)
	}
	// A deadline already past sends 0 rather than a negative, which the client
	// reads as "no usable deadline" and backstops with its own default instead
	// of dismissing the prompt instantly.
	if got := dtos[1].TimeoutMs; got != 0 {
		t.Fatalf("lapsed timeout_ms = %d, want 0", got)
	}
	if got := dtos[2].TimeoutMs; got != 0 {
		t.Fatalf("missing-expiry timeout_ms = %d, want 0", got)
	}
}
