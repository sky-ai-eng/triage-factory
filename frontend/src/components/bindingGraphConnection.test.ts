import { describe, it, expect } from 'vitest'
import type { Connection } from '@xyflow/react'
import {
  EVENT_ALREADY_RUNS_BLUEPRINT,
  planConnection,
  type Membership,
} from './bindingGraphConnection'

function connect(source: string, target: string): Connection {
  return { source, target, sourceHandle: null, targetHandle: null }
}

// One two-step blueprint whose entry an event on ci_check_failed already runs.
const triggered: Membership = {
  firstPrompt: { bp: 'p0' },
  steps: { bp: ['p0', 'p1'] },
  promptToBlueprint: { p0: 'bp', p1: 'bp' },
  triggerEvents: { bp: ['github:pr:ci_check_failed'] },
}

describe('planConnection', () => {
  it('connects a second event to a triggered blueprint’s entry', () => {
    expect(
      planConnection(connect('et:github:pr:review_changes_requested', 'p:p0'), triggered),
    ).toEqual({
      ok: true,
      kind: 'trigger',
      eventType: 'github:pr:review_changes_requested',
      blueprintId: 'bp',
    })
  })

  it('refuses an event that already runs the blueprint', () => {
    expect(planConnection(connect('et:github:pr:ci_check_failed', 'p:p0'), triggered)).toEqual({
      ok: false,
      reason: EVENT_ALREADY_RUNS_BLUEPRINT,
    })
  })
})
