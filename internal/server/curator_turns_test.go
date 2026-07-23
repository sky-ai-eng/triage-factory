package server

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSynthesizeCuratorTurns_DerivesCostAndTokensFromMessages pins the turn
// wire shape's accounting derivation: cost and the token breakdown are SUMs
// over each turn's message rows (the ledger — including the terminal lump on
// the turn's last row), while duration/turns come from the executing claim's
// telemetry. Two turns prove the per-turn windowing; the second turn's lump
// on its user row covers the nothing-streamed settlement.
func TestSynthesizeCuratorTurns_DerivesCostAndTokensFromMessages(t *testing.T) {
	fp := func(v float64) *float64 { return &v }
	ip := func(v int) *int { return &v }
	bp := func(v bool) *bool { return &v }
	released := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	claim1 := domain.Claim{
		ID: "cl-1", ConversationID: "conv", ExecutorID: "e",
		ClaimedAt: released.Add(-time.Minute), ReleasedAt: &released,
		Outcome: "completed", DurationMs: ip(1200), NumTurns: ip(3),
	}
	claim2 := domain.Claim{
		ID: "cl-2", ConversationID: "conv", ExecutorID: "e",
		ClaimedAt: released.Add(time.Minute), ReleasedAt: &released,
		Outcome: "failed", Error: "boom", DurationMs: ip(400), NumTurns: ip(1),
	}

	messages := []domain.AgentMessage{
		// Turn 1: user row + two assistant rows, the lump on the last one.
		{ID: 1, RunID: "conv", Role: "user", Subtype: "text", Content: "first turn",
			UserID: "u1", ClaimID: "cl-1", Delivered: bp(true)},
		{ID: 2, RunID: "conv", Role: "assistant", Subtype: "text", Content: "a",
			ClaimID: "cl-1", InputTokens: ip(100), OutputTokens: ip(20),
			CacheReadTokens: ip(1000), CacheCreationTokens: ip(7), Delivered: bp(true)},
		{ID: 3, RunID: "conv", Role: "assistant", Subtype: "text", Content: "b",
			ClaimID: "cl-1", InputTokens: ip(50), OutputTokens: ip(5),
			CacheReadTokens: ip(500), CacheCreationTokens: ip(3),
			CostUSD: fp(0.05), Delivered: bp(true)},
		// Turn 2: nothing streamed — the lump settled on the user row itself.
		{ID: 4, RunID: "conv", Role: "user", Subtype: "text", Content: "second turn",
			UserID: "u1", ClaimID: "cl-2", CostUSD: fp(0.01), Delivered: bp(true)},
	}

	turns := synthesizeCuratorTurns("proj", messages, []domain.Claim{claim1, claim2})
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}

	t1 := turns[0]
	if t1.Status != "done" {
		t.Errorf("turn1 status = %q, want done", t1.Status)
	}
	if t1.CostUSD != 0.05 {
		t.Errorf("turn1 cost = %v, want 0.05 (the SUM over its rows)", t1.CostUSD)
	}
	if t1.InputTokens != 150 || t1.OutputTokens != 25 ||
		t1.CacheReadTokens != 1500 || t1.CacheCreationTokens != 10 {
		t.Errorf("turn1 tokens = (%d,%d,%d,%d), want (150,25,1500,10)",
			t1.InputTokens, t1.OutputTokens, t1.CacheReadTokens, t1.CacheCreationTokens)
	}
	if t1.DurationMs != 1200 || t1.NumTurns != 3 {
		t.Errorf("turn1 telemetry = (%d,%d), want (1200,3) from the claim", t1.DurationMs, t1.NumTurns)
	}

	t2 := turns[1]
	if t2.Status != "failed" || t2.ErrorMsg != "boom" {
		t.Errorf("turn2 (status, error) = (%q, %q), want (failed, boom)", t2.Status, t2.ErrorMsg)
	}
	if t2.CostUSD != 0.01 {
		t.Errorf("turn2 cost = %v, want 0.01 (the user-row fallback lump)", t2.CostUSD)
	}
	if t2.InputTokens != 0 {
		t.Errorf("turn2 tokens leaked from turn1: input = %d, want 0", t2.InputTokens)
	}
	if t2.DurationMs != 400 || t2.NumTurns != 1 {
		t.Errorf("turn2 telemetry = (%d,%d), want (400,1)", t2.DurationMs, t2.NumTurns)
	}
}
