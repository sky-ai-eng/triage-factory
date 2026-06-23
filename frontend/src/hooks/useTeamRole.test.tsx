import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import type { TeamSummary } from '../types'
import { useTeamRole } from './useTeamRole'

// Mutable mock of the shared /api/teams cache — each test seeds the viewer's
// teams (with their per-team role) then renders the probe.
const mockState = vi.hoisted(() => ({
  teams: [] as TeamSummary[],
  loaded: true,
}))

vi.mock('./useTeams', () => ({
  useTeams: () => ({
    teams: mockState.teams,
    lastActingTeamId: '',
    multi: mockState.teams.length >= 2,
    loading: false,
    loaded: mockState.loaded,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
}))

// Probe renders the hook's verdicts for a target team so we can assert via DOM.
function Probe({ teamId }: { teamId?: string }) {
  const { roleForTeam, canWriteTeam, isViewer } = useTeamRole()
  return (
    <div>
      <span data-testid="role">{String(roleForTeam(teamId))}</span>
      <span data-testid="canWrite">{String(canWriteTeam(teamId))}</span>
      <span data-testid="isViewer">{String(isViewer(teamId))}</span>
    </div>
  )
}

function team(id: string, role: string): TeamSummary {
  return { id, name: id, slug: id, role }
}

beforeEach(() => {
  mockState.teams = []
  mockState.loaded = true
})

describe('useTeamRole', () => {
  it('admin can write and is not a viewer', () => {
    mockState.teams = [team('t1', 'admin')]
    render(<Probe teamId="t1" />)
    expect(screen.getByTestId('role').textContent).toBe('admin')
    expect(screen.getByTestId('canWrite').textContent).toBe('true')
    expect(screen.getByTestId('isViewer').textContent).toBe('false')
  })

  it('member can write and is not a viewer', () => {
    mockState.teams = [team('t1', 'member')]
    render(<Probe teamId="t1" />)
    expect(screen.getByTestId('canWrite').textContent).toBe('true')
    expect(screen.getByTestId('isViewer').textContent).toBe('false')
  })

  it('viewer cannot write and is flagged as a viewer', () => {
    mockState.teams = [team('t1', 'viewer')]
    render(<Probe teamId="t1" />)
    expect(screen.getByTestId('role').textContent).toBe('viewer')
    expect(screen.getByTestId('canWrite').textContent).toBe('false')
    expect(screen.getByTestId('isViewer').textContent).toBe('true')
  })

  it('resolves role per team for a multi-team viewer', () => {
    mockState.teams = [team('t1', 'viewer'), team('t2', 'member')]
    const { rerender } = render(<Probe teamId="t1" />)
    expect(screen.getByTestId('canWrite').textContent).toBe('false')
    rerender(<Probe teamId="t2" />)
    expect(screen.getByTestId('canWrite').textContent).toBe('true')
  })

  it('unknown team / undefined defaults permissive (no false gating)', () => {
    mockState.teams = [team('t1', 'viewer')]
    // A team the viewer isn't in, or an org-level surface with no team.
    render(<Probe teamId="other" />)
    expect(screen.getByTestId('role').textContent).toBe('null')
    expect(screen.getByTestId('canWrite').textContent).toBe('true')
    expect(screen.getByTestId('isViewer').textContent).toBe('false')

    render(<Probe teamId={undefined} />)
    // Two probes now mounted; the latest (undefined) is appended — assert both
    // canWrite cells read writable.
    for (const cell of screen.getAllByTestId('canWrite')) {
      expect(cell.textContent).toBe('true')
    }
  })
})
