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
	}, now, nil)

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
	}, now, nil)

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

// TestPendingPermissionDTOs_LiveWaitBeatsStoredExpiry is the state a system
// suspend leaves behind: the stored expiry, projected on the wall clock, is
// already past, while the process still waiting has most of its window left
// because its clock stopped during the sleep. The wire must carry the wait's
// remaining time. A prompt the reporting process doesn't hold falls back to
// the stored expiry, and a live wait already at its end sends 0 rather than
// a negative.
func TestPendingPermissionDTOs_LiveWaitBeatsStoredExpiry(t *testing.T) {
	now := time.Now().UTC()
	beforeTheSleep := now.Add(-10 * time.Minute)
	in30s := now.Add(30 * time.Second)

	dtos := PendingPermissionDTOs([]ConversationPermission{
		{ToolCallID: "toolu_slept", ExpiresAt: &beforeTheSleep},
		{ToolCallID: "toolu_not_held", ExpiresAt: &in30s},
		{ToolCallID: "toolu_ending", ExpiresAt: &in30s},
	}, now, map[string]time.Duration{
		"toolu_slept":  140 * time.Second,
		"toolu_ending": -time.Millisecond,
	})

	if got := dtos[0].TimeoutMs; got != 140_000 {
		t.Fatalf("slept timeout_ms = %d, want 140000 (the live wait, not the lapsed wall-clock expiry)", got)
	}
	if got := dtos[1].TimeoutMs; got < 29_000 || got > 30_000 {
		t.Fatalf("not-held timeout_ms = %d, want ~30000 (the stored expiry)", got)
	}
	if got := dtos[2].TimeoutMs; got != 0 {
		t.Fatalf("ending timeout_ms = %d, want 0", got)
	}
}
