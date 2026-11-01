package delegate

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
)

// sandboxMemoryMetrics backs the sandbox-memory instruments with a manual
// reader for the test's duration and returns a collector of what they hold.
func sandboxMemoryMetrics(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := sandboxMemory.Swap(newSandboxMemoryStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { sandboxMemory.Store(prev) })
	return func() metricdata.ResourceMetrics {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		return rm
	}
}

// counterAt is the value of the int64 counter name at exactly attrs, zero
// when no such point exists.
func counterAt(t *testing.T, rm metricdata.ResourceMetrics, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	want := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// peakHistogram is the peak-memory histogram's point for orgID, and whether
// there is one.
func peakHistogram(t *testing.T, rm metricdata.ResourceMetrics, orgID string) (metricdata.HistogramDataPoint[float64], bool) {
	t.Helper()
	want := attribute.NewSet(attribute.String("org.id", orgID))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "sandbox.peak_memory_ratio" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("sandbox.peak_memory_ratio is %T, want a float64 histogram", m.Data)
			}
			for _, dp := range h.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp, true
				}
			}
		}
	}
	return metricdata.HistogramDataPoint[float64]{}, false
}

func TestRecordMemoryKills_CountsEachKindPerOrg(t *testing.T) {
	collect := sandboxMemoryMetrics(t)
	recordMemoryKills("org-a", agentloop.MemoryKillBashBudget, 1)
	recordMemoryKills("org-a", agentloop.MemoryKillCeiling, 2)
	recordMemoryKills("org-a", agentloop.MemoryKillCeiling, 1)
	recordMemoryKills("org-b", agentloop.MemoryKillJailLost, 1)
	recordMemoryKills("org-b", agentloop.MemoryKillJailLost, 0)

	rm := collect()
	for _, tc := range []struct {
		org  string
		kind agentloop.MemoryKill
		want int64
	}{
		{"org-a", agentloop.MemoryKillBashBudget, 1},
		{"org-a", agentloop.MemoryKillCeiling, 3},
		{"org-a", agentloop.MemoryKillJailLost, 0},
		{"org-b", agentloop.MemoryKillJailLost, 1},
	} {
		got := counterAt(t, rm, "sandbox.memory_kills", attribute.String("kind", string(tc.kind)), attribute.String("org.id", tc.org))
		if got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.org, tc.kind, got, tc.want)
		}
	}
}

// Each jail of an engagement is observed against the ceiling it ran under,
// its own peak rather than the claim's running maximum, and a jail with no
// ceiling or no measured peak is not observed at all.
func TestRestartableJail_EachJailsPeakIsObservedAgainstItsCeiling(t *testing.T) {
	launch := func(limitMB int, actuals ...cell.Actuals) {
		t.Helper()
		c := &fakeCell{actuals: actuals}
		j, err := launchRestartableJail(context.Background(), c,
			cell.LaunchSpec{RunTree: "/tree", OrgID: "org", ClaimID: "claim", MemoryLimitMB: limitMB},
			func(jail cell.Jail, _ net.Conn) agentloop.ToolHost {
				return jailNamedToolHost{n: jail.(*fakeJail).n, closed: new(bool)}
			})
		if err != nil {
			t.Fatalf("launchRestartableJail: %v", err)
		}
		for range actuals[1:] {
			if err := j.toolHost().Restart(context.Background()); err != nil {
				t.Fatalf("Restart: %v", err)
			}
		}
		_ = j.Close()
	}

	t.Run("against the ceiling", func(t *testing.T) {
		collect := sandboxMemoryMetrics(t)
		launch(1000, cell.Actuals{PeakMemMB: intp(950)}, cell.Actuals{PeakMemMB: intp(300)})
		dp, ok := peakHistogram(t, collect(), "org")
		if !ok {
			t.Fatal("no peak observed")
		}
		if dp.Count != 2 || dp.Sum != 1.25 {
			t.Errorf("observed (count %d, sum %v), want each jail's own share: (2, 1.25)", dp.Count, dp.Sum)
		}
		// Bounds 0.25, 0.5, 0.75, 0.9, 1: 0.3 lands in the second bucket and
		// 0.95 in the fifth.
		if want := []uint64{0, 1, 0, 0, 1, 0}; len(dp.BucketCounts) != len(want) {
			t.Fatalf("buckets = %v, want %v", dp.BucketCounts, want)
		} else {
			for i := range want {
				if dp.BucketCounts[i] != want[i] {
					t.Errorf("buckets = %v, want %v", dp.BucketCounts, want)
					break
				}
			}
		}
	})
	t.Run("no ceiling, or no peak", func(t *testing.T) {
		collect := sandboxMemoryMetrics(t)
		launch(0, cell.Actuals{PeakMemMB: intp(950)})
		launch(1000, cell.Actuals{CPUUsec: i64p(10)})
		if dp, ok := peakHistogram(t, collect(), "org"); ok && dp.Count != 0 {
			t.Errorf("observed %d peaks, want none", dp.Count)
		}
	})
}

// An engagement's start creates its org's series at zero, so the first kill or
// park after them is a change increase() can see rather than a series that
// appears already counted.
func TestOpenSandboxMemorySeries_CreatesEachSeriesAtZero(t *testing.T) {
	collect := sandboxMemoryMetrics(t)
	openSandboxMemorySeries("org-a")
	openSandboxMemorySeries("org-a") // a later engagement adds nothing

	type point struct{ name, label, value string }
	got := map[point]int64{}
	for _, sm := range collect().ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if org, _ := dp.Attributes.Value("org.id"); org.AsString() != "org-a" {
					continue
				}
				for _, key := range []attribute.Key{"kind", "memory_limit"} {
					if v, ok := dp.Attributes.Value(key); ok {
						got[point{m.Name, string(key), v.AsString()}] = dp.Value
					}
				}
			}
		}
	}
	want := []point{
		{"sandbox.memory_kills", "kind", "bash_budget"},
		{"sandbox.memory_kills", "kind", "ceiling"},
		{"sandbox.memory_kills", "kind", "jail_lost"},
		{"conversations.sandbox_lost_parks", "memory_limit", "true"},
		{"conversations.sandbox_lost_parks", "memory_limit", "false"},
	}
	if len(got) != len(want) {
		t.Errorf("series = %v, want exactly %v", got, want)
	}
	for _, p := range want {
		if v, ok := got[p]; !ok || v != 0 {
			t.Errorf("%s{%s=%q} = (%d, present %v), want present at 0", p.name, p.label, p.value, v, ok)
		}
	}
}
