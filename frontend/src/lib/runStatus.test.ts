import { describe, it, expect } from 'vitest'
import type { Conversation } from '../types'
import { CLAIM_PHASES, RUN_STATUSES, TERMINAL_RUN_STATUSES } from '../types'
import {
  ACTIVE_STATUSES,
  isActiveStatus,
  isClaimPhase,
  isFailedStatus,
  isPermissionTerminalStatus,
  isResumableRun,
  isTerminalStatus,
  queueDwellMs,
  workStartedAt,
} from './runStatus'

const base = (over: Partial<Conversation>): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'running',
    StartedAt: '2026-07-16T10:00:00Z',
    ...over,
  }) as Conversation

const T = (iso: string) => new Date(iso).getTime()

// The vocabulary itself is checked against Go by
// TestFrontendMirrorsRunStatusVocabulary; these cover the sets derived from it.
describe('status classification', () => {
  it('counts every claim phase as active, mirroring domain.IsActiveRunStatus', () => {
    for (const phase of CLAIM_PHASES) {
      expect(isActiveStatus(phase), `${phase} should be active`).toBe(true)
      expect(isClaimPhase(phase)).toBe(true)
    }
    expect(isActiveStatus('running')).toBe(true)
    expect(ACTIVE_STATUSES).toHaveLength(CLAIM_PHASES.length + 1)
  })

  it('leaves queued, open and the terminals out of the active set', () => {
    for (const status of ['queued', 'open', ...TERMINAL_RUN_STATUSES]) {
      expect(isActiveStatus(status), `${status} should not be active`).toBe(false)
    }
  })

  // The ghosts. Each was a live UI branch that the backend had never emitted;
  // an unknown status must now classify as nothing at all rather than land in
  // an arm by accident.
  it.each(['initializing', 'worktree_created', ''])(
    'classifies the removed status %j as neither active, terminal, nor a phase',
    (ghost) => {
      expect(RUN_STATUSES as readonly string[]).not.toContain(ghost)
      expect(isActiveStatus(ghost)).toBe(false)
      expect(isTerminalStatus(ghost)).toBe(false)
      expect(isClaimPhase(ghost)).toBe(false)
    },
  )

  it('treats every terminal but completed as a failure for styling', () => {
    expect(isFailedStatus('completed')).toBe(false)
    for (const status of TERMINAL_RUN_STATUSES.filter((s) => s !== 'completed')) {
      expect(isFailedStatus(status), `${status} should style as failed`).toBe(true)
    }
    for (const status of ['queued', 'running', 'open', ...CLAIM_PHASES]) {
      expect(isFailedStatus(status)).toBe(false)
    }
  })

  it('drops parked permission prompts on every terminal and no other status', () => {
    for (const status of TERMINAL_RUN_STATUSES) {
      expect(isPermissionTerminalStatus(status)).toBe(true)
    }
    for (const status of ['queued', 'running', 'open', ...CLAIM_PHASES]) {
      expect(isPermissionTerminalStatus(status)).toBe(false)
    }
  })
})

describe('isResumableRun', () => {
  it('wakes a parked run and an aborted one', () => {
    expect(isResumableRun(base({ Status: 'open' }))).toBe(true)
    expect(isResumableRun(base({ Status: 'completed', Outcome: 'abort' }))).toBe(true)
  })

  it('excludes a finished run and anything still in flight', () => {
    expect(isResumableRun(base({ Status: 'completed', Outcome: 'finish' }))).toBe(false)
    expect(isResumableRun(base({ Status: 'running' }))).toBe(false)
    expect(isResumableRun(base({ Status: 'awaiting_credentials' }))).toBe(false)
  })
})

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
