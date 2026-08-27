import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ArtifactList from './ArtifactList'
import type { Artifact } from '../types'
import { jsonBody } from '../test/apiResponse'

// One fetch mock returning the given artifact list for GET …/artifacts.
function mockArtifacts(arts: Artifact[]) {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    ...jsonBody(arts),
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const art = (over: Partial<Artifact>): Artifact => ({
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

describe('ArtifactList', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('fetches the conversation-scoped endpoint and renders each artifact (kind / state / link)', async () => {
    const fetchMock = mockArtifacts([
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
    render(<ArtifactList conversationId="r1" onOpenApproval={vi.fn()} />)

    // Targets + state badges for both rows render once the fetch resolves.
    expect(await screen.findByText('org/repo#18')).toBeInTheDocument()
    expect(screen.getByText('org/repo')).toBeInTheDocument()
    expect(screen.getByText('pushed')).toBeInTheDocument()
    expect(screen.getByText('draft')).toBeInTheDocument()

    // The conversation-scoped read API was hit.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/agent/conversations/r1/artifacts',
      expect.anything(),
    )

    // The branch row links out to its url; the PR row carries an external link too.
    const links = screen.getAllByRole('link')
    const hrefs = links.map((l) => l.getAttribute('href'))
    expect(hrefs).toContain('https://gh/branch')
    expect(hrefs).toContain('https://gh/pr')
  })

  it('opens the approval overlay for pull_request and review rows by artifact id', async () => {
    const onOpen = vi.fn()
    mockArtifacts([
      art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' }),
      art({ id: 'rv1', kind: 'review', state: 'pending', target: 'org/repo#18' }),
    ])
    render(<ArtifactList conversationId="r1" onOpenApproval={onOpen} />)

    fireEvent.click(await screen.findByRole('button', { name: /Open pull request/ }))
    expect(onOpen).toHaveBeenCalledWith('pr', 'pr1')

    fireEvent.click(screen.getByRole('button', { name: /Open review/ }))
    expect(onOpen).toHaveBeenCalledWith('review', 'rv1')
  })

  it('renders RESOLVED pull_request / review rows as link-outs to the posted object, not overlay openers', async () => {
    const onOpen = vi.fn()
    mockArtifacts([
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
    render(<ArtifactList conversationId="r1" onOpenApproval={onOpen} />)

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
    mockArtifacts([
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
    render(<ArtifactList conversationId="r1" onOpenApproval={onOpen} />)

    const branch = await screen.findByRole('link', { name: /Open branch/ })
    expect(branch).toHaveAttribute('href', 'https://gh/branch')
    // No overlay-opening buttons exist for these kinds.
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(onOpen).not.toHaveBeenCalled()
  })

  it('sorts the pending set to the top in the projection order, with a dismiss on those rows only', async () => {
    mockArtifacts([
      art({ id: 'b1', kind: 'branch', state: 'pushed', target: 'branch-row' }),
      art({ id: 'rv1', kind: 'review', state: 'pending', target: 'review-row' }),
      art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'pr-row' }),
    ])
    render(
      <ArtifactList
        conversationId="r1"
        onOpenApproval={vi.fn()}
        pendingArtifactIds={['pr1', 'rv1']}
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
      .mockResolvedValueOnce({ ok: true, ...jsonBody([draft]) })
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({
        ok: true,
        ...jsonBody([{ ...draft, state: 'closed' }]),
      })
    vi.stubGlobal('fetch', fetchMock)
    render(
      <ArtifactList conversationId="r1" pendingArtifactIds={['pr1']} onResolved={onResolved} />,
    )

    fireEvent.click(await screen.findByRole('button', { name: /dismiss pull request/i }))

    await waitFor(() => expect(onResolved).toHaveBeenCalled())
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/artifacts/pr1/dismiss',
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
      .mockResolvedValueOnce({ ok: true, ...jsonBody([review]) })
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({
        ok: true,
        ...jsonBody([{ ...review, state: 'dismissed' }]),
      })
    vi.stubGlobal('fetch', fetchMock)
    render(
      <ArtifactList conversationId="r1" pendingArtifactIds={['rv1']} onResolved={onResolved} />,
    )

    fireEvent.click(await screen.findByRole('button', { name: /dismiss review/i }))

    await waitFor(() => expect(onResolved).toHaveBeenCalled())
    // Abandoning a staged review reaches nothing outside the process, so it is
    // a field write — the /dismiss verb is the draft PR's, and it closes a real
    // PR on GitHub.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/artifacts/rv1/review',
      expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ state: 'dismissed' }) }),
    )
    expect(await screen.findByText('dismissed')).toBeInTheDocument()
  })

  it('soft-refetches on a refreshKey change, keeping the stale rows until the new set lands', async () => {
    const draft = art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' })
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...jsonBody([draft]) })
      .mockResolvedValueOnce({
        ok: true,
        ...jsonBody([{ ...draft, state: 'open' }]),
      })
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<ArtifactList conversationId="r1" refreshKey="2:pr1" />)
    expect(await screen.findByText('draft')).toBeInTheDocument()

    rerender(<ArtifactList conversationId="r1" refreshKey="2:" />)
    expect(await screen.findByText('open')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
    // A soft refetch never resets to the loading state — that's conversationId-change only.
    expect(screen.queryByText(/Loading artifacts/)).not.toBeInTheDocument()
  })

  it('keeps the rows it has when a soft refetch fails, instead of blanking to the error', async () => {
    const draft = art({ id: 'pr1', kind: 'pull_request', state: 'draft', target: 'org/repo#18' })
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...jsonBody([draft]) })
      .mockResolvedValueOnce({
        ok: false,
        status: 500,
        text: () => Promise.resolve(''),
      })
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<ArtifactList conversationId="r1" refreshKey="2:pr1" />)
    expect(await screen.findByText('org/repo#18')).toBeInTheDocument()

    rerender(<ArtifactList conversationId="r1" refreshKey="2:" />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))

    // The rows were true a moment ago — a transient failure must not blank an
    // always-mounted list; the next set-shape change retries.
    expect(screen.getByText('org/repo#18')).toBeInTheDocument()
    expect(screen.queryByText(/Couldn't load this run's artifacts/)).not.toBeInTheDocument()
  })

  it('ignores the previous conversation’s in-flight response after a conversation change', async () => {
    const oldRow = art({ id: 'a-old', kind: 'branch', target: 'old-run-row' })
    let resolveOld!: (v: unknown) => void
    const fetchMock = vi
      .fn()
      // r1's GET hangs until we resolve it by hand, after the conversation has changed.
      .mockImplementationOnce(() => new Promise((r) => (resolveOld = r)))
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(<ArtifactList conversationId="r1" />)

    // The conversation goes away (a cleared selection) with r1's fetch still in flight;
    // the falsy conversationId means no new load ever runs to supersede it.
    rerender(<ArtifactList conversationId="" />)
    resolveOld({ ok: true, ...jsonBody([oldRow]) })
    // Let the stale promise chain run to completion before asserting.
    await new Promise((r) => setTimeout(r, 0))

    // The reset must hold: the old conversation's artifacts never land in the new state.
    expect(screen.queryByText('old-run-row')).not.toBeInTheDocument()
    expect(screen.getByText(/Loading artifacts/)).toBeInTheDocument()
  })

  it('shows a quiet empty state when the conversation produced no artifacts', async () => {
    mockArtifacts([])
    render(<ArtifactList conversationId="r1" />)
    expect(await screen.findByText(/No artifacts yet/)).toBeInTheDocument()
  })

  it('surfaces a load error, preferring the server error envelope', async () => {
    failingFetch({
      status: 500,
      jsonBody: { errors: [{ reason: 'INTERNAL', message: 'boom' }] },
    })
    render(<ArtifactList conversationId="r1" />)
    // The server's message reaches the user verbatim — no fallback prefix.
    await waitFor(() => expect(screen.getByText('boom')).toBeInTheDocument())
  })

  it('falls back to the caller sentence for a non-JSON error body', async () => {
    // The raw body ("<html>Bad Gateway</html>") must never reach the UI.
    failingFetch({ status: 502, textBody: '<html>Bad Gateway</html>' })
    render(<ArtifactList conversationId="r1" />)
    await waitFor(() =>
      expect(screen.getByText("Couldn't load this run's artifacts.")).toBeInTheDocument(),
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
