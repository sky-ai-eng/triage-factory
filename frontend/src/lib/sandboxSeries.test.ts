import { describe, it, expect } from 'vitest'

import { averageCores, coreMinutes, cpuRateSeries, memSeries } from './sandboxSeries'
import type { FleetSandboxSample } from '../types'

const at = (minutes: number) => new Date(Date.UTC(2026, 6, 15, 12, minutes, 0)).toISOString()

describe('cpuRateSeries', () => {
  it('differences the cumulative counter into average cores per interval', () => {
    // 60s of wall clock, 30s of CPU → half a core; then 60s of wall, 120s of
    // CPU → two cores.
    const samples: FleetSandboxSample[] = [
      { at: at(0), cpu_usec_cum: 0 },
      { at: at(1), cpu_usec_cum: 30_000_000 },
      { at: at(2), cpu_usec_cum: 150_000_000 },
    ]
    const got = cpuRateSeries(samples)
    expect(got).toHaveLength(2)
    expect(got[0].v).toBeCloseTo(0.5, 6)
    expect(got[1].v).toBeCloseTo(2, 6)
    expect(got[0].t).toBe(Date.parse(at(1)))
  })

  it('renders sanely across a dropped tick', () => {
    // The 12:02 sample never landed. The cumulative counter means the wider
    // interval still carries the CPU that tick would have reported, so the
    // rate is the true average across the gap — not a hole, and not a spike.
    const samples: FleetSandboxSample[] = [
      { at: at(0), cpu_usec_cum: 0 },
      { at: at(1), cpu_usec_cum: 60_000_000 }, // one full core over 60s
      { at: at(3), cpu_usec_cum: 180_000_000 }, // 120s of CPU over 120s of wall
    ]
    const got = cpuRateSeries(samples)
    expect(got).toHaveLength(2)
    expect(got[0].v).toBeCloseTo(1, 6)
    expect(got[1].v).toBeCloseTo(1, 6)
    // The gap is a wider interval, not a missing point: the second rate is
    // stamped at the sample that closed it.
    expect(got[1].t).toBe(Date.parse(at(3)))
  })

  it('yields nothing for a series too short to have a rate', () => {
    expect(cpuRateSeries([])).toEqual([])
    expect(cpuRateSeries([{ at: at(0), cpu_usec_cum: 10 }])).toEqual([])
  })

  it('skips samples with no CPU reading and duplicate timestamps', () => {
    const got = cpuRateSeries([
      { at: at(0), cpu_usec_cum: 0 },
      { at: at(1), mem_current_mb: 100 }, // partial sample: no CPU
      { at: at(1), cpu_usec_cum: 60_000_000 },
      { at: at(1), cpu_usec_cum: 60_000_000 }, // duplicate instant: no interval
    ])
    expect(got).toHaveLength(1)
    expect(got[0].v).toBeCloseTo(1, 6)
  })

  it('clamps a backwards counter rather than plotting negative cores', () => {
    const got = cpuRateSeries([
      { at: at(0), cpu_usec_cum: 100_000_000 },
      { at: at(1), cpu_usec_cum: 0 },
    ])
    expect(got).toHaveLength(1)
    expect(got[0].v).toBe(0)
  })
})

describe('memSeries', () => {
  it('plots each reading and drops unread ones instead of zeroing them', () => {
    const got = memSeries([
      { at: at(0), mem_current_mb: 120 },
      { at: at(1), cpu_usec_cum: 5 }, // no memory read this tick
      { at: at(2), mem_current_mb: 400 },
    ])
    expect(got.map((p) => p.v)).toEqual([120, 400])
  })

  it('is empty for a claim that was never sampled', () => {
    expect(memSeries([])).toEqual([])
  })
})

describe('scalar derivations', () => {
  it('converts CPU microseconds to core-minutes', () => {
    expect(coreMinutes(6e7)).toBeCloseTo(1, 6)
    expect(coreMinutes(9e7)).toBeCloseTo(1.5, 6)
  })

  it('averages cores over the engagement wall clock', () => {
    // 60s of CPU across 120s of wall clock → half a core held on average.
    expect(averageCores(60_000_000, 120)).toBeCloseTo(0.5, 6)
    // No interval to divide by — a claim released in the same instant it was
    // taken reports no average rather than Infinity.
    expect(averageCores(60_000_000, 0)).toBeNull()
  })
})
