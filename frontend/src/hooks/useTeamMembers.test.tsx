import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import type { TeamSummary } from '../types'
import { useTeamMembers } from './useDeploymentConfig'
import { jsonBody } from '../test/apiResponse'

// The roster is addressed by team, and WHICH team is the point: the roster a
// consumer reads has to be the team its page is acting as, or a user who
// belongs to two of them sees one team's people inside a page scoped to the
// other. These tests pin the resolution — the acting team the shared teams
// cache reports, not the first team and not an implied one — and the states
// consumers branch on while it resolves.

const mockState = vi.hoisted(() => ({
  teams: [] as TeamSummary[],
  lastActingTeamId: '',
  loaded: true,
}))

vi.mock('./useTeams', async (importOriginal) => {
  // pickerDefault is real: it is the rule under test (last-acting, else the
  // first team, validated against the live set), and a stubbed copy would pin
  // the stub instead.
  const actual = await importOriginal<typeof import('./useTeams')>()
  return {
    ...actual,
    useTeams: () => ({
      teams: mockState.teams,
      lastActingTeamId: mockState.lastActingTeamId,
      multi: mockState.teams.length >= 2,
      loading: false,
      loaded: mockState.loaded,
      error: null,
      refresh: vi.fn(),
      createTeam: vi.fn(),
    }),
  }
})

function team(id: string): TeamSummary {
  return { id, name: id, slug: id, role: 'member' }
}

/** stubRoster answers any fetch with one roster payload and records the paths
 *  it was asked for. */
function stubRoster(body: unknown) {
  const paths: string[] = []
  const fetchMock = vi.fn((input: unknown) => {
    paths.push(String(input))
    return Promise.resolve({ ok: true, ...jsonBody(body) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return paths
}

const member = (id: string) => ({
  user_id: id,
  display_name: id,
  github_username: null,
  jira_account_id: null,
  role: 'member',
  is_current_user: false,
})

beforeEach(() => {
  mockState.teams = []
  mockState.lastActingTeamId = ''
  mockState.loaded = true
})

describe('useTeamMembers', () => {
  it('reads the acting team, not the first one', async () => {
    mockState.teams = [team('team-a'), team('team-b')]
    mockState.lastActingTeamId = 'team-b'
    const paths = stubRoster({ items: [member('u1')], total_count: 1, bot: null })

    const { result } = renderHook(() => useTeamMembers())

    await waitFor(() => expect(result.current.members).toHaveLength(1))
    expect(paths).toEqual(['/api/teams/team-b/members/list'])
  })

  it('falls back to the first team when no acting team is recorded', async () => {
    mockState.teams = [team('team-a'), team('team-b')]
    const paths = stubRoster({ items: [], total_count: 0, bot: null })

    renderHook(() => useTeamMembers())

    await waitFor(() => expect(paths).toEqual(['/api/teams/team-a/members/list']))
  })

  it('carries the bot beside the members', async () => {
    mockState.teams = [team('team-a')]
    stubRoster({
      items: [member('u1')],
      total_count: 1,
      bot: { agent_id: 'a1', display_name: 'Bot', enabled: true, model: 'm', autonomy: null },
    })

    const { result } = renderHook(() => useTeamMembers())

    await waitFor(() => expect(result.current.bot?.agent_id).toBe('a1'))
    expect(result.current.members.map((m) => m.user_id)).toEqual(['u1'])
  })

  it('stays loading until the teams cache resolves the acting team', async () => {
    // An empty roster while the team is still unknown would read as "solo
    // team" to the predicate editor, which picks its variant on roster size.
    mockState.loaded = false
    const paths = stubRoster({ items: [], total_count: 0, bot: null })

    const { result } = renderHook(() => useTeamMembers())

    expect(result.current.loading).toBe(true)
    expect(paths).toEqual([])
  })

  it('reports an error and stops loading when the read fails', async () => {
    mockState.teams = [team('team-a')]
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: false, status: 500, ...jsonBody({ error: 'boom' }) })),
    )

    const { result } = renderHook(() => useTeamMembers())

    await waitFor(() => expect(result.current.error).not.toBeNull())
    expect(result.current.loading).toBe(false)
    expect(result.current.members).toEqual([])
  })
})
