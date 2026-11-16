package factory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A factory card's run carries the same snake_case keys as the conversation
// routes.
func TestFactoryRunSummaryKeysAreSnakeCase(t *testing.T) {
	at := time.Unix(1, 0).UTC()
	cost, ms, turns := 0.5, 10, 2
	b, err := json.Marshal(toFactoryRunSummary(domain.Conversation{
		ID:           "c1",
		StartedAt:    at,
		CompletedAt:  &at,
		TotalCostUSD: &cost,
		DurationMs:   &ms,
		NumTurns:     &turns,
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key := range wire {
		if key != strings.ToLower(key) {
			t.Errorf("key %q holds an uppercase letter", key)
		}
	}
}
