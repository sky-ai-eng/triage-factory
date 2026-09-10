package agentproc

import "testing"

// TestMergeResult_AccountingIsRunningTotal pins the fold's reading of the
// wire: every per-turn result carries the process's running totals, so the
// fold takes the newest figure rather than adding — a three-turn process
// reporting 0.10, 0.25, 0.40 spent 0.40, not 0.75.
func TestMergeResult_AccountingIsRunningTotal(t *testing.T) {
	turns := []*Result{
		{CostUSD: 0.10, DurationMs: 1000, NumTurns: 1, Result: "first"},
		{CostUSD: 0.25, DurationMs: 2500, NumTurns: 2, Result: "second"},
		{CostUSD: 0.40, DurationMs: 4000, NumTurns: 3, Result: "third"},
	}
	merged := turns[0]
	for _, r := range turns[1:] {
		merged = MergeResult(merged, r)
	}
	if merged.CostUSD != 0.40 || merged.DurationMs != 4000 || merged.NumTurns != 3 {
		t.Errorf("accounting must be the newest running total: cost=%v dur=%v turns=%v", merged.CostUSD, merged.DurationMs, merged.NumTurns)
	}
	if merged.Result != "third" {
		t.Errorf("result text = %q, want the newest turn's", merged.Result)
	}
	if turns[0].CostUSD != 0.10 {
		t.Error("MergeResult must not mutate its inputs")
	}
}

// TestMergeResult_DispositionIsSticky: an error or an interrupt seen on any
// turn stays on the fold, and a later turn with no text or subtype does not
// blank what an earlier one reported.
func TestMergeResult_DispositionIsSticky(t *testing.T) {
	base := &Result{IsError: true, Interrupted: true, Subtype: "error_during_execution", Result: "paused"}
	merged := MergeResult(base, &Result{CostUSD: 0.5})
	if !merged.IsError || !merged.Interrupted {
		t.Error("IsError / Interrupted must stay set across turns")
	}
	if merged.Subtype != "error_during_execution" || merged.Result != "paused" {
		t.Errorf("empty later fields must not blank earlier ones: subtype=%q result=%q", merged.Subtype, merged.Result)
	}
}
