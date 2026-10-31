import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import DeliverableList from './DeliverableList'
import type { Deliverable } from '../types'
import { listBody } from '../test/apiResponse'

// One fetch mock answering POST …/deliverables/list with the given rows as one page.
function mockDeliverables(arts: Deliverable[]) {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    ...listBody(arts),
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const art = (over: Partial<Deliverable>): Deliverable => ({
  id: 'a1',
  kind: 'branch',
  provider: 'git',
  state: 'pushed',
  target: 'org/repo',
  external_id: 'refs/heads/x',
  url: 'https://example.test/x',
  details: null,
  created_at: '2026-06-25T00:00:00Z',
  ...over,
})

describe('DeliverableList', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('fetches the conversation-scoped endpoint and renders each deliverable (kind / state / link)', async () => {
    const fetchMock = mockDeliverables([
      art({
        id: 'b1',
        kind: 'branch',
        state: 'pushed',
        target: 'org/repo',
        url: 'https://gh/branch',
      }),
      art({
        id: 'pr1',
        kind: 'pull_request',
        state: 'draft',
        target: 'org/repo#18',
        url: 'https://gh/pr',
      }),
    ])
    render(<DeliverableList conversationId="r1" onOpenApproval={vi.fn()} />)

    // Targets + state badges for both rows render once the fetch resolves.
    expect(await screen.findByText('org/repo#18')).toBeInTheDocument()
    expect(screen.getByText('org/repo')).toBeInTheDocument()
    expect(screen.getByText('pushed')).toBeInTheDocument()
    expect(screen.getByText('draft')).toBeInTheDocument()

    // The conversation-scoped read API was hit.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/agent/conversations/r1/deliverables/list',
      expect.objectContaining({ method: 'POST' }),
    )

    // The branch row links out to its url; the PR row carries an external link too.
    const links = screen.getAllByRole('link')
    const hrefs = links.map((l) => l.getAttribute('href'))
    expect(hrefs).toContain('https://gh/branch')
    expect(hrefs).toContain('https://gh/pr')
  })

  it('opens the approval overlay for pull_request and review rows by deliverable id', async () => {
    const onOpen = vi.fn()
    mockDeliverables([
      art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' }),
      art({ id: 'rv1', kind: 'review', state: 'pending', target: 'org/repo#18' }),
    ])
    render(<DeliverableList conversationId="r1" onOpenApproval={onOpen} />)

    fireEvent.click(await screen.findByRole('button', { name: /Open pull request/ }))
    expect(onOpen).toHaveBeenCalledWith('pr', 'pr1')

    fireEvent.click(screen.getByRole('button', { name: /Open review/ }))
    expect(onOpen).toHaveBeenCalledWith('review', 'rv1')
  })

  it('renders RESOLVED pull_request / review rows as link-outs to the posted object, not overlay openers', async () => {
    const onOpen = vi.fn()
    mockDeliverables([
      art({
        id: 'rv1',
        kind: 'review',
        state: 'submitted',
        target: 'org/repo#18',
        url: 'https://gh/pull/18#pullrequestreview-9',
      }),
      art({
        id: 'pr1',
        kind: 'pull_request',
        state: 'open',
        target: 'org/repo#18',
        url: 'https://gh/pr',
      }),
    ])
    render(<DeliverableList conversationId="r1" onOpenApproval={onOpen} />)

    // The submitted review's row IS the link to the posted review on GitHub —
    // clicking it must not reopen the stale TF-side draft editor.
    const review = await screen.findByRole('link', { name: /Open review/ })
    expect(review).toHaveAttribute('href', 'https://gh/pull/18#pullrequestreview-9')
    const pr = screen.getByRole('link', { name: /Open pull request/ })
    expect(pr).toHaveAttribute('href', 'https://gh/pr')
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(onOpen).not.toHaveBeenCalled()
  })

  it('renders branch / issue / comment rows as plain link-outs, not overlay openers', async () => {
    const onOpen = vi.fn()
    mockDeliverables([
      art({ id: 'b1', kind: 'branch', target: 'org/repo', url: 'https://gh/branch' }),
      art({
        id: 'i1',
        kind: 'issue',
        provider: 'jira',
        state: 'created',
        target: 'SKY-1',
        url: 'https://jira/1',
      }),
    ])
    render(<DeliverableList conversationId="r1" onOpenApproval={onOpen} />)

    const branch = await screen.findByRole('link', { name: /Open branch/ })
    expect(branch).toHaveAttribute('href', 'https://gh/branch')
    // No overlay-opening buttons exist for these kinds.
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(onOpen).not.toHaveBeenCalled()
  })

  it('sorts the pending set to the top in the projection order, with a dismiss on those rows only', async () => {
    mockDeliverables([
      art({ id: 'b1', kind: 'branch', state: 'pushed', target: 'branch-row' }),
      art({ id: 'rv1', kind: 'review', state: 'pending', target: 'review-row' }),
      art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'pr-row' }),
    ])
    render(
      <DeliverableList
        conversationId="r1"
        onOpenApproval={vi.fn()}
        pendingDeliverableIds={['pr1', 'rv1']}
      />,
    )

    // The projection's order (draft PRs first, then ready reviews) leads; the
    // rest keep the fetch order below.
    const rows = await screen.findAllByRole('listitem')
    expect(rows[0]).toHaveTextContent('pr-row')
    expect(rows[1]).toHaveTextContent('review-row')
    expect(rows[2]).toHaveTextContent('branch-row')

    // Only the unresolved rows carry the in-place [x] — the branch never does.
    expect(screen.getByRole('button', { name: /dismiss pull request/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /dismiss review/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /dismiss branch/i })).not.toBeInTheDocument()
  })

  it('dismisses a pending row in place: POST, refetch, onResolved', async () => {
    const draft = art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' })
    const onResolved = vi.fn()
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...listBody([draft]) })
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({
        ok: true,
        ...listBody([{ ...draft, state: 'closed' }]),
      })
    vi.stubGlobal('fetch', fetchMock)
    render(
      <DeliverableList
        conversationId="r1"
        pendingDeliverableIds={['pr1']}
        onResolved={onResolved}
      />,
    )

    fireEvent.click(await screen.findByRole('button', { name: /dismiss pull request/i }))

    await waitFor(() => expect(onResolved).toHaveBeenCalled())
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/deliverables/pr1/dismiss',
      expect.objectContaining({ method: 'POST' }),
    )
    // The reloaded row shows its resolved state and, no longer actionable,
    // loses the [x] even before the conversation projection catches up.
    expect(await screen.findByText('closed')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /dismiss pull request/i })).not.toBeInTheDocument()
  })

  it('dismisses a review through its state field, not the PR verb', async () => {
    const review = art({ id: 'rv1', kind: 'review', state: 'pending', target: 'org/repo#18' })
    const onResolved = vi.fn()
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...listBody([review]) })
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({
        ok: true,
        ...listBody([{ ...review, state: 'dismissed' }]),
      })
    vi.stubGlobal('fetch', fetchMock)
    render(
      <DeliverableList
        conversationId="r1"
        pendingDeliverableIds={['rv1']}
        onResolved={onResolved}
      />,
    )

    fireEvent.click(await screen.findByRole('button', { name: /dismiss review/i }))

    await waitFor(() => expect(onResolved).toHaveBeenCalled())
    // Abandoning a staged review reaches nothing outside the process, so it is
    // a field write — the /dismiss verb is the draft PR's, and it closes a real
    // PR on GitHub.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/deliverables/rv1/review',
      expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ state: 'dismissed' }) }),
    )
    expect(await screen.findByText('dismissed')).toBeInTheDocument()
  })

  it('soft-refetches on a refreshKey change, keeping the stale rows until the new set lands', async () => {
    const draft = art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' })
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...listBody([draft]) })
      .mockResolvedValueOnce({
        ok: true,
        ...listBody([{ ...draft, state: 'open' }]),
      })
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<DeliverableList conversationId="r1" refreshKey="2:pr1" />)
    expect(await screen.findByText('draft')).toBeInTheDocument()

    rerender(<DeliverableList conversationId="r1" refreshKey="2:" />)
    expect(await screen.findByText('open')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
    // A soft refetch never resets to the loading state — that's conversationId-change only.
    expect(screen.queryByText(/Loading deliverables/)).not.toBeInTheDocument()
  })

  it('keeps the rows it has when a soft refetch fails, instead of blanking to the error', async () => {
    const draft = art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' })
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...listBody([draft]) })
      .mockResolvedValueOnce({
        ok: false,
        status: 500,
        text: () => Promise.resolve(''),
      })
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<DeliverableList conversationId="r1" refreshKey="2:pr1" />)
    expect(await screen.findByText('org/repo#18')).toBeInTheDocument()

    rerender(<DeliverableList conversationId="r1" refreshKey="2:" />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))

    // The rows were true a moment ago — a transient failure must not blank an
    // always-mounted list; the next set-shape change retries.
    expect(screen.getByText('org/repo#18')).toBeInTheDocument()
    expect(screen.queryByText(/Couldn't load this run's deliverables/)).not.toBeInTheDocument()
  })

  it('ignores the previous conversation’s in-flight response after a conversation change', async () => {
    const oldRow = art({ id: 'a-old', kind: 'branch', target: 'old-run-row' })
    let resolveOld!: (v: unknown) => void
    const fetchMock = vi
      .fn()
      // r1's list read hangs until we resolve it by hand, after the conversation has changed.
      .mockImplementationOnce(() => new Promise((r) => (resolveOld = r)))
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<DeliverableList conversationId="r1" />)

    // The conversation goes away (a cleared selection) with r1's fetch still in flight;
    // the falsy conversationId means no new load ever runs to supersede it.
    rerender(<DeliverableList conversationId="" />)
    resolveOld({ ok: true, ...listBody([oldRow]) })
    // Let the stale promise chain run to completion before asserting.
    await new Promise((r) => setTimeout(r, 0))

    // The reset must hold: the old conversation's deliverables never land in the new state.
    expect(screen.queryByText('old-run-row')).not.toBeInTheDocument()
    expect(screen.getByText(/Loading deliverables/)).toBeInTheDocument()
  })

  it('pages: a token from the first page loads the next one behind a Load more', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({
        ok: true,
        ...listBody([art({ id: 'b1', target: 'first-page-row' })], 'tok-2', 2),
      })
      .mockResolvedValueOnce({
        ok: true,
        ...listBody([art({ id: 'b2', target: 'second-page-row' })], '', 2),
      })
    vi.stubGlobal('fetch', fetchMock)
    render(<DeliverableList conversationId="r1" />)

    expect(await screen.findByText('first-page-row')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Load more (1 of 2)' }))

    expect(await screen.findByText('second-page-row')).toBeInTheDocument()
    expect(screen.getByText('first-page-row')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenLastCalledWith(
      '/api/agent/conversations/r1/deliverables/list',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ page_size: 200, page_token: 'tok-2' }),
      }),
    )
    expect(screen.queryByRole('button', { name: /Load more/ })).not.toBeInTheDocument()
  })

  it('shows a quiet empty state when the conversation produced no deliverables', async () => {
    mockDeliverables([])
    render(<DeliverableList conversationId="r1" />)
    expect(await screen.findByText(/No deliverables yet/)).toBeInTheDocument()
  })

  it('surfaces a load error, preferring the server error envelope', async () => {
    failingFetch({
      status: 500,
      jsonBody: { errors: [{ reason: 'INTERNAL', message: 'boom' }] },
    })
    render(<DeliverableList conversationId="r1" />)
    // The server's message reaches the user verbatim — no fallback prefix.
    await waitFor(() => expect(screen.getByText('boom')).toBeInTheDocument())
  })

  it('falls back to the caller sentence for a non-JSON error body', async () => {
    // The raw body ("<html>Bad Gateway</html>") must never reach the UI.
    failingFetch({ status: 502, textBody: '<html>Bad Gateway</html>' })
    render(<DeliverableList conversationId="r1" />)
    await waitFor(() =>
      expect(screen.getByText("Couldn't load this run's deliverables.")).toBeInTheDocument(),
    )
    expect(screen.queryByText(/Bad Gateway/)).not.toBeInTheDocument()
  })
})

// failingFetch stubs a non-ok Response shaped the way apiClient reads one:
// HttpError carries the body from text(), and httpErrorMessage parses the
// first message out of the server's error envelope. There is no clone()/json()
// path any more — the wrapper reads the body exactly once.
function failingFetch({
  status = 500,
  jsonBody,
  textBody = '',
}: {
  status?: number
  jsonBody?: unknown
  textBody?: string
}) {
  const body = jsonBody !== undefined ? JSON.stringify(jsonBody) : textBody
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: false,
      status,
      text: () => Promise.resolve(body),
    }),
  )
}
