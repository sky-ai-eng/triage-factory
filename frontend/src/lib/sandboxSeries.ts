import type { FleetSandboxSample } from '../types'

// Derivations over one sandbox's in-run series (TFAC-711/712), kept out of the
// component so the arithmetic that matters — turning a cumulative CPU counter
// into a rate across an irregular sampling cadence — is testable on its own.
//
// The honest-display contract these feed: this series is SHAPE, sampled on a
// timer; the claim's peak_mem_mb / cpu_usec columns are TRUTH, read from the
// kernel at teardown. A periodic sampler misses the sub-minute spikes the
// high-watermark catches, so the peak column legitimately sits above the
// chart's highest point. Never reconcile them numerically.

export interface SandboxPoint {
  /** Epoch milliseconds — the x axis for both series. */
  t: number
  v: number
}

/**
 * memSeries plots resident memory per sample, in MB. Samples with no memory
 * reading are dropped rather than plotted as zero: the sampler writes a
 * partial row when it could read one metric and not the other, and a zero
 * there would draw a collapse that never happened.
 */
export function memSeries(samples: FleetSandboxSample[]): SandboxPoint[] {
  const out: SandboxPoint[] = []
  for (const s of samples) {
    if (s.mem_current_mb == null) continue
    const t = Date.parse(s.at)
    if (Number.isNaN(t)) continue
    out.push({ t, v: s.mem_current_mb })
  }
  return out
}

/**
 * cpuRateSeries differences the cumulative CPU counter into average cores used
 * over each interval: Δµs of CPU ÷ Δµs of wall clock.
 *
 * It necessarily starts at the SECOND sample — a rate needs two readings — so
 * this returns one fewer point than it was given, and a single-sample series
 * yields nothing. That is why memory and CPU render as two charts rather than
 * two lines on one: they do not share a point count.
 *
 * A dropped tick needs no special handling and gets none. Because the counter
 * is cumulative, the wider interval carries the CPU the missing tick would
 * have reported, so the rate comes out as the true average across the gap
 * instead of a hole that reads as an idle sandbox.
 */
export function cpuRateSeries(samples: FleetSandboxSample[]): SandboxPoint[] {
  const out: SandboxPoint[] = []
  let prev: { t: number; cpu: number } | null = null
  for (const s of samples) {
    if (s.cpu_usec_cum == null) continue
    const t = Date.parse(s.at)
    if (Number.isNaN(t)) continue
    const cur = { t, cpu: s.cpu_usec_cum }
    if (prev) {
      const wallUsec = (cur.t - prev.t) * 1000
      // A non-advancing clock can only come from duplicate timestamps; there
      // is no interval to average over, so there is no point to plot.
      if (wallUsec > 0) {
        // Clamp a backwards counter to zero. It should be impossible per
        // claim (the counter is monotonic within one cgroup), but a negative
        // "cores used" is nonsense that would rescale the whole chart.
        const cpuUsec = Math.max(0, cur.cpu - prev.cpu)
        out.push({ t: cur.t, v: cpuUsec / wallUsec })
      }
    }
    prev = cur
  }
  return out
}

/** Total CPU expressed in core-minutes (1 core busy for 1 minute = 1). */
export function coreMinutes(cpuUsec: number): number {
  return cpuUsec / 6e7
}

/**
 * averageCores is the run's CPU spread over its wall clock — how many cores it
 * held on average. The denominator is the ENGAGEMENT's wall time (claimed →
 * released), the interval the cgroup counter accumulated inside; a shorter
 * denominator would report cores the box never gave it. Null when there is no
 * interval to divide by.
 */
export function averageCores(cpuUsec: number, durationSeconds: number): number | null {
  if (durationSeconds <= 0) return null
  return cpuUsec / (durationSeconds * 1e6)
}
