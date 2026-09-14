import { describe, it, expect } from 'vitest'
import { deriveCard } from './cardModel'
import type { Conversation, Task } from '../../types'

// What a card shows follows from what it HAS. These pin the derivation the
// board and the card both read: a task is queued until it is being worked and
// a conversation is attached; the run's account outranks the reporter's; a
// failure reports nothing; a queued task keeps the artifacts a requeue handed
// back with it while showing none of the run.

const NOW = Date.parse('2026-09-13T12:00:00Z')

function task(over: Partial<Task> = {}): Task {
  return {
    id: 't1',
    title: 'Serialize the cgroup read',
    status: 'in_progress',
    scoring_status: 'scored',
    ai_summary: 'Two readers hit the cgroup file at once.',
    created_at: '2026-09-13T08:00:00Z',
    memory_pending: false,
    ...over,
  } as Task
}

function conversation(over: Partial<Conversation> = {}): Conversation {
  return {
    ID: 'c1',
    TaskID: 't1',
    Status: 'running',
    Model: 'claude-opus-5',
    StartedAt: '2026-09-13T11:00:00Z',
    ClaimedAt: '2026-09-13T11:30:00Z',
    ResultSummary: '',
    ...over,
  } as Conversation
}

describe('deriveCard lifecycle', () => {
  it('reads a task with no conversation as queued, with its age and no elapsed', () => {
    const m = deriveCard(task({ status: 'queued' }), undefined, undefined, NOW)
    expect(m.lifecycle).toBe('queued')
    expect(m.age).toBe('4h ago')
    expect(m.elapsed).toBeUndefined()
    expect(m.command).toBeUndefined()
  })

  it('keeps a queued task queued whatever conversation it carries, artifacts included', () => {
    const m = deriveCard(
      task({ status: 'queued' }),
      conversation({
        Status: 'completed',
        ResultSummary: 'opened a draft pull request',
        DurationMs: 252_000,
        artifact_counts: { branch: 1, pull_request: 1 },
        unresolved_pr_count: 1,
        unresolved_review_count: 0,
      }),
      undefined,
      NOW,
    )
    expect(m.lifecycle).toBe('queued')
    expect(m.elapsed).toBeUndefined()
    expect(m.age).toBe('4h ago')
    // The reporter's words: nobody is working on it, so the run's account is
    // not the card's.
    expect(m.summary).toBe('Two readers hit the cgroup file at once.')
    expect(m.artifacts).toEqual({ branch: 1, pulls: 1 })
    expect(m.pending).toEqual({ pulls: 1 })
  })

  it('reads a running conversation as working, ticking from the claim stamp', () => {
    const m = deriveCard(
      task(),
      conversation({ Status: 'running', current_action: 'Reading internal/delegate/teardown.go' }),
      undefined,
      NOW,
    )
    expect(m.lifecycle).toBe('working')
    expect(m.command).toBe('Reading internal/delegate/teardown.go')
    expect(m.elapsed).toBe('30m 0s')
    expect(m.ticking).toBe(true)
    expect(m.liveState).toBe('running')
    expect(m.age).toBeUndefined()
  })

  it('names the queue wait on a queued run and holds the mark idle', () => {
    const m = deriveCard(
      task(),
      conversation({ Status: 'queued', QueuedAt: '2026-09-13T11:58:00Z', queue_position: 3 }),
      undefined,
      NOW,
    )
    expect(m.lifecycle).toBe('working')
    expect(m.command).toBe('Waiting for a run slot · 2 ahead')
    expect(m.liveState).toBe('idle')
    expect(m.elapsed).toBe('2m 0s')
  })

  it('names the setup phase as activity, in the words the Overview uses', () => {
    const phase = (s: string) => deriveCard(task(), conversation({ Status: s }), undefined, NOW)
    expect(phase('fetching').command).toBe('Fetching the workspace')
    expect(phase('cloning').command).toBe('Cloning the repository')
    expect(phase('agent_starting').command).toBe('Starting the agent')
    expect(phase('awaiting_credentials').command).toBe('Waiting on credentials')
    // A phase wins over a stale action: on a resume the transcript still
    // carries the previous turn's last call, and a cloning run edits nothing.
    expect(
      deriveCard(
        task(),
        conversation({ Status: 'cloning', current_action: 'Editing internal/server/agent.go' }),
        undefined,
        NOW,
      ).command,
    ).toBe('Cloning the repository')
  })

  it('keeps the live line while the agent is up but has not acted yet', () => {
    // Between the agent coming live and its first tool call the server has
    // no action to derive, and the row says the state rather than going back
    // to the destination label.
    const m = deriveCard(task(), conversation({ Status: 'running' }), undefined, NOW)
    expect(m.lifecycle).toBe('working')
    expect(m.command).toBe('Working')
  })

  it('reads the terminals: done, failed, and the stop that is not a finish', () => {
    expect(
      deriveCard(task(), conversation({ Status: 'completed', DurationMs: 60_000 }), undefined, NOW)
        .lifecycle,
    ).toBe('done')
    const failed = deriveCard(
      task(),
      conversation({ Status: 'failed', DurationMs: 60_000, ResultSummary: 'it broke' }),
      undefined,
      NOW,
    )
    expect(failed.lifecycle).toBe('failed')
    // No failure reason on the card: why is a run-page fact.
    expect(failed.summary).toBeUndefined()
    expect(failed.elapsed).toBe('1m 0s')
    expect(
      deriveCard(
        task(),
        conversation({
          Status: 'completed',
          Outcome: 'abort',
          blueprint_step_index: 0,
          blueprint_step_count: 2,
        }),
        undefined,
        NOW,
      ).lifecycle,
    ).toBe('canceled')
  })

  it('reads a parked conversation as idle', () => {
    const m = deriveCard(
      task(),
      conversation({ Status: 'open', DurationMs: 380_000 }),
      undefined,
      NOW,
    )
    expect(m.lifecycle).toBe('idle')
    expect(m.elapsed).toBe('6m 20s')
    expect(m.ticking).toBe(false)
  })

  it('prefers the run’s own account of the work once it wrote one', () => {
    const m = deriveCard(
      task(),
      conversation({ Status: 'completed', ResultSummary: 'Serialized the read behind a mutex.' }),
      undefined,
      NOW,
    )
    expect(m.summary).toBe('Serialized the read behind a mutex.')
  })

  it('marks the summary pending while the scorer has not written one', () => {
    const m = deriveCard(
      task({ status: 'queued', ai_summary: undefined, scoring_status: 'pending' }),
      undefined,
      undefined,
      NOW,
    )
    expect(m.summaryPending).toBe(true)
    expect(m.summary).toBeUndefined()
  })

  it('derives the event that made the task, and keeps it on every card', () => {
    // A known type in the vocabulary EventTag already reads, with its tone.
    const m = deriveCard(
      task({ event_type: 'github:pr:ci_check_failed' }),
      undefined,
      undefined,
      NOW,
    )
    expect(m.event).toEqual({
      label: 'CI Failed',
      description: 'A CI check failed on a PR',
      tone: 'problem',
    })
    expect(
      deriveCard(task({ event_type: 'github:pr:review_requested' }), undefined, undefined, NOW)
        .event?.tone,
    ).toBe('attention')
    // A type this build does not know falls back to the generic entry rather
    // than vanishing: the summary slot always says something.
    expect(
      deriveCard(task({ event_type: 'slack:thread:unheard_of' }), undefined, undefined, NOW).event,
    ).toEqual({ label: 'Event', description: 'A triage event occurred', tone: 'neutral' })
    // No type, no event.
    expect(deriveCard(task(), undefined, undefined, NOW).event).toBeUndefined()
    // A fact about the task, not the run: it survives a requeue and shows on
    // a done card.
    expect(
      deriveCard(
        task({ status: 'queued', event_type: 'github:pr:ci_check_failed' }),
        conversation({ Status: 'open' }),
        undefined,
        NOW,
      ).event?.label,
    ).toBe('CI Failed')
    expect(
      deriveCard(
        task({ status: 'done', event_type: 'github:pr:ci_check_failed' }),
        conversation({ Status: 'completed', DurationMs: 60_000 }),
        undefined,
        NOW,
      ).event?.label,
    ).toBe('CI Failed')
  })

  it('reads a task ended by hand with no run as done', () => {
    expect(deriveCard(task({ status: 'done' }), undefined, undefined, NOW).lifecycle).toBe('done')
  })

  it('reports the chain as shape, only past one step', () => {
    const steps = [
      conversation({ ID: 's0', Status: 'completed' }),
      conversation({ ID: 's1', Status: 'running' }),
      conversation({ ID: 's2', Status: '' }),
    ]
    expect(deriveCard(task(), steps[1], steps, NOW).chain).toEqual({ done: 1, total: 3 })
    expect(deriveCard(task(), steps[1], [steps[1]], NOW).chain).toBeUndefined()
    // Returned to the queue, the task keeps its conversation's artifacts but
    // not the run's shape: the steps are over.
    expect(deriveCard(task({ status: 'queued' }), steps[1], steps, NOW).chain).toBeUndefined()
  })

  it('says when a snoozed task wakes in place of its age', () => {
    const m = deriveCard(
      task({ status: 'queued', snooze_until: new Date(NOW + 5 * 3600_000 + 60_000).toISOString() }),
      undefined,
      undefined,
      NOW,
    )
    expect(m.age).toBe('wakes in 5h')
    expect(m.ageTitle).toMatch(/^Snoozed until /)
  })
})
