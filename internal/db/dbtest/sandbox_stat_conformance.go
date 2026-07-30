package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SandboxStatStoreFactory hands the conformance suite a wired store.
// sandbox_stats has no FK chain (claim_id is a bare identifier, the
// telemetry-family convention), so there is nothing to seed.
type SandboxStatStoreFactory func(t *testing.T) db.SandboxStatStore

// Claim ids the suite writes against. Real uuids because the Postgres column
// is a uuid — a bare label would fail the bind there and pass on SQLite,
// which is exactly the dialect drift the shared suite exists to catch.
const (
	sandboxStatClaimA = "11111111-1111-4111-8111-111111111111"
	sandboxStatClaimB = "22222222-2222-4222-8222-222222222222"
)

func i64ptr(v int64) *int64 { return &v }

// RunSandboxStatStoreConformance covers the contract every backend must hold:
// a whole tick's batch round-trips including nullable metrics; ListForClaim is
// per-claim and ordered oldest-first; an empty batch touches nothing; a
// duplicate (claim_id, at) is ignored rather than erroring; and Reap deletes
// only rows past the cutoff and reports the count.
func RunSandboxStatStoreConformance(t *testing.T, mk SandboxStatStoreFactory) {
	t.Helper()
	ctx := context.Background()
	// A fixed clock so window math is exact across backends.
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	t.Run("batch_roundtrips_with_nulls", func(t *testing.T) {
		store := mk(t)
		// One tick, two live jails: a full sample and a partial one (only CPU
		// readable). Both share the tick's timestamp.
		err := store.RecordBatch(ctx, []domain.SandboxStat{
			{ClaimID: sandboxStatClaimA, At: base, MemCurrentMB: iptr(310), CPUUsecCum: i64ptr(1_200_000)},
			{ClaimID: sandboxStatClaimB, At: base, CPUUsecCum: i64ptr(9)},
		})
		if err != nil {
			t.Fatalf("record batch: %v", err)
		}

		gotA, err := store.ListForClaim(ctx, sandboxStatClaimA)
		if err != nil {
			t.Fatalf("list A: %v", err)
		}
		if len(gotA) != 1 {
			t.Fatalf("expected 1 sample for claim A, got %d: %+v", len(gotA), gotA)
		}
		a := gotA[0]
		if a.MemCurrentMB == nil || *a.MemCurrentMB != 310 {
			t.Fatalf("memory lost: %+v", a)
		}
		if a.CPUUsecCum == nil || *a.CPUUsecCum != 1_200_000 {
			t.Fatalf("cumulative cpu lost: %+v", a)
		}
		if !a.At.Equal(base) {
			t.Fatalf("at = %v, want %v", a.At, base)
		}

		gotB, err := store.ListForClaim(ctx, sandboxStatClaimB)
		if err != nil {
			t.Fatalf("list B: %v", err)
		}
		if len(gotB) != 1 {
			t.Fatalf("expected 1 sample for claim B, got %d: %+v", len(gotB), gotB)
		}
		if gotB[0].MemCurrentMB != nil {
			t.Fatalf("unread memory must scan to nil, not 0: %+v", gotB[0])
		}
		if gotB[0].CPUUsecCum == nil || *gotB[0].CPUUsecCum != 9 {
			t.Fatalf("partial sample's cpu lost: %+v", gotB[0])
		}
	})

	t.Run("series_is_per_claim_and_time_ordered", func(t *testing.T) {
		store := mk(t)
		// Three ticks for claim A, written out of order, plus one for claim B
		// that must not leak into A's series.
		for _, off := range []time.Duration{2 * time.Minute, 0, time.Minute} {
			cum := int64(off/time.Minute) * 500_000
			if err := store.RecordBatch(ctx, []domain.SandboxStat{
				{ClaimID: sandboxStatClaimA, At: base.Add(off), MemCurrentMB: iptr(200), CPUUsecCum: &cum},
			}); err != nil {
				t.Fatalf("record at +%v: %v", off, err)
			}
		}
		if err := store.RecordBatch(ctx, []domain.SandboxStat{
			{ClaimID: sandboxStatClaimB, At: base, MemCurrentMB: iptr(999)}}); err != nil {
			t.Fatalf("record other claim: %v", err)
		}

		got, err := store.ListForClaim(ctx, sandboxStatClaimA)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 samples for claim A, got %d: %+v", len(got), got)
		}
		for i, want := range []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute)} {
			if !got[i].At.Equal(want) {
				t.Fatalf("sample %d at = %v, want %v (series must be oldest-first)", i, got[i].At, want)
			}
		}
		// Cumulative CPU is what a consumer differences into a rate, so it has
		// to come back non-decreasing along the ordered series.
		for i := 1; i < len(got); i++ {
			if *got[i].CPUUsecCum < *got[i-1].CPUUsecCum {
				t.Fatalf("cumulative cpu went backwards: %+v", got)
			}
		}
	})

	t.Run("empty_batch_is_a_noop", func(t *testing.T) {
		store := mk(t)
		if err := store.RecordBatch(ctx, nil); err != nil {
			t.Fatalf("nil batch must be a no-op, got %v", err)
		}
		if err := store.RecordBatch(ctx, []domain.SandboxStat{}); err != nil {
			t.Fatalf("empty batch must be a no-op, got %v", err)
		}
	})

	t.Run("duplicate_pk_is_noop", func(t *testing.T) {
		store := mk(t)
		first := []domain.SandboxStat{{ClaimID: sandboxStatClaimA, At: base, MemCurrentMB: iptr(100)}}
		second := []domain.SandboxStat{{ClaimID: sandboxStatClaimA, At: base, MemCurrentMB: iptr(999)}}
		if err := store.RecordBatch(ctx, first); err != nil {
			t.Fatalf("record 1: %v", err)
		}
		if err := store.RecordBatch(ctx, second); err != nil {
			t.Fatalf("retried tick must not error (ON CONFLICT DO NOTHING): %v", err)
		}
		got, _ := store.ListForClaim(ctx, sandboxStatClaimA)
		if len(got) != 1 || got[0].MemCurrentMB == nil || *got[0].MemCurrentMB != 100 {
			t.Fatalf("duplicate must keep the first row: %+v", got)
		}
	})

	t.Run("unsampled_claim_has_no_series", func(t *testing.T) {
		store := mk(t)
		got, err := store.ListForClaim(ctx, sandboxStatClaimA)
		if err != nil {
			t.Fatalf("list of a never-sampled claim must not error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no samples, got %+v", got)
		}
	})

	t.Run("reap_deletes_past_retention_and_reports_count", func(t *testing.T) {
		store := mk(t)
		if err := store.RecordBatch(ctx, []domain.SandboxStat{
			{ClaimID: sandboxStatClaimA, At: base.Add(-48 * time.Hour), MemCurrentMB: iptr(10)},
			{ClaimID: sandboxStatClaimB, At: base.Add(-48 * time.Hour), MemCurrentMB: iptr(20)},
			{ClaimID: sandboxStatClaimA, At: base.Add(-time.Hour), MemCurrentMB: iptr(30)},
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		n, err := store.Reap(ctx, base.Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if n != 2 {
			t.Fatalf("reap should delete the 2 rows past retention, deleted %d", n)
		}
		gotA, _ := store.ListForClaim(ctx, sandboxStatClaimA)
		if len(gotA) != 1 || gotA[0].MemCurrentMB == nil || *gotA[0].MemCurrentMB != 30 {
			t.Fatalf("only the recent row should survive: %+v", gotA)
		}
		gotB, _ := store.ListForClaim(ctx, sandboxStatClaimB)
		if len(gotB) != 0 {
			t.Fatalf("claim B's aged-out row should be gone: %+v", gotB)
		}
	})
}
