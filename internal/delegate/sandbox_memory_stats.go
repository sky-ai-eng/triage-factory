package delegate

import (
	"context"
	"strconv"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// sandboxMemory owns this package's sandbox-memory instruments. Package-level
// because a metric instrument is process-global by nature, and swapped
// atomically so a test can back it with a manual reader while an engagement
// may still be counting.
var sandboxMemory atomic.Pointer[sandboxMemoryStats]

func init() {
	sandboxMemory.Store(newSandboxMemoryStats(otel.GetMeterProvider()))
}

type sandboxMemoryStats struct {
	kills     metric.Int64Counter
	lostParks metric.Int64Counter
	peak      metric.Float64Histogram
}

// peakMemoryBuckets are the shares of the ceiling the peak histogram splits
// at. The top two are the ones read: a jail that peaked past 0.9 came within
// a tenth of its ceiling, and one at 1 reached it.
var peakMemoryBuckets = []float64{0.25, 0.5, 0.75, 0.9, 1}

// newSandboxMemoryStats builds the instruments against mp — production passes
// the global provider, which telemetry.Init installs. An instrument-creation
// error can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newSandboxMemoryStats(mp metric.MeterProvider) *sandboxMemoryStats {
	m := mp.Meter("internal/delegate")
	kills, err := m.Int64Counter("sandbox.memory_kills",
		metric.WithDescription("What memory ended in a native run's sandbox, by kind: bash_budget (a command over the per-command budget), "+
			"ceiling (a process the kernel killed at the sandbox's memory limit), jail_lost (a sandbox the limit ended)."))
	if err != nil {
		dispatchLog.Warn("sandbox memory kill counter setup failed", "error", err)
	}
	lostParks, err := m.Int64Counter("conversations.sandbox_lost_parks",
		metric.WithDescription("Conversations parked for a person after losing their sandbox as many times in a row as the jail budget allows, "+
			"by whether the memory limit ended the last one."))
	if err != nil {
		dispatchLog.Warn("sandbox-lost park counter setup failed", "error", err)
	}
	peak, err := m.Float64Histogram("sandbox.peak_memory_ratio",
		metric.WithDescription("Each native run sandbox's peak memory as a share of its memory limit, observed when the sandbox exits."),
		metric.WithExplicitBucketBoundaries(peakMemoryBuckets...))
	if err != nil {
		dispatchLog.Warn("sandbox peak memory histogram setup failed", "error", err)
	}
	return &sandboxMemoryStats{kills: kills, lostParks: lostParks, peak: peak}
}

// openSandboxMemorySeries creates orgID's kill and park series at zero, ahead
// of anything they count. A counter series otherwise appears at its first
// increment, already at 1, and increase() over a series that starts at 1
// reads 0 — so the first kill or park after an executor restart, or on a
// series that stops being scraped within the window, would never reach an
// alert. Called at each engagement's start, which is ahead of any kill or park
// that engagement counts.
func openSandboxMemorySeries(orgID string) {
	stats := sandboxMemory.Load()
	if stats == nil {
		return
	}
	org := telemetry.OrgID(orgID)
	if stats.kills != nil {
		for _, kind := range []agentloop.MemoryKill{agentloop.MemoryKillBashBudget, agentloop.MemoryKillCeiling, agentloop.MemoryKillJailLost} {
			stats.kills.Add(context.Background(), 0, metric.WithAttributes(attribute.String("kind", string(kind)), org))
		}
	}
	if stats.lostParks != nil {
		for _, memoryLimit := range []bool{true, false} {
			stats.lostParks.Add(context.Background(), 0,
				metric.WithAttributes(attribute.String("memory_limit", strconv.FormatBool(memoryLimit)), org))
		}
	}
}

// recordMemoryKills counts n kills of kind in one of orgID's sandboxes.
func recordMemoryKills(orgID string, kind agentloop.MemoryKill, n int) {
	stats := sandboxMemory.Load()
	if stats == nil || stats.kills == nil || n <= 0 {
		return
	}
	stats.kills.Add(context.Background(), int64(n),
		metric.WithAttributes(attribute.String("kind", string(kind)), telemetry.OrgID(orgID)))
}

// recordSandboxLostPark counts one of orgID's conversations parked because its
// sandbox kept being lost, memoryLimit saying whether the ceiling ended the
// last one. Called only for a park that flipped the row open, so a fenced,
// failed or no-op one counts nothing.
func recordSandboxLostPark(orgID string, memoryLimit bool) {
	stats := sandboxMemory.Load()
	if stats == nil || stats.lostParks == nil {
		return
	}
	stats.lostParks.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("memory_limit", strconv.FormatBool(memoryLimit)), telemetry.OrgID(orgID)))
}

// recordPeakMemory observes one jail's peak memory against the ceiling it ran
// under. A jail with no ceiling has no share to observe, and one that measured
// no peak (a kernel without memory.peak) has nothing to say.
func recordPeakMemory(orgID string, peakMB *int, limitMB int) {
	stats := sandboxMemory.Load()
	if stats == nil || stats.peak == nil || peakMB == nil || limitMB <= 0 {
		return
	}
	stats.peak.Record(context.Background(), float64(*peakMB)/float64(limitMB),
		metric.WithAttributes(telemetry.OrgID(orgID)))
}
