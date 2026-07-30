package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// The per-sandbox resource series rides the instance-stat sampler's tick — the
// same interval, the same retention, the same reaper cadence. There is no
// second loop and no cadence knob of its own: if an operator retunes the
// instance sampler, the series retunes with it, which is the only behavior
// that can't drift into two policies for one family of telemetry.
//
// The two reads are silent, capability-free, and off the privsep hot path.
// cgroup v2 stat files are world-readable, so the unprivileged orchestrator
// reads them directly — the cap-broker's monopoly is on cgroup lifecycle and
// mutation, never observation, so nothing here crosses the broker.

// liveJailsFn and sampleJailFn are the sampler's two seams onto the live
// sandboxes. Package vars so a test can inject a stand-in registry and count
// how many cgroup reads a tick actually performs — the "idle costs nothing"
// property is only meaningful if it's observable.
var (
	liveJailsFn  = agentproc.LiveJails
	sampleJailFn = sandbox.SampleRunCgroup
)

// sampleSandboxStatsOnce appends one row per live jail: what each sandbox is
// holding right now (memory) and what it has spent in total (CPU). CPU is
// recorded cumulative rather than as a rate so a dropped tick widens the next
// interval instead of leaving a gap that reads as an idle sandbox.
//
// Every sandbox on a tick shares one timestamp, so a multi-sandbox read lines
// the series up without bucketing.
//
// Three ways this does nothing, all of them normal:
//
//   - No live jails — a control pod, or an idle executor. Zero cgroup reads,
//     zero rows, no connection touched.
//   - A jail that ended between the registry snapshot and its cgroup read. The
//     files are gone with the group, so the sample carries nothing and no row
//     is written for it. Silent: a warning here would fire once per finished
//     run forever.
//   - No store wired.
//
// Best-effort as a whole, and independent of the instance-stats write that
// shares its tick: a failure here is one logged line and costs one tick of
// series data.
func (s *Spawner) sampleSandboxStatsOnce(ctx context.Context) {
	if s.sandboxStats == nil {
		return
	}
	jails := liveJailsFn()
	if len(jails) == 0 {
		return
	}
	at := time.Now().UTC()
	batch := make([]domain.SandboxStat, 0, len(jails))
	for _, j := range jails {
		sample := sampleJailFn(j.ContainerID)
		if !sample.Observed() {
			continue
		}
		batch = append(batch, domain.SandboxStat{
			ClaimID:      j.ClaimID,
			At:           at,
			MemCurrentMB: sample.MemCurrentMB,
			CPUUsecCum:   sample.CPUUsecCum,
		})
	}
	if len(batch) == 0 {
		return
	}
	if err := s.sandboxStats.RecordBatch(ctx, batch); err != nil {
		dispatchLog.Warn("sandbox stat batch failed", "samples", len(batch), "error", err)
	}
}

// reapSandboxStats trims the series to the same retention window the instance
// samples use, on the same slow timer. Split from the instance reap so a
// failure in either leaves the other's trimming intact.
func (s *Spawner) reapSandboxStats(ctx context.Context, cutoff time.Time) {
	if s.sandboxStats == nil {
		return
	}
	n, err := s.sandboxStats.Reap(ctx, cutoff)
	if err != nil {
		dispatchLog.Warn("sandbox stat reap failed", "error", err)
	} else if n > 0 {
		dispatchLog.Debug("sandbox stat reap", "deleted", n)
	}
}
