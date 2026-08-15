import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ActivityFeed from './ActivityFeed'
import type { ActivityAction, ActivityArtifact } from '../types'
import { jsonBody } from '../test/apiResponse'

const action = (over: Partial<ActivityAction>): ActivityAction => ({
  id: 'x1',
  provider: 'github',
  action: 'pr_created',
  target: 'org/repo#1',
  url: 'https://example.test/1',
  credential: 'github_app',
  occurred_at: '2026-06-25T00:00:00Z',
  ...over,
})

const artifact = (over: Partial<ActivityArtifact>): ActivityArtifact => ({
  id: 'a1',
  kind: 'pull_request',
  provider: 'github',
  state: 'merged',
  target: 'org/repo#9',
  external_id: '9',
  url: 'https://example.test/9',
  details: null,
  created_at: '2026-06-25T00:00:00Z',
  ...over,
})

// stubByView returns action rows for ?view=actions requests and artifact rows for
// ?view=objects, recording every request url. The default lens is actions, so the
// first load hits the actions branch; toggling to Objects refetches the other.
function stubByView(actionsRows: ActivityAction[], objectsRows: ActivityArtifact[] = []) {
  const fetchMock = vi.fn().mockImplementation((url: string) => {
    const rows = String(url).includes('view=objects') ? objectsRows : actionsRows
    return Promise.resolve({ ok: true, ...jsonBody(rows) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function calledUrls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0]))
}

describe('ActivityFeed', () => {
  beforeEach(() => vi.restoreAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  it('defaults to the Actions lens: renders verb + target + actor rows, requesting view=actions', async () => {
    const fetchMock = stubByView([
      action({ id: 'pr', action: 'pr_created', target: 'org/repo#9', url: 'https://gh/pr9' }),
      action({
        id: 'mv',
        action: 'issue_transitioned',
        provider: 'jira',
        target: 'SKY-1',
        from_state: 'To Do',
        to_state: 'In Progress',
        url: '',
      }),
    ])

    render(<ActivityFeed baseUrl="/api/usage/teams/t1/activity" />)

    // The action's target + humanized verb render once the feed loads (the verb
    // also labels an action-filter <option>, so assert on the row via getAllByText).
    expect(await screen.findByText('org/repo#9')).toBeInTheDocument()
    expect(screen.getAllByText('PR opened').length).toBeGreaterThan(0)
    // The transition row shows its from→to endpoints.
    expect(screen.getByText('SKY-1')).toBeInTheDocument()
    expect(screen.getByText('In Progress')).toBeInTheDocument()
    // A team-feed row with no resolved actor reads as a bot action.
    expect(screen.getAllByText('bot').length).toBeGreaterThan(0)

    // The first request selected the actions lens + the default page size.
    expect(calledUrls(fetchMock)[0]).toContain('/api/usage/teams/t1/activity')
    expect(calledUrls(fetchMock)[0]).toContain('view=actions')
    expect(calledUrls(fetchMock)[0]).toContain('limit=50')
  })

  it('toggles to the Objects lens — refetches view=objects and renders artifact rows', async () => {
    const fetchMock = stubByView(
      [action({ id: 'pr', target: 'org/repo#1' })],
      [artifact({ id: 'art', target: 'org/repo#42', url: 'https://gh/pr42', state: 'merged' })],
    )

    render(<ActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    await screen.findByText('org/repo#1') // actions lens first

    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))

    // The Objects lens row (an artifact) appears, and a request carried view=objects.
    expect(await screen.findByText('org/repo#42')).toBeInTheDocument()
    await waitFor(() =>
      expect(calledUrls(fetchMock).some((u) => u.includes('view=objects'))).toBe(true),
    )
    // The merged artifact's state badge rides its row (terminal states included).
    const prLink = screen
      .getAllByRole('link')
      .find((l) => l.getAttribute('href') === 'https://gh/pr42')
    expect(prLink?.textContent).toContain('merged')
  })

  it('holds the loaded rows while the next request is in flight (no skeleton collapse)', async () => {
    // Park the Objects request so the mid-switch render is observable: the feed
    // must keep the Actions rows on screen rather than blanking to the skeleton,
    // which is what made the section's height jump on every toggle.
    let releaseObjects: (rows: ActivityArtifact[]) => void = () => {}
    const fetchMock = vi.fn().mockImplementation((url: string) => {
      if (String(url).includes('view=objects')) {
        return new Promise((resolve) => {
          releaseObjects = (rows) => resolve({ ok: true, ...jsonBody(rows) })
        })
      }
      return Promise.resolve({
        ok: true,
        ...jsonBody([action({ id: 'pr', target: 'org/repo#1' })]),
      })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<ActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    await screen.findByText('org/repo#1')

    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))
    // Mid-switch: the held row is still mounted and no skeleton replaced it.
    expect(screen.getByText('org/repo#1')).toBeInTheDocument()
    expect(screen.getByText(/1 action/)).toBeInTheDocument()

    releaseObjects([artifact({ id: 'art', target: 'org/repo#42' })])
    expect(await screen.findByText('org/repo#42')).toBeInTheDocument()
    expect(screen.queryByText('org/repo#1')).not.toBeInTheDocument()
  })

  it('shows a zero state when the log is empty', async () => {
    stubByView([])
    render(<ActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    expect(await screen.findByText(/no actions/i)).toBeInTheDocument()
  })

  it('drives a server-side query param when the action filter changes', async () => {
    const fetchMock = stubByView([action({ id: 'pr', target: 'org/repo#9' })])
    render(<ActivityFeed baseUrl="/api/usage/org/activity" showTeam />)
    await screen.findByText('org/repo#9')

    // Picking an action refetches with ?action=… (the backend filters + pages).
    fireEvent.change(screen.getByLabelText('action'), { target: { value: 'pr_created' } })
    await waitFor(() =>
      expect(calledUrls(fetchMock).some((u) => u.includes('action=pr_created'))).toBe(true),
    )

    // Picking a time range adds ?since=… (the range tab → a date bound).
    fireEvent.click(screen.getByText('30D'))
    await waitFor(() => expect(calledUrls(fetchMock).some((u) => u.includes('since='))).toBe(true))
  })

  it('org Actions feed shows the owning team + actor, and narrows in-memory by team', async () => {
    stubByView([
      action({
        id: 'a',
        team_id: 't1',
        team_name: 'Platform',
        actor_user_id: 'u1',
        actor_name: 'Ada',
        target: 'org/repo#1',
      }),
      action({
        id: 'b',
        team_id: 't2',
        team_name: 'Growth',
        actor_user_id: 'u2',
        actor_name: 'Bo',
        target: 'org/repo#2',
      }),
    ])

    render(<ActivityFeed baseUrl="/api/usage/org/activity" showTeam />)

    // Both rows present, tagged with team + the authorizing actor.
    expect(await screen.findByText('org/repo#1')).toBeInTheDocument()
    expect(screen.getByText('org/repo#2')).toBeInTheDocument()
    expect(screen.getAllByText('Platform').length).toBeGreaterThan(0)
    // 'Ada' is both the row's actor chip and an actor-filter <option>.
    expect(screen.getAllByText('Ada').length).toBeGreaterThan(0)

    // Scope to team t1 (client-side) — the t2 row drops without a refetch.
    fireEvent.change(screen.getByLabelText('team'), { target: { value: 't1' } })
    expect(screen.getByText('org/repo#1')).toBeInTheDocument()
    expect(screen.queryByText('org/repo#2')).not.toBeInTheDocument()
  })

  it('pages with "load more" when a full page comes back, appending the next page', async () => {
    const page1 = Array.from({ length: 50 }, (_, i) =>
      action({ id: `p1-${i}`, target: `org/repo#${i}` }),
    )
    const page2 = [action({ id: 'p2-0', target: 'org/repo#999' })]
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...jsonBody(page1) })
      .mockResolvedValueOnce({ ok: true, ...jsonBody(page2) })
    vi.stubGlobal('fetch', fetchMock)

    render(<ActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    await screen.findByText('org/repo#0')

    fireEvent.click(screen.getByRole('button', { name: /load more/i }))

    expect(await screen.findByText('org/repo#999')).toBeInTheDocument()
    expect(calledUrls(fetchMock).some((u) => u.includes('offset=50'))).toBe(true)
  })

  it('surfaces a load error with context', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        text: () => Promise.resolve(JSON.stringify({ error: 'boom' })),
      }),
    )
    render(<ActivityFeed baseUrl="/api/usage/teams/t1/activity" />)
    // The server's message reaches the user verbatim — no fallback prefix.
    await waitFor(() => expect(screen.getByText('boom')).toBeInTheDocument())
  })
})
