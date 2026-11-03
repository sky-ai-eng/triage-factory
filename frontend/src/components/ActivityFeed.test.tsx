import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ActivityFeed from './ActivityFeed'
import type { ActivityAction, ActivityFacet, ActivityObject } from '../types'
import { jsonBody, listBody } from '../test/apiResponse'

const action = (over: Partial<ActivityAction>): ActivityAction => ({
  id: 'x1',
  provider: 'github',
  action: 'pr_created',
  target: 'org/repo#1',
  url: 'https://example.test/1',
  performed_by: 'agent',
  credential: 'github_app',
  occurred_at: '2026-06-25T00:00:00Z',
  ...over,
})

const object = (over: Partial<ActivityObject>): ActivityObject => ({
  object_id: 'b2JqZWN0',
  object_kind: 'pull_request',
  provider: 'github',
  target: 'org/repo#9',
  name: '',
  url: 'https://example.test/9',
  last_action: 'pr_created',
  action_count: 1,
  run_count: 1,
  last_at: '2026-06-25T00:00:00Z',
  deliverable: null,
  ...over,
})

interface StubFacets {
  teams?: ActivityFacet[]
  actors?: ActivityFacet[]
}

// facetsReply is what a facets node answers: the actions node carries both
// cuts, the artifacts node by_team alone.
function facetsReply(url: string, f: StubFacets) {
  const teams = f.teams ?? []
  return url.includes('/artifacts/facets')
    ? { by_team: teams }
    : { by_actor: f.actors ?? [], by_team: teams }
}

// stubByLens answers the actions list route with action rows, the artifacts
// list route with object rows, and each facets node with facets, recording
// every request. The lenses are separate routes, so the stub branches on the
// path: a request can only reach the rows whose shape it can render.
function stubByLens(
  actionsRows: ActivityAction[],
  objectsRows: ActivityObject[] = [],
  facets: StubFacets = {},
) {
  const fetchMock = vi.fn().mockImplementation((url: string) => {
    const u = String(url)
    if (u.endsWith('/facets'))
      return Promise.resolve({ ok: true, ...jsonBody(facetsReply(u, facets)) })
    const rows: (ActivityAction | ActivityObject)[] = u.includes('/artifacts/list')
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
// there now, not in a query string. suffix keeps the requests to one kind of
// route ('/list' or '/facets').
function calledBodies(fetchMock: ReturnType<typeof vi.fn>, suffix = ''): Record<string, unknown>[] {
  return fetchMock.mock.calls
    .filter((c) => String(c[0]).endsWith(suffix))
    .map((c) => JSON.parse(String((c[1] as RequestInit)?.body ?? '{}')))
}

const facet = (value: string, count: number, name: string): ActivityFacet => ({
  value,
  count,
  name,
})

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
    // An agent row with no authorizing person reads as the agent alone.
    expect(screen.getAllByText('agent').length).toBeGreaterThan(0)

    // The first request addressed the actions resource and asked for the
    // default page size in the body.
    expect(calledUrls(fetchMock)[0]).toBe('/api/teams/t1/usage/actions/list')
    expect(calledBodies(fetchMock)[0]).toMatchObject({ page_size: 50 })
  })

  it('toggles to the Objects lens — reads the artifacts route and renders one row per object', async () => {
    const fetchMock = stubByLens(
      [action({ id: 'pr', target: 'org/repo#1' })],
      [
        object({
          object_id: 'YnJhbmNo',
          object_kind: 'branch',
          target: 'org/repo',
          url: 'https://gh/tree/fix',
          last_action: 'branch_pushed',
          action_count: 4,
          run_count: 3,
        }),
        object({
          object_id: 'cHI0Mg',
          target: 'org/repo#42',
          url: 'https://gh/pr42',
          deliverable: { id: 'd1', kind: 'pull_request', state: 'merged' },
        }),
      ],
    )

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    await screen.findByText('org/repo#1') // actions lens first

    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))

    expect(await screen.findByText('org/repo#42')).toBeInTheDocument()
    await waitFor(() =>
      expect(calledUrls(fetchMock)).toContain('/api/teams/t1/usage/artifacts/list'),
    )
    const links = screen.getAllByRole('link')
    const branch = links.find((l) => l.getAttribute('href') === 'https://gh/tree/fix')
    const pr = links.find((l) => l.getAttribute('href') === 'https://gh/pr42')
    // The branch three runs pushed is one row reading "3 runs", with its last
    // action's verb.
    expect(branch?.textContent).toContain('3 runs')
    expect(branch?.textContent).toContain('Branch pushed')
    expect(branch?.hasAttribute('data-deliverable')).toBe(false)
    // One run says nothing about runs; the PR that is a deliverable is
    // highlighted with the deliverable's state.
    expect(pr?.textContent).not.toMatch(/runs?\b/)
    expect(pr?.getAttribute('data-deliverable')).toBe('merged')
    expect(pr?.textContent).toContain('merged')
    // The team feed is one team: no chips.
    expect(screen.queryByTitle('Platform')).not.toBeInTheDocument()
  })

  it('names two branches of one repository by their branches', async () => {
    const branch = (name: string) =>
      object({
        object_id: `YnJhbmNo-${name}`,
        object_kind: 'branch',
        target: 'org/repo',
        name,
        url: `https://gh/tree/${name}`,
        last_action: 'branch_pushed',
      })
    stubByLens([], [branch('fix-ci'), branch('add-docs')])

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))

    const fix = await screen.findByRole('link', { name: /^branch fix-ci in org\/repo,/ })
    const docs = screen.getByRole('link', { name: /^branch add-docs in org\/repo,/ })
    expect(fix.getAttribute('href')).toBe('https://gh/tree/fix-ci')
    expect(docs.getAttribute('href')).toBe('https://gh/tree/add-docs')
    expect(fix.textContent).toContain('fix-ci org/repo')
    expect(docs.textContent).toContain('add-docs org/repo')
  })

  it('renders an object once, as the later page read it, when that page serves it again', async () => {
    // Offset paging over the latest action: a third run pushed the branch
    // between the two reads, so page 2 repeats it with the newer counts.
    const branch = object({
      object_id: 'YnJhbmNo',
      object_kind: 'branch',
      target: 'org/repo',
      run_count: 2,
    })
    const pages = [
      listBody([branch], 'tok-2', 3),
      listBody(
        [{ ...branch, run_count: 3 }, object({ object_id: 'cHI0Mg', target: 'org/repo#42' })],
        '',
        3,
      ),
    ]
    const fetchMock = vi.fn().mockImplementation((url: string) =>
      Promise.resolve({
        ok: true,
        ...(String(url).endsWith('/facets')
          ? jsonBody(facetsReply(String(url), {}))
          : String(url).includes('/artifacts/list')
            ? pages.shift()
            : listBody([])),
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))
    expect(await screen.findByText('org/repo')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /load more/i }))

    expect(await screen.findByText('org/repo#42')).toBeInTheDocument()
    expect(screen.getAllByText('org/repo')).toHaveLength(1)
    expect(screen.getByText('3 runs')).toBeInTheDocument()
    expect(screen.queryByText('2 runs')).not.toBeInTheDocument()
    // The header is the list's own total, not a count of the rows held.
    expect(screen.getByText('3 objects')).toBeInTheDocument()
  })

  it("folds an object's teams past three into one +N chip naming them", async () => {
    stubByLens(
      [],
      [
        object({
          object_id: 'YnJhbmNo',
          object_kind: 'branch',
          target: 'org/repo',
          url: 'https://gh/tree/fix',
          teams: [
            { id: 't1', name: 'Platform' },
            { id: 't2', name: 'Growth' },
            { id: 't3', name: 'Billing' },
            { id: 't4', name: 'Infra' },
          ],
        }),
      ],
    )
    render(<ActivityFeed basePath="/api/orgs/o1/usage" showTeam />)
    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))
    await screen.findByText('org/repo')

    const row = screen
      .getAllByRole('link')
      .find((l) => l.getAttribute('href') === 'https://gh/tree/fix')
    expect(row?.textContent).toContain('Platform')
    expect(row?.textContent).toContain('Growth')
    expect(row?.textContent).not.toContain('Billing')
    expect(screen.getByTitle('Billing, Infra').textContent).toBe('+2')
  })

  it('org Objects feed tags each object with every team that changed it', async () => {
    const fetchMock = stubByLens(
      [],
      [
        object({
          object_id: 'YnJhbmNo',
          object_kind: 'branch',
          target: 'org/repo',
          url: 'https://gh/tree/fix',
          run_count: 2,
          teams: [
            { id: 't1', name: 'Platform' },
            { id: 't2', name: 'Growth' },
          ],
        }),
        object({
          object_id: 'aXNzdWU',
          object_kind: 'issue',
          provider: 'jira',
          target: 'SKY-1',
          url: 'https://jira/SKY-1',
          last_action: 'issue_transitioned',
          teams: [{ id: 't2', name: 'Growth' }],
        }),
      ],
      { teams: [facet('t2', 2, 'Growth'), facet('t1', 1, 'Platform')] },
    )

    render(<ActivityFeed basePath="/api/orgs/o1/usage" showTeam />)
    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))
    expect(await screen.findByText('org/repo')).toBeInTheDocument()

    const branch = screen
      .getAllByRole('link')
      .find((l) => l.getAttribute('href') === 'https://gh/tree/fix')
    expect(branch?.textContent).toContain('Platform')
    expect(branch?.textContent).toContain('Growth')
    expect(branch?.textContent).toContain('2 runs')

    // The team options are the artifacts facets node's, in its order.
    await waitFor(() =>
      expect(
        Array.from((screen.getByLabelText('team') as HTMLSelectElement).options).map(
          (o) => o.textContent,
        ),
      ).toEqual(['All teams', 'Growth', 'Platform']),
    )
    expect(calledUrls(fetchMock)).toContain('/api/orgs/o1/usage/artifacts/facets')

    // Picking a team is a read: the list and the facets both carry team_id,
    // and the rows the server answers all render, a row naming only another
    // team included.
    fireEvent.change(screen.getByLabelText('team'), { target: { value: 't1' } })
    await waitFor(() =>
      expect(calledBodies(fetchMock, '/artifacts/list').some((b) => b.team_id === 't1')).toBe(true),
    )
    expect(calledBodies(fetchMock, '/artifacts/facets').some((b) => b.team_id === 't1')).toBe(true)
    await waitFor(() => expect(screen.getByLabelText('team')).toHaveValue('t1'))
    expect(screen.getByText('org/repo')).toBeInTheDocument()
    expect(screen.getByText('SKY-1')).toBeInTheDocument()
  })

  it('sends the Objects filters as the artifacts route spells them', async () => {
    const fetchMock = stubByLens([], [object({})])
    render(<ActivityFeed basePath="/api/orgs/o1/usage" showTeam />)
    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))
    await screen.findByText('org/repo#9')

    fireEvent.change(screen.getByLabelText('kind'), { target: { value: 'branch' } })
    await waitFor(() =>
      expect(calledBodies(fetchMock).some((b) => b.object_kind === 'branch')).toBe(true),
    )
    fireEvent.change(screen.getByLabelText('provider'), { target: { value: 'linear' } })
    await waitFor(() =>
      expect(calledBodies(fetchMock).some((b) => b.provider === 'linear')).toBe(true),
    )
    // The lens has no state filter, and never sends the deliverables vocabulary.
    expect(screen.queryByLabelText('state')).not.toBeInTheDocument()
    expect(calledBodies(fetchMock).every((b) => !('state' in b) && !('kind' in b))).toBe(true)
  })

  it('holds the loaded rows while the next request is in flight (no skeleton collapse)', async () => {
    // Park the Objects request so the mid-switch render is observable: the feed
    // must keep the Actions rows on screen rather than blanking to the skeleton,
    // which is what made the section's height jump on every toggle.
    let releaseObjects: (rows: ActivityObject[]) => void = () => {}
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

    releaseObjects([object({ target: 'org/repo#42' })])
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

    // The actions body never carries the Objects lens's filter — the routes
    // decode strictly, so a stray `object_kind` would be a 400, not a no-op.
    expect(calledBodies(fetchMock).every((b) => !('object_kind' in b))).toBe(true)
  })

  it('org Actions feed shows the owning team + actor, and filters by team on the server', async () => {
    const fetchMock = stubByLens(
      [
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
          performed_by: 'person',
          action: 'pr_marked_ready',
          target: 'org/repo#2',
        }),
        action({
          id: 'c',
          team_id: 't2',
          team_name: 'Growth',
          actor_user_id: 'u3',
          performed_by: 'system',
          action: 'issue_transitioned',
          provider: 'jira',
          target: 'SKY-3',
        }),
        action({
          id: 'd',
          team_id: 't1',
          team_name: 'Platform',
          actor_user_id: 'u4',
          target: 'org/repo#4',
        }),
      ],
      [],
      {
        teams: [facet('t2', 2, 'Growth'), facet('t1', 2, 'Platform')],
        actors: [facet('u1', 1, 'Ada'), facet('u2', 1, 'Bo')],
      },
    )

    render(<ActivityFeed basePath="/api/orgs/o1/usage" showTeam />)

    // Both rows present, tagged with team + the authorizing actor.
    expect(await screen.findByText('org/repo#1')).toBeInTheDocument()
    expect(screen.getByText('org/repo#2')).toBeInTheDocument()
    expect(screen.getAllByText('Platform').length).toBeGreaterThan(0)
    // The agent row of Ada's run reads as the agent, authorized by Ada; Bo's
    // own act reads as Bo. ('Ada' and 'Bo' are also actor-filter options.)
    expect(screen.getByText('agent · Ada')).toBeInTheDocument()
    expect(screen.getByTitle('Agent action, authorized by Ada')).toBeInTheDocument()
    expect(screen.getByTitle('Performed by Bo')).toBeInTheDocument()
    // TF's own row reads as TF, never as a person.
    expect(screen.getByTitle('Performed by Triage Factory itself').textContent).toBe('TF')
    // An agent row whose authorizer has no resolved name says a person
    // authorized it without naming one.
    expect(screen.getByTitle('Agent action, authorized by a person').textContent).toBe('agent')

    // Picking a team sends team_id to the list and the facets; the feed
    // renders what the server answers, so the stub's t2 rows stay on screen.
    await waitFor(() => expect(screen.getByLabelText('team')).toBeInTheDocument())
    fireEvent.change(screen.getByLabelText('team'), { target: { value: 't1' } })
    await waitFor(() =>
      expect(calledBodies(fetchMock, '/actions/list').some((b) => b.team_id === 't1')).toBe(true),
    )
    expect(calledBodies(fetchMock, '/actions/facets').some((b) => b.team_id === 't1')).toBe(true)
    await waitFor(() => expect(screen.getByLabelText('team')).toHaveValue('t1'))
    expect(screen.getByText('org/repo#1')).toBeInTheDocument()
    expect(screen.getByText('org/repo#2')).toBeInTheDocument()

    // The actor options are the facets node's, and a pick is the list's
    // `actor` field.
    fireEvent.change(screen.getByLabelText('actor'), { target: { value: 'u2' } })
    await waitFor(() =>
      expect(
        calledBodies(fetchMock, '/actions/list').some(
          (b) => b.actor === 'u2' && b.team_id === 't1',
        ),
      ).toBe(true),
    )
    expect(screen.getByText('org/repo#1')).toBeInTheDocument()
  })

  it('team Actions feed offers its actors from the team facets node, and never sends team_id', async () => {
    const fetchMock = stubByLens(
      [action({ id: 'a', actor_user_id: 'u1', target: 'org/repo#1' })],
      [],
      {
        teams: [facet('t1', 1, 'Platform')],
        actors: [facet('u1', 1, 'Ada')],
      },
    )
    render(<ActivityFeed basePath="/api/teams/t1/usage" />)
    await screen.findByText('org/repo#1')
    await waitFor(() => expect(screen.getByLabelText('actor')).toBeInTheDocument())
    expect(calledUrls(fetchMock)).toContain('/api/teams/t1/usage/actions/facets')
    // The team feed is one team: no team select.
    expect(screen.queryByLabelText('team')).not.toBeInTheDocument()

    fireEvent.change(screen.getByLabelText('actor'), { target: { value: 'u1' } })
    await waitFor(() =>
      expect(calledBodies(fetchMock, '/actions/list').some((b) => b.actor === 'u1')).toBe(true),
    )
    expect(calledBodies(fetchMock).every((b) => !('team_id' in b))).toBe(true)
    // Switching lens drops the actor: the artifacts route has no such field.
    fireEvent.click(screen.getByRole('tab', { name: /objects/i }))
    await waitFor(() =>
      expect(calledUrls(fetchMock)).toContain('/api/teams/t1/usage/artifacts/list'),
    )
    expect(calledBodies(fetchMock, '/artifacts/list').every((b) => !('actor' in b))).toBe(true)
  })

  it('pages with "load more", sending the server\'s token back for the next page', async () => {
    const page1 = Array.from({ length: 50 }, (_, i) =>
      action({ id: `p1-${i}`, target: `org/repo#${i}` }),
    )
    const page2 = [action({ id: 'p2-0', target: 'org/repo#999' })]
    const pages = [listBody(page1, 'tok-2', 51), listBody(page2, '', 51)]
    const fetchMock = vi.fn().mockImplementation((url: string) =>
      Promise.resolve({
        ok: true,
        ...(String(url).endsWith('/facets')
          ? jsonBody(facetsReply(String(url), {}))
          : pages.shift()),
      }),
    )
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
