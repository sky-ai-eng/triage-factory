import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import type {
  AccessLogResponse,
  UsageCategoryBucket,
  UsageMeResponse,
  UsageOrgResponse,
  UsageTeamResponse,
} from '../types'

// Drive each viewer shape by toggling the mocked role hooks. useOrgRole gates the
// Org section; useTeams (filtered to role === 'admin') sources the Team section.
const roleMock = vi.hoisted(() => ({ isAdmin: false }))
vi.mock('../hooks/useOrgRole', () => ({
  useOrgRole: () => ({
    role: roleMock.isAdmin ? 'admin' : 'member',
    isAdmin: roleMock.isAdmin,
    loading: false,
  }),
}))

const teamsMock = vi.hoisted(() => ({
  teams: [] as { id: string; name: string; slug: string; role: string }[],
}))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: teamsMock.teams,
    lastActingTeamId: '',
    multi: teamsMock.teams.length >= 2,
    loading: false,
    loaded: true,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
}))

// useOptionalAuth is null in local mode (N=1, no AuthProvider) and a context
// object in multi mode. The page only checks `=== null` to decide whether the
// local user counts as the org admin, so the shape doesn't matter here.
const authMock = vi.hoisted(() => ({ local: false }))
vi.mock('../contexts/AuthContext', () => ({
  useOptionalAuth: () => (authMock.local ? null : ({} as unknown)),
}))

// useEntitlements gates the EE access-log band. Default off (so the spend-only
// tests above never render or fetch it); the access-log tests flip it on. loaded
// is always true here — the real hook's pre-resolve flash isn't under test.
const entMock = vi.hoisted(() => ({ governance: false }))
vi.mock('../hooks/useEntitlements', () => ({
  FeatureGovernance: 'governance',
  useEntitlements: () => ({
    has: (f: string) => f === 'governance' && entMock.governance,
    loaded: true,
  }),
}))

import Usage from './Usage'

// --- fixtures -------------------------------------------------------------

const cat = (category: string, cost: number): UsageCategoryBucket => ({
  category,
  cost,
  input_tokens: 100,
  output_tokens: 50,
  cache_read_tokens: 0,
  cache_creation_tokens: 0,
})

const ME: UsageMeResponse = {
  total_cost_usd: 12.5,
  by_category: [cat('manual', 8), cat('curator', 4.5)],
  by_model: [{ model: 'claude-opus-4-8', cost: 8 }],
  by_day: [{ date: '2026-06-01', cost: 12.5 }],
}

const ME_EMPTY: UsageMeResponse = {
  total_cost_usd: 0,
  by_category: [],
  by_model: [],
  by_day: [],
}

// A category bucket with non-zero cache tokens, to assert the burn-bar legend
// tooltip's total (in + out + cache) agrees with its parenthetical breakdown.
const TOK_CATEGORY: UsageCategoryBucket = {
  category: 'autonomous',
  cost: 25,
  input_tokens: 100,
  output_tokens: 50,
  cache_read_tokens: 30,
  cache_creation_tokens: 20,
}

const TEAM: UsageTeamResponse = {
  team_id: 't1',
  team_name: 'Platform',
  total_cost_usd: 25,
  by_category: [cat('autonomous', 25)],
  // Bob is team-only (org by_user below has Alice only), so it disambiguates the
  // Team section from the Org section when both render.
  by_user: [
    { user_id: 'u1', display_name: 'Alice', cost: 15 },
    { user_id: 'u2', display_name: 'Bob', cost: 10 },
  ],
  by_rule: [{ trigger_id: 'tr1', rule_name: 'CI Fixer', cost: 25 }],
  by_model: [{ model: 'claude-opus-4-8', cost: 25 }],
  by_day: [{ date: '2026-06-02', cost: 25 }],
}

const TEAM_T2: UsageTeamResponse = {
  ...TEAM,
  team_id: 't2',
  team_name: 'Growth',
  by_rule: [{ trigger_id: 'tr2', rule_name: 'Stale Sweeper', cost: 25 }],
}

const ORG: UsageOrgResponse = {
  total_cost_usd: 100,
  by_team: [
    { team_id: 't1', team_name: 'Platform', cost: 60 },
    { team_id: 't2', team_name: 'Growth', cost: 30 },
  ],
  by_user: [{ user_id: 'u1', display_name: 'Alice', cost: 40 }],
  org_level: [{ category: 'system_overhead', cost: 10 }],
  by_category: [cat('autonomous', 50), cat('curator', 40), cat('system_overhead', 10)],
  by_model: [{ model: 'claude-opus-4-8', cost: 90 }],
  by_day: [{ date: '2026-06-03', cost: 100 }],
  // by_rule is folded into /org in local mode only (multi-tenant orgs omit it).
  // LocalConsole reads it straight from the rollup — no second team fetch.
  by_rule: [{ trigger_id: 'tr1', rule_name: 'CI Fixer', cost: 50 }],
}

// stubUsageFetch routes GET /api/usage/* by path (query stripped) to a canned
// payload; an unmapped path resolves to a 404-shaped response (readError-safe).
function stubUsageFetch(payloads: Record<string, unknown>) {
  const fetchMock = vi.fn((input: unknown) => {
    const path = String(input).split('?')[0]
    if (path in payloads) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(payloads[path]) })
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

function fetchedPaths(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0]).split('?')[0])
}

// Access-log fixtures: one membership row + one credential row, newest-first.
const ACCESS_LOG: AccessLogResponse = {
  items: [
    {
      id: 'a1',
      action: 'org_role_changed',
      action_label: 'changed Bob from member to admin',
      actor_name: 'Alice',
      target_name: 'Bob',
      created_at: '2026-06-20T11:00:00Z',
    },
    {
      id: 'a2',
      action: 'credential_set',
      action_label: 'set the GitHub PAT for github.example.com',
      actor_name: 'Alice',
      created_at: '2026-06-20T10:00:00Z',
    },
  ],
  limit: 50,
  offset: 0,
  has_more: false,
}

const ACCESS_LOG_EMPTY: AccessLogResponse = { items: [], limit: 50, offset: 0, has_more: false }

// fullUrls returns each fetched URL with its query intact (fetchedPaths strips
// the query) — for asserting the access-log category param.
function fullUrls(fetchMock: ReturnType<typeof vi.fn>): string[] {
  return fetchMock.mock.calls.map((c) => String(c[0]))
}

beforeEach(() => {
  roleMock.isAdmin = false
  teamsMock.teams = []
  authMock.local = false
  entMock.governance = false
})
afterEach(() => vi.unstubAllGlobals())

describe('Usage page', () => {
  it('org admin sees personal + team + org sections', async () => {
    roleMock.isAdmin = true
    teamsMock.teams = [
      { id: 't1', name: 'Platform', slug: 'platform', role: 'admin' },
      { id: 't2', name: 'Growth', slug: 'growth', role: 'admin' },
    ]
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': TEAM,
      '/api/usage/teams/t2': TEAM_T2,
      '/api/usage/org': ORG,
    })

    render(<Usage />)

    // All three section bands render.
    expect(await screen.findByText('Personal')).toBeInTheDocument()
    expect(screen.getByText('Team')).toBeInTheDocument()
    expect(screen.getByText('Org')).toBeInTheDocument()

    // Section content: personal total, a team-only label, an org-only label.
    expect(await screen.findByText('$12.50')).toBeInTheDocument() // personal total
    expect(await screen.findByText('CI Fixer')).toBeInTheDocument() // team by_rule
    expect(await screen.findByText('Bob')).toBeInTheDocument() // team by_user (org has Alice only)
    expect(await screen.findByText('Growth')).toBeInTheDocument() // org by_team
    expect(await screen.findByText('$100.00')).toBeInTheDocument() // org total

    // The three role-scoped endpoints were all hit (team defaults to the first
    // admin team).
    const paths = fetchedPaths(fetchMock)
    expect(paths).toContain('/api/usage/me')
    expect(paths).toContain('/api/usage/teams/t1')
    expect(paths).toContain('/api/usage/org')
  })

  it('regular member sees personal only — no team or org reads', async () => {
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Platform', slug: 'platform', role: 'member' }]
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': TEAM,
      '/api/usage/org': ORG,
    })

    render(<Usage />)

    expect(await screen.findByText('Personal')).toBeInTheDocument()
    await screen.findByText('$12.50')
    expect(screen.queryByText('Team')).not.toBeInTheDocument()
    expect(screen.queryByText('Org')).not.toBeInTheDocument()

    const paths = fetchedPaths(fetchMock)
    expect(paths).toContain('/api/usage/me')
    expect(paths.some((p) => p.startsWith('/api/usage/teams'))).toBe(false)
    expect(paths).not.toContain('/api/usage/org')
  })

  it('team admin (not org admin) sees personal + team, no org', async () => {
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Platform', slug: 'platform', role: 'admin' }]
    const fetchMock = stubUsageFetch({ '/api/usage/me': ME, '/api/usage/teams/t1': TEAM })

    render(<Usage />)

    expect(await screen.findByText('Team')).toBeInTheDocument()
    expect(await screen.findByText('CI Fixer')).toBeInTheDocument()
    expect(screen.queryByText('Org')).not.toBeInTheDocument()

    expect(fetchedPaths(fetchMock)).toContain('/api/usage/teams/t1')
    expect(fetchedPaths(fetchMock)).not.toContain('/api/usage/org')
  })

  it('renders a zero state for an empty window instead of crashing', async () => {
    roleMock.isAdmin = false
    teamsMock.teams = []
    stubUsageFetch({ '/api/usage/me': ME_EMPTY })

    render(<Usage />)

    expect(await screen.findByText('Personal')).toBeInTheDocument()
    expect(await screen.findByText(/no settled burn/i)).toBeInTheDocument()
  })

  it('switching the team via the dropdown refetches the selected team', async () => {
    roleMock.isAdmin = false
    teamsMock.teams = [
      { id: 't1', name: 'Platform', slug: 'platform', role: 'admin' },
      { id: 't2', name: 'Growth', slug: 'growth', role: 'admin' },
    ]
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': TEAM,
      '/api/usage/teams/t2': TEAM_T2,
    })

    render(<Usage />)

    // Defaults to the first admin team's breakdown.
    expect(await screen.findByText('CI Fixer')).toBeInTheDocument()

    // Open the switcher (≥2 admin teams) and pick Growth.
    fireEvent.click(screen.getByTitle(/active team/i))
    fireEvent.click(await screen.findByRole('button', { name: 'Growth' }))

    // The t2 breakdown loads (its distinct rule name) and the endpoint was hit.
    expect(await screen.findByText('Stale Sweeper')).toBeInTheDocument()
    await waitFor(() => expect(fetchedPaths(fetchMock)).toContain('/api/usage/teams/t2'))
  })

  it('local mode (N=1) renders one flat console from a single /org fetch', async () => {
    // Local mode: no AuthProvider (useOptionalAuth null), useOrgRole reports
    // non-admin, the sole team reports admin. Instead of three duplicated
    // sections, LocalConsole renders a flat layout: over-time + allocation /
    // category + by-rule, ALL from one /api/usage/org read — in local mode the
    // rollup folds in by_rule, so there's no second team round-trip.
    authMock.local = true
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Default', slug: 'default', role: 'admin' }]
    const fetchMock = stubUsageFetch({
      '/api/usage/org': ORG,
    })

    render(<Usage />)

    // Flat instruments, no section headings.
    expect(await screen.findByText('Over time')).toBeInTheDocument()
    expect(screen.getByText('Allocation')).toBeInTheDocument()
    expect(screen.getByText('Category')).toBeInTheDocument()
    expect(screen.getByText('By rule')).toBeInTheDocument()
    expect(screen.queryByText('Personal')).not.toBeInTheDocument()

    // System-overhead surfaces (allocation), by_rule comes from the rollup itself,
    // and the headline carries the org total.
    expect((await screen.findAllByText('System')).length).toBeGreaterThan(0)
    expect(await screen.findByText('CI Fixer')).toBeInTheDocument()
    expect(await screen.findByText('$100.00')).toBeInTheDocument()

    // Exactly one endpoint — the org rollup. No team fetch at all.
    await waitFor(() => expect(fetchedPaths(fetchMock)).toContain('/api/usage/org'))
    expect(fetchedPaths(fetchMock).some((p) => p.startsWith('/api/usage/teams'))).toBe(false)
  })

  it('over-time chart breaks spend out by model — top-N keyed, the tail folded into "other"', async () => {
    // Six models exercise the whole model-series path: the top five become their
    // own keyed/colored series and the sixth folds into a single "other", both in
    // the side key (modelSeries) and the stacked trace's pivot (by_day_model).
    // Personal is empty so the team's key is the only one on screen.
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Platform', slug: 'platform', role: 'admin' }]
    stubUsageFetch({
      '/api/usage/me': ME_EMPTY,
      '/api/usage/teams/t1': {
        ...TEAM,
        by_model: [
          { model: 'claude-opus-4-8', cost: 30 },
          { model: 'claude-sonnet-4-6', cost: 20 },
          { model: 'claude-haiku-4-5', cost: 10 },
          { model: 'claude-opus-4-7', cost: 8 },
          { model: 'claude-opus-4-6', cost: 6 },
          { model: 'claude-fable-5', cost: 2 }, // 6th by cost → folds into "other"
        ],
        by_day_model: [
          { date: '2026-06-01', model: 'claude-opus-4-8', cost: 18 },
          { date: '2026-06-01', model: 'claude-sonnet-4-6', cost: 12 },
          { date: '2026-06-01', model: 'claude-haiku-4-5', cost: 6 },
          { date: '2026-06-01', model: 'claude-opus-4-7', cost: 5 },
          { date: '2026-06-01', model: 'claude-opus-4-6', cost: 4 },
          { date: '2026-06-01', model: 'claude-fable-5', cost: 1 },
          { date: '2026-06-02', model: 'claude-opus-4-8', cost: 12 },
          { date: '2026-06-02', model: 'claude-sonnet-4-6', cost: 8 },
        ],
      },
    })

    render(<Usage />)

    // The key carries shortModel-trimmed labels for the top models...
    expect(await screen.findByText('opus-4-8')).toBeInTheDocument()
    expect(await screen.findByText('haiku-4-5')).toBeInTheDocument()
    // ...the sixth model is folded into one "other" row, never shown by name...
    expect(await screen.findByText('other')).toBeInTheDocument()
    expect(screen.queryByText('fable-5')).not.toBeInTheDocument()
    // ...and with by_day_model present the stacked path renders (key + trace), so
    // neither the empty-trace nor empty-key fallback shows.
    expect(screen.queryByText('no activity')).not.toBeInTheDocument()
    expect(screen.queryByText('no model spend')).not.toBeInTheDocument()
  })

  it('has no manual refresh button (the page auto-refreshes)', async () => {
    roleMock.isAdmin = false
    teamsMock.teams = []
    stubUsageFetch({ '/api/usage/me': ME })

    render(<Usage />)

    await screen.findByText('$12.50')
    expect(screen.queryByRole('button', { name: /refresh/i })).not.toBeInTheDocument()
  })

  it('team header uses the selected team name, not the (possibly stale) response', async () => {
    // Holding the previous response while a switch is in flight means the
    // response team_name can lag the selection — so the label must come from the
    // locally-known team. Here the response name differs to prove which wins.
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Platform', slug: 'platform', role: 'admin' }]
    stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': { ...TEAM, team_name: 'STALE-NAME' },
    })

    render(<Usage />)

    // Single admin team → no switcher; the header shows "/ Platform" (local name).
    expect(await screen.findByText(/Platform/)).toBeInTheDocument()
    expect(screen.queryByText(/STALE-NAME/)).not.toBeInTheDocument()
  })

  it('category tooltip total matches its in/out/cache breakdown', async () => {
    // Category lives in Team/Org now (dropped from Personal), so exercise it via
    // a team admin's Team band.
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Platform', slug: 'platform', role: 'admin' }]
    stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': { ...TEAM, by_category: [TOK_CATEGORY] },
    })

    render(<Usage />)

    // 100 in + 50 out + (30 read + 20 creation = 50 cached) = 200 — and the
    // breakdown parts sum to that stated total.
    expect(await screen.findByTitle('200 tokens (100 in · 50 out · 50 cached)')).toBeInTheDocument()
  })

  it('roster shows an avatar image when present, a monogram otherwise', async () => {
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Platform', slug: 'platform', role: 'admin' }]
    stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': {
        ...TEAM,
        by_user: [
          {
            user_id: 'u1',
            display_name: 'Alice Smith',
            avatar_url: 'https://ex/alice.png',
            cost: 15,
          },
          { user_id: 'u2', display_name: 'Bob', cost: 10 }, // no avatar → monogram
        ],
      },
    })

    const { container } = render(<Usage />)

    expect(await screen.findByText('Alice Smith')).toBeInTheDocument()
    expect(container.querySelector('img[src="https://ex/alice.png"]')).not.toBeNull()
    expect(screen.getByText('BO')).toBeInTheDocument() // Bob's monogram fallback
  })
})

describe('Usage access & credential change-log (EE governance)', () => {
  it('org admin with governance licensed sees the audit band + rendered rows', async () => {
    roleMock.isAdmin = true
    teamsMock.teams = []
    entMock.governance = true
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/org': ORG,
      '/api/usage/org/access-log': ACCESS_LOG,
    })

    render(<Usage />)

    // The band header + both server-rendered action labels render.
    expect(await screen.findByText('Access & credential changes')).toBeInTheDocument()
    expect(await screen.findByText('changed Bob from member to admin')).toBeInTheDocument()
    expect(await screen.findByText('set the GitHub PAT for github.example.com')).toBeInTheDocument()
    // The endpoint was hit.
    expect(fetchedPaths(fetchMock)).toContain('/api/usage/org/access-log')
  })

  it('hides the audit band when the governance entitlement is absent', async () => {
    roleMock.isAdmin = true
    teamsMock.teams = []
    entMock.governance = false // unlicensed
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/org': ORG,
      '/api/usage/org/access-log': ACCESS_LOG,
    })

    render(<Usage />)

    // Org section still renders (proves the page mounted), but the audit band
    // does not — and its endpoint is never fetched.
    expect(await screen.findByText('Org')).toBeInTheDocument()
    expect(screen.queryByText('Access & credential changes')).not.toBeInTheDocument()
    expect(fetchedPaths(fetchMock)).not.toContain('/api/usage/org/access-log')
  })

  it('hides the audit band from a non-org-admin even when licensed', async () => {
    roleMock.isAdmin = false // plain member
    teamsMock.teams = []
    entMock.governance = true
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/org/access-log': ACCESS_LOG,
    })

    render(<Usage />)

    expect(await screen.findByText('Personal')).toBeInTheDocument()
    expect(screen.queryByText('Access & credential changes')).not.toBeInTheDocument()
    expect(fetchedPaths(fetchMock)).not.toContain('/api/usage/org/access-log')
  })

  it('shows the audit band in local mode (N=1 owner) when governance is licensed', async () => {
    // showOrg = isAdmin || isLocal, so the local implicit owner sees the band
    // too (the backend short-circuits the admin gate in local mode). The flat
    // LocalConsole renders above it, not the three multi-mode sections.
    authMock.local = true
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Default', slug: 'default', role: 'admin' }]
    entMock.governance = true
    const fetchMock = stubUsageFetch({
      '/api/usage/org': ORG,
      '/api/usage/org/access-log': ACCESS_LOG,
    })

    render(<Usage />)

    expect(await screen.findByText('Access & credential changes')).toBeInTheDocument()
    expect(await screen.findByText('changed Bob from member to admin')).toBeInTheDocument()
    // Local layout: no multi-mode section headers.
    expect(screen.queryByText('Personal')).not.toBeInTheDocument()
    expect(fetchedPaths(fetchMock)).toContain('/api/usage/org/access-log')
  })

  it('renders a zero state for an empty log', async () => {
    roleMock.isAdmin = true
    teamsMock.teams = []
    entMock.governance = true
    stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/org': ORG,
      '/api/usage/org/access-log': ACCESS_LOG_EMPTY,
    })

    render(<Usage />)

    expect(await screen.findByText('Access & credential changes')).toBeInTheDocument()
    expect(await screen.findByText(/no access or credential changes yet/i)).toBeInTheDocument()
  })

  it('filtering by category refetches the log with the category param', async () => {
    roleMock.isAdmin = true
    teamsMock.teams = []
    entMock.governance = true
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/org': ORG,
      '/api/usage/org/access-log': ACCESS_LOG,
    })

    render(<Usage />)

    // Wait for the band, then pick the Credential filter.
    expect(await screen.findByText('Access & credential changes')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Credential' }))

    await waitFor(() =>
      expect(fullUrls(fetchMock).some((u) => u.includes('category=credential'))).toBe(true),
    )
  })
})
