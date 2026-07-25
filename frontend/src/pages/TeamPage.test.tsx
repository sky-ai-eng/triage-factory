import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { TeamSummary } from '../types'

// Mutable mock state — each test seeds teams + the viewer's org role, then
// renders. The child tab bodies are stubbed (their own slices test them); these
// tests cover the shell: tab render/deep-link, the shared switcher's sticky
// default, and the zero-team safe landing.
const mockState = vi.hoisted(() => ({
  teams: [] as TeamSummary[],
  lastActingTeamId: '',
  orgAdmin: false,
  slackEnt: false,
}))

vi.mock('../contexts/OrgContext', () => ({ useActiveOrgId: () => 'org-1' }))
// Gates the Slack tab (TFAC-544) — default off so existing tests (written
// before the tab existed) see the same [Members · Settings · Prompts] shell.
vi.mock('../hooks/useEntitlements', () => ({
  FeatureSlack: 'slack',
  useEntitlements: () => ({
    has: (f: string) => f === 'slack' && mockState.slackEnt,
    loaded: true,
  }),
}))
vi.mock('../hooks/useOrgRole', () => ({
  useOrgRole: () => ({
    role: mockState.orgAdmin ? 'admin' : 'member',
    isAdmin: mockState.orgAdmin,
    loading: false,
  }),
}))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: mockState.teams,
    lastActingTeamId: mockState.lastActingTeamId,
    multi: mockState.teams.length >= 2,
    loading: false,
    loaded: true,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
}))
// useOrgHref passthrough so ZeroTeamState's "Create a team" link has a target.
vi.mock('../hooks/useOrgHref', () => ({ useOrgHref: () => (p: string) => p }))

// Stub the tab bodies — expose the team they were handed + their canManage gate.
vi.mock('../components/TeamMembersPanel', () => ({
  default: ({ teamId, canManage }: { teamId: string; canManage: boolean }) => (
    <div data-testid="members" data-team={teamId} data-manage={String(canManage)}>
      members
    </div>
  ),
}))
vi.mock('./settings/stack/TeamSettings', () => ({
  default: ({ teamId }: { teamId: string }) => (
    <div data-testid="settings" data-team={teamId}>
      settings
    </div>
  ),
}))
vi.mock('../components/PromptsWorkspace', () => ({
  default: ({ teamId }: { teamId: string }) => (
    <div data-testid="prompts" data-team={teamId}>
      prompts
    </div>
  ),
}))
vi.mock('../components/TeamSlackChannelsPanel', () => ({
  default: ({ teamId, orgIsAdmin }: { teamId: string; orgIsAdmin: boolean }) => (
    <div data-testid="slack" data-team={teamId} data-org-admin={String(orgIsAdmin)}>
      slack
    </div>
  ),
}))
// Stub the switcher as a plain <select> so the persistence test drives it
// through a stable interface instead of coupling to TeamSwitch's dropdown DOM.
vi.mock('../components/TeamSwitch', () => ({
  default: ({
    value,
    onChange,
    teams,
  }: {
    value: string
    onChange: (id: string) => void
    teams?: TeamSummary[]
  }) =>
    (teams?.length ?? 0) >= 2 ? (
      <select data-testid="team-switch" value={value} onChange={(e) => onChange(e.target.value)}>
        {teams!.map((t) => (
          <option key={t.id} value={t.id}>
            {t.name}
          </option>
        ))}
      </select>
    ) : null,
}))

import TeamPage from './TeamPage'

function team(id: string, name: string, role = 'member'): TeamSummary {
  return { id, name, slug: name.toLowerCase().replace(/\s+/g, '-'), role }
}

function renderAt(path = '/team') {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <TeamPage />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  mockState.teams = [team('t-a', 'Team A'), team('t-b', 'Team B')]
  mockState.lastActingTeamId = ''
  mockState.orgAdmin = false
  mockState.slackEnt = false
  localStorage.clear()
})

describe('TeamPage — tab shell', () => {
  it('defaults to the Members tab and shows the tab strip', () => {
    renderAt()
    expect(screen.getByTestId('members')).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Members' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Prompts' })).toBeInTheDocument()
  })

  it('deep-links to the Prompts tab via ?tab=prompts', () => {
    renderAt('/team?tab=prompts')
    expect(screen.getByTestId('prompts')).toBeInTheDocument()
    expect(screen.queryByTestId('members')).not.toBeInTheDocument()
  })

  it('switches tabs on click', () => {
    renderAt()
    fireEvent.click(screen.getByRole('tab', { name: 'Prompts' }))
    expect(screen.getByTestId('prompts')).toBeInTheDocument()
  })

  it('moves between tabs with Left/Right arrows', () => {
    mockState.teams = [team('t-a', 'Team A', 'admin')]
    renderAt()
    // Members → Settings → Prompts via ArrowRight.
    fireEvent.keyDown(screen.getByRole('tablist'), { key: 'ArrowRight' })
    expect(screen.getByRole('tab', { name: 'Settings' })).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(screen.getByRole('tablist'), { key: 'ArrowRight' })
    expect(screen.getByTestId('prompts')).toBeInTheDocument()
    // ArrowRight wraps back to Members.
    fireEvent.keyDown(screen.getByRole('tablist'), { key: 'ArrowRight' })
    expect(screen.getByTestId('members')).toBeInTheDocument()
  })

  it('hides the Settings tab for non-managers and floors ?tab=settings to Members', () => {
    renderAt('/team?tab=settings')
    // member of the team, not an org admin → no Settings tab, no settings body
    expect(screen.queryByRole('tab', { name: 'Settings' })).not.toBeInTheDocument()
    expect(screen.queryByTestId('settings')).not.toBeInTheDocument()
    expect(screen.getByTestId('members')).toBeInTheDocument()
  })

  it('shows the Settings tab for a team admin', () => {
    mockState.teams = [team('t-a', 'Team A', 'admin'), team('t-b', 'Team B')]
    renderAt('/team?tab=settings')
    expect(screen.getByRole('tab', { name: 'Settings' })).toBeInTheDocument()
    expect(screen.getByTestId('settings')).toHaveAttribute('data-team', 't-a')
  })
})

describe('TeamPage — Slack tab (TFAC-544)', () => {
  it('is absent, and ?tab=slack floors to Members, when the org lacks the entitlement', () => {
    mockState.slackEnt = false
    renderAt('/team?tab=slack')
    expect(screen.queryByRole('tab', { name: 'Slack' })).not.toBeInTheDocument()
    expect(screen.queryByTestId('slack')).not.toBeInTheDocument()
    expect(screen.getByTestId('members')).toBeInTheDocument()
  })

  it('is visible to a plain member (not gated on canManage like Settings) when entitled', () => {
    mockState.slackEnt = true
    renderAt('/team?tab=slack')
    expect(screen.getByRole('tab', { name: 'Slack' })).toBeInTheDocument()
    expect(screen.getByTestId('slack')).toHaveAttribute('data-team', 't-a')
  })

  it('passes orgIsAdmin through to the panel', () => {
    mockState.slackEnt = true
    mockState.orgAdmin = true
    renderAt('/team?tab=slack')
    expect(screen.getByTestId('slack')).toHaveAttribute('data-org-admin', 'true')
  })
})

describe('TeamPage — shared switcher', () => {
  it('seeds the active team from the sticky server default (last-acting team)', () => {
    mockState.lastActingTeamId = 't-b'
    renderAt()
    expect(screen.getByTestId('members')).toHaveAttribute('data-team', 't-b')
  })

  it('persists the switcher selection and re-seeds from it on remount', () => {
    const { unmount } = renderAt()
    // Default (no sticky) is the first team.
    expect(screen.getByTestId('members')).toHaveAttribute('data-team', 't-a')

    // Pick Team B via the switcher (mocked as a <select> with a stable testid).
    fireEvent.change(screen.getByTestId('team-switch'), { target: { value: 't-b' } })
    expect(screen.getByTestId('members')).toHaveAttribute('data-team', 't-b')
    expect(localStorage.getItem('tf.activeTeam.team')).toBe('t-b')

    // Remount — the sticky pick wins over the oldest-first default.
    unmount()
    renderAt()
    expect(screen.getByTestId('members')).toHaveAttribute('data-team', 't-b')
  })

  it('a ?team= override (e.g. the marketplace install success link) wins over the sticky pick and persists it', () => {
    localStorage.setItem('tf.activeTeam.team', 't-a')
    renderAt('/team?tab=prompts&team=t-b')
    expect(screen.getByTestId('prompts')).toHaveAttribute('data-team', 't-b')
    expect(localStorage.getItem('tf.activeTeam.team')).toBe('t-b')
  })

  it('ignores a ?team= override that is not one of the caller’s teams', () => {
    localStorage.setItem('tf.activeTeam.team', 't-a')
    renderAt('/team?team=not-a-real-team')
    expect(screen.getByTestId('members')).toHaveAttribute('data-team', 't-a')
  })
})

describe('TeamPage — zero-team safe landing', () => {
  it('renders the friendly empty state (no crash) when the user is on no team', () => {
    mockState.teams = []
    renderAt()
    expect(screen.getByText(/not on a team yet/i)).toBeInTheDocument()
    // No tabs, no tab bodies.
    expect(screen.queryByTestId('members')).not.toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: 'Members' })).not.toBeInTheDocument()
  })

  it('shows a "Create a team" CTA for org admins', () => {
    mockState.teams = []
    mockState.orgAdmin = true
    renderAt()
    const cta = screen.getByRole('link', { name: /create a team/i })
    expect(cta).toBeInTheDocument()
    expect(cta).toHaveAttribute('href', expect.stringContaining('/org'))
  })

  it('tells a non-admin to ask an org admin (no create CTA)', () => {
    mockState.teams = []
    mockState.orgAdmin = false
    renderAt()
    expect(screen.getByText(/ask an org admin/i)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /create a team/i })).not.toBeInTheDocument()
  })
})
