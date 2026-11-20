package domain

import (
	"encoding/json"
	"testing"
)

// wireKeys round-trips a DTO through JSON so the assertions below read the
// decoded document rather than the encoder's formatting of a float.
func wireKeys(t *testing.T, dto MessageDTO) map[string]any {
	t.Helper()
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return wire
}

// A live surface accumulates a conversation's spend from the streamed rows between reads
// of the conversation's SUM, so the per-row stamp has to survive the wire —
// including the difference between "no stamp" and "settled at zero", which a
// non-pointer field would collapse.
func TestMessageDTOCostStamp(t *testing.T) {
	t.Run("a stamped row carries its dollars", func(t *testing.T) {
		cost := 0.0412
		dto := Message{ID: 7, CostUSD: &cost}.ToDTO()
		if dto.CostUSD == nil || *dto.CostUSD != cost {
			t.Fatalf("CostUSD = %v, want %v", dto.CostUSD, cost)
		}
		got, ok := wireKeys(t, dto)["cost_usd"]
		if !ok {
			t.Fatal("wire shape missing cost_usd")
		}
		if got != cost {
			t.Errorf("wire cost_usd = %v, want %v", got, cost)
		}
	})

	t.Run("a free settlement row is not the same as an unstamped one", func(t *testing.T) {
		zero := 0.0
		got, ok := wireKeys(t, Message{ID: 7, CostUSD: &zero}.ToDTO())["cost_usd"]
		if !ok {
			t.Fatal("a zero stamp must stay on the wire")
		}
		if got != zero {
			t.Errorf("wire cost_usd = %v, want 0", got)
		}

		if _, ok := wireKeys(t, Message{ID: 7}.ToDTO())["cost_usd"]; ok {
			t.Error("an unstamped row must omit the key")
		}
	})
}

// The tools fingerprint is the native loop's own record, so it stays off the
// wire while the rest of a row's metadata crosses it, and the row keeps it.
func TestMessageDTOLeavesOutTheToolsFingerprint(t *testing.T) {
	row := Message{ID: 7, Metadata: map[string]any{
		MessageMetadataToolsFingerprint: "abc",
		"compaction_path":               "forced",
	}}
	metadata, _ := wireKeys(t, row.ToDTO())["metadata"].(map[string]any)
	if _, ok := metadata[MessageMetadataToolsFingerprint]; ok {
		t.Error("the wire shape carries the tools fingerprint")
	}
	if metadata["compaction_path"] != "forced" {
		t.Errorf("wire metadata = %v, want the other keys kept", metadata)
	}
	if row.Metadata[MessageMetadataToolsFingerprint] != "abc" {
		t.Error("projecting the row removed the fingerprint from the row itself")
	}

	alone := Message{ID: 8, Metadata: map[string]any{MessageMetadataToolsFingerprint: "abc"}}
	if _, ok := wireKeys(t, alone.ToDTO())["metadata"]; ok {
		t.Error("a row whose only metadata is the fingerprint must omit the key")
	}
}

// A system update's parts are what the next claim compares with, and its
// content already says what changed, so they stay off the wire too.
func TestMessageDTOLeavesOutTheSystemParts(t *testing.T) {
	row := Message{ID: 9, Role: "system", Subtype: MessageSubtypeSystemUpdate, Metadata: map[string]any{
		MessageMetadataSystemParts: map[string]any{"shared": []any{}},
		"compaction_path":          "forced",
	}}
	metadata, _ := wireKeys(t, row.ToDTO())["metadata"].(map[string]any)
	if _, ok := metadata[MessageMetadataSystemParts]; ok {
		t.Error("the wire shape carries a system update's parts")
	}
	if metadata["compaction_path"] != "forced" {
		t.Errorf("wire metadata = %v, want the other keys kept", metadata)
	}
	if _, ok := row.Metadata[MessageMetadataSystemParts]; !ok {
		t.Error("projecting the row removed the parts from the row itself")
	}

	both := Message{ID: 10, Metadata: map[string]any{
		MessageMetadataSystemParts:      map[string]any{},
		MessageMetadataToolsFingerprint: "abc",
	}}
	if _, ok := wireKeys(t, both.ToDTO())["metadata"]; ok {
		t.Error("a row whose only metadata is runtime-only must omit the key")
	}
}
