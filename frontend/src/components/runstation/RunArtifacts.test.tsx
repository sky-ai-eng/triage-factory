import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, render, screen, within, fireEvent, waitFor } from '@testing-library/react'
import { RunArtifacts } from './RunArtifacts'
import { rowTone } from '../deliverableMeta'
import { listBody } from '../../test/apiResponse'
import type {
  ActivityAction,
  Conversation,
  ConversationArtifact,
  Deliverable,
  WSEvent,
} from '../../types'

// The section subscribes to the socket; the mock hands the test the handler so
// a frame can be dispatched with no socket in play.
let dispatch: ((event: WSEvent) => void) | null = null
vi.mock('../../hooks/useWebSocket', () => ({
  useWebSocket: (handler: (event: WSEvent) => void) => {
    dispatch = handler
  },
}))

const conversation = (over: Partial<Conversation> = {}): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'open',
    Model: 'claude-opus-5',
    StartedAt: '2026-10-31T00:00:00Z',
    ResultSummary: '',
    artifact_count: 0,
    pending_deliverable_ids: [],
    ...over,
  }) as Conversation

const deliverable = (over: Partial<Deliverable>): Deliverable => ({
  id: 'd1',
  kind: 'review',
  provider: 'github',
  state: 'pending',
  target: 'org/repo#19',
  external_id: '',
  url: '',
  details: null,
  created_at: '2026-10-31T00:00:00Z',
  ...over,
})

const artifact = (over: Partial<ConversationArtifact>): ConversationArtifact => ({
  object_id: 'b2JqMQ',
  object_kind: 'branch',
  provider: 'github',
  scope: 'https://github.com',
  target: 'org/repo:fix-ci',
  url: 'https://github.com/org/repo/tree/fix-ci',
  last_action: 'branch_pushed',
  action_count: 1,
  first_at: '2026-10-31T00:00:00Z',
  last_at: '2026-10-31T00:00:00Z',
  deliverable: null,
  ...over,
})

// stubReads answers the section's three reads, routing on the path and, for
// the actions list, on whether the body asks for the refused count.
function stubReads({
  deliverables = [] as Deliverable[],
  artifacts = [] as ConversationArtifact[],
  refused = 0,
  actions = [] as ActivityAction[],
}) {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    const body = init?.body ? JSON.parse(String(init.body)) : {}
    if (url.endsWith('/deliverables/list')) return { ok: true, ...listBody(deliverables) }
    if (url.endsWith('/artifacts/list')) return { ok: true, ...listBody(artifacts) }
    if (url.endsWith('/actions/list')) {
      if (body.page_size === 0) return { ok: true, ...listBody([], '', refused) }
      return { ok: true, ...listBody(actions) }
    }
    throw new Error(`unexpected request ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('RunArtifacts', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    dispatch = null
  })

  it('lists a deliverable no artifact names first, highlighted, and opens its overlay', async () => {
    stubReads({
      deliverables: [deliverable({ id: 'rv1', kind: 'review', state: 'pending' })],
      artifacts: [artifact({})],
    })
    const onOpen = vi.fn()
    render(
      <RunArtifacts
        conversation={conversation({ artifact_count: 1, pending_deliverable_ids: ['rv1'] })}
        onOpenDeliverable={onOpen}
      />,
    )

    const review = await screen.findByRole('button', { name: 'Open review: org/repo#19' })
    const rows = screen.getAllByRole('listitem')
    expect(within(rows[0]).getByRole('button', { name: /open review/i })).toBe(review)
    expect(within(rows[1]).getByRole('link', { name: 'org/repo:fix-ci' })).toBeInTheDocument()
    expect(review.className).toContain(rowTone(true))
    expect(screen.getByRole('button', { name: 'Dismiss review' })).toBeInTheDocument()

    fireEvent.click(review)
    expect(onOpen).toHaveBeenCalledWith('review', 'rv1')
  })

  it('highlights an artifact its deliverable names, once, with the deliverable controls', async () => {
    stubReads({
      deliverables: [deliverable({ id: 'pr1', kind: 'pull_request', state: 'draft' })],
      artifacts: [
        artifact({
          object_id: 'cHIx',
          object_kind: 'pull_request',
          target: 'org/repo#18',
          url: 'https://github.com/org/repo/pull/18',
          last_action: 'pr_edited',
          action_count: 3,
          deliverable: { id: 'pr1', kind: 'pull_request', state: 'draft' },
        }),
        artifact({}),
      ],
    })
    const onOpen = vi.fn()
    render(
      <RunArtifacts
        conversation={conversation({ artifact_count: 2, pending_deliverable_ids: ['pr1'] })}
        onOpenDeliverable={onOpen}
      />,
    )

    const pr = await screen.findByRole('link', { name: 'org/repo#18' })
    // The pull request is one row: the matched deliverable is not listed again.
    expect(screen.getAllByRole('listitem')).toHaveLength(2)
    const prRow = pr.closest('li')!
    expect(prRow.firstElementChild!.className).toContain(rowTone(true))
    expect(within(prRow).getByText('PR edited')).toBeInTheDocument()
    expect(within(prRow).getByText('×3')).toBeInTheDocument()
    expect(within(prRow).getByRole('button', { name: 'Dismiss pull request' })).toBeInTheDocument()

    const branchRow = screen.getByRole('link', { name: 'org/repo:fix-ci' }).closest('li')!
    expect(branchRow.firstElementChild!.className).toContain(rowTone(false))
    expect(within(branchRow).queryByText(/^×/)).not.toBeInTheDocument()

    fireEvent.click(within(prRow).getByRole('button', { name: 'Open pull request: org/repo#18' }))
    expect(onOpen).toHaveBeenCalledWith('pr', 'pr1')
  })

  it('expands an artifact into the actions on its object', async () => {
    const fetchMock = stubReads({
      artifacts: [artifact({ object_id: 'YnIx', action_count: 2 })],
      actions: [
        {
          id: 'x1',
          provider: 'git',
          action: 'branch_pushed',
          target: 'org/repo:fix-ci',
          performed_by: 'agent',
          credential: 'github_app',
          occurred_at: '2026-10-31T00:00:00Z',
        },
      ],
    })
    render(<RunArtifacts conversation={conversation({ artifact_count: 1 })} />)

    fireEvent.click(
      await screen.findByRole('button', { name: 'Show the actions on org/repo:fix-ci' }),
    )

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/agent/conversations/r1/actions/list',
        expect.objectContaining({ body: JSON.stringify({ object_id: 'YnIx' }) }),
      ),
    )
  })

  it('shows the refused line only when the count is above zero', async () => {
    stubReads({ artifacts: [artifact({})], refused: 0 })
    const { unmount } = render(<RunArtifacts conversation={conversation({ artifact_count: 1 })} />)
    await screen.findByRole('link', { name: 'org/repo:fix-ci' })
    expect(screen.queryByText(/refused or failed/)).not.toBeInTheDocument()
    unmount()

    stubReads({ refused: 1 })
    render(<RunArtifacts conversation={conversation()} />)
    expect(await screen.findByText('1 refused or failed')).toBeInTheDocument()
  })

  it('renders nothing when the run changed nothing and nothing waits', async () => {
    const fetchMock = stubReads({})
    const { container } = render(<RunArtifacts conversation={conversation()} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3))
    await act(async () => {})
    expect(container).toBeEmptyDOMElement()
  })

  it('refetches on a deliverable_updated frame for this run', async () => {
    const fetchMock = stubReads({ artifacts: [artifact({})] })
    render(<RunArtifacts conversation={conversation({ artifact_count: 1 })} />)
    await screen.findByRole('link', { name: 'org/repo:fix-ci' })
    const before = fetchMock.mock.calls.length

    act(() =>
      dispatch?.({
        type: 'deliverable_updated',
        conversation_id: 'r1',
        data: { deliverable_id: 'pr1', state: 'merged' },
      }),
    )
    await waitFor(() => expect(fetchMock.mock.calls.length).toBe(before + 3))
  })
})
