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
