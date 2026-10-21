package delegate

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The per-sandbox resource series rides the instance-stat sampler's tick — the
// same interval, the same retention, the same reaper cadence. There is no
// second loop and no cadence knob of its own: if an operator retunes the
// instance sampler, the series retunes with it, which is the only behavior
// that can't drift into two policies for one family of telemetry.
//
// Each jail's Sample is silent, cheap and off the privsep hot path by
// contract (cell.Jail), so a tick costs one read per live jail and nothing
// when none is live.

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
//   - No live jails — a control pod, or an idle executor. Zero reads, zero
//     rows, no connection touched.
//   - A jail that is not running when it is read: one registered before it
//     started, or one that ended between the snapshot and the read. Its
//     sample carries nothing and no row is written for it. Silent: a warning
//     here would fire once per finished run forever.
//   - No store wired.
//
// Best-effort as a whole, and independent of the instance-stats write that
// shares its tick: a failure here is one logged line and costs one tick of
// series data.
func (s *Spawner) sampleSandboxStatsOnce(ctx context.Context) {
	if s.sandboxStats == nil {
		return
	}
	jails := s.liveJails.snapshot()
	if len(jails) == 0 {
		return
	}
	at := time.Now().UTC()
	batch := make([]domain.SandboxStat, 0, len(jails))
	for _, j := range jails {
		sample := j.jail.Sample()
		if !sample.Observed() {
			continue
		}
		batch = append(batch, domain.SandboxStat{
			ClaimID:      j.claimID,
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
