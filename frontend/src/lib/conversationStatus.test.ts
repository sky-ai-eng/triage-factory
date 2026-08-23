import { describe, it, expect } from 'vitest'
import type { Conversation } from '../types'
import { CLAIM_PHASES, CONVERSATION_STATUSES, TERMINAL_CONVERSATION_STATUSES } from '../types'
import {
  ACTIVE_STATUSES,
  canResumeConversation,
  chainPosition,
  completionGloss,
  completionKind,
  isActiveStatus,
  isClaimPhase,
  isFailedStatus,
  isPermissionTerminalStatus,
  isResumableConversation,
  isTerminalStatus,
  queueDwellMs,
  resumeBlockedCopy,
  workStartedAt,
} from './conversationStatus'

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
// TestFrontendMirrorsConversationStatusVocabulary; these cover the sets derived from it.
describe('status classification', () => {
  it('counts every claim phase as active, mirroring domain.IsActiveConversationStatus', () => {
    for (const phase of CLAIM_PHASES) {
      expect(isActiveStatus(phase), `${phase} should be active`).toBe(true)
      expect(isClaimPhase(phase)).toBe(true)
    }
    expect(isActiveStatus('running')).toBe(true)
    expect(ACTIVE_STATUSES).toHaveLength(CLAIM_PHASES.length + 1)
  })

  it('leaves queued, open and the terminals out of the active set', () => {
    for (const status of ['queued', 'open', ...TERMINAL_CONVERSATION_STATUSES]) {
      expect(isActiveStatus(status), `${status} should not be active`).toBe(false)
    }
  })

  // Values from outside the vocabulary: a raw NULL read as '', and two names
  // the UI once branched on that the backend has never emitted. The
  // classifiers are closed-world, so anything they don't recognize must fall
  // through every set rather than land in an arm by accident — which is the
  // property worth testing on values that are, deliberately, not statuses.
  it.each(['initializing', 'worktree_created', ''])(
    'classifies the unknown value %j as neither active, terminal, nor a phase',
    (unknown) => {
      expect(CONVERSATION_STATUSES as readonly string[]).not.toContain(unknown)
      expect(isActiveStatus(unknown)).toBe(false)
      expect(isTerminalStatus(unknown)).toBe(false)
      expect(isClaimPhase(unknown)).toBe(false)
    },
  )

  it('treats every terminal but completed as a failure for styling', () => {
    expect(isFailedStatus('completed')).toBe(false)
    for (const status of TERMINAL_CONVERSATION_STATUSES.filter((s) => s !== 'completed')) {
      expect(isFailedStatus(status), `${status} should style as failed`).toBe(true)
    }
    for (const status of ['queued', 'running', 'open', ...CLAIM_PHASES]) {
      expect(isFailedStatus(status)).toBe(false)
    }
  })

  it('drops parked permission prompts on every terminal and no other status', () => {
    for (const status of TERMINAL_CONVERSATION_STATUSES) {
      expect(isPermissionTerminalStatus(status)).toBe(true)
    }
    for (const status of ['queued', 'running', 'open', ...CLAIM_PHASES]) {
      expect(isPermissionTerminalStatus(status)).toBe(false)
    }
  })
})

// A completed conversation is three different endings wearing one status, and
// the discriminator is POSITION, not outcome: `continue` is what an ordinary
// completion records under the native loop (every step, last one included),
// while the SDK's terminal step reports `finish`. Branching on the outcome
// alone would label an ordinary single-prompt native conversation as handed off.
describe('chain position and completion', () => {
  const step = (index: number, total: number, over: Partial<Conversation> = {}) =>
    base({
      Status: 'completed',
      blueprint_run_id: 'br1',
      blueprint_step_index: index,
      blueprint_step_count: total,
      ...over,
    })

  it('reads a plan of one as no chain at all — that is the plain single-prompt conversation', () => {
    expect(chainPosition(step(0, 1))).toBeNull()
    expect(completionKind(step(0, 1, { Outcome: 'continue' }))).toBe('done')
  })

  it('treats an unresolved plan length as unknown position, not as step one of none', () => {
    // 0 is what the server sends when it could not read the blueprint row
    // (a teammate's manual blueprint run under RLS); absent is a client predating it.
    expect(chainPosition(step(0, 0))).toBeNull()
    expect(chainPosition(base({ Status: 'completed', blueprint_step_index: 0 }))).toBeNull()
    expect(completionKind(step(0, 0, { Outcome: 'continue' }))).toBe('done')
  })

  it('numbers a mid-chain step for display and marks the last one final', () => {
    expect(chainPosition(step(1, 4))).toEqual({ step: 2, total: 4, isFinal: false })
    expect(chainPosition(step(3, 4))).toEqual({ step: 4, total: 4, isFinal: true })
  })

  it('calls a non-final step that concluded normally a hand-off, not the task finishing', () => {
    expect(completionKind(step(1, 4, { Outcome: 'continue' }))).toBe('handoff')
    expect(completionGloss(step(1, 4, { Outcome: 'continue' }))).toBe(
      'step 2 of 4 done — handed off to the next step',
    )
  })

  it('calls the chain’s last step done, even though it too records continue', () => {
    expect(completionKind(step(3, 4, { Outcome: 'continue' }))).toBe('done')
    expect(completionKind(step(3, 4, { Outcome: 'finish' }))).toBe('done')
  })

  it('calls a mid-chain finish done — it ended the whole workflow early', () => {
    expect(completionKind(step(1, 4, { Outcome: 'finish' }))).toBe('done')
    expect(completionGloss(step(1, 4, { Outcome: 'finish' }))).toBe(
      'ended the workflow early — the later steps were skipped',
    )
  })

  it('calls a non-final step with no recorded outcome stopped, mirroring the orchestrator', () => {
    // decideBlueprintStep aborts the blueprint on this ("no-outcome"), so
    // nothing runs after the step and a human owns the task. Reading it as a
    // hand-off would claim a successor that will never come; reading it as
    // done would be the original bug.
    expect(completionKind(step(1, 4, { Outcome: '' }))).toBe('stopped')
    expect(completionKind(step(1, 4))).toBe('stopped')
    expect(completionGloss(step(1, 4, { Outcome: '' }))).toBe(
      'ended without a usable outcome — the workflow stopped here for a human',
    )
    // The abort keeps its own wording — the agent decided to stop, which is a
    // different thing to tell the viewer than a step that just never said.
    expect(completionGloss(step(1, 4, { Outcome: 'abort' }))).toBe(
      'stopped without finishing — the task stays open for a human',
    )
  })

  it('leaves the final step and the single-prompt conversation alone when the outcome is missing', () => {
    // Same missing outcome, opposite disposition: decideBlueprintStep resolves
    // it to a finish once there is no step left to hand off to. This is the
    // arm that DOES branch on position.
    expect(completionKind(step(3, 4, { Outcome: '' }))).toBe('done')
    expect(completionKind(step(0, 1, { Outcome: '' }))).toBe('done')
  })

  it('stops on an outcome the vocabulary does not know, at every position', () => {
    // The arm that does NOT branch on position: decideBlueprintStep's default
    // aborts on an unrecognized value wherever it lands, refusing to close a
    // task on a name it can't interpret. The final step is the one that would
    // otherwise slip through as a green verdict on an aborted blueprint —
    // this wire field is deliberately open-world, so a build reading a name
    // added after it shipped has to land somewhere honest.
    expect(completionKind(step(1, 4, { Outcome: 'from_the_future' }))).toBe('stopped')
    expect(completionKind(step(3, 4, { Outcome: 'from_the_future' }))).toBe('stopped')
    expect(completionKind(step(0, 1, { Outcome: 'from_the_future' }))).toBe('stopped')
    // Position it cannot read at all doesn't rescue it either — the
    // orchestrator's answer here doesn't depend on the position.
    expect(completionKind(step(1, 0, { Outcome: 'from_the_future' }))).toBe('stopped')
    expect(completionGloss(step(3, 4, { Outcome: 'from_the_future' }))).toBe(
      'ended without a usable outcome — the workflow stopped here for a human',
    )
  })

  it('separates an abort from a success wherever it lands in the chain', () => {
    expect(completionKind(step(1, 4, { Outcome: 'abort' }))).toBe('stopped')
    expect(completionKind(step(3, 4, { Outcome: 'abort' }))).toBe('stopped')
    expect(completionKind(base({ Status: 'completed', Outcome: 'abort' }))).toBe('stopped')
    expect(completionGloss(base({ Status: 'completed', Outcome: 'abort' }))).toBe(
      'stopped without finishing — the task stays open for a human',
    )
  })

  it('answers for completed conversations only', () => {
    for (const status of ['queued', 'running', 'open', 'failed', ...CLAIM_PHASES]) {
      expect(completionKind(base({ Status: status })), status).toBeNull()
      expect(completionGloss(base({ Status: status })), status).toBe('')
    }
  })
})

describe('isResumableConversation', () => {
  it('wakes a parked conversation and any conclusion, mirroring the backend resumableState', () => {
    expect(isResumableConversation(base({ Status: 'open' }))).toBe(true)
    expect(isResumableConversation(base({ Status: 'completed', Outcome: 'abort' }))).toBe(true)
    // Finished work is followed up on — outcome stopped discriminating when
    // every completed terminal started snapshotting its workspace.
    expect(isResumableConversation(base({ Status: 'completed', Outcome: 'finish' }))).toBe(true)
  })

  it('excludes the failed terminal and anything still in flight', () => {
    expect(isResumableConversation(base({ Status: 'failed' }))).toBe(false)
    expect(isResumableConversation(base({ Status: 'queued' }))).toBe(false)
    expect(isResumableConversation(base({ Status: 'running' }))).toBe(false)
    expect(isResumableConversation(base({ Status: 'awaiting_credentials' }))).toBe(false)
  })
})

describe('canResumeConversation', () => {
  it('defers to the server when it answered', () => {
    expect(canResumeConversation(base({ Status: 'open', resumable: true }))).toBe(true)
    // The whole point of the field: status says resumable, the workspace says
    // otherwise, and only the server can see the difference.
    expect(
      canResumeConversation(
        base({ Status: 'open', resumable: false, resume_blocked_reason: 'workspace_expired' }),
      ),
    ).toBe(false)
  })

  it('falls back to status when the server omitted the field', () => {
    // Absent means "not answered" — an older response, or a conversation the detail read
    // skips — never false.
    expect(canResumeConversation(base({ Status: 'open' }))).toBe(true)
    expect(canResumeConversation(base({ Status: 'failed' }))).toBe(false)
  })

  it('never opens the composer on a status that cannot take a follow-up', () => {
    expect(canResumeConversation(base({ Status: 'failed', resumable: true }))).toBe(false)
  })
})

describe('resumeBlockedCopy', () => {
  it('names the workspace and the blueprint cases separately', () => {
    expect(resumeBlockedCopy(base({ resume_blocked_reason: 'workspace_expired' }))).toMatch(
      /workspace expired/i,
    )
    expect(resumeBlockedCopy(base({ resume_blocked_reason: 'blueprint_concluded' }))).toMatch(
      /blueprint/i,
    )
  })

  it('tells a cancelled workflow apart from one that moved past the step', () => {
    // A cancelled workflow moved past nothing — for a single-step blueprint
    // there is no later step to have moved to — so its copy must not claim it
    // did. The cancel gets its own words.
    const cancelled = resumeBlockedCopy(base({ resume_blocked_reason: 'blueprint_cancelled' }))
    expect(cancelled).toMatch(/cancelled/i)
    expect(cancelled).not.toMatch(/moved past/i)
    expect(cancelled).not.toBe(
      resumeBlockedCopy(base({ resume_blocked_reason: 'blueprint_concluded' })),
    )
  })

  it('tells a just-handed-off step apart from one the blueprint has left behind', () => {
    // Two refusals that both mention the blueprint, and the difference is the
    // whole message: one is over, the other is a moment.
    const handedOff = resumeBlockedCopy(base({ resume_blocked_reason: 'step_handed_off' }))
    expect(handedOff).toMatch(/handed off/i)
    expect(handedOff).toMatch(/latest step/i)
    expect(handedOff).not.toBe(
      resumeBlockedCopy(base({ resume_blocked_reason: 'blueprint_concluded' })),
    )
  })

  it('says something true for a reason this build does not know', () => {
    expect(resumeBlockedCopy(base({ resume_blocked_reason: 'reason_from_the_future' }))).toMatch(
      /follow-up/i,
    )
    expect(resumeBlockedCopy(base({}))).toMatch(/follow-up/i)
  })
})

describe('workStartedAt', () => {
  it('prefers the claim stamp so queue dwell never inflates working elapsed', () => {
    const conversation = base({ ClaimedAt: '2026-07-16T10:06:00Z' })
    expect(workStartedAt(conversation)).toBe('2026-07-16T10:06:00Z')
  })

  it('falls back to the mint stamp for legacy rows without queue columns', () => {
    expect(workStartedAt(base({}))).toBe('2026-07-16T10:00:00Z')
  })
})

describe('queueDwellMs', () => {
  it('ticks live from QueuedAt while the conversation is still queued', () => {
    const conversation = base({ Status: 'queued', QueuedAt: '2026-07-16T10:00:00Z' })
    expect(queueDwellMs(conversation, T('2026-07-16T10:06:00Z'))).toBe(6 * 60_000)
  })

  it('falls back to StartedAt for a queued legacy row', () => {
    const conversation = base({ Status: 'queued' })
    expect(queueDwellMs(conversation, T('2026-07-16T10:01:00Z'))).toBe(60_000)
  })

  it('settles to ClaimedAt − QueuedAt once the conversation started', () => {
    const conversation = base({
      QueuedAt: '2026-07-16T10:00:00Z',
      ClaimedAt: '2026-07-16T10:06:00Z',
    })
    expect(queueDwellMs(conversation, T('2026-07-16T11:00:00Z'))).toBe(6 * 60_000)
  })

  it('is unknowable (null) for a started legacy row', () => {
    expect(queueDwellMs(base({}), T('2026-07-16T11:00:00Z'))).toBeNull()
  })

  it('clamps clock skew to zero rather than going negative', () => {
    const conversation = base({ Status: 'queued', QueuedAt: '2026-07-16T10:00:05Z' })
    expect(queueDwellMs(conversation, T('2026-07-16T10:00:00Z'))).toBe(0)
  })
})
