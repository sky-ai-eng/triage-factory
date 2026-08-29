import { describe, it, expect } from 'vitest'
import type { Conversation, Task } from '../../types'
import {
  askProse,
  compactAge,
  donutSegments,
  lifecycleOf,
  money,
  needsRow,
  refOf,
  runningRow,
  seenLead,
  sourceOf,
  utcMidnightISO,
  windowSums,
  workingProse,
} from './data'

const NOW = Date.parse('2026-08-29T12:00:00Z')

function conv(over: Partial<Conversation>): Conversation {
  return {
    ID: 'c1',
    TaskID: 't1',
    Status: 'running',
    Model: 'claude-opus-5',
    StartedAt: '2026-08-29T11:00:00Z',
    ResultSummary: '',
    ...over,
  } as Conversation
}

function task(over: Partial<Task>): Task {
  return {
    id: 't1',
    entity_id: 'e1',
    source: 'github',
    source_id: 'acme/factory-api#761',
    source_url: '',
    title: 'Fix the flaking test',
    entity_kind: 'pr',
    event_type: 'github:ci:check_failed',
    scoring_status: 'scored',
    created_at: '2026-08-29T10:00:00Z',
    status: 'in_progress',
    priority_score: null,
    autonomy_suitability: null,
    ...over,
  } as Task
}

describe('compactAge', () => {
  it('wears the elapsed the way the row does', () => {
    expect(compactAge('2026-08-29T11:59:20Z', NOW)).toBe('40s')
    expect(compactAge('2026-08-29T11:49:00Z', NOW)).toBe('11m')
    expect(compactAge('2026-08-28T18:00:00Z', NOW)).toBe('18h')
    expect(compactAge('2026-08-26T11:00:00Z', NOW)).toBe('3d')
  })
  it('collapses clock skew to zero rather than going negative', () => {
    expect(compactAge('2026-08-29T12:05:00Z', NOW)).toBe('0s')
  })
})

describe('utcMidnightISO', () => {
  it('opens the window where the backend calendar does', () => {
    expect(utcMidnightISO(NOW)).toBe('2026-08-29T00:00:00Z')
  })
})

describe('sourceOf / refOf / lifecycleOf', () => {
  it('names the source, not the event — and failure takes its own mark', () => {
    expect(sourceOf(conv({}), task({}))).toBe('pull')
    expect(sourceOf(conv({}), task({ source: 'jira', source_id: 'SKY-412' }))).toBe('ticket')
    expect(sourceOf(conv({}), undefined)).toBe('manual')
    expect(sourceOf(conv({ Status: 'failed' }), task({}))).toBe('alert')
  })
  it('writes the reference the source way, dropping the GitHub owner', () => {
    expect(refOf(task({}))).toBe('factory-api#761')
    expect(refOf(task({ source: 'jira', source_id: 'SKY-412' }))).toBe('SKY-412')
    expect(refOf(task({ source: 'slack', source_id: 'C123/169.42' }))).toBeNull()
    expect(refOf(undefined)).toBeNull()
  })
  it('folds the status vocabulary onto the row axis', () => {
    expect(lifecycleOf(conv({ Status: 'queued' }))).toBe('queued')
    expect(lifecycleOf(conv({ Status: 'cloning' }))).toBe('working')
    expect(lifecycleOf(conv({ Status: 'running' }))).toBe('working')
    expect(lifecycleOf(conv({ Status: 'failed' }))).toBe('failed')
    expect(lifecycleOf(conv({ Status: 'completed' }))).toBe('done')
    expect(lifecycleOf(conv({ Status: 'open' }))).toBe('done')
  })
})

describe('workingProse', () => {
  it('prefers the agent-authored action and never fabricates one', () => {
    expect(
      workingProse(conv({ current_action: 'Editing internal/server/agent.go' }), undefined),
    ).toBe('Editing internal/server/agent.go')
    expect(workingProse(conv({ Status: 'cloning' }), undefined)).toBe('Cloning the repository')
    expect(workingProse(conv({ Status: 'awaiting_credentials' }), undefined)).toBe(
      'Waiting on credentials',
    )
    expect(workingProse(conv({}), undefined)).toBe('Working')
  })
  it('gives a queued run the work it will do, not the wait', () => {
    expect(workingProse(conv({ Status: 'queued' }), task({}))).toBe('Fix the flaking test')
    expect(workingProse(conv({ Status: 'queued' }), undefined)).toBe('Waiting for a slot')
  })
})

describe('askProse', () => {
  it('names a failure from the machine-readable kind only', () => {
    expect(askProse(conv({ Status: 'failed', FailureKind: 'memory_limit' }))).toBe(
      'Killed at the memory limit',
    )
    expect(askProse(conv({ Status: 'failed' }))).toBe('Failed')
  })
  it('names what to approve from the unresolved counts', () => {
    expect(askProse(conv({ unresolved_pr_count: 1 }))).toBe('Approve the draft pull request')
    expect(askProse(conv({ unresolved_pr_count: 2 }))).toBe('Approve 2 draft pull requests')
    expect(askProse(conv({ unresolved_review_count: 1 }))).toBe('Approve the pending review')
    expect(askProse(conv({ unresolved_pr_count: 1, unresolved_review_count: 2 }))).toBe(
      'Approve 3 items',
    )
  })
  it('reads the remainder of the attention set as a waiting prompt', () => {
    expect(askProse(conv({}))).toBe('Waiting on your approval')
  })
})

describe('rows', () => {
  it('builds a running row that ticks from the claim stamp', () => {
    const r = runningRow(
      conv({ ClaimedAt: '2026-08-29T11:56:00Z', current_action: 'Running go test ./...' }),
      task({}),
      '/runs/c1',
      NOW,
    )
    expect(r).toMatchObject({
      id: 'c1',
      source: 'pull',
      lifecycle: 'working',
      activity: 'Running go test ./...',
      ref: 'factory-api#761',
      age: '4m',
      queue: null,
      href: '/runs/c1',
    })
  })
  it('gives a queued row its wait and the places ahead of it', () => {
    const r = runningRow(
      conv({ Status: 'queued', QueuedAt: '2026-08-29T11:59:00Z', queue_position: 3 }),
      task({}),
      '/runs/c1',
      NOW,
    )
    // Position 3 in line is two ahead — the mark wears the wait, not the rank.
    expect(r.queue).toBe(2)
    expect(r.age).toBe('1m')
    expect(r.lifecycle).toBe('queued')
  })
  it('builds a needs row that always asks and ages from settled time', () => {
    const r = needsRow(
      conv({ Status: 'completed', CompletedAt: '2026-08-29T10:00:00Z', unresolved_pr_count: 1 }),
      task({}),
      '/runs/c1',
      NOW,
    )
    expect(r.asks).toBe(true)
    expect(r.activity).toBe('Approve the draft pull request')
    expect(r.age).toBe('2h')
  })
})

describe('windowSums', () => {
  it('sums the window and clamps absorption at zero', () => {
    const sums = windowSums([
      { date: '2026-08-28', events: 100, tasks: 4, merged: 5, failed: 1 },
      { date: '2026-08-29', events: 12, tasks: 12, merged: 1, failed: 1 },
    ])
    expect(sums).toEqual({ events: 112, merged: 6, failed: 2, filtered: 96 })
    expect(windowSums([{ date: 'd', events: 1, tasks: 5, merged: 0, failed: 0 }]).filtered).toBe(0)
  })
})

describe('seenLead', () => {
  it('anchors to midnight, and says so, before a marker exists', () => {
    expect(seenLead(null, NOW)).toBe('Your first look since midnight.')
    expect(seenLead('not a time', NOW)).toBe('Your first look since midnight.')
  })
  it('names the visit at the resolution a reader thinks in', () => {
    expect(seenLead('2026-08-29T09:15:00Z', NOW)).toMatch(/^You were last here at \d{2}:\d{2}\.$/)
    // 26 hours back is the previous calendar day at this test's noon anchor.
    expect(seenLead('2026-08-28T10:00:00Z', NOW)).toMatch(
      /^You were last here at \d{2}:\d{2} yesterday\.$/,
    )
    expect(seenLead('2026-08-25T10:00:00Z', NOW)).toMatch(/^You were last here on [A-Z][a-z]+\.$/)
    expect(seenLead('2026-06-01T10:00:00Z', NOW)).toMatch(/^You were last here on /)
  })
})

describe('donutSegments / money', () => {
  it('shares the ring by model, largest first, with the segment gap deducted', () => {
    const segs = donutSegments([
      { model: 'claude-sonnet-5', cost: 13.1 },
      { model: 'claude-opus-5', cost: 24.8 },
    ])
    expect(segs).toHaveLength(2)
    // Largest first: opus takes the first (fullest-warm) slice.
    const first = parseFloat(segs[0].dash)
    const second = parseFloat(segs[1].dash)
    expect(first).toBeGreaterThan(second)
    expect(segs[0].offset).toBe('0.00')
    expect(parseFloat(segs[1].offset)).toBeLessThan(0)
  })
  it('draws nothing for a day with no model-attributed spend', () => {
    expect(donutSegments([])).toEqual([])
    expect(donutSegments([{ model: 'm', cost: 0 }])).toEqual([])
  })
  it('renders dollars with cents', () => {
    expect(money(41.2)).toBe('$41.20')
  })
})
