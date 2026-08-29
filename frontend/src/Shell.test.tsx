import { render, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { describe, expect, it, vi } from 'vitest'
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

// The teams-list read, deliberately UNRESOLVED — the state the rail is in on
// first paint. Before this ticket the team-admin grant was derived from this
// cache, so it could only arrive on a second render.
vi.mock('./hooks/useTeams', () => ({
  useTeams: () => ({
    teams: [],
    lastActingTeamId: '',
    multi: false,
    loading: true,
    loaded: false,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
  // The shell's scope selection (the org · team mark). Unresolved here — these
  // tests are about grants, and an empty teams list resolves no scope anyway.
  useActiveTeam: () => ({
    teamId: '',
    setTeamId: vi.fn(),
    multi: false,
    ready: false,
    teams: [],
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
