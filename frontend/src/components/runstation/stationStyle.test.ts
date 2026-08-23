import { describe, it, expect } from 'vitest'
import type { Conversation } from '../../types'
import { CONVERSATION_STATUSES } from '../../types'
import { isActiveStatus } from '../../lib/conversationStatus'
import { HMI_CYAN, stationState, type StationKey } from './stationStyle'

const conversation = (over: Partial<Conversation>): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'running',
    StartedAt: '2026-07-16T10:00:00Z',
    ResultSummary: '',
    ...over,
  }) as Conversation

describe('stationState', () => {
  // A stopped conversation parks `open`, which used to be a distinct
  // CANCELLED light. The station's answer is that the machine is powered and
  // waiting, not dark: the conversation is resumable, and the dock's composer
  // says so right below this.
  it('lights a parked conversation as IDLE — powered and waiting, not concluded', () => {
    const state = stationState(conversation({ Status: 'open' }))
    expect(state.key).toBe('open')
    expect(state.label).toBe('IDLE')
    expect(state.live).toBe(false)
    expect(state.scanner).toBe(false)
    // The belt idles rather than stopping: a terminal run's belt is 0.
    expect(state.belt).toBeGreaterThan(0)
  })

  it('gives every claim phase the one working light', () => {
    for (const status of ['fetching', 'cloning', 'agent_starting', 'awaiting_credentials']) {
      const state = stationState(conversation({ Status: status }))
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
    for (const status of CONVERSATION_STATUSES) {
      const want = isActiveStatus(status) ? 'working' : expected[status]
      expect(want, `${status} has no decided station light`).toBeDefined()
      expect(stationState(conversation({ Status: status })).key, status).toBe(want)
    }
  })

  it('echoes a status this build predates rather than guessing a light', () => {
    expect(stationState(conversation({ Status: 'from_the_future' })).label).toBe('FROM_THE_FUTURE')
  })

  // The machine wears ONE light, so a mid-chain step wearing the same green
  // DONE plate as a finished task is the whole bug: the viewer reads the
  // biggest thing on the page and concludes the work stopped.
  describe('a completed conversation is three endings, not one', () => {
    const step = (index: number, total: number, outcome: string) =>
      conversation({
        Status: 'completed',
        blueprint_run_id: 'br1',
        blueprint_step_index: index,
        blueprint_step_count: total,
        Outcome: outcome,
      })

    it('says which step finished, in cyan, when the chain moves on', () => {
      const state = stationState(step(1, 4, 'continue'))
      expect(state.key).toBe('handoff')
      expect(state.label).toBe('STEP 2/4 DONE')
      // Not the success green: green is what says nothing follows.
      expect(state.light).toBe(HMI_CYAN)
      // The belt keeps idling — the work moved down the line, it didn't stop.
      expect(state.belt).toBeGreaterThan(0)
    })

    it('keeps the unqualified DONE for the last step and the single-prompt conversation', () => {
      expect(stationState(step(3, 4, 'continue')).label).toBe('DONE')
      expect(stationState(step(0, 1, 'continue')).label).toBe('DONE')
      // A mid-chain `finish` ended the whole workflow: the task IS over.
      expect(stationState(step(1, 4, 'finish')).label).toBe('DONE')
    })

    it('lights an abort amber — the agent gave up and the task is still open', () => {
      const state = stationState(conversation({ Status: 'completed', Outcome: 'abort' }))
      expect(state.key).toBe('stopped')
      expect(state.label).toBe('STOPPED')
      expect(state.light).toBe('var(--color-warm)')
    })

    it('lights a mid-chain step with no recorded outcome amber too — nothing follows it', () => {
      // The orchestrator aborts the blueprint here, so a green DONE would be
      // the reported bug wearing a different outcome.
      expect(stationState(step(1, 4, '')).key).toBe('stopped')
    })

    it('never puts a success light on an outcome it cannot interpret', () => {
      // Including on the LAST step, where the orchestrator still aborts.
      expect(stationState(step(1, 4, 'from_the_future')).key).toBe('stopped')
      expect(stationState(step(3, 4, 'from_the_future')).key).toBe('stopped')
    })

    it('falls back to the plain state word when the position is unknowable', () => {
      // blueprint_step_count 0 — the server could not resolve the plan.
      expect(stationState(step(1, 0, 'continue')).label).toBe('DONE')
    })
  })
})
