import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { resetModelCatalogForTest } from '../../hooks/useModelCatalog'

// The team overview's two wired verbs: the band as navigation, and the archive.
//
// Both are here because both were once drawn but not connected — a source row
// that looked like a way in and was not, and an Archive button that went to
// another page instead of archiving. What a control LOOKS like it does is not
// checkable in a screenshot; what it calls is.

// The page reads through the shared helpers (fetchTeamRoster, fetchTeamRepos),
// and those read through apiClient — so stubbing the client covers the page and
// the sub-pages it mounts alike. apiList and apiListAll are stubbed beside
// apiJSON because they call it through apiClient's own local binding, which a
// mocked export does not reach.
const api = vi.hoisted(() => ({
  apiJSON: vi.fn(),
  apiFetch: vi.fn(),
  apiList: vi.fn(),
  apiListAll: vi.fn(),
}))
vi.mock('../../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/apiClient')>()
  return { ...actual, ...api }
})

const life = vi.hoisted(() => ({ fetchArchivePreview: vi.fn(), archiveTeam: vi.fn() }))
vi.mock('../../lib/teamLifecycle', () => life)

const toasts = vi.hoisted(() => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))
vi.mock('../../components/Toast/toastStore', () => toasts)

// Run mode, mutable per test. The page reads it the way every other surface
// does — an absent auth context IS local — so a test that wants the multi-mode
// roster has to say so, and the local case is a test rather than an accident.
const session = vi.hoisted(() => ({ present: true }))
vi.mock('../../contexts/AuthContext', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../contexts/AuthContext')>()
  return { ...actual, useOptionalAuth: () => (session.present ? ({} as never) : null) }
})

// The two roles this page splits on, mutable per test: team admin gates the
// roster's verbs, org admin gates the archive.
const roles = vi.hoisted(() => ({ team: 'admin', org: true }))
vi.mock('../../hooks/useOrgRole', () => ({
  useOrgRole: () => ({ role: roles.org ? 'admin' : 'member', isAdmin: roles.org, loading: false }),
}))
vi.mock('../../hooks/useTeamRole', () => ({
  useTeamRole: () => ({ roleForTeam: () => roles.team, loading: false }),
}))

// The availability read, controllable per test — mocked so the real module's
// per-org cache can't leak one test's answer into another.
const sources = vi.hoisted(() => ({ state: {} as Record<string, string | undefined> }))
vi.mock('../../hooks/useEventSources', () => ({
  useEventSources: () => ({
    sources: [],
    loaded: true,
    stateOf: (kind: string) => sources.state[kind],
    canProduce: (kind: string) => sources.state[kind] === undefined,
  }),
}))

const teamsRefresh = vi.hoisted(() => vi.fn())
vi.mock('../../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: [{ id: 't1', name: 'platform', slug: 'platform' }],
    lastActingTeamId: 't1',
    multi: false,
    loading: false,
    loaded: true,
    error: null,
    refresh: teamsRefresh,
    createTeam: vi.fn(),
  }),
}))
vi.mock('../../contexts/OrgContext', () => ({ useActiveOrgId: () => 'org-1' }))
vi.mock('../../hooks/useOrgHref', () => ({ useOrgHref: () => (p: string) => p }))

import TeamSettings from './TeamSettings'

const MEMBERS = {
  members: [
    {
      user_id: 'u1',
      display_name: 'robin',
      github_username: 'robin',
      jira_account_id: null,
      role: 'admin',
      is_current_user: true,
    },
    {
      user_id: 'u2',
      display_name: 'dana',
      github_username: 'dana',
      jira_account_id: null,
      role: 'member',
      is_current_user: false,
    },
  ],
}

// The flow node and the spend slice the figures read. Slack's null count is
// the shape's point: an undefined measurement renders as a dash, not a zero.
const ACTIVITY = {
  team_id: 't1',
  by_day: [{ date: '2026-08-20', events: 8, tasks: 4 }],
  by_day_source: [{ date: '2026-08-20', source: 'github', events: 5, tasks: 2 }],
  by_source: [
    { source: 'github', events: 5, tasks: 2, runs: 1, last_poll_at: '2026-08-20T00:00:00Z' },
    { source: 'jira', events: 3, tasks: 1, runs: 0, last_poll_at: null },
    { source: 'slack', events: null, tasks: 1, runs: 0, last_poll_at: null },
  ],
}
const USAGE = {
  total_cost_usd: 12.4,
  by_user: [{ user_id: 'u1', display_name: 'robin', cost: 9.152 }],
  // Opus carries the whole model-attributed window: sonnet is enabled with no
  // spend (a 0% reading), haiku is team-unenabled (a dash, nothing to report).
  by_model: [{ model: 'claude-opus-5', cost: 9.3 }],
}

// The org's enable-set as the catalog read answers it. GPT X is org-disabled:
// the team surface must never list it — a model the org has not enabled is an
// inventory of other people's decisions.
const MODELS_READ = {
  items: [
    {
      key: 'claude-opus-5',
      display_name: 'Claude Opus 5',
      provider: 'anthropic',
      provider_display_name: 'Anthropic',
      enabled: true,
      prices_per_mtok: { input: 5, output: 25, cache_read: 0.5, cache_write: 6.25 },
      display_order: 0,
    },
    {
      key: 'claude-sonnet-5',
      display_name: 'Claude Sonnet 5',
      provider: 'anthropic',
      provider_display_name: 'Anthropic',
      enabled: true,
      prices_per_mtok: { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75 },
      display_order: 1,
    },
    {
      key: 'claude-haiku-4-5',
      display_name: 'Claude Haiku 4.5',
      provider: 'anthropic',
      provider_display_name: 'Anthropic',
      enabled: true,
      prices_per_mtok: { input: 1, output: 4, cache_read: 0.1, cache_write: 1.25 },
      display_order: 2,
    },
    {
      key: 'gpt-x',
      display_name: 'GPT X',
      provider: 'openai',
      provider_display_name: 'OpenAI',
      enabled: false,
      display_order: 3,
    },
  ],
}

// The team's stored choices: opus is the default, haiku is org-enabled but not
// team-enabled — the row the panel exists to switch on.
const TEAM_SETTINGS = {
  team_settings: {
    DefaultModel: 'claude-opus-5',
    EnabledModels: ['claude-opus-5', 'claude-sonnet-5'],
  },
}

beforeEach(() => {
  session.present = true
  roles.team = 'admin'
  roles.org = true
  sources.state = {}
  // The catalog hook caches per org at module level; without the reset, the
  // first test's answer would be every later test's answer.
  resetModelCatalogForTest()
  api.apiJSON.mockImplementation((path: string, init?: RequestInit) => {
    if (path.endsWith('/members/list'))
      return Promise.resolve({ items: MEMBERS.members, total_count: MEMBERS.members.length })
    if (path.endsWith('/github-repos'))
      return Promise.resolve({ repos: ['sky/planner', 'sky/runner'] })
    if (path.includes('/activity?')) return Promise.resolve(ACTIVITY)
    if (path.includes('/usage?')) return Promise.resolve(USAGE)
    if (path.endsWith('/models')) return Promise.resolve(MODELS_READ)
    // The settings PATCH answers with the resource as a follow-up GET would,
    // which is what the page reconciles its optimistic state from.
    if (path.endsWith('/settings') && init?.method === 'PATCH') {
      const body = JSON.parse(String(init.body)) as {
        ai_model?: string
        enabled_models?: string[]
      }
      return Promise.resolve({
        team_settings: {
          DefaultModel: body.ai_model ?? TEAM_SETTINGS.team_settings.DefaultModel,
          EnabledModels: body.enabled_models ?? TEAM_SETTINGS.team_settings.EnabledModels,
        },
      })
    }
    if (path.endsWith('/settings')) return Promise.resolve(TEAM_SETTINGS)
    return Promise.reject(new Error('no route: ' + path))
  })
  // The org directory behind `+ add teammate`. Empty is the honest default
  // here: no test in this file adds anyone, and a candidate nobody asked for
  // would just be a row to explain.
  api.apiList.mockResolvedValue({ items: [], next_page_token: '', total_count: 0 })
  api.apiListAll.mockResolvedValue([])
  api.apiFetch.mockResolvedValue({ ok: true })
  life.fetchArchivePreview.mockResolvedValue({
    team_id: 't1',
    name: 'platform',
    archived: false,
    active_runs: 3,
  })
  life.archiveTeam.mockResolvedValue({ cancelled_runs: 3 })
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

afterEach(() => {
  vi.clearAllMocks()
  vi.unstubAllGlobals()
})

function renderPage() {
  return render(
    <MemoryRouter>
      <TeamSettings />
    </MemoryRouter>,
  )
}

/** The band's own EVENT SOURCES head — the opened panel names itself the same. */
const bandHead = () => document.querySelector<HTMLElement>('.ts-panel-right .ts-panel-t')

describe('the band', () => {
  it('takes a named source to its own page', async () => {
    renderPage()
    await waitFor(() => expect(bandHead()).not.toBeNull())

    fireEvent.click(screen.getByText('GitHub'))

    // Not the panel that lists the sources — the source itself.
    await waitFor(() => expect(document.querySelector('.sp[data-source="github"]')).not.toBeNull())
    expect(screen.getByText('event source · platform')).toBeInTheDocument()
  })

  it('lets a row the org cannot reach fall through to the panel, not its page', async () => {
    sources.state = { jira: 'disabled' }
    renderPage()
    await waitFor(() => expect(bandHead()).not.toBeNull())

    // Not a control: a source with nothing to configure offers no way in.
    const row = screen.getByText('Jira').closest('.ts-flow-row')!
    expect(row.tagName).toBe('DIV')
    expect(row).toHaveAttribute('data-off')

    // The click lands on the cell behind it — the panel opens, the source
    // page does not, and the card says why the source is off.
    fireEvent.click(screen.getByText('Jira'))
    expect(screen.getByText('Pick a source to configure')).toBeInTheDocument()
    expect(document.querySelector('.sp[data-source="jira"]')).toBeNull()
    expect(screen.getByText('Jira is turned off — an org admin paused it.')).toBeInTheDocument()
  })

  it('opens the panel from the cell around the rows, not only from its head', async () => {
    renderPage()
    await waitFor(() => expect(bandHead()).not.toBeNull())

    fireEvent.click(screen.getByText('last 7 days · repositories, projects, channels'))
    expect(screen.getByText('Pick a source to configure')).toBeInTheDocument()

    // And the head hands it back, rather than toggling twice through the cell
    // behind it and landing where it started.
    fireEvent.click(bandHead()!)
    expect(screen.queryByText('Pick a source to configure')).not.toBeInTheDocument()
  })
})

describe('archiving the team', () => {
  it('states what it will stop, then stops it', async () => {
    renderPage()
    await waitFor(() => expect(screen.getByText('Archive this team')).toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: 'Archive' }))

    await waitFor(() => expect(screen.getByText('Archive platform?')).toBeInTheDocument())
    const panel = document.querySelector('.dlg')!

    // It OPENS WHOLE, on the first beat of its build. The panel measures an
    // empty frame and then fills it, so a line that arrived after that beat
    // would resize the thing the beat just claimed to have sized — which is why
    // both reads finish before the dialog is asked for.
    expect(panel.getAttribute('data-phase')).toBe('frame')
    expect(panel.classList.contains('built')).toBe(false)

    // Every consequence is read, not assumed: the repositories from the team's
    // tracked set, the members from the roster, the live work from the preview.
    expect(screen.getByText('2 repositories stop being watched')).toBeInTheDocument()
    expect(screen.getByText('2 members lose access')).toBeInTheDocument()
    expect(screen.getByText('3 delegations stop now')).toBeInTheDocument()
    expect(screen.getByText('Run history and cost records are kept')).toBeInTheDocument()

    // And then it arrives, through the sequence rather than instead of it.
    await waitFor(() => expect(panel.classList.contains('built')).toBe(true), { timeout: 3000 })

    const confirm = screen.getAllByRole('button', { name: 'Archive' })
    fireEvent.click(confirm[confirm.length - 1])

    await waitFor(() => expect(life.archiveTeam).toHaveBeenCalledWith('t1'))
    // The team is gone from /api/teams, so the page has to be told to look again.
    await waitFor(() => expect(teamsRefresh).toHaveBeenCalled())
    expect(toasts.toast.success).toHaveBeenCalled()
  })

  it('withdraws the question when it cannot say what would be lost', async () => {
    life.fetchArchivePreview.mockRejectedValue(new Error('nope'))
    renderPage()
    await waitFor(() => expect(screen.getByText('Archive this team')).toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: 'Archive' }))

    // A destructive confirm that cannot state its consequences is not one.
    await waitFor(() => expect(toasts.toast.error).toHaveBeenCalled())
    expect(screen.queryByText('Archive platform?')).not.toBeInTheDocument()
    expect(life.archiveTeam).not.toHaveBeenCalled()
  })

  it('is absent for a team admin who is not an org admin', async () => {
    roles.org = false
    renderPage()
    // Named twice on the row — once as the person, once as the account.
    await waitFor(() => expect(screen.getAllByText('robin').length).toBeGreaterThan(0))

    // Absent, not disabled: the endpoint is the org's, and 404s for them.
    expect(screen.queryByText('Archive this team')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Archive' })).not.toBeInTheDocument()
  })
})

describe('the sources panel', () => {
  it('sinks a source the org cannot reach, and counts only the connected', async () => {
    sources.state = { jira: 'unconfigured' }
    renderPage()
    await waitFor(() => expect(bandHead()).not.toBeNull())

    fireEvent.click(bandHead()!)

    // The card says the one sentence that would change the state, loses its
    // numbers, and stops taking a click; the foot counts what is actually on.
    expect(screen.getByText('Jira is not connected for this workspace.')).toBeInTheDocument()
    const card = document.querySelector('.tf-src[data-state="unavailable"]')
    expect(card).not.toBeNull()
    expect(card?.getAttribute('data-clickable')).toBeNull()
    expect(screen.getByText(/2 sources connected/)).toBeInTheDocument()
  })
})

describe('the figures', () => {
  const figureValue = (label: string) => {
    const l = Array.from(document.querySelectorAll('.ts-fig-l')).find(
      (e) => e.textContent === label,
    )
    return l?.closest('.ts-fig')?.querySelector('.ts-fig-v')?.textContent ?? null
  }

  it('fills the header, the band, and the roster from the two reads', async () => {
    renderPage()
    await waitFor(() => expect(screen.getByText('$12.40')).toBeInTheDocument())

    // tasks · 7 days is the by-source tasks summed; every flow figure on the
    // page derives from the one node read, so they agree by construction.
    expect(figureValue('tasks · 7 days')).toBe('4')

    // The band: total events in the head, per-source values in the rows —
    // slack's undefined count stays a dash — and the two stream stats.
    expect(document.querySelector('.ts-panel-right .ts-panel-n')?.textContent).toBe('8 events')
    const rowValues = Array.from(document.querySelectorAll('.ts-flow-row-v')).map(
      (e) => e.textContent,
    )
    expect(rowValues).toEqual(['5', '3', '—'])
    const stats = Array.from(document.querySelectorAll('.ts-stat-v')).map((e) => e.textContent)
    expect(stats).toEqual(['8', '4'])

    // The roster's spend column: a member in by_user shows their cost, a
    // member absent from it genuinely spent nothing once the read answered.
    expect(screen.getByText('$9.15')).toBeInTheDocument()
    expect(screen.getByText('$0.00')).toBeInTheDocument()
  })

  it('never fires the spend read for a member, and still fills the flow', async () => {
    roles.team = 'member'
    roles.org = false
    renderPage()
    await waitFor(() => expect(figureValue('tasks · 7 days')).toBe('4'))

    // The endpoint is admin-gated; a member firing it would just collect a
    // refusal. The gate is the hook's enabled flag, so the call never leaves.
    expect(api.apiJSON.mock.calls.some((c) => String(c[0]).includes('/usage?'))).toBe(false)
    expect(figureValue('spent · 14 days')).toBeNull()
  })
})

// The roster's verbs against the one invariant the database enforces: a team
// keeps an admin. The point of testing it here rather than trusting the 409 is
// that the refusal has to be legible BEFORE the gesture, and a control that
// merely looks live is exactly what a screenshot cannot catch.
describe('the last admin', () => {
  /** Select the roster row whose name cell reads `name`. */
  function selectRow(name: string) {
    const row = Array.from(document.querySelectorAll<HTMLElement>('.tb-row')).find((r) =>
      r.textContent?.includes(name),
    )
    if (!row) throw new Error('no roster row for ' + name)
    fireEvent.click(row)
  }

  const bar = () => document.querySelector<HTMLElement>('.selbar-count')

  /** The roles the open picker is offering. */
  function roleChoices(): string[] {
    fireEvent.click(screen.getByLabelText('Change role'))
    return Array.from(document.querySelectorAll('[role="option"]')).map((o) =>
      (o.textContent || '').trim(),
    )
  }

  it('narrows the picker to promotion and withdraws remove when the seat is at stake', async () => {
    renderPage()
    await waitFor(() => expect(document.querySelectorAll('.tb-row').length).toBeGreaterThan(0))

    // robin is the roster's only admin.
    selectRow('robin')
    await waitFor(() => expect(bar()).not.toBeNull())

    // Removing them empties the seat, so the verb is not offered.
    expect(screen.queryByText('Hold to remove')).not.toBeInTheDocument()
    // Demoting them empties it too — but promoting cannot, so the picker
    // narrows to that rather than closing, and the way out stays in reach.
    expect(roleChoices()).toEqual(['Team admin'])
    // And the bar stays a bar: a count, not a paragraph.
    expect(bar()?.textContent).toBe('1 selected')
  })

  it('keeps every choice when an admin is left behind', async () => {
    renderPage()
    await waitFor(() => expect(document.querySelectorAll('.tb-row').length).toBeGreaterThan(0))

    selectRow('dana')

    await waitFor(() => expect(bar()?.textContent).toBe('1 selected'))
    expect(screen.getByText('Hold to remove')).toBeInTheDocument()
    expect(roleChoices()).toEqual(['Team admin', 'Member', 'Viewer — read only'])
  })

  it('promotes a selection that holds every admin, since that cannot empty the seat', async () => {
    api.apiJSON.mockImplementation((path: string) => {
      if (path.endsWith('/members/list'))
        return Promise.resolve({
          items: [MEMBERS.members[0], { ...MEMBERS.members[1], role: 'admin' }],
          total_count: 2,
        })
      if (path.endsWith('/github-repos')) return Promise.resolve({ repos: [] })
      if (path.includes('/activity?')) return Promise.resolve(ACTIVITY)
      if (path.includes('/usage?')) return Promise.resolve(USAGE)
      return Promise.reject(new Error('no route: ' + path))
    })
    renderPage()
    await waitFor(() => expect(document.querySelectorAll('.tb-row').length).toBe(2))

    selectRow('robin')
    selectRow('dana')
    await waitFor(() => expect(bar()?.textContent).toBe('2 selected'))

    // Both admins are selected, so demotion is gone — but promotion is still
    // offered, and taking it is a legal write the server accepts.
    expect(roleChoices()).toEqual(['Team admin'])
    fireEvent.click(screen.getByRole('option', { name: 'Team admin' }))
    expect(screen.queryByText('Hold to remove')).not.toBeInTheDocument()
  })

  it('counts admins over the whole roster, not the rows the filter left showing', async () => {
    api.apiJSON.mockImplementation((path: string) => {
      if (path.endsWith('/members/list'))
        return Promise.resolve({
          items: [
            { ...MEMBERS.members[0] },
            { ...MEMBERS.members[1], role: 'admin' },
            {
              user_id: 'u3',
              display_name: 'sam',
              github_username: 'sam',
              jira_account_id: null,
              role: 'member',
              is_current_user: false,
            },
          ],
          total_count: 3,
        })
      if (path.endsWith('/github-repos')) return Promise.resolve({ repos: [] })
      if (path.includes('/activity?')) return Promise.resolve(ACTIVITY)
      if (path.includes('/usage?')) return Promise.resolve(USAGE)
      return Promise.reject(new Error('no route: ' + path))
    })
    renderPage()
    await waitFor(() => expect(document.querySelectorAll('.tb-row').length).toBe(3))

    // Hide the second admin behind the filter, then act on the first. The seat
    // is held by the roster, not by what is on screen — counting the visible
    // rows here would refuse a removal that is perfectly legal.
    fireEvent.change(screen.getByLabelText('Filter members'), { target: { value: 'robin' } })
    await waitFor(() => expect(document.querySelectorAll('.tb-row').length).toBe(1))
    selectRow('robin')

    await waitFor(() => expect(bar()?.textContent).toBe('1 selected'))
    expect(screen.getByText('Hold to remove')).toBeInTheDocument()
  })

  it('draws no roster verbs at all in local mode', async () => {
    session.present = false
    renderPage()
    await waitFor(() => expect(document.querySelectorAll('.tb-row').length).toBeGreaterThan(0))

    // Every team-member route 404s in local, so the verbs are absent rather
    // than drawn over an endpoint that cannot serve them. No selection to make,
    // and nobody to add.
    expect(document.querySelector('.tb-row')?.getAttribute('data-selectable')).toBeNull()
    expect(screen.queryByText('add teammate')).not.toBeInTheDocument()
  })
})

// The models surface: the band cell's preview and the panel it opens into.
// The reads are the org catalog, the team's settings slice, and the admin-gated
// spend cut; the writes are one PATCH each — and what a control refuses is as
// load-bearing as what it sends.
describe('the models panel', () => {
  /** Scope to the opened panel's table — the band above repeats the names. */
  const table = () => document.querySelector('.ts-tablewrap') as HTMLElement

  const patches = () =>
    api.apiJSON.mock.calls.filter(
      ([, init]) => (init as RequestInit | undefined)?.method === 'PATCH',
    )

  async function openPanel() {
    renderPage()
    fireEvent.click(await screen.findByText('CONFIGURED MODELS'))
    await screen.findByText('Claude Haiku 4.5')
  }

  it('moves the star onto an unenabled row and enables it in the same PATCH', async () => {
    await openPanel()
    const row = within(table()).getByText('Claude Haiku 4.5').closest('.tb-row') as HTMLElement
    fireEvent.click(within(row).getByRole('button'))

    await waitFor(() => expect(patches()).toHaveLength(1))
    const [path, init] = patches()[0]
    expect(path).toBe('/api/teams/t1/settings')
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      ai_model: 'claude-haiku-4-5',
      enabled_models: ['claude-opus-5', 'claude-sonnet-5', 'claude-haiku-4-5'],
    })
  })

  it('refuses the toggle on the default row: the fallback cannot be switched off', async () => {
    await openPanel()
    const row = within(table()).getByText('Claude Opus 5').closest('.tb-row') as HTMLElement
    fireEvent.click(within(row).getByRole('checkbox', { name: 'ENABLED' }))

    expect(patches()).toHaveLength(0)
  })

  it('replaces the whole set when a row is switched off', async () => {
    await openPanel()
    const row = within(table()).getByText('Claude Sonnet 5').closest('.tb-row') as HTMLElement
    fireEvent.click(within(row).getByRole('checkbox', { name: 'ENABLED' }))

    await waitFor(() => expect(patches()).toHaveLength(1))
    expect(JSON.parse(String((patches()[0][1] as RequestInit).body))).toEqual({
      enabled_models: ['claude-opus-5'],
    })
  })

  it('reads honestly per row: enabled-no-spend is 0%, unenabled is a dash', async () => {
    await openPanel()
    // Only the org's enable-set appears at all — a model the org has not
    // enabled is not offerable here, whoever is looking.
    expect(screen.queryByText('GPT X')).toBeNull()
    await waitFor(() => {
      const values = Array.from(table().querySelectorAll('.tb-share-value')).map(
        (el) => el.textContent,
      )
      expect(values).toEqual(['100%', '0%', '—'])
    })
  })

  it('gives a member no star, no toggle, and no spend claims', async () => {
    roles.team = 'member'
    roles.org = false
    await openPanel()

    // Absent, not disabled: a verb that 403s is not information.
    expect(table().querySelector('.tb-mark')).toBeNull()
    expect(table().querySelector('.tb-toggle')).toBeNull()
    // The usage read never fired for them, and unknown is not zero.
    const values = Array.from(table().querySelectorAll('.tb-share-value')).map(
      (el) => el.textContent,
    )
    expect(values).toEqual(['—', '—', '—'])
  })

  it('fills the band cell from the reads: enabled models, default, out-price, providers', async () => {
    renderPage()
    await screen.findByText('(default)')
    const cell = document.querySelector('.ts-band .ts-panel:not(.ts-panel-right)') as HTMLElement

    // Team-enabled models only — haiku is org-enabled but off for this team,
    // and GPT X is org-disabled.
    const names = Array.from(cell.querySelectorAll('.ts-row-n')).map((el) => el.textContent)
    expect(names).toEqual(['Claude Opus 5', 'Claude Sonnet 5'])
    // The default is tagged on the row that carries it.
    const opusRow = within(cell).getByText('Claude Opus 5').closest('.ts-row') as HTMLElement
    expect(within(opusRow).getByText('(default)')).toBeInTheDocument()
    // Out-price per M straight from the API's price fields.
    expect(within(cell).getByText('$25 / M')).toBeInTheDocument()
    expect(within(cell).getByText('$15 / M')).toBeInTheDocument()
    // Distinct providers among the org's enable-set, not a model count.
    expect(within(cell).getByText('1 provider')).toBeInTheDocument()
    // The admin's spend bars: opus carries the whole cut, sonnet reads zero.
    await waitFor(() => {
      const fills = Array.from(cell.querySelectorAll<HTMLElement>('.ts-meter-fill')).map(
        (el) => el.style.width,
      )
      expect(fills).toEqual(['100%', '0%'])
    })
  })
})
