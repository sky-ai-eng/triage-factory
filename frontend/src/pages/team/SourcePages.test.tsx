import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import GitHubSource from './GitHubSource'
import JiraSource from './JiraSource'
import SlackSource from './SlackSource'

// The three source pages, each mounted against a stubbed API.
//
// What is worth pinning is the honesty rule, because it is the thing a
// screenshot cannot check and the thing most likely to be "fixed" later by
// somebody filling a dash with a zero: EVERY FIGURE IS EITHER REAL OR AN EM
// DASH. A zero would be a claim about a team's week, and several of these
// figures need aggregations the API does not have yet.
//
// The rest is the wiring a member must never see, and the set-replace each
// verb resolves to.

const BODY = {
  teamId: 't1',
  teamName: 'platform',
  isAdmin: true,
  onBack: () => {},
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
afterEach(() => vi.unstubAllGlobals())

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

  it('draws a dash, never a zero, for every figure nothing counts yet', async () => {
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
        JiraProjects: ['PLAT'],
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
          pickup: { members: ['Ready'] },
          in_progress: { members: ['In Progress'], canonical: 'In Progress' },
          done: { members: ['Done'], canonical: 'Done' },
        },
      ],
      member_count: 4,
      role: 'admin',
    },
    '/api/jira/statuses': [
      { id: '1', name: 'Ready' },
      { id: '2', name: 'In Progress' },
      { id: '3', name: 'Done' },
      { id: '4', name: 'QA' },
    ],
  }

  it('has no chart — the board is its build', async () => {
    stub(SETTINGS)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText('What each status means')).toBeInTheDocument())

    expect(document.querySelector('.sc')).toBeNull()
    expect(document.querySelector('.sr-board')).not.toBeNull()
  })

  it('maps the stored rules into the board and names the project it shows', async () => {
    stub(SETTINGS)
    render(<JiraSource {...BODY} />)

    await waitFor(() => expect(screen.getByText(/showing PLAT/)).toBeInTheDocument())
    // A status nobody mapped sits in the tray rather than being hidden.
    expect(screen.getByText('QA')).toBeInTheDocument()
  })

  it('offers no drag, because nothing would persist it', async () => {
    stub(SETTINGS)
    render(<JiraSource {...BODY} />)
    await waitFor(() => expect(screen.getByText(/showing PLAT/)).toBeInTheDocument())

    // Absent, not disabled: a cursor that says "pick me up" over a chip that
    // cannot be picked up promises and then fails.
    expect(document.querySelector('[draggable="true"]')).toBeNull()
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

    // The registry carries a timestamp; nothing counts mentions in a window.
    await waitFor(() => expect(screen.getByText('4m')).toBeInTheDocument())
    expect(row('mentions in 7 days').textContent).toContain('—')
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
