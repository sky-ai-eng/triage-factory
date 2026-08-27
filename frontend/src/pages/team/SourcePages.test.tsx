import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import GitHubSource from './GitHubSource'
import JiraSource from './JiraSource'
import SlackSource from './SlackSource'

// The three source pages, each mounted against a stubbed API.
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
    '/api/teams/t1/github-repos': { repos: ['sky/agent-runner', 'sky/planner'] },
    '/api/github/repos/list': [
      { full_name: 'sky/agent-runner' },
      { full_name: 'sky/planner' },
      { full_name: 'sky/docs-site' },
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

  it('lists a tracked repository the picker can no longer see', async () => {
    stub({
      '/api/teams/t1/github-repos': { repos: ['sky/vanished'] },
      '/api/github/repos/list': [{ full_name: 'sky/planner' }],
    })
    render(<GitHubSource {...BODY} />)

    // Dropping it would make the page lie about what the team watches.
    await waitFor(() => expect(screen.getByText('sky/vanished')).toBeInTheDocument())
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
})

describe('Jira source page', () => {
  const SETTINGS = {
    '/api/teams/t1/settings': {
      team_settings: {
        JiraProjects: ['PLAT', 'SKY'],
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
    // team could watch. The stub matches by path, so every project's status
    // read answers the same list — per-project routing is asserted on the
    // fetch calls instead.
    '/api/jira/projects/list': [
      { key: 'PLAT', name: 'Platform Core' },
      { key: 'SKY', name: 'Skyworks' },
      { key: 'OPS', name: 'Operations' },
    ],
    '/api/jira/statuses': [
      { id: '1', name: 'Ready' },
      { id: '2', name: 'In Progress' },
      { id: '3', name: 'Done' },
      { id: '4', name: 'QA' },
      { id: '5', name: 'Blocked' },
    ],
  }

  /** The fixture with a single watched project — the no-strip shape. */
  const ONE_PROJECT = {
    ...SETTINGS,
    '/api/teams/t1/settings': {
      ...SETTINGS['/api/teams/t1/settings'],
      jira_projects: SETTINGS['/api/teams/t1/settings'].jira_projects.slice(0, 1),
    },
  }

  it('has no chart — the board is its build', async () => {
    stub(SETTINGS)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('What each status means')).toBeInTheDocument())

    expect(document.querySelector('.sc')).toBeNull()
    expect(document.querySelector('.sr-board')).not.toBeNull()
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
    const statusReads = fetchMock.mock.calls
      .map((c) => String(c[0]))
      .filter((u) => u.includes('/api/jira/statuses'))
    expect(statusReads).toEqual(['/api/jira/statuses?project=PLAT'])
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
    const statusReads = fetchMock.mock.calls
      .map((c) => String(c[0]))
      .filter((u) => u.includes('/api/jira/statuses'))
    expect(statusReads).toEqual([
      '/api/jira/statuses?project=PLAT',
      '/api/jira/statuses?project=SKY',
    ])
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

  it('states why Jira is off and holds the live reads, keeping the config', async () => {
    sources.state = { jira: 'disabled' }
    const fetchMock = stub(SETTINGS)
    render(<JiraSource {...BODY} />)

    await waitFor(() =>
      expect(screen.getByText('Jira is turned off — an org admin paused it.')).toBeInTheDocument(),
    )
    // The stored configuration still renders — watched keys, the board's map —
    // because the team's config is TF's data, not Jira's.
    expect(screen.getAllByText('PLAT').length).toBeGreaterThan(0)
    expect(screen.getByText('In Progress')).toBeInTheDocument()

    // No catalog page, no status vocabulary: both reads would just collect
    // refusals while the source is off.
    const urls = fetchMock.mock.calls.map((c) => String(c[0]))
    expect(urls.some((u) => u.includes('/api/jira/projects/list'))).toBe(false)
    expect(urls.some((u) => u.includes('/api/jira/statuses'))).toBe(false)
    // And with no catalog there is nothing to offer Watch on: only the
    // watched rows are listed.
    expect(screen.queryByText('Operations')).not.toBeInTheDocument()
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
      expect(plat.key).toBe('PLAT')
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
})
