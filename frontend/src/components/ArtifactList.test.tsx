import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ArtifactList from './ArtifactList'
import type { Artifact } from '../types'

// One fetch mock returning the given artifact list for GET …/artifacts.
function mockArtifacts(arts: Artifact[]) {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve(arts),
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
  afterEach(() => vi.unstubAllGlobals())

  it('fetches the run-scoped endpoint and renders each artifact (kind / state / link)', async () => {
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
    render(<ArtifactList runId="r1" onOpenApproval={vi.fn()} />)

    // Targets + state badges for both rows render once the fetch resolves.
    expect(await screen.findByText('org/repo#18')).toBeInTheDocument()
    expect(screen.getByText('org/repo')).toBeInTheDocument()
    expect(screen.getByText('pushed')).toBeInTheDocument()
    expect(screen.getByText('draft')).toBeInTheDocument()

    // The run-scoped read API was hit.
    expect(fetchMock).toHaveBeenCalledWith('/api/agent/runs/r1/artifacts')

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
    render(<ArtifactList runId="r1" onOpenApproval={onOpen} />)

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
    render(<ArtifactList runId="r1" onOpenApproval={onOpen} />)

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
    render(<ArtifactList runId="r1" onOpenApproval={onOpen} />)

    const branch = await screen.findByRole('link', { name: /Open branch/ })
    expect(branch).toHaveAttribute('href', 'https://gh/branch')
    // No overlay-opening buttons exist for these kinds.
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(onOpen).not.toHaveBeenCalled()
  })

  it('shows a quiet empty state when the run produced no artifacts', async () => {
    mockArtifacts([])
    render(<ArtifactList runId="r1" />)
    expect(await screen.findByText(/No artifacts yet/)).toBeInTheDocument()
  })

  it('surfaces a load error with context, preferring the server JSON error', async () => {
    failingFetch({ status: 500, jsonBody: { error: 'boom' } })
    render(<ArtifactList runId="r1" />)
    // Context is preserved ("Couldn't load artifacts: …"), not a bare "boom".
    await waitFor(() =>
      expect(screen.getByText(/Couldn't load artifacts: boom/)).toBeInTheDocument(),
    )
  })

  it('falls back to a non-JSON error body', async () => {
    failingFetch({ status: 502, textBody: '<html>Bad Gateway</html>' })
    render(<ArtifactList runId="r1" />)
    await waitFor(() =>
      expect(
        screen.getByText(/Couldn't load artifacts: <html>Bad Gateway<\/html>/),
      ).toBeInTheDocument(),
    )
  })
})

// failingFetch stubs fetch with a non-ok Response shaped enough for readError:
// a clone().json() path (the server's JSON `error`) and a text() fallback for
// non-JSON bodies.
function failingFetch({
  status = 500,
  jsonBody,
  textBody = '',
}: {
  status?: number
  jsonBody?: unknown
  textBody?: string
}) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: false,
      status,
      text: () => Promise.resolve(textBody),
      clone: () => ({
        json: () =>
          jsonBody !== undefined
            ? Promise.resolve(jsonBody)
            : Promise.reject(new Error('not json')),
      }),
    }),
  )
}
