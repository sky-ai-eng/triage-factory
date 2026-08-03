import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import RunStation from './RunStation'
import type { Conversation } from '../../types'

// The dock's composer is the surface this suite is about: whether the input is
// offered, and what stands in its place when it isn't.
function station(over: Partial<Conversation>) {
  const run = {
    ID: 'r1',
    TaskID: '',
    Status: 'open',
    Model: 'claude-opus-5',
    StartedAt: '2026-07-30T00:00:00Z',
    ResultSummary: '',
    artifact_count: 0,
    ...over,
  } as Conversation
  return render(
    <MemoryRouter>
      <RunStation
        run={run}
        task={null}
        messages={[]}
        now={new Date('2026-07-30T00:01:00Z').getTime()}
        actions={{ onBack: () => {}, onMessage: () => {} }}
      />
    </MemoryRouter>,
  )
}

const composer = () => screen.queryByLabelText(/message to resume/i)

describe('RunStation composer gate', () => {
  beforeEach(() => {
    // The station's chrome reads deployment config; nothing here depends on
    // the answer, and the helper degrades to raw paths without it.
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })
  afterEach(() => vi.unstubAllGlobals())

  it('offers the input on a parked run the server says is resumable', () => {
    station({ Status: 'open', resumable: true })
    expect(composer()).toBeInTheDocument()
  })

  it('offers it on a response that predates the field, falling back to status', () => {
    station({ Status: 'open' })
    expect(composer()).toBeInTheDocument()
  })

  it('replaces it with the workspace copy when the workspace is gone', () => {
    // The bug the field exists for: from Status alone this row is
    // indistinguishable from the one above, and every message it accepted
    // would come back 410.
    station({ Status: 'open', resumable: false, resume_blocked_reason: 'workspace_expired' })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.getByText(/workspace expired/i)).toBeInTheDocument()
  })

  it('names the blueprint when that is what refused', () => {
    station({
      Status: 'completed',
      Outcome: 'finish',
      resumable: false,
      resume_blocked_reason: 'blueprint_concluded',
    })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.getByText(/blueprint/i)).toBeInTheDocument()
  })

  it('says nothing about resuming a failed run', () => {
    // Not a resumable status to begin with, so there is no offer to withdraw
    // and no explanation to give.
    station({ Status: 'failed' })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.queryByText(/can’t be resumed|can’t take a follow-up/i)).not.toBeInTheDocument()
  })
})
