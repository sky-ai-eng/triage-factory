package delegate

import (
	"context"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/hostmem"
	"github.com/sky-ai-eng/triage-factory/internal/hoststat"
)

// Fleet telemetry sampler cadence + retention (TFAC-589, spec §8.2). One row
// per pod per minute is negligible write load and answers every time-series
// question the dashboard has; a ~30d reaper keeps the table bounded.
const (
	DefaultInstanceStatSampleInterval = time.Minute
	DefaultInstanceStatRetention      = 30 * 24 * time.Hour
	// instanceStatCPUWindow is how long the CPU sampler diffs /proc/stat. A
	// 1s window inside a 60s cadence is a negligible, accurate slice.
	instanceStatCPUWindow = time.Second
	// instanceStatReapInterval is the slow cadence the retention reaper runs
	// on — trimming is a plain age-range DELETE, cheap and idempotent, so it
	// need not be frequent or leader-gated (any pod may run it).
	instanceStatReapInterval = time.Hour
)

// TakeClaims returns the number of run claims since the last call and resets
// the counter to zero — the per-sample claim delta. Atomic swap, so a claim
// racing the read lands in the next interval, never lost.
func (s *Spawner) TakeClaims() int {
	return int(s.claimCount.Swap(0))
}

// recordSpawnMS appends one run's bring-up latency (claim → agent start) for
// the next sample's spawn_p50_ms. Bounded implicitly by the drain cadence:
// at most one entry per run started in the window.
func (s *Spawner) recordSpawnMS(d time.Duration) {
	ms := int(d.Milliseconds())
	if ms < 0 {
		ms = 0
	}
	s.spawnMu.Lock()
	s.spawnDurationsMS = append(s.spawnDurationsMS, ms)
	s.spawnMu.Unlock()
}

// TakeSpawnP50MS returns the median bring-up latency across the runs started
// since the last call and clears the buffer. ok is false when no run started
// in the window (the sampler leaves spawn_p50_ms NULL for that sample).
func (s *Spawner) TakeSpawnP50MS() (int, bool) {
	s.spawnMu.Lock()
	buf := s.spawnDurationsMS
	s.spawnDurationsMS = nil
	s.spawnMu.Unlock()
	if len(buf) == 0 {
		return 0, false
	}
	sort.Ints(buf)
	return buf[len(buf)/2], true
}

// RunInstanceStatSampler writes one instance_stats row per interval for this
// pod, and reaps rows past `retention` on a slower cadence. Every pod samples
// (control pods too — cpu/load/mem/oom are deployment-wide truth); the
// executor-only fields (active/queued/claims/spawn) stay NULL where
// reportCapacity is off. Nil store, empty identity, or a cancelled ctx make it
// a logged no-op / clean return, same discipline as RunInstanceHeartbeat.
func (s *Spawner) RunInstanceStatSampler(ctx context.Context, interval, retention time.Duration) {
	if s.instanceStats == nil {
		dispatchLog.Warn("instance stat sampler not started: no InstanceStatStore wired")
		return
	}
	id, _ := s.executorIdentity()
	if id == "" {
		dispatchLog.Warn("instance stat sampler not started: no executor identity set")
		return
	}
	if interval <= 0 {
		interval = DefaultInstanceStatSampleInterval
	}
	if retention <= 0 {
		retention = DefaultInstanceStatRetention
	}

	// Baseline the cumulative OOM counter so the first sample's delta is 0,
	// not the container's whole lifetime tally.
	lastOOM := hoststat.OOMKills()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reap := time.NewTicker(instanceStatReapInterval)
	defer reap.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastOOM = s.sampleInstanceStatsOnce(ctx, id, lastOOM)
		case <-reap.C:
			n, err := s.instanceStats.Reap(ctx, time.Now().Add(-retention))
			if err != nil {
				dispatchLog.Warn("instance stat reap failed", "error", err)
			} else if n > 0 {
				dispatchLog.Debug("instance stat reap", "deleted", n)
			}
		}
	}
}

// sampleInstanceStatsOnce writes one telemetry row and returns the new OOM
// baseline. CPU/load/mem/oom are read on every pod; the run-scoped fields only
// where this pod actually dispatches runs (reportCapacity).
func (s *Spawner) sampleInstanceStatsOnce(ctx context.Context, id string, lastOOM int) int {
	stat := domain.InstanceStat{InstanceID: id, At: time.Now().UTC()}

	// CPU + load average (blocks for the CPU window).
	hs := hoststat.Read(ctx, instanceStatCPUWindow)
	stat.CPUPct = hs.CPUPct
	stat.Load1 = hs.Load1

	// Available memory — cgroup-aware instance truth via the same probe the
	// dispatch guardrail reads (never a host-wide /proc/meminfo figure).
	s.mu.Lock()
	probe := s.memAvailMB
	s.mu.Unlock()
	if probe != nil {
		if avail := probe(); avail != hostmem.Unknown {
			stat.MemAvailableMB = &avail
		}
	}

	// OOM kills since the last sample (cumulative counter diffed to a delta).
	newLastOOM := lastOOM
	if cur := hoststat.OOMKills(); cur != hoststat.Unknown {
		newLastOOM = cur
		if lastOOM != hoststat.Unknown && cur >= lastOOM {
			d := cur - lastOOM
			stat.OOMKills = &d
		}
	}

	if s.reportCapacity.Load() {
		active := s.ActiveRuns()
		stat.ActiveRuns = &active
		claims := s.TakeClaims()
		stat.Claims = &claims
		if s.runQueue != nil {
			if q, err := s.runQueue.CountQueuedSystem(ctx); err != nil {
				dispatchLog.Warn("instance stat queue-depth read failed", "error", err)
			} else {
				stat.QueuedVisible = &q
			}
		}
		if p50, ok := s.TakeSpawnP50MS(); ok {
			stat.SpawnP50MS = &p50
		}
	}

	if err := s.instanceStats.Record(ctx, stat); err != nil {
		dispatchLog.Warn("instance stat record failed", "error", err)
	}
	return newLastOOM
}
