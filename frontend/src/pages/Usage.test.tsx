import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import type {
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

// A category with non-zero cache tokens, to assert the donut tooltip's total
// (in + out + cache) agrees with its parenthetical breakdown.
const ME_TOKENS: UsageMeResponse = {
  total_cost_usd: 8,
  by_category: [
    {
      category: 'manual',
      cost: 8,
      input_tokens: 100,
      output_tokens: 50,
      cache_read_tokens: 30,
      cache_creation_tokens: 20,
    },
  ],
  by_model: [{ model: 'claude-opus-4-8', cost: 8 }],
  by_day: [{ date: '2026-06-01', cost: 8 }],
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

beforeEach(() => {
  roleMock.isAdmin = false
  teamsMock.teams = []
  authMock.local = false
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

  it('local mode (N=1) shows the org rollup so system-overhead spend is visible', async () => {
    // Local mode: no AuthProvider (useOptionalAuth null), useOrgRole reports
    // non-admin, and the sole team reports admin. The local user is effectively
    // the org admin, so the org rollup — the only section that surfaces
    // system-overhead spend (NULL creator + NULL team) — must render.
    authMock.local = true
    roleMock.isAdmin = false
    teamsMock.teams = [{ id: 't1', name: 'Default', slug: 'default', role: 'admin' }]
    const fetchMock = stubUsageFetch({
      '/api/usage/me': ME,
      '/api/usage/teams/t1': TEAM,
      '/api/usage/org': ORG,
    })

    render(<Usage />)

    // All three sections render, and the org rollup is actually fetched.
    expect(await screen.findByText('Personal')).toBeInTheDocument()
    expect(await screen.findByText('Team')).toBeInTheDocument()
    expect(await screen.findByText('Org')).toBeInTheDocument()
    await waitFor(() => expect(fetchedPaths(fetchMock)).toContain('/api/usage/org'))

    // System-overhead (NULL creator + NULL team) surfaces only in the org rollup
    // — here as the allocation meter's overhead ("ovh") segment. This is exactly
    // the spend that's invisible without the org section.
    expect((await screen.findAllByText('System')).length).toBeGreaterThan(0)
    expect((await screen.findAllByText('ovh')).length).toBeGreaterThan(0)
    expect(await screen.findByText('$100.00')).toBeInTheDocument()
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
    roleMock.isAdmin = false
    teamsMock.teams = []
    stubUsageFetch({ '/api/usage/me': ME_TOKENS })

    render(<Usage />)

    // 100 in + 50 out + (30 read + 20 creation = 50 cached) = 200 — and the
    // breakdown parts sum to that stated total.
    expect(await screen.findByTitle('200 tokens (100 in · 50 out · 50 cached)')).toBeInTheDocument()
  })
})
