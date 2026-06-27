import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import BotActivityFeed from './BotActivityFeed'
import type { ActivityArtifact } from '../types'

const art = (over: Partial<ActivityArtifact>): ActivityArtifact => ({
  id: 'a1',
  kind: 'pull_request',
  provider: 'github',
  state: 'merged',
  target: 'org/repo#1',
  external_id: '1',
  url: 'https://example.test/1',
  details: null,
  created_at: '2026-06-25T00:00:00Z',
  ...over,
})

// stubFetch returns the given rows for every call (and records the request urls).
function stubFetch(rows: ActivityArtifact[]) {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve(rows) })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function calledUrls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0]))
}

describe('BotActivityFeed', () => {
  beforeEach(() => vi.restoreAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  it('renders the artifact history as link-out rows (target + state + url), with a default page request', async () => {
    const fetchMock = stubFetch([
      art({
        id: 'pr1',
        kind: 'pull_request',
        state: 'merged',
        target: 'org/repo#9',
        url: 'https://gh/pr9',
      }),
      art({
        id: 'b1',
        kind: 'branch',
        provider: 'git',
        state: 'pushed',
        target: 'org/repo',
        url: 'https://gh/b',
      }),
    ])

    render(<BotActivityFeed baseUrl="/api/usage/teams/t1/activity" />)

    // Rows render once the feed loads — terminal 'merged' included (it's a log).
    expect(await screen.findByText('org/repo#9')).toBeInTheDocument()

    // Feed rows are link-outs (no approval overlay), even the PR row; each row
    // carries its state badge (asserted on the row, not the filter's options,
    // which also list every state string).
    const links = screen.getAllByRole('link')
    const prLink = links.find((l) => l.getAttribute('href') === 'https://gh/pr9')
    const branchLink = links.find((l) => l.getAttribute('href') === 'https://gh/b')
    expect(prLink?.textContent).toContain('merged')
    expect(branchLink?.textContent).toContain('pushed')

    // The team activity endpoint was hit with the default page size.
    expect(calledUrls(fetchMock)[0]).toContain('/api/usage/teams/t1/activity')
    expect(calledUrls(fetchMock)[0]).toContain('limit=50')
  })

  it('shows a zero state when the history is empty', async () => {
    stubFetch([])
    render(<BotActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    expect(await screen.findByText(/no bot activity/i)).toBeInTheDocument()
  })

  it('drives a server-side query param when a filter changes', async () => {
    const fetchMock = stubFetch([art({ id: 'pr1', target: 'org/repo#9' })])
    render(<BotActivityFeed baseUrl="/api/usage/org/activity" showTeam />)
    await screen.findByText('org/repo#9')

    // Picking a kind refetches with ?kind=… (the backend filters + pages).
    fireEvent.change(screen.getByLabelText('kind'), { target: { value: 'pull_request' } })
    await waitFor(() =>
      expect(calledUrls(fetchMock).some((u) => u.includes('kind=pull_request'))).toBe(true),
    )

    // Picking a time range adds ?since=… (the range tab → a date bound).
    fireEvent.click(screen.getByText('30D'))
    await waitFor(() => expect(calledUrls(fetchMock).some((u) => u.includes('since='))).toBe(true))
  })

  it('org feed shows the owning team per row and narrows in-memory by the team filter', async () => {
    stubFetch([
      art({ id: 'a', team_id: 't1', team_name: 'Platform', target: 'org/repo#1' }),
      art({ id: 'b', team_id: 't2', team_name: 'Growth', target: 'org/repo#2' }),
    ])

    render(<BotActivityFeed baseUrl="/api/usage/org/activity" showTeam />)

    // Both rows present, each tagged with its team.
    expect(await screen.findByText('org/repo#1')).toBeInTheDocument()
    expect(screen.getByText('org/repo#2')).toBeInTheDocument()
    expect(screen.getAllByText('Platform').length).toBeGreaterThan(0)

    // Scope to team t1 (client-side) — the t2 row drops without a refetch.
    fireEvent.change(screen.getByLabelText('team'), { target: { value: 't1' } })
    expect(screen.getByText('org/repo#1')).toBeInTheDocument()
    expect(screen.queryByText('org/repo#2')).not.toBeInTheDocument()
  })

  it('pages with "load more" when a full page comes back, appending the next page', async () => {
    // A full first page (PAGE_SIZE rows) flips hasMore on, so the button shows.
    const page1 = Array.from({ length: 50 }, (_, i) =>
      art({ id: `p1-${i}`, target: `org/repo#${i}` }),
    )
    const page2 = [art({ id: 'p2-0', target: 'org/repo#999' })]
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, json: () => Promise.resolve(page1) })
      .mockResolvedValueOnce({ ok: true, json: () => Promise.resolve(page2) })
    vi.stubGlobal('fetch', fetchMock)

    render(<BotActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    await screen.findByText('org/repo#0')

    const more = screen.getByRole('button', { name: /load more/i })
    fireEvent.click(more)

    // The appended page's row shows, and the second request carried offset=50.
    expect(await screen.findByText('org/repo#999')).toBeInTheDocument()
    expect(calledUrls(fetchMock).some((u) => u.includes('offset=50'))).toBe(true)
  })

  it('surfaces a load error with context', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        text: () => Promise.resolve(''),
        clone: () => ({ json: () => Promise.resolve({ error: 'boom' }) }),
      }),
    )
    render(<BotActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    await waitFor(() =>
      expect(screen.getByText(/Couldn't load bot activity: boom/)).toBeInTheDocument(),
    )
  })
})
