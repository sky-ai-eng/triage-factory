package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstanceStatStoreFactory hands the conformance suite a wired store.
// instance_stats has no FK chain (instance_id is a plain text column, the
// instances-family convention), so there is nothing to seed.
type InstanceStatStoreFactory func(t *testing.T) db.InstanceStatStore

func iptr(v int) *int         { return &v }
func fptr(v float64) *float64 { return &v }

// RunInstanceStatStoreConformance covers the contract every backend must hold:
// record round-trips including nullable fields; ListSince honors the window and
// orders ascending by `at`; the (instance_id, at) PK makes a duplicate insert a
// no-op; Reap deletes only rows older than the cutoff and reports the count.
func RunInstanceStatStoreConformance(t *testing.T, mk InstanceStatStoreFactory) {
	t.Helper()
	ctx := context.Background()
	// A fixed clock so window math is exact across backends (Date.now is not
	// available in tests here; use a literal reference time).
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	t.Run("record_and_list_roundtrip_with_nulls", func(t *testing.T) {
		store := mk(t)
		full := domain.InstanceStat{
			InstanceID: "exec-1", At: base,
			ActiveRuns: iptr(3), QueuedVisible: iptr(7), MemAvailableMB: iptr(40960),
			CPUPct: fptr(42.5), Load1: fptr(1.25), Claims: iptr(9), SpawnP50MS: iptr(820), OOMKills: iptr(0),
		}
		// A partial (control-pod-shaped) sample: only cpu/load/mem populated.
		partial := domain.InstanceStat{
			InstanceID: "ctrl-1", At: base.Add(30 * time.Second),
			MemAvailableMB: iptr(2048), CPUPct: fptr(5.0), Load1: fptr(0.1),
		}
		if err := store.Record(ctx, full); err != nil {
			t.Fatalf("record full: %v", err)
		}
		if err := store.Record(ctx, partial); err != nil {
			t.Fatalf("record partial: %v", err)
		}

		got, err := store.ListSince(ctx, base.Add(-time.Minute))
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 samples, got %d: %+v", len(got), got)
		}
		// Ascending by at: full (base) before partial (base+30s).
		if got[0].InstanceID != "exec-1" || got[1].InstanceID != "ctrl-1" {
			t.Fatalf("not ordered by at ascending: %+v", got)
		}
		g := got[0]
		if g.ActiveRuns == nil || *g.ActiveRuns != 3 || g.Claims == nil || *g.Claims != 9 {
			t.Fatalf("int fields lost: %+v", g)
		}
		if g.CPUPct == nil || *g.CPUPct != 42.5 || g.Load1 == nil || *g.Load1 != 1.25 {
			t.Fatalf("float fields lost: %+v", g)
		}
		p := got[1]
		if p.ActiveRuns != nil || p.QueuedVisible != nil || p.Claims != nil || p.SpawnP50MS != nil || p.OOMKills != nil {
			t.Fatalf("nil fields should scan to nil, not 0: %+v", p)
		}
		if p.MemAvailableMB == nil || *p.MemAvailableMB != 2048 {
			t.Fatalf("partial mem lost: %+v", p)
		}
	})

	t.Run("list_since_windows", func(t *testing.T) {
		store := mk(t)
		_ = store.Record(ctx, domain.InstanceStat{InstanceID: "e", At: base.Add(-2 * time.Hour)})
		_ = store.Record(ctx, domain.InstanceStat{InstanceID: "e", At: base})
		got, err := store.ListSince(ctx, base.Add(-time.Hour))
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || !got[0].At.Equal(base) {
			t.Fatalf("window filter wrong: %+v", got)
		}
	})

	t.Run("duplicate_pk_is_noop", func(t *testing.T) {
		store := mk(t)
		s1 := domain.InstanceStat{InstanceID: "e", At: base, ActiveRuns: iptr(1)}
		s2 := domain.InstanceStat{InstanceID: "e", At: base, ActiveRuns: iptr(99)}
		if err := store.Record(ctx, s1); err != nil {
			t.Fatalf("record 1: %v", err)
		}
		if err := store.Record(ctx, s2); err != nil {
			t.Fatalf("record dup must not error (ON CONFLICT DO NOTHING): %v", err)
		}
		got, _ := store.ListSince(ctx, base.Add(-time.Minute))
		if len(got) != 1 || got[0].ActiveRuns == nil || *got[0].ActiveRuns != 1 {
			t.Fatalf("duplicate must keep the first row: %+v", got)
		}
	})

	t.Run("reap_deletes_old_and_reports_count", func(t *testing.T) {
		store := mk(t)
		_ = store.Record(ctx, domain.InstanceStat{InstanceID: "e", At: base.Add(-48 * time.Hour)})
		_ = store.Record(ctx, domain.InstanceStat{InstanceID: "e", At: base.Add(-1 * time.Hour)})
		n, err := store.Reap(ctx, base.Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if n != 1 {
			t.Fatalf("reap should delete 1 old row, deleted %d", n)
		}
		got, _ := store.ListSince(ctx, base.Add(-72*time.Hour))
		if len(got) != 1 {
			t.Fatalf("only the recent row should survive: %+v", got)
		}
	})
}
