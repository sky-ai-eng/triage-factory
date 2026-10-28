import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import GitHubSource from './GitHubSource'
import JiraSource from './JiraSource'
import SlackSource from './SlackSource'
import LinearSource from './LinearSource'
import { jsonBody } from '../../test/apiResponse'

// The source pages, each mounted against a stubbed API.
//
// What is worth pinning is the honesty rule, because it is the thing a
// screenshot cannot check and the thing most likely to be "fixed" later by
// somebody filling a dash with a zero: EVERY FIGURE IS EITHER REAL OR AN EM
// DASH. A figure is a dash until the team activity node answers, and stays
// one where the node cannot define the measurement at all — Slack's mention
// count has no tracked set to scope it. A zero would be a claim about a
// team's week.
//
// The rest is the wiring a member must never see, and the set-replace each
// verb resolves to.

// The availability read, controllable per test. The real hook keeps a
// module-level per-org cache, so letting it fetch would leak one test's
// answer into every later one; a mutable map gives each test its own truth.
const sources = vi.hoisted(() => ({ state: {} as Record<string, string | undefined> }))
vi.mock('../../hooks/useEventSources', () => ({
  useEventSources: () => ({
    sources: [],
    loaded: true,
    stateOf: (kind: string) => sources.state[kind],
    canProduce: (kind: string) => sources.state[kind] === undefined,
  }),
}))

vi.mock('../../contexts/OrgContext', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../contexts/OrgContext')>()
  return { ...actual, useActiveOrgId: () => 'org-1' }
})

const BODY = {
  teamId: 't1',
  teamName: 'platform',
  isAdmin: true,
  onBack: () => {},
}

/** A UTC calendar day `n` days back, in the node's own date grammar. */
const day = (n: number) => new Date(Date.now() - n * 86400_000).toISOString().slice(0, 10)

// The team activity node's answer, where a test wants the figures filled.
// Slack's null events is the payload's point: the count is undefined (no
// tracked set), which must render as a dash and an unanswered chart.
const ACTIVITY = {
  team_id: 't1',
  by_day: [{ date: day(1), events: 6, tasks: 2 }],
  by_day_source: [
    { date: day(1), source: 'github', events: 6, tasks: 2 },
    { date: day(3), source: 'slack', events: 0, tasks: 1 },
  ],
  by_source: [
    {
      source: 'github',
      events: 6,
      tasks: 2,
      runs: 3,
      last_poll_at: new Date(Date.now() - 40_000).toISOString(),
    },
    { source: 'jira', events: 0, tasks: 0, runs: 0, last_poll_at: null },
    { source: 'slack', events: null, tasks: 1, runs: 0, last_poll_at: null },
  ],
  // Per unit, keyed by the id each page holds: the GitHub page's registry
  // ids, the Jira page's registry rows, the Linear page's team ids. A quiet
  // watched unit is a row of zeros, never absent.
  by_tracked: [
    {
      source: 'github',
      unit_id: 'r1',
      events: 6,
      tasks: 2,
      runs: 3,
      last_event_at: new Date(Date.now() - 4 * 60000).toISOString(),
    },
    { source: 'github', unit_id: 'r2', events: 0, tasks: 0, runs: 0, last_event_at: null },
    { source: 'jira', unit_id: 'reg-PLAT', events: 7, tasks: 0, runs: 0, last_event_at: null },
    { source: 'jira', unit_id: 'reg-SKY', events: 0, tasks: 0, runs: 0, last_event_at: null },
    { source: 'linear', unit_id: 'team-eng', events: 4, tasks: 0, runs: 0, last_event_at: null },
  ],
}

function stub(payloads: Record<string, unknown>) {
  const fetchMock = vi.fn((input: unknown, init?: RequestInit) => {
    const path = String(input).split('?')[0]
    if (path in payloads) {
      // A `/list` route answers the paging envelope, so its payload is written
      // here as the rows alone and wrapped on the way out — the reads are POSTs
      // and are matched by path like any other, since the method is about how
      // the request is framed rather than whether it writes.
      const raw = payloads[path]
      const body = path.endsWith('/list')
        ? { items: raw, next_page_token: '', total_count: (raw as unknown[]).length }
        : raw
      const text = JSON.stringify(body)
      // Both readers: fetchTeamSettings takes .json(), apiJSON takes .text()
      // and parses it itself so a 200 index.html cannot reach a caller as a
      // native SyntaxError.
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve(body),
        text: () => Promise.resolve(text),
      })
    }
    // Every write these pages make is a replace-set whose ack carries nothing
    // the page reads back.
    if (init && init.method && init.method !== 'GET') {
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{}') })
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      clone: () => ({ json: () => Promise.resolve({ error: 'not found' }) }),
      text: () => Promise.resolve('not found'),
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** The digit a ramp lands on: 22px a line, on the third pass. */
function reading(figure: HTMLElement): string {
  return Array.from(figure.querySelectorAll('.odo-ramp'))
    .map((r) => {
      const roll = Number((r as HTMLElement).style.getPropertyValue('--roll').replace('px', ''))
      return String(-roll / 22 - 20)
    })
    .join('')
}

/** The row whose label reads `label`, from the left column. */
function row(label: string): HTMLElement {
  const found = Array.from(document.querySelectorAll('.sp-figrow')).find((r) =>
    r.textContent?.includes(label),
  )
  if (!found) throw new Error('no figure row labelled ' + label)
  return found as HTMLElement
}

/** commitVerb selects the table row reading `rowText`, presses `verb` for
 *  `holdMs` until it fires, and runs out the window that holds its save back. */
function commitVerb(rowText: string, verb: string | RegExp, holdMs: number) {
  vi.useFakeTimers()
  try {
    const target = screen.getByText(rowText).closest<HTMLElement>('[role="row"]')!
    fireEvent.click(within(target).getByRole('checkbox'))
    fireEvent.pointerDown(screen.getByRole('button', { name: verb }), {
      button: 0,
      clientX: 0,
      clientY: 0,
    })
    act(() => {
      vi.advanceTimersByTime(holdMs)
    })
    act(() => {
      vi.advanceTimersByTime(10_000)
    })
  } finally {
    vi.useRealTimers()
  }
}

beforeEach(() => {
  sources.state = {}
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  )
})

describe('GitHub source page', () => {
  const REPOS = {
    '/api/teams/t1/github-repos': {
      repos: [
        { id: 'r1', github_id: '1', owner: 'sky', repo: 'agent-runner' },
        { id: 'r2', github_id: '2', owner: 'sky', repo: 'planner' },
      ],
    },
    '/api/github/repos/list': [
      { github_id: '1', full_name: 'sky/agent-runner' },
      { github_id: '2', full_name: 'sky/planner' },
      { github_id: '3', full_name: 'sky/docs-site' },
    ],
  }

  it('rolls the tracked count and names what it is out of', async () => {
    stub(REPOS)
    render(<GitHubSource {...BODY} />)

    await waitFor(() => expect(screen.getByText('tracked of 3 visible')).toBeInTheDocument())
    expect(reading(row('tracked of 3 visible'))).toBe('2')
  })

  it('draws a dash, never a zero, while the node has not answered', async () => {
    stub(REPOS)
    render(<GitHubSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('tracked of 3 visible')).toBeInTheDocument())

    for (const label of [
      'events in 7 days',
      'became tasks',
      'runs against these repositories',
      'since the last poll',
    ]) {
      expect(row(label).textContent).toContain('—')
      expect(row(label).querySelector('.odo-ramp')).toBeNull()
    }
    // The band's three, for the same reason.
    expect(screen.getByText('events · 7 days').parentElement?.textContent).toContain('—')
    expect(screen.getByText('since last poll').parentElement?.textContent).toContain('—')
  })

  it('fills the figures and the chart once the node answers', async () => {
    stub({ ...REPOS, '/api/teams/t1/activity': ACTIVITY })
    render(<GitHubSource {...BODY} />)

    await waitFor(() => expect(reading(row('events in 7 days'))).toBe('6'))
    expect(row('became tasks').textContent).toContain('2')
    expect(reading(row('runs against these repositories'))).toBe('3')
    // Elapsed time keeps elapsing, so the reading is shape-checked: a small
    // number of seconds, not a dash.
    expect(row('since the last poll').textContent).toMatch(/\d+s/)

    // The frame's own three, from the same answer.
    expect(screen.getByText('events · 7 days').parentElement?.textContent).toContain('6')
    expect(screen.getByText('since last poll').parentElement?.textContent).toMatch(/\d+s/)

    // The fortnight drew: legend keys exist and the empty label is gone.
    expect(screen.queryByText('no daily series yet')).not.toBeInTheDocument()
    expect(screen.getByText('EVENTS')).toBeInTheDocument()
  })

  it("reads each watched repository's runs and last event, and dashes the rest", async () => {
    stub({ ...REPOS, '/api/teams/t1/activity': ACTIVITY })
    render(<GitHubSource {...BODY} />)
    const rowOf = (name: string) => screen.getByText(name).closest<HTMLElement>('[role="row"]')!

    await waitFor(() => expect(rowOf('sky/agent-runner').textContent).toContain('4m'))
    expect(within(rowOf('sky/agent-runner')).getByText('3')).toBeInTheDocument()
    // Watched and quiet: a real zero, and no last event because it has
    // never sent one.
    expect(within(rowOf('sky/planner')).getByText('0')).toBeInTheDocument()
    expect(within(rowOf('sky/planner')).getByText('—')).toBeInTheDocument()
    // Not watched: nothing counts it, so neither column is a number.
    expect(within(rowOf('sky/docs-site')).getAllByText('—')).toHaveLength(2)
  })

  it('lists a tracked repository the picker can no longer see', async () => {
    stub({
      '/api/teams/t1/github-repos': {
        repos: [{ id: 'r9', github_id: '9', owner: 'sky', repo: 'vanished' }],
      },
      '/api/github/repos/list': [{ github_id: '2', full_name: 'sky/planner' }],
    })
    render(<GitHubSource {...BODY} />)

    // Dropping it would make the page lie about what the team watches.
    await waitFor(() => expect(screen.getByText('sky/vanished')).toBeInTheDocument())
  })

  it('tells a retired repository from the one now under its name', async () => {
    stub({
      '/api/teams/t1/github-repos': {
        repos: [
          {
            id: 'r9',
            github_id: '9',
            owner: 'sky',
            repo: 'planner',
            retired_at: '2026-10-20T12:00:00Z',
          },
        ],
      },
      '/api/github/repos/list': [{ github_id: '2', full_name: 'sky/planner' }],
    })
    render(<GitHubSource {...BODY} />)

    // Two rows under one name: the tracked, retired row and the repository
    // GitHub serves under the name now. Only the first carries the tag.
    await waitFor(() => expect(screen.getAllByText(/sky\/planner/)).toHaveLength(2))
    expect(screen.getAllByText('no longer on GitHub')).toHaveLength(1)
  })

  // A tracked row with no GitHub id pairs with an option only through the
  // registry id the server resolved for it, never through the shared name.
  it.each([
    ['keeps a retired row apart from its namesake', '2026-10-20T12:00:00Z', '', 2],
    ['pairs a row with the option that carries its registry id', null, 'r9', 1],
  ])('%s', async (_, retiredAt, repositoryId, rows) => {
    stub({
      '/api/teams/t1/github-repos': {
        repos: [{ id: 'r9', github_id: '', owner: 'sky', repo: 'planner', retired_at: retiredAt }],
      },
      '/api/github/repos/list': [
        { repository_id: repositoryId, github_id: '2', full_name: 'sky/planner' },
      ],
    })
    render(<GitHubSource {...BODY} />)

    await waitFor(() => expect(screen.getAllByText(/sky\/planner/)).toHaveLength(rows))
  })

  it('stops watching a retired repository and nothing else', async () => {
    const fetchMock = stub({
      '/api/teams/t1/github-repos': {
        repos: [
          {
            id: 'r9',
            github_id: '9',
            owner: 'sky',
            repo: 'planner',
            retired_at: '2026-10-20T12:00:00Z',
          },
          { id: 'r1', github_id: '1', owner: 'sky', repo: 'docs-site' },
        ],
      },
      '/api/github/repos/list': [
        { github_id: '2', full_name: 'sky/planner' },
        { github_id: '1', full_name: 'sky/docs-site' },
      ],
    })
    const puts = () =>
      fetchMock.mock.calls
        .filter(([path, init]) => path === '/api/teams/t1/github-repos' && init?.method === 'PUT')
        .map(([, init]) => JSON.parse(String(init!.body)).repository_ids)
    render(<GitHubSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('no longer on GitHub')).toBeInTheDocument())

    // A destructive verb is held, not pressed.
    commitVerb('no longer on GitHub', /^Stop watching/, 1_200)
    await waitFor(() => expect(puts()).toEqual([['r1']]))
  })

  it('holds its baseline where the fortnight would be', async () => {
    stub(REPOS)
    render(<GitHubSource {...BODY} />)

    expect(screen.getByText('no daily series yet')).toBeInTheDocument()
    expect(document.querySelector('.sc-base')).not.toBeNull()
    // No legend keys, because there are no lines for them to name.
    expect(screen.queryByText('EVENTS')).not.toBeInTheDocument()
  })

  it('gives a member the page without the verbs', async () => {
    stub(REPOS)
    render(<GitHubSource {...BODY} isAdmin={false} />)
    await waitFor(() => expect(screen.getByText('sky/planner')).toBeInTheDocument())

    // Selection exists for the verbs, so it goes with them.
    expect(document.querySelector('.tb-check')).toBeNull()
    expect(screen.getByText('tracked of 3 visible')).toBeInTheDocument()
  })

  it('sends a later save only after one that registers a repository first', async () => {
    const fetchMock = stub({
      ...REPOS,
      '/api/github/repos/list': [
        ...REPOS['/api/github/repos/list'],
        { github_id: '4', full_name: 'sky/web' },
      ],
    })
    // The first registration is held open until the test answers it.
    let answerRegister: () => void = () => {}
    let registrations = 0
    const answer = fetchMock.getMockImplementation()!
    fetchMock.mockImplementation((input: unknown, init?: RequestInit) => {
      if (String(input) !== '/api/repos') return answer(input, init)
      const githubId = JSON.parse(String(init!.body)).github_id
      const ok = {
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify({ id: 'r' + githubId, github_id: githubId })),
      }
      if (registrations++ > 0) return Promise.resolve(ok)
      return new Promise((resolve) => {
        answerRegister = () => resolve(ok)
      })
    })
    const puts = () =>
      fetchMock.mock.calls
        .filter(([path, init]) => path === '/api/teams/t1/github-repos' && init?.method === 'PUT')
        .map(([, init]) => JSON.parse(String(init!.body)).repository_ids)
    const { unmount } = render(<GitHubSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('sky/docs-site')).toBeInTheDocument())
    const watch = (name: string) => {
      fireEvent.click(within(screen.getByText(name).closest('[role="row"]')!).getByRole('checkbox'))
      fireEvent.pointerDown(screen.getByRole('button', { name: 'Watch' }), {
        button: 0,
        clientX: 0,
        clientY: 0,
      })
      // A verb fires once the press rings settle.
      act(() => {
        vi.advanceTimersByTime(300)
      })
    }

    // The first window runs out and its save goes out; the second verb's
    // window is closed by leaving the page while that registration is open.
    vi.useFakeTimers()
    try {
      watch('sky/docs-site')
      act(() => {
        vi.advanceTimersByTime(10_000)
      })
      watch('sky/web')
      unmount()
    } finally {
      vi.useRealTimers()
    }
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)))
    expect(registrations).toBe(1)
    expect(puts()).toEqual([])

    answerRegister()
    await waitFor(() => expect(puts()).toHaveLength(2))
    expect(puts()).toEqual([
      ['r1', 'r2', 'r3'],
      ['r1', 'r2', 'r3', 'r4'],
    ])
  })
})

describe('Jira source page', () => {
  const JIRA = '/api/orgs/org-1/jira/projects'
  const STATUSES = [
    { id: '1', name: 'Ready' },
    { id: '2', name: 'In Progress' },
    { id: '3', name: 'Done' },
    { id: '4', name: 'QA' },
    { id: '5', name: 'Blocked' },
  ]
  const isStatusRead = (u: string) => u.startsWith(JIRA) && u.endsWith('/statuses/list')
  const SETTINGS = {
    '/api/teams/t1/settings': {
      team_settings: {
        AIReprioritizeThreshold: 0,
        AIPreferenceUpdateInterval: 0,
        DefaultModel: 'sonnet',
        AutoDelegateEnabled: true,
        BranchTemplate: 'tfac/<ticket-id>',
        ReviewPosture: 'identity',
        BaseBranchPushPolicy: 'never',
        PermissionAbsentGraceMS: 15000,
        PermissionAbsentAutodenyEnabled: true,
      },
      jira_projects: [
        {
          id: 'reg-PLAT',
          jira_id: '10010',
          key: 'PLAT',
          pickup: { members: [{ id: '1', name: 'Ready' }] },
          in_progress: {
            members: [{ id: '2', name: 'In Progress' }],
            canonical: { id: '2', name: 'In Progress' },
          },
          in_review: { members: [{ id: '4', name: 'QA' }], canonical: { id: '4', name: 'QA' } },
          done: { members: [{ id: '3', name: 'Done' }], canonical: { id: '3', name: 'Done' } },
        },
        // Watched but never armed — a valid saved state since watch-then-arm,
        // and the strip's marker case.
        {
          id: 'reg-SKY',
          jira_id: '10020',
          key: 'SKY',
          pickup: { members: [] },
          in_progress: { members: [] },
          in_review: { members: [] },
          done: { members: [] },
        },
      ],
      member_count: 4,
      role: 'admin',
    },
    // The org credential's live catalog: both watched projects plus one the
    // team could watch. Both watched projects' status reads answer the same
    // list — per-project routing is asserted on the fetch calls instead.
    [`${JIRA}/list`]: [
      { jira_id: '10010', key: 'PLAT', name: 'Platform Core' },
      { jira_id: '10020', key: 'SKY', name: 'Skyworks' },
      { jira_id: '10030', key: 'OPS', name: 'Operations' },
    ],
    [`${JIRA}/10010/statuses/list`]: STATUSES,
    [`${JIRA}/10020/statuses/list`]: STATUSES,
  }

  /** The fixture with a single watched project — the no-strip shape. */
  const ONE_PROJECT = {
    ...SETTINGS,
    '/api/teams/t1/settings': {
      ...SETTINGS['/api/teams/t1/settings'],
      jira_projects: SETTINGS['/api/teams/t1/settings'].jira_projects.slice(0, 1),
    },
  }

  it('draws the fortnight beside the board, over the projects', async () => {
    stub(SETTINGS)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('What each status means')).toBeInTheDocument())

    expect(screen.getByText('WHAT THE PROJECTS SENT')).toBeInTheDocument()
    expect(document.querySelector('.sp-listcol .sc')).not.toBeNull()
    expect(document.querySelector('.sp-mapcol .sr-board')).not.toBeNull()
  })

  it('draws a skeleton board under what to do when nothing is watched', async () => {
    const none = {
      ...SETTINGS,
      '/api/teams/t1/settings': { ...SETTINGS['/api/teams/t1/settings'], jira_projects: [] },
    }
    stub(none)
    const admin = render(<JiraSource {...BODY} />)
    await waitFor(() =>
      expect(
        screen.getByText('Track a Jira project to the right to map ticket statuses.'),
      ).toBeInTheDocument(),
    )
    expect(document.querySelector('.sr[data-placeholder]')).not.toBeNull()
    expect(document.querySelector('[draggable="true"]')).toBeNull()
    admin.unmount()

    // A member has no verb to follow, so is told only what is true.
    stub(none)
    render(<JiraSource {...BODY} isAdmin={false} />)
    await waitFor(() =>
      expect(screen.getByText('No Jira project is watched by this team yet.')).toBeInTheDocument(),
    )
  })

  it("maps the stored rules into the board, from its own project's statuses", async () => {
    const fetchMock = stub(SETTINGS)
    render(<JiraSource {...BODY} />)

    // A status nobody mapped sits in the tray rather than being hidden — and
    // 'Blocked' is the gate as well as the claim, because it is the one status
    // reachable ONLY through the read this test is about: it is in PLAT's
    // vocabulary and in none of its stored rules. A mapped chip would not do.
    // 'QA' is an `in_review` member, so it renders straight off the settings
    // payload, in the commit BEFORE the one whose passive effect issues the
    // statuses read — waiting on it would leave the assertion below racing a
    // fetch that has not been called yet.
    await waitFor(() => expect(screen.getByText('Blocked')).toBeInTheDocument())
    expect(screen.getByText('QA')).toBeInTheDocument()
    expect(screen.getByText('In Progress')).toBeInTheDocument()

    // The vocabulary is the shown project's own, never an intersection
    // across the watched set — two workflows differ the moment they can.
    const statusReads = fetchMock.mock.calls.map((c) => String(c[0])).filter(isStatusRead)
    expect(statusReads).toEqual([`${JIRA}/10010/statuses/list`])
  })

  it('names the project it shows when there is no strip to', async () => {
    stub(ONE_PROJECT)
    render(<JiraSource {...BODY} />)

    await waitFor(() => expect(screen.getByText(/showing PLAT/)).toBeInTheDocument())
    expect(document.querySelector('.sp-projstrip')).toBeNull()
  })

  it('switches the board through the strip, and marks the unmapped key', async () => {
    const fetchMock = stub(SETTINGS)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Blocked')).toBeInTheDocument())

    // Two keys, the shown one selected, the unarmed one marked.
    const keys = Array.from(document.querySelectorAll('.sp-projkey'))
    expect(keys.map((k) => k.textContent)).toEqual(['PLAT', 'SKY'])
    expect(keys[0].getAttribute('aria-selected')).toBe('true')
    expect(keys[0].hasAttribute('data-unarmed')).toBe(false)
    expect(keys[1].hasAttribute('data-unarmed')).toBe(true)

    fireEvent.click(keys[1])

    // SKY's board: nothing mapped, so the page says so — and SKY's own
    // statuses were asked for, not reused from PLAT's read.
    await waitFor(() => expect(screen.getByText(/not armed yet/)).toBeInTheDocument())
    expect(keys[1].getAttribute('aria-selected')).toBe('true')
    // That note is drawn from the stored config, so it says nothing about
    // whether SKY's vocabulary landed. The tray refilling is what does: the
    // board re-keys on the switch, so it is empty until SKY's own read
    // resolves — which is the read the assertion below is about.
    await waitFor(() => expect(screen.getByText('Blocked')).toBeInTheDocument())
    const statusReads = fetchMock.mock.calls.map((c) => String(c[0])).filter(isStatusRead)
    expect(statusReads).toEqual([`${JIRA}/10010/statuses/list`, `${JIRA}/10020/statuses/list`])
  })

  it('lists the catalog with names, the watched projects first', async () => {
    stub(SETTINGS)
    render(<JiraSource {...BODY} />)

    // The candidate is offerable, named, and last — the set the team acts on
    // outranks the set it could.
    await waitFor(() => expect(screen.getByText('Operations')).toBeInTheDocument())
    expect(screen.getByText('Platform Core')).toBeInTheDocument()
    const keys = Array.from(document.querySelectorAll('.tb-row [data-col="key"], .tb-cell')).map(
      (e) => e.textContent,
    )
    const order = ['PLAT', 'SKY', 'OPS'].map((k) => keys.findIndex((t) => t === k))
    expect(order[0]).toBeGreaterThanOrEqual(0)
    expect(order[0]).toBeLessThan(order[2])
    expect(order[1]).toBeLessThan(order[2])
  })

  it("counts a watched project's events, and dashes a catalog project's", async () => {
    stub({ ...SETTINGS, '/api/teams/t1/activity': ACTIVITY })
    render(<JiraSource {...BODY} />)
    const rowOf = (name: string) => screen.getByText(name).closest<HTMLElement>('[role="row"]')!

    await waitFor(() => expect(within(rowOf('Platform Core')).getByText('7')).toBeInTheDocument())
    expect(within(rowOf('Skyworks')).getByText('0')).toBeInTheDocument()
    expect(within(rowOf('Operations')).getByText('—')).toBeInTheDocument()
  })

  it('states why Jira is off and holds the live reads, keeping the config', async () => {
    sources.state = { jira: 'disabled' }
    const fetchMock = stub(SETTINGS)
    render(<JiraSource {...BODY} />)

    await waitFor(() =>
      expect(
        screen.getByText(
          'Jira is turned off — an org admin paused it. The mapping below is what is stored; a project can still be unwatched.',
        ),
      ).toBeInTheDocument(),
    )
    // The stored configuration still renders — watched keys, the board's map —
    // because the team's config is TF's data, not Jira's.
    expect(screen.getAllByText('PLAT').length).toBeGreaterThan(0)
    expect(screen.getByText('In Progress')).toBeInTheDocument()

    // No catalog page, no status vocabulary: both reads would just collect
    // refusals while the source is off.
    const urls = fetchMock.mock.calls.map((c) => String(c[0]))
    expect(urls.some((u) => u.startsWith(JIRA))).toBe(false)
    // And with no catalog there is nothing to offer Watch on: only the
    // watched rows are listed.
    expect(screen.queryByText('Operations')).not.toBeInTheDocument()
    // Nor anything live to report on: the figures and the chart hold their
    // dashes even where the node has counted.
    expect(screen.getByText('no daily series yet')).toBeInTheDocument()
  })

  it('gives a member a board with nothing to reach, and an admin the drag', async () => {
    stub(SETTINGS)
    const member = render(<JiraSource {...BODY} isAdmin={false} />)
    await waitFor(() => expect(screen.getByText('Blocked')).toBeInTheDocument())

    // Absent, not disabled: a cursor that says "pick me up" over a chip that
    // cannot be picked up promises and then fails.
    expect(document.querySelector('[draggable="true"]')).toBeNull()
    member.unmount()

    stub(SETTINGS)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Blocked')).toBeInTheDocument())
    expect(document.querySelector('[draggable="true"]')).not.toBeNull()
  })

  it('saves a board gesture as the four rules, ids on the wire', async () => {
    // The PUT answers the stored set; echoing the gesture's outcome is what
    // the page adopts.
    const fetchMock = stub({
      ...SETTINGS,
      '/api/teams/t1/jira-projects': {
        jira_projects: [
          {
            id: 'reg-PLAT',
            jira_id: '10010',
            key: 'PLAT',
            pickup: { members: [{ id: '1', name: 'Ready' }] },
            in_progress: {
              members: [{ id: '2', name: 'In Progress' }],
              canonical: { id: '2', name: 'In Progress' },
            },
            in_review: {
              members: [
                { id: '4', name: 'QA' },
                { id: '5', name: 'Blocked' },
              ],
              canonical: { id: '4', name: 'QA' },
            },
            done: { members: [{ id: '3', name: 'Done' }], canonical: { id: '3', name: 'Done' } },
          },
          SETTINGS['/api/teams/t1/settings'].jira_projects[1],
        ],
      },
    })
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Blocked')).toBeInTheDocument())

    // Stage the tray's Blocked and land it in IN REVIEW from the keyboard.
    fireEvent.keyDown(screen.getByRole('button', { name: /Blocked/ }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put Blocked in IN REVIEW' }), {
      key: 'Enter',
    })

    // One replace-set PUT, the gesture expressed as status IDS: Blocked joins
    // the in_review members, QA keeps the ★ it already had.
    await waitFor(() => {
      const put = fetchMock.mock.calls.find(
        (c) => String(c[0]).includes('/jira-projects') && (c[1] as RequestInit)?.method === 'PUT',
      )
      expect(put).toBeTruthy()
      const body = JSON.parse(String((put![1] as RequestInit).body))
      const plat = body.jira_projects[0]
      expect(plat.id).toBe('reg-PLAT')
      expect(plat).not.toHaveProperty('key')
      expect(plat.in_review).toEqual({ member_ids: ['4', '5'], canonical_id: '4' })
      expect(plat.pickup).toEqual({ member_ids: ['1'] })
    })

    // The landed chip sits in the review column; the tray no longer offers it.
    const review = Array.from(document.querySelectorAll('.sr-col')).find(
      (c) => c.querySelector('.sr-col-label')?.textContent === 'IN REVIEW',
    )
    expect(review?.textContent).toContain('Blocked')
    expect(screen.queryByRole('button', { name: /^Blocked$/ })).not.toBeInTheDocument()
  })

  it('watches a catalog project by its Jira id, and keeps the tracked ones by registry id', async () => {
    const fetchMock = stub(SETTINGS)
    const puts = (): Record<string, unknown>[][] =>
      fetchMock.mock.calls
        .filter(([path, init]) => path === '/api/teams/t1/jira-projects' && init?.method === 'PUT')
        .map(([, init]) => JSON.parse(String(init!.body)).jira_projects)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Operations')).toBeInTheDocument())

    commitVerb('Operations', 'Watch', 300)
    await waitFor(() => expect(puts()).toHaveLength(1))
    const [sent] = puts()
    expect(sent.map((p) => p.id ?? `jira:${p.jira_id}`)).toEqual([
      'reg-PLAT',
      'reg-SKY',
      'jira:10030',
    ])
    expect(sent.some((p) => 'key' in p)).toBe(false)
  })
})

describe('Slack source page', () => {
  const CHANNELS = {
    '/api/slack/teams/t1/channels': {
      role: 'admin',
      warnings: [],
      channels: [
        {
          channel_id: 'C1',
          name: 'deploys',
          workspace_id: 'W',
          is_private: false,
          bot_is_member: true,
          tracked: true,
          is_primary: true,
          tracked_by: [{ team_id: 't1', team_name: 'platform', is_primary: true }],
          last_mention_at: new Date(Date.now() - 4 * 60000).toISOString(),
          source: 'tracked',
        },
        {
          channel_id: 'C2',
          name: 'oncall-platform',
          workspace_id: 'W',
          is_private: false,
          bot_is_member: true,
          tracked: true,
          is_primary: false,
          // Watched by somebody, owned by nobody — the one state on this page
          // that needs a person.
          tracked_by: [{ team_id: 't1', team_name: 'platform', is_primary: false }],
          source: 'tracked',
        },
        {
          channel_id: 'C3',
          name: 'eng-general',
          workspace_id: 'W',
          is_private: false,
          bot_is_member: false,
          tracked: false,
          is_primary: false,
          tracked_by: [],
          source: 'sighting',
        },
      ],
    },
  }

  it('counts what it watches, what it owns, and what nobody owns', async () => {
    stub(CHANNELS)
    render(<SlackSource {...BODY} />)

    await waitFor(() => expect(screen.getByText('watched of 3 visible')).toBeInTheDocument())
    expect(reading(row('watched of 3 visible'))).toBe('2')
    expect(reading(row('primary here'))).toBe('1')
    expect(reading(row('with no primary team'))).toBe('1')
  })

  it('reads a last event it has and dashes the count it does not', async () => {
    stub(CHANNELS)
    render(<SlackSource {...BODY} />)

    // The registry carries a timestamp; the node has not answered here, so
    // the windowed count holds its dash.
    await waitFor(() => expect(screen.getByText('4m')).toBeInTheDocument())
    expect(row('mentions in 7 days').textContent).toContain('—')
  })

  it('keeps the dash where the node answers that the count is undefined', async () => {
    stub({ ...CHANNELS, '/api/teams/t1/activity': ACTIVITY })
    render(<SlackSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('watched of 3 visible')).toBeInTheDocument())

    // The node answered — became-tasks is real — but the mention count is
    // null: undefined for a source with no tracked set, so the figure and
    // the chart whose outer line it is both stay unanswered.
    expect(screen.getByText('became tasks · 7 days').parentElement?.textContent).toContain('1')
    expect(row('mentions in 7 days').textContent).toContain('—')
    expect(screen.getByText('no daily series yet')).toBeInTheDocument()
    // No poller behind Slack: the dash is by construction, not a gap.
    expect(row('since the last poll').textContent).toContain('—')
  })

  it('marks the primary channels and only those', async () => {
    stub(CHANNELS)
    render(<SlackSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('#deploys')).toBeInTheDocument())

    const tones = Array.from(document.querySelectorAll('.sp-star')).map((s) =>
      s.getAttribute('data-tone'),
    )
    // Ours, nobody's — and no mark at all on the channel this team does not
    // watch.
    expect(tones).toEqual(['ours', 'none'])
  })

  it('reads another team as theirs whatever that team is called', async () => {
    const read = CHANNELS['/api/slack/teams/t1/channels']
    // Watched here, primary on a team whose name spells what the table prints
    // for this team or for nobody.
    const ownedBy = (name: string, teamName: string) => ({
      ...read.channels[1],
      channel_id: name,
      name,
      tracked_by: [
        ...read.channels[1].tracked_by,
        { team_id: 't2', team_name: teamName, is_primary: true },
      ],
    })
    stub({
      '/api/slack/teams/t1/channels': {
        ...read,
        channels: [ownedBy('named-no-one', 'no one'), ownedBy('named-this-team', 'this team')],
      },
    })
    render(<SlackSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('#named-no-one')).toBeInTheDocument())

    const tones = Array.from(document.querySelectorAll('.sp-star')).map((s) =>
      s.getAttribute('data-tone'),
    )
    expect(tones).toEqual(['theirs', 'theirs'])
    expect(reading(row('with no primary team'))).toBe('0')
  })
})

describe('Linear source page', () => {
  const ENG = 'team-eng'
  const OPS = 'team-ops'
  const LINEAR = '/api/orgs/org-1/linear/teams'
  const state = (id: string, name: string, type: string, position: number) => ({
    id,
    name,
    type,
    position,
  })
  const ref = ({ id, name, type }: { id: string; name: string; type: string }) => ({
    id,
    name,
    type,
  })
  const TODO = state('s-todo', 'Todo', 'unstarted', 1)
  const DOING = state('s-doing', 'In Progress', 'started', 2)
  const REVIEW = state('s-review', 'In Review', 'started', 3)
  const DONE = state('s-done', 'Done', 'completed', 4)
  // Linear's seventh category: offered like any other state, and unmapped
  // here, so it is the one chip reachable only through the workflow read.
  const DUP = state('s-dup', 'Duplicate', 'duplicate', 5)
  const ENG_STATES = [TODO, DOING, REVIEW, DONE, DUP]
  const OPS_STATES = [state('o-todo', 'Queued', 'unstarted', 1)]

  const ENG_ROW = {
    id: ENG,
    key: 'ENG',
    name: 'Engineering',
    armed: true,
    pickup: { members: [ref(TODO)] },
    in_progress: { members: [ref(DOING)], canonical: ref(DOING) },
    in_review: { members: [], canonical: null },
    done: { members: [ref(DONE)], canonical: ref(DONE) },
  }
  // Watched and never mapped: a valid stored state, and the strip's marker.
  const OPS_ROW = {
    id: OPS,
    key: 'OPS',
    name: 'Operations',
    armed: false,
    pickup: { members: [] },
    in_progress: { members: [], canonical: null },
    in_review: { members: [], canonical: null },
    done: { members: [], canonical: null },
  }
  const CATALOG = [
    { id: ENG, key: 'ENG', name: 'Engineering', private: false },
    { id: OPS, key: 'OPS', name: 'Operations', private: false },
    { id: 'team-des', key: 'DES', name: 'Design', private: false },
  ]
  const payloads = (linear_teams: unknown[], over: Record<string, unknown> = {}) => ({
    '/api/teams/t1/settings': { linear_teams },
    [`${LINEAR}/list`]: CATALOG,
    [`${LINEAR}/${ENG}/states/list`]: ENG_STATES,
    [`${LINEAR}/${OPS}/states/list`]: OPS_STATES,
    ...over,
  })
  const BOTH = payloads([ENG_ROW, OPS_ROW])

  const urls = (fetchMock: ReturnType<typeof stub>) => fetchMock.mock.calls.map((c) => String(c[0]))
  const puts = (fetchMock: ReturnType<typeof stub>): Record<string, unknown>[][] =>
    fetchMock.mock.calls
      .filter(([path, init]) => path === '/api/teams/t1/linear-teams' && init?.method === 'PUT')
      .map(([, init]) => JSON.parse(String(init!.body)).linear_teams)
  const column = (label: string) =>
    Array.from(document.querySelectorAll('.sr-col')).find(
      (c) => c.querySelector('.sr-col-label')?.textContent === label,
    )!
  const rowOf = (text: string) => screen.getByText(text).closest<HTMLElement>('[role="row"]')!

  beforeEach(() => {
    sources.state = { linear: 'available' }
  })

  it("maps the stored rules into four columns, from the team's own workflow", async () => {
    const fetchMock = stub(BOTH)
    render(<LinearSource {...BODY} />)

    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())
    expect(column('READY').textContent).toContain('Todo')
    expect(column('IN PROGRESS').textContent).toContain('In Progress')
    expect(column('IN REVIEW')).toBeDefined()
    expect(column('DONE').textContent).toContain('Done')
    // The shown team's workflow only, never another watched team's.
    expect(urls(fetchMock).filter((u) => u.endsWith('/states/list'))).toEqual([
      `${LINEAR}/${ENG}/states/list`,
    ])
    expect(screen.getByText('WHAT THE TEAMS SENT')).toBeInTheDocument()
  })

  it('names the team it shows when there is no strip to', async () => {
    stub(payloads([ENG_ROW]))
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(screen.getByText(/showing ENG/)).toBeInTheDocument())
    expect(screen.getByText(/saves as it lands/)).toBeInTheDocument()
    expect(document.querySelector('.sp-projstrip')).toBeNull()
  })

  it('switches the board through the strip, and marks the unmapped key', async () => {
    const fetchMock = stub(BOTH)
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())

    const keys = Array.from(document.querySelectorAll('.sp-projkey'))
    expect(keys.map((k) => k.textContent)).toEqual(['ENG', 'OPS'])
    expect(keys[0].hasAttribute('data-unarmed')).toBe(false)
    expect(keys[1].hasAttribute('data-unarmed')).toBe(true)

    fireEvent.click(keys[1])
    await waitFor(() =>
      expect(
        screen.getByText('not armed yet · drag states into the columns to arm it'),
      ).toBeInTheDocument(),
    )
    await waitFor(() => expect(screen.getByText('Queued')).toBeInTheDocument())
    expect(urls(fetchMock).filter((u) => u.endsWith('/states/list'))).toEqual([
      `${LINEAR}/${ENG}/states/list`,
      `${LINEAR}/${OPS}/states/list`,
    ])
  })

  it('saves a board gesture as the four rules, state ids on the wire', async () => {
    const fetchMock = stub({
      ...BOTH,
      '/api/teams/t1/linear-teams': {
        linear_teams: [
          { ...ENG_ROW, in_review: { members: [ref(REVIEW)], canonical: ref(REVIEW) } },
          OPS_ROW,
        ],
      },
    })
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())

    // Stage the tray's In Review and land it in IN REVIEW from the keyboard.
    fireEvent.keyDown(screen.getByRole('button', { name: 'In Review' }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put In Review in IN REVIEW' }), {
      key: 'Enter',
    })

    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1))
    const [sent] = puts(fetchMock)
    // The first state into a write-target column takes its ★.
    expect(sent[0]).toEqual({
      id: ENG,
      pickup: { member_ids: ['s-todo'] },
      in_progress: { member_ids: ['s-doing'], canonical_id: 's-doing' },
      in_review: { member_ids: ['s-review'], canonical_id: 's-review' },
      done: { member_ids: ['s-done'], canonical_id: 's-done' },
    })
    expect(sent[1]).toEqual({
      id: OPS,
      pickup: { member_ids: [] },
      in_progress: { member_ids: [], canonical_id: '' },
      in_review: { member_ids: [], canonical_id: '' },
      done: { member_ids: [], canonical_id: '' },
    })
    await waitFor(() => expect(column('IN REVIEW').textContent).toContain('In Review'))
  })

  it('rolls a refused gesture back and says why', async () => {
    const fetchMock = stub(BOTH)
    const answer = fetchMock.getMockImplementation()!
    fetchMock.mockImplementation((input: unknown, init?: RequestInit) =>
      String(input) === '/api/teams/t1/linear-teams'
        ? Promise.resolve({
            ok: false,
            status: 502,
            ...jsonBody({
              errors: [{ reason: 'UPSTREAM_UNAVAILABLE', message: 'could not reach Linear' }],
            }),
          })
        : answer(input, init),
    )
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())

    fireEvent.keyDown(screen.getByRole('button', { name: 'Duplicate' }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put Duplicate in DONE' }), {
      key: 'Enter',
    })

    await waitFor(() => expect(screen.getByText('could not reach Linear')).toBeInTheDocument())
    expect(column('DONE').textContent).not.toContain('Duplicate')
    expect(screen.getByRole('button', { name: 'Duplicate' })).toBeInTheDocument()
  })

  it('watches a catalog team with every rule empty', async () => {
    const fetchMock = stub(BOTH)
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Design')).toBeInTheDocument())

    commitVerb('Design', 'Watch', 300)
    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1))
    const [sent] = puts(fetchMock)
    expect(sent.map((t) => t.id)).toEqual([ENG, OPS, 'team-des'])
    expect(sent[2]).toEqual({
      id: 'team-des',
      pickup: { member_ids: [] },
      in_progress: { member_ids: [], canonical_id: '' },
      in_review: { member_ids: [], canonical_id: '' },
      done: { member_ids: [], canonical_id: '' },
    })
  })

  it("counts a watched team's events, and dashes a catalog team's", async () => {
    stub({ ...BOTH, '/api/teams/t1/activity': ACTIVITY })
    render(<LinearSource {...BODY} />)

    // The catalog row loads on its own request, so wait for it too.
    await waitFor(() => expect(within(rowOf('Engineering')).getByText('4')).toBeInTheDocument())
    await waitFor(() => expect(within(rowOf('Design')).getByText('—')).toBeInTheDocument())
  })

  it('gives a member the page without the verbs', async () => {
    const fetchMock = stub(BOTH)
    render(<LinearSource {...BODY} isAdmin={false} />)
    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())

    // The catalog and the workflow still read — a member is shown what is
    // watched and what could be — but nothing picks up, stars or selects.
    expect(urls(fetchMock)).toContain(`${LINEAR}/list`)
    expect(document.querySelector('[draggable="true"]')).toBeNull()
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0)
    expect(screen.queryByText(/saves as it lands/)).toBeNull()
  })

  it('keeps a paused Linear readable and its mapping editable', async () => {
    sources.state = { linear: 'disabled' }
    const fetchMock = stub(BOTH)
    render(<LinearSource {...BODY} />)

    await waitFor(() =>
      expect(
        screen.getByText(
          'Linear is paused for this organization. Nothing is polled until it resumes; the mapping stays editable.',
        ),
      ).toBeInTheDocument(),
    )
    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())
    expect(urls(fetchMock)).toContain(`${LINEAR}/list`)
    expect(document.querySelector('[draggable="true"]')).not.toBeNull()
  })

  it('shows what is stored while Linear is not connected, and still unwatches', async () => {
    sources.state = { linear: 'unconfigured' }
    const fetchMock = stub({ ...BOTH, '/api/teams/t1/activity': ACTIVITY })
    render(<LinearSource {...BODY} />)

    await waitFor(() =>
      expect(
        screen.getByText(
          'Linear is not connected for this organization. The mapping below is what is stored; a team can still be unwatched.',
        ),
      ).toBeInTheDocument(),
    )
    await waitFor(() => expect(screen.getByText('Engineering')).toBeInTheDocument())
    // Nothing Linear could answer is asked, and nothing live is reported.
    expect(urls(fetchMock).some((u) => u.startsWith(LINEAR))).toBe(false)
    expect(screen.queryByText('Design')).not.toBeInTheDocument()
    expect(screen.getByText('no daily series yet')).toBeInTheDocument()
    expect(within(rowOf('Engineering')).getByText('—')).toBeInTheDocument()
    expect(document.querySelector('[draggable="true"]')).toBeNull()

    commitVerb('Engineering', /^Stop watching/, 1_200)
    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1))
    expect(puts(fetchMock)[0].map((t) => t.id)).toEqual([OPS])
  })

  it('draws a skeleton board under what to do when nothing is watched', async () => {
    stub(payloads([]))
    const admin = render(<LinearSource {...BODY} />)
    await waitFor(() =>
      expect(
        screen.getByText('Track a Linear team to the right to map workflow statuses.'),
      ).toBeInTheDocument(),
    )
    expect(document.querySelector('.sr[data-placeholder] .sr-board[aria-hidden="true"]')).not.toBe(
      null,
    )
    admin.unmount()

    stub(payloads([]))
    render(<LinearSource {...BODY} isAdmin={false} />)
    await waitFor(() =>
      expect(screen.getByText('No Linear team is tracked by this team yet.')).toBeInTheDocument(),
    )
  })

  it('tags a private team and says what that means for its tasks', async () => {
    stub(
      payloads([ENG_ROW], {
        [`${LINEAR}/list`]: [{ ...CATALOG[0], private: true }, CATALOG[2]],
      }),
    )
    render(<LinearSource {...BODY} />)

    await waitFor(() =>
      expect(within(rowOf('Engineering')).getByText('private')).toBeInTheDocument(),
    )
    expect(
      screen.getByText(
        'ENG is private in Linear. Its issues still become tasks every member of this team can read.',
      ),
    ).toBeInTheDocument()
  })

  it('marks a watched team the whole catalog no longer has as out of reach', async () => {
    stub({ ...BOTH, [`${LINEAR}/list`]: CATALOG.slice(1) })
    render(<LinearSource {...BODY} />)

    await waitFor(() =>
      expect(
        screen.getByText(
          'Linear no longer shows ENG to this organization’s Linear connection, so nothing is polled from it. The stored mapping stays until it is unwatched.',
        ),
      ).toBeInTheDocument(),
    )
    expect(within(rowOf('Engineering')).getByText('out of reach')).toBeInTheDocument()
    const eng = Array.from(document.querySelectorAll('.sp-projkey'))[0]
    expect(eng.hasAttribute('data-alarm')).toBe(true)
    // Every change to it would ask Linear about a team it no longer serves.
    expect(document.querySelector('[draggable="true"]')).toBeNull()
  })

  it('asks after a watched team by id when the catalog does not fit one page', async () => {
    const fetchMock = stub({
      ...BOTH,
      [`${LINEAR}/${OPS}`]: { ...CATALOG[1], private: true },
    })
    const answer = fetchMock.getMockImplementation()!
    fetchMock.mockImplementation((input: unknown, init?: RequestInit) =>
      String(input) === `${LINEAR}/list`
        ? Promise.resolve({
            ok: true,
            status: 200,
            ...jsonBody({ items: [CATALOG[2]], next_page_token: 'more', total_count: 0 }),
          })
        : answer(input, init),
    )
    render(<LinearSource {...BODY} />)

    // ENG answers 404 (the stub's default), OPS answers as a private team.
    await waitFor(() =>
      expect(within(rowOf('Engineering')).getByText('out of reach')).toBeInTheDocument(),
    )
    await waitFor(() =>
      expect(within(rowOf('Operations')).getByText('private')).toBeInTheDocument(),
    )
    expect(screen.getByText('more teams exist · narrow the filter')).toBeInTheDocument()
  })

  it('flags a ★ Linear deleted, and drops it when a gesture changes its rule', async () => {
    const fetchMock = stub({
      ...BOTH,
      [`${LINEAR}/${ENG}/states/list`]: ENG_STATES.filter((s) => s.id !== 's-doing'),
    })
    render(<LinearSource {...BODY} />)

    await waitFor(() =>
      expect(
        screen.getByText(
          'In Progress, the ★ in IN PROGRESS, was deleted in Linear. A claim can’t move an issue until another state there is starred.',
        ),
      ).toBeInTheDocument(),
    )
    expect(Array.from(document.querySelectorAll('.sp-projkey'))[0].hasAttribute('data-alarm')).toBe(
      true,
    )
    // The flag carries the instruction, so the note does not repeat one.
    expect(screen.queryByText(/drag to remap/)).toBeNull()

    // Land In Review beside the deleted ★: the rule changed, so it sheds the
    // state Linear no longer has, and the ★ passes to what remains.
    fireEvent.keyDown(screen.getByRole('button', { name: 'In Review' }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put In Review in IN PROGRESS' }), {
      key: 'Enter',
    })
    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1))
    const eng = puts(fetchMock)[0][0]
    expect(eng.in_progress).toEqual({ member_ids: ['s-review'], canonical_id: 's-review' })
    // A rule the gesture did not touch goes out as stored.
    expect(eng.done).toEqual({ member_ids: ['s-done'], canonical_id: 's-done' })
  })

  it('tells a member who can repair a deleted ★', async () => {
    stub({
      ...BOTH,
      [`${LINEAR}/${ENG}/states/list`]: ENG_STATES.filter((s) => s.id !== 's-doing'),
    })
    render(<LinearSource {...BODY} isAdmin={false} />)
    await waitFor(() =>
      expect(
        screen.getByText(
          'In Progress, the ★ in IN PROGRESS, was deleted in Linear. Claims can’t move ENG issues until an admin stars another state.',
        ),
      ).toBeInTheDocument(),
    )
  })

  it('rolls two refused gestures back to what the server holds, not the first gesture', async () => {
    const fetchMock = stub(BOTH)
    const answer = fetchMock.getMockImplementation()!
    // Both PUTs are held open until the test refuses them, in order.
    const refusals: Array<() => void> = []
    fetchMock.mockImplementation((input: unknown, init?: RequestInit) =>
      String(input) === '/api/teams/t1/linear-teams'
        ? new Promise((resolve) => {
            refusals.push(() =>
              resolve({
                ok: false,
                status: 502,
                ...jsonBody({
                  errors: [{ reason: 'UPSTREAM_UNAVAILABLE', message: 'could not reach Linear' }],
                }),
              }),
            )
          })
        : answer(input, init),
    )
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())

    fireEvent.keyDown(screen.getByRole('button', { name: 'Duplicate' }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put Duplicate in DONE' }), {
      key: 'Enter',
    })
    fireEvent.keyDown(screen.getByRole('button', { name: 'In Review' }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put In Review in IN REVIEW' }), {
      key: 'Enter',
    })
    await waitFor(() => expect(refusals).toHaveLength(2))

    // The first refusal lands while the second write is still out, so the page
    // keeps showing the newer gesture; the second one rolls everything back.
    act(() => refusals[0]())
    act(() => refusals[1]())
    await waitFor(() => expect(screen.getByText('could not reach Linear')).toBeInTheDocument())
    expect(column('DONE').textContent).not.toContain('Duplicate')
    expect(column('IN REVIEW').textContent).not.toContain('In Review')
    expect(screen.getByRole('button', { name: 'Duplicate' })).toBeInTheDocument()
  })

  it('asks for a workflow again when a failed read’s board is shown again', async () => {
    const fetchMock = stub(BOTH)
    const answer = fetchMock.getMockImplementation()!
    const engStates = `${LINEAR}/${ENG}/states/list`
    let engReads = 0
    fetchMock.mockImplementation((input: unknown, init?: RequestInit) => {
      if (String(input) === engStates && ++engReads === 1) {
        return Promise.resolve({ ok: false, status: 502, ...jsonBody({ errors: [] }) })
      }
      return answer(input, init)
    })
    render(<LinearSource {...BODY} />)
    await waitFor(() => expect(engReads).toBe(1))
    expect(screen.queryByText('Duplicate')).not.toBeInTheDocument()

    const keys = () => Array.from(document.querySelectorAll('.sp-projkey'))
    fireEvent.click(keys()[1])
    await waitFor(() => expect(screen.getByText('Queued')).toBeInTheDocument())
    fireEvent.click(keys()[0])

    await waitFor(() => expect(screen.getByText('Duplicate')).toBeInTheDocument())
    expect(engReads).toBe(2)
  })
})
