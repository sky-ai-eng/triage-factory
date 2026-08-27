import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ActivityFeed from './ActivityFeed'
import type { ActivityAction, ActivityArtifact } from '../types'
import { listBody } from '../test/apiResponse'

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

// stubByLens answers the actions list route with action rows and the artifacts
// list route with artifact rows, recording every request. The two lenses are
// two routes now, so the stub branches on the path rather than on a ?view= —
// which is exactly the property the split bought: a request can only reach the
// rows whose shape it can render.
function stubByLens(actionsRows: ActivityAction[], objectsRows: ActivityArtifact[] = []) {
  const fetchMock = vi.fn().mockImplementation((url: string) => {
    const rows: (ActivityAction | ActivityArtifact)[] = String(url).includes('/artifacts/list')
      ? objectsRows
      : actionsRows
    return Promise.resolve({ ok: true, ...listBody(rows) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function calledUrls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0]))
}

// calledBodies parses each recorded request's JSON body — the filters live
// there now, not in a query string.
function calledBodies(fetchMock: ReturnType<typeof vi.fn>): Record<string, unknown>[] {
  return fetchMock.mock.calls.map((c) => JSON.parse(String((c[1] as RequestInit)?.body ?? '{}')))
}

describe('ActivityFeed', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('defaults to the Actions lens: renders verb + target + actor rows, reading the actions route', async () => {
    const fetchMock = stubByLens([
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

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)

    // The action's target + humanized verb render once the feed loads (the verb
    // also labels an action-filter <option>, so assert on the row via getAllByText).
    expect(await screen.findByText('org/repo#9')).toBeInTheDocument()
    expect(screen.getAllByText('PR opened').length).toBeGreaterThan(0)
    // The transition row shows its from→to endpoints.
    expect(screen.getByText('SKY-1')).toBeInTheDocument()
    expect(screen.getByText('In Progress')).toBeInTheDocument()
    // A team-feed row with no resolved actor reads as a bot action.
    expect(screen.getAllByText('bot').length).toBeGreaterThan(0)

    // The first request addressed the actions resource and asked for the
    // default page size in the body.
    expect(calledUrls(fetchMock)[0]).toBe('/api/teams/t1/usage/actions/list')
    expect(calledBodies(fetchMock)[0]).toMatchObject({ page_size: 50 })
  })

  it('toggles to the Objects lens — reads the artifacts route and renders artifact rows', async () => {
    const fetchMock = stubByLens(
      [action({ id: 'pr', target: 'org/repo#1' })],
      [artifact({ id: 'art', target: 'org/repo#42', url: 'https://gh/pr42', state: 'merged' })],
    )

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    await screen.findByText('org/repo#1') // actions lens first

    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))

    // The Objects lens row (an artifact) appears, and a request hit the
    // artifacts route.
    expect(await screen.findByText('org/repo#42')).toBeInTheDocument()
    await waitFor(() =>
      expect(calledUrls(fetchMock)).toContain('/api/teams/t1/usage/artifacts/list'),
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
      if (String(url).includes('/artifacts/list')) {
        return new Promise((resolve) => {
          releaseObjects = (rows) => resolve({ ok: true, ...listBody(rows) })
        })
      }
      return Promise.resolve({
        ok: true,
        ...listBody([action({ id: 'pr', target: 'org/repo#1' })]),
      })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
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
    stubByLens([])
    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    expect(await screen.findByText(/no actions/i)).toBeInTheDocument()
  })

  it('drives a server-side filter in the body when the action filter changes', async () => {
    const fetchMock = stubByLens([action({ id: 'pr', target: 'org/repo#9' })])
    render(<ActivityFeed basePath="/api/orgs/o1/usage" showTeam />)
    await screen.findByText('org/repo#9')

    // Picking an action refetches with action in the body (the backend filters
    // + pages).
    fireEvent.change(screen.getByLabelText('action'), { target: { value: 'pr_created' } })
    await waitFor(() =>
      expect(calledBodies(fetchMock).some((b) => b.action === 'pr_created')).toBe(true),
    )

    // Picking a time range adds `since` (the range tab → a date bound).
    fireEvent.click(screen.getByText('30D'))
    await waitFor(() =>
      expect(calledBodies(fetchMock).some((b) => typeof b.since === 'string')).toBe(true),
    )

    // The actions body never carries an artifacts-only filter — the routes
    // decode strictly, so a stray `state` would be a 400, not a no-op.
    expect(calledBodies(fetchMock).every((b) => !('state' in b) && !('kind' in b))).toBe(true)
  })

  it('org Actions feed shows the owning team + actor, and narrows in-memory by team', async () => {
    stubByLens([
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

    render(<ActivityFeed basePath="/api/orgs/o1/usage" showTeam />)

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

  it('pages with "load more", sending the server\'s token back for the next page', async () => {
    const page1 = Array.from({ length: 50 }, (_, i) =>
      action({ id: `p1-${i}`, target: `org/repo#${i}` }),
    )
    const page2 = [action({ id: 'p2-0', target: 'org/repo#999' })]
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...listBody(page1, 'tok-2', 51) })
      .mockResolvedValueOnce({ ok: true, ...listBody(page2, '', 51) })
    vi.stubGlobal('fetch', fetchMock)

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    await screen.findByText('org/repo#0')

    fireEvent.click(screen.getByRole('button', { name: /load more/i }))

    expect(await screen.findByText('org/repo#999')).toBeInTheDocument()
    // The next page is addressed by the token the server minted, not by an
    // offset the client counted — the client never computes a position.
    expect(calledBodies(fetchMock).some((b) => b.page_token === 'tok-2')).toBe(true)
  })

  it('surfaces a load error with context', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        text: () =>
          Promise.resolve(JSON.stringify({ errors: [{ reason: 'CONFLICT', message: 'boom' }] })),
      }),
    )
    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    // The server's message reaches the user verbatim — no fallback prefix.
    await waitFor(() => expect(screen.getByText('boom')).toBeInTheDocument())
  })
})
