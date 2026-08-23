import { describe, it, expect } from 'vitest'
import { activitySeries, activitySource, sinceParts, sinceLabel } from './useTeamActivity'
import type { TeamActivity } from './useTeamActivity'

const day = (n: number) => new Date(Date.now() - n * 86400_000).toISOString().slice(0, 10)

const ACTIVITY: TeamActivity = {
  team_id: 't1',
  by_day: [],
  by_day_source: [
    { date: day(0), source: 'github', events: 4, tasks: 1 },
    { date: day(2), source: 'github', events: 2, tasks: 0 },
    { date: day(1), source: 'jira', events: 9, tasks: 9 },
  ],
  by_source: [
    { source: 'github', events: 6, tasks: 1, runs: 2, last_poll_at: null },
    { source: 'slack', events: null, tasks: 0, runs: 0, last_poll_at: null },
  ],
}

describe('activitySeries', () => {
  it('is null while the node has not answered — a flat zero line is a claim', () => {
    expect(activitySeries(null, 'github', 14)).toBeNull()
  })

  it('zero-fills the window once it has: a day with no rows is a real zero', () => {
    const s = activitySeries(ACTIVITY, 'github', 4)!
    // Oldest first: day(3) day(2) day(1) day(0). The jira row never leaks in.
    expect(s).toEqual([
      { events: 0, tasks: 0 },
      { events: 2, tasks: 0 },
      { events: 0, tasks: 0 },
      { events: 4, tasks: 1 },
    ])
  })
})

describe('activitySource', () => {
  it('answers the row or null, never a fabricated zero row', () => {
    expect(activitySource(ACTIVITY, 'github')?.runs).toBe(2)
    expect(activitySource(ACTIVITY, 'slack')?.events).toBeNull()
    expect(activitySource(ACTIVITY, 'jira')).toBeNull()
    expect(activitySource(null, 'github')).toBeNull()
  })
})

describe('sinceParts', () => {
  const ago = (ms: number) => new Date(Date.now() - ms).toISOString()

  it('picks the unit a human would', () => {
    expect(sinceParts(ago(40_000))).toEqual({ value: 40, unit: 's' })
    expect(sinceParts(ago(120_000))).toEqual({ value: 2, unit: 'm' })
    expect(sinceParts(ago(5_400_000))).toEqual({ value: 1, unit: 'h' })
    expect(sinceParts(ago(200_000_000))).toEqual({ value: 2, unit: 'd' })
  })

  it('answers null for nothing, garbage, and the future', () => {
    expect(sinceParts(null)).toBeNull()
    expect(sinceParts('not a timestamp')).toBeNull()
    expect(sinceParts(new Date(Date.now() + 60_000).toISOString())).toBeNull()
  })

  it('reads as one figure through sinceLabel', () => {
    expect(sinceLabel(ago(40_000))).toBe('40s')
    expect(sinceLabel(null)).toBeNull()
  })
})
