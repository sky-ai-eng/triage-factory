package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// A live surface accumulates a run's spend from the streamed rows between reads
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
		b, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(b), `"cost_usd":0.0412`) {
			t.Errorf("wire shape missing the stamp: %s", b)
		}
	})

	t.Run("a free settlement row is not the same as an unstamped one", func(t *testing.T) {
		zero := 0.0
		b, err := json.Marshal(Message{ID: 7, CostUSD: &zero}.ToDTO())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(b), `"cost_usd":0`) {
			t.Errorf("a zero stamp must stay on the wire, got: %s", b)
		}

		b, err = json.Marshal(Message{ID: 7}.ToDTO())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "cost_usd") {
			t.Errorf("an unstamped row must omit the key, got: %s", b)
		}
	})
}
