import { fireEvent, render, screen, within } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { MeResponse } from './types'

// This file is about ONE thing: where the rail's grants come from. The rail's
// behaviour across grants is reviewed in ui/shell/Shell.test.tsx; what is
// reviewed here is the adapter's answer to "which grants does this viewer
// hold", and specifically that the team-tier answer is ready on first paint.

const meMock = vi.hoisted(() => ({ me: null as MeResponse | null }))
vi.mock('./contexts/AuthContext', () => ({
  useOptionalAuth: () => ({
    status: 'authed',
    me: meMock.me,
    orgs: meMock.me?.orgs ?? [],
    serverActiveOrgId: meMock.me?.active_org_id ?? null,
    error: null,
    refresh: vi.fn(),
    logout: vi.fn(),
  }),
}))

// The org tier is a separate grant and not what is under test; keep it off so a
// Team row can only have come from the team tier.
vi.mock('./hooks/useOrgRole', () => ({
  useOrgRole: () => ({ role: 'member', isAdmin: false, loading: false }),
}))

// The teams-list read defaults to UNRESOLVED — the state the rail is in on
// first paint, which is what the grants tests below are about. The scope tests
// fill `teamsMock` in to drive the org · team mark instead.
const teamsMock = vi.hoisted(() => ({
  teams: [] as Array<{ id: string; name: string; slug: string; role: string }>,
  activeTeamId: '',
  setTeamId: vi.fn(),
}))
vi.mock('./hooks/useTeams', () => ({
  useTeams: () => ({
    teams: teamsMock.teams,
    lastActingTeamId: '',
    multi: teamsMock.teams.length > 0,
    loading: teamsMock.teams.length === 0,
    loaded: teamsMock.teams.length > 0,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
  // The shell's scope selection (the org · team mark).
  useActiveTeam: () => ({
    teamId: teamsMock.activeTeamId,
    setTeamId: teamsMock.setTeamId,
    multi: teamsMock.teams.length > 0,
    ready: teamsMock.teams.length > 0,
    teams: teamsMock.teams,
  }),
}))

vi.mock('./hooks/useEntitlements', () => ({
  FeatureFleet: 'fleet',
  useEntitlements: () => ({ has: () => false, loaded: true }),
}))

// The real hook resolves the active org through OrgContext + the deployment
// config, neither of which this render has a reason to mount.
vi.mock('./hooks/useOrgHref', () => ({ useOrgHref: () => (p: string) => p }))

import Shell from './Shell'
import { useShellScope } from './hooks/useShellScope'

beforeEach(() => {
  meMock.me = null
  teamsMock.teams = []
  teamsMock.activeTeamId = ''
  teamsMock.setTeamId = vi.fn()
})

function me(teams: MeResponse['teams']): MeResponse {
  return {
    id: 'u1',
    email: 'someone@example.com',
    orgs: [{ id: 'o1', name: 'Acme', role: 'member' }],
    teams,
    active_org_id: 'o1',
    org_creation_enabled: true,
    is_operator: false,
  }
}

function rail() {
  return document.querySelector('.sh-rail') as HTMLElement
}

function renderShell() {
  render(
    <MemoryRouter initialEntries={['/board']}>
      <Shell />
    </MemoryRouter>,
  )
}

describe('Shell — grants come from the read that already blocks first paint', () => {
  it('holds team-admin on the first render, from /api/me alone', () => {
    meMock.me = me([{ id: 't1', name: 'Platform', org_id: 'o1', role: 'admin' }])
    renderShell()

    // No re-render, no waitFor: the assertion is that the rail's first paint
    // already has the grant, with the teams-list read still in flight.
    expect(within(rail()).getByText('ADMINISTRATION')).toBeInTheDocument()
    expect(within(rail()).getByText('Team')).toBeInTheDocument()
    // team-admin does not imply the org tier.
    expect(within(rail()).queryByText('Organization')).not.toBeInTheDocument()
  })

  it('reads the role, not the membership — a member of every team holds nothing', () => {
    meMock.me = me([
      { id: 't1', name: 'Platform', org_id: 'o1', role: 'member' },
      { id: 't2', name: 'Reviewers', org_id: 'o2', role: 'viewer' },
    ])
    renderShell()

    expect(within(rail()).getByText('CAPACITY')).toBeInTheDocument()
    expect(within(rail()).queryByText('Team')).not.toBeInTheDocument()
  })

  it('takes an admin role in any of the viewer orgs', () => {
    meMock.me = me([
      { id: 't1', name: 'Platform', org_id: 'o1', role: 'member' },
      { id: 't2', name: 'Reviewers', org_id: 'o2', role: 'admin' },
    ])
    renderShell()

    expect(within(rail()).getByText('Team')).toBeInTheDocument()
  })

  it('renders the rail at all with no teams', () => {
    meMock.me = me([])
    renderShell()

    expect(within(rail()).getByText('Board')).toBeInTheDocument()
    expect(within(rail()).queryByText('Team')).not.toBeInTheDocument()
  })
})

// The outlet-context half of the scope control: what a page receives is the
// RESOLVED team, and both directions of the wiring trade in ids — display
// names are faces, and nothing makes them unique.
function ScopeProbe() {
  const scope = useShellScope()
  return <div data-testid="scope-probe">{scope.teamId + ':' + scope.teamName}</div>
}

describe('Shell — the scope mark drives the outlet context by id', () => {
  it('publishes the picked team to pages and reports a popup pick by id', () => {
    meMock.me = me([])
    teamsMock.teams = [
      { id: 't1', name: 'Platform', slug: 'platform', role: 'member' },
      { id: 't2', name: 'Platform', slug: 'platform-2', role: 'member' },
    ]
    teamsMock.activeTeamId = 't2'

    render(
      <MemoryRouter initialEntries={['/board']}>
        <Routes>
          <Route element={<Shell />}>
            <Route path="/board" element={<ScopeProbe />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    )

    // Two teams share the face "Platform"; the context carries t2 because the
    // selection is its id, not its name.
    expect(screen.getByTestId('scope-probe')).toHaveTextContent('t2:Platform')

    // A pick in the popup reports the clicked row's id straight through.
    fireEvent.click(screen.getByText('Acme'))
    const rows = screen.getAllByRole('button', { name: 'Platform' })
    expect(rows).toHaveLength(2)
    fireEvent.click(rows[0])
    expect(teamsMock.setTeamId).toHaveBeenCalledWith('t1')
  })
})
