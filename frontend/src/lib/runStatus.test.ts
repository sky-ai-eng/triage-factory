import { describe, it, expect } from 'vitest'
import type { AgentRun } from '../types'
import { queueDwellMs, workStartedAt } from './runStatus'

const base = (over: Partial<AgentRun>): AgentRun =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'running',
    StartedAt: '2026-07-16T10:00:00Z',
    ...over,
  }) as AgentRun

const T = (iso: string) => new Date(iso).getTime()

describe('workStartedAt', () => {
  it('prefers the claim stamp so queue dwell never inflates working elapsed', () => {
    const run = base({ ClaimedAt: '2026-07-16T10:06:00Z' })
    expect(workStartedAt(run)).toBe('2026-07-16T10:06:00Z')
  })

  it('falls back to the mint stamp for legacy rows without queue columns', () => {
    expect(workStartedAt(base({}))).toBe('2026-07-16T10:00:00Z')
  })
})

describe('queueDwellMs', () => {
  it('ticks live from QueuedAt while the run is still queued', () => {
    const run = base({ Status: 'queued', QueuedAt: '2026-07-16T10:00:00Z' })
    expect(queueDwellMs(run, T('2026-07-16T10:06:00Z'))).toBe(6 * 60_000)
  })

  it('falls back to StartedAt for a queued legacy row', () => {
    const run = base({ Status: 'queued' })
    expect(queueDwellMs(run, T('2026-07-16T10:01:00Z'))).toBe(60_000)
  })

  it('settles to ClaimedAt − QueuedAt once the run started', () => {
    const run = base({
      QueuedAt: '2026-07-16T10:00:00Z',
      ClaimedAt: '2026-07-16T10:06:00Z',
    })
    expect(queueDwellMs(run, T('2026-07-16T11:00:00Z'))).toBe(6 * 60_000)
  })

  it('is unknowable (null) for a started legacy row', () => {
    expect(queueDwellMs(base({}), T('2026-07-16T11:00:00Z'))).toBeNull()
  })

  it('clamps clock skew to zero rather than going negative', () => {
    const run = base({ Status: 'queued', QueuedAt: '2026-07-16T10:00:05Z' })
    expect(queueDwellMs(run, T('2026-07-16T10:00:00Z'))).toBe(0)
  })
})
