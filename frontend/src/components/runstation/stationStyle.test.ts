import { describe, it, expect } from 'vitest'
import type { Conversation } from '../../types'
import { RUN_STATUSES } from '../../types'
import { isActiveStatus } from '../../lib/runStatus'
import { stationState, type StationKey } from './stationStyle'

const run = (over: Partial<Conversation>): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'running',
    StartedAt: '2026-07-16T10:00:00Z',
    ResultSummary: '',
    ...over,
  }) as Conversation

describe('stationState', () => {
  // A stopped run parks `open`, which used to be a distinct CANCELLED light.
  // The station's answer is that the machine is powered and waiting, not dark:
  // the run is resumable, and the dock's composer says so right below this.
  it('lights a parked run as IDLE — powered and waiting, not concluded', () => {
    const state = stationState(run({ Status: 'open' }))
    expect(state.key).toBe('open')
    expect(state.label).toBe('IDLE')
    expect(state.live).toBe(false)
    expect(state.scanner).toBe(false)
    // The belt idles rather than stopping: a terminal run's belt is 0.
    expect(state.belt).toBeGreaterThan(0)
  })

  it('gives every claim phase the one working light', () => {
    for (const status of ['fetching', 'cloning', 'agent_starting', 'awaiting_credentials']) {
      const state = stationState(run({ Status: status }))
      expect(state.key, `${status} should read as working`).toBe('working')
      expect(state.live).toBe(true)
    }
  })

  it('gives every status in the vocabulary a decided light', () => {
    // The table is the decision, and it has to cover the whole vocabulary: a
    // status added in Go and mirrored here without a light would otherwise
    // land silently in the fallback, which is how a real claim phase once
    // rendered as inert grey for months.
    const expected: Record<string, StationKey> = {
      queued: 'queued',
      open: 'open',
      completed: 'done',
      failed: 'failed',
    }
    for (const status of RUN_STATUSES) {
      const want = isActiveStatus(status) ? 'working' : expected[status]
      expect(want, `${status} has no decided station light`).toBeDefined()
      expect(stationState(run({ Status: status })).key, status).toBe(want)
    }
  })

  it('echoes a status this build predates rather than guessing a light', () => {
    expect(stationState(run({ Status: 'from_the_future' })).label).toBe('FROM_THE_FUTURE')
  })
})
