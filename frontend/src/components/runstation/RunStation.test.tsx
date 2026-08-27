import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import RunStation, { type StationActions } from './RunStation'
import type { Conversation } from '../../types'
import { jsonBody } from '../../test/apiResponse'

// The dock is the surface this suite is about: whether the composer input is
// offered (and what stands in its place when it isn't), and how the approval
// affordance routes over the unresolved artifact set.
function station(over: Partial<Conversation>, actions: Partial<StationActions> = {}) {
  const conversation = {
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
        conversation={conversation}
        task={null}
        messages={[]}
        now={new Date('2026-07-30T00:01:00Z').getTime()}
        actions={{ onBack: () => {}, onMessage: () => {}, ...actions }}
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

  it('offers the input on a parked conversation the server says is resumable', () => {
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

  it('says nothing about resuming a failed conversation', () => {
    // Not a resumable status to begin with, so there is no offer to withdraw
    // and no explanation to give.
    station({ Status: 'failed' })
    expect(composer()).not.toBeInTheDocument()
    expect(screen.queryByText(/can’t be resumed|can’t take a follow-up/i)).not.toBeInTheDocument()
  })
})

describe('RunStation dock approval affordance', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })

  it('routes a lone unresolved item straight to its editor — no list detour', () => {
    const onOpenArtifact = vi.fn()
    station(
      {
        Status: 'running',
        artifact_count: 1,
        has_unresolved_artifacts: true,
        unresolved_pr_count: 1,
        unresolved_review_count: 0,
        pending_artifact_ids: ['pr1'],
      },
      { onOpenArtifact },
    )
    fireEvent.click(screen.getByRole('button', { name: /open pr/i }))
    expect(onOpenArtifact).toHaveBeenCalledWith('pr', 'pr1')
  })

  it('raises the artifact-list popover for a mixed set and forwards a row pick', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        ...jsonBody([
          {
            id: 'pr1',
            kind: 'pull_request',
            provider: 'github',
            state: 'draft',
            target: 'org/repo#18',
            external_id: '18',
            url: 'https://gh/pr',
            details: null,
            created_at: '2026-07-30T00:00:00Z',
          },
          {
            id: 'rv1',
            kind: 'review',
            provider: 'github',
            state: 'pending',
            target: 'org/repo#19',
            external_id: '19',
            url: 'https://gh/rv',
            details: null,
            created_at: '2026-07-30T00:00:00Z',
          },
        ]),
      }),
    )
    const onOpenArtifact = vi.fn()
    station(
      {
        Status: 'running',
        artifact_count: 2,
        has_unresolved_artifacts: true,
        unresolved_pr_count: 1,
        unresolved_review_count: 1,
        pending_artifact_ids: ['pr1', 'rv1'],
      },
      { onOpenArtifact },
    )

    fireEvent.click(screen.getByRole('button', { name: /review 2 items/i }))

    // The popover carries its own list instance (the telemetry rail renders
    // one too), so scope the row pick to the dialog.
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(await within(dialog).findByRole('button', { name: /open pull request/i }))
    expect(onOpenArtifact).toHaveBeenCalledWith('pr', 'pr1')
  })
})
