import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import ActionList from './ActionList'
import type { ActivityAction } from '../types'
import { jsonBody, listBody } from '../test/apiResponse'

// One fetch mock returning the given rows as one complete page of
// POST …/actions/list.
function mockActions(actions: ActivityAction[], next = '', total = actions.length) {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    ...listBody(actions, next, total),
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const action = (over: Partial<ActivityAction>): ActivityAction => ({
  id: 'x1',
  provider: 'github',
  action: 'comment_posted',
  target: 'org/repo#18',
  credential: 'github_app',
  occurred_at: '2026-08-01T00:00:00Z',
  ...over,
})

describe('ActionList', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('fetches the conversation-scoped endpoint and names each write', async () => {
    const fetchMock = mockActions([
      action({ id: 'a1', action: 'comment_posted', target: 'org/repo#18' }),
      action({ id: 'a2', action: 'branch_pushed', target: 'org/repo' }),
    ])
    render(<ActionList conversationId="r1" />)

    expect(await screen.findByText('Comment posted')).toBeInTheDocument()
    expect(screen.getByText('Branch pushed')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/agent/conversations/r1/actions/list',
      expect.objectContaining({ method: 'POST' }),
    )
  })

  it('shows an unclassified write as the request it actually was', async () => {
    // The incident this closes: a raw `gh api` write landed on GitHub and its
    // row said nothing beyond "action". Method + path + status are the row's
    // only identifying content, so they have to be on screen.
    mockActions([
      action({
        id: 'a1',
        action: 'gh_channel_write',
        target: 'org/repo',
        details: {
          method: 'POST',
          path: '/repos/org/repo/pulls/18/comments/9/replies',
          http_status: 201,
        },
      }),
    ])
    render(<ActionList conversationId="r1" />)

    expect(await screen.findByText('Raw gh write')).toBeInTheDocument()
    expect(
      screen.getByText(/POST \/repos\/org\/repo\/pulls\/18\/comments\/9\/replies/),
    ).toBeInTheDocument()
  })

  it('names a refused write and why', async () => {
    mockActions([
      action({
        id: 'a1',
        action: 'egress_denied',
        provider: 'network',
        credential: 'none',
        target: 'evil.example.com:443',
        details: { target: 'evil.example.com:443', reason: 'not_on_allowlist' },
      }),
    ])
    render(<ActionList conversationId="r1" />)

    expect(await screen.findByText('Network refused')).toBeInTheDocument()
    expect(screen.getByText('reason not_on_allowlist')).toBeInTheDocument()
  })

  it('announces the same subject a sighted reader sees when there is no target', async () => {
    // The aria-label REPLACES the row's content for a screen reader, so it has
    // to resolve the target the same way the visible row does. A GraphQL write
    // whose subject was unreadable has no target, and a label built from that
    // field alone would announce nothing over text that is on screen.
    mockActions([
      action({
        id: 'a1',
        action: 'graphql_write',
        target: '',
        external_id: 'PR_kwDOabc',
        details: { operation: 'ClosePr' },
      }),
    ])
    render(<ActionList conversationId="r1" />)

    expect(
      await screen.findByLabelText('Raw gh GraphQL write: PR_kwDOabc (operation ClosePr)'),
    ).toBeInTheDocument()
  })

  it('says a conversation touched nothing outside the box, rather than hiding', async () => {
    mockActions([])
    render(<ActionList conversationId="r1" />)
    expect(await screen.findByText('No external actions yet.')).toBeInTheDocument()
  })

  it('offers to fetch the rest instead of announcing a truncation', async () => {
    // It used to cap at 200 rows and print "older actions are in the activity
    // feed"; the older actions are now one click away on this surface.
    mockActions([action({ id: 'a1' })], 'tok-2', 250)
    render(<ActionList conversationId="r1" />)
    expect(
      await screen.findByRole('button', { name: /Load more \(1 of 250\)/ }),
    ).toBeInTheDocument()
  })

  it('surfaces a failed load instead of an empty audit list', async () => {
    // "No external actions yet" on a failed fetch would be a false statement
    // about a governance surface — worse than an error.
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 500, ...jsonBody({}) }))
    render(<ActionList conversationId="r1" />)
    expect(await screen.findByText(/Couldn't load this run's actions\./)).toBeInTheDocument()
  })

  it('keeps the rows it has when a soft refetch fails', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...listBody([action({ id: 'a1' })]) })
      .mockResolvedValueOnce({ ok: false, status: 500, ...jsonBody({}) })
    vi.stubGlobal('fetch', fetchMock)

    const { rerender } = render(<ActionList conversationId="r1" refreshKey="running:1" />)
    expect(await screen.findByText('Comment posted')).toBeInTheDocument()

    rerender(<ActionList conversationId="r1" refreshKey="running:2" />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(screen.getByText('Comment posted')).toBeInTheDocument()
  })
})
