import { describe, it, expect, vi, beforeEach } from 'vitest'
import { preselectClaimTeam, resolveClaimTeam, type ClaimTeamContext } from './claimTeam'
import { jsonBody } from '../test/apiResponse'
import type { TeamSummary } from '../types'

// How a claim or delegate gesture learns the team the task lands on: from the
// candidates the task's detail read lists, and without any read at all for a
// person on one team.

const fetchMock = vi.fn()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})

const team = (id: string): TeamSummary => ({ id, name: id, slug: id })

function ctx(over: Partial<ClaimTeamContext> = {}): ClaimTeamContext {
  return {
    multi: true,
    teams: [team('t-alpha'), team('t-beta'), team('t-gamma')],
    lastActingTeamId: '',
    teamFilter: [],
    ...over,
  }
}

/** detail answers GET /api/tasks/{id} with the given candidates; `undefined`
 *  leaves the key off, as the server does when its read failed. */
function detail(candidates?: string[]) {
  fetchMock.mockResolvedValue({
    ok: true,
    status: 200,
    ...jsonBody({ id: 'task-1', version: 3, claim_team_ids: candidates }),
  })
}

describe('resolveClaimTeam', () => {
  it('sends no team, and reads nothing, for a person on one team', async () => {
    expect(await resolveClaimTeam('task-1', ctx({ multi: false }))).toEqual({ kind: 'send' })
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('goes on with the one candidate', async () => {
    detail(['t-beta'])
    expect(await resolveClaimTeam('task-1', ctx())).toEqual({ kind: 'send', teamId: 't-beta' })
    expect(fetchMock).toHaveBeenCalledWith('/api/tasks/task-1', expect.anything())
  })

  it('asks when more than one team can take the task', async () => {
    detail(['t-alpha', 't-beta'])
    expect(await resolveClaimTeam('task-1', ctx())).toEqual({
      kind: 'ask',
      candidates: ['t-alpha', 't-beta'],
      preselected: 't-alpha',
    })
  })

  it('writes nothing when no team can take the task', async () => {
    detail([])
    expect(await resolveClaimTeam('task-1', ctx())).toEqual({ kind: 'none' })
  })

  it('sends no team when the detail read fails, so the server answers the write', async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
    expect(await resolveClaimTeam('task-1', ctx())).toEqual({ kind: 'send' })
  })

  it('sends no team when the detail read answers without the candidates', async () => {
    detail(undefined)
    expect(await resolveClaimTeam('task-1', ctx())).toEqual({ kind: 'send' })
  })
})

describe('preselectClaimTeam', () => {
  const candidates = ['t-alpha', 't-beta']

  it('takes the page’s team filter when it names one team that is a candidate', () => {
    expect(preselectClaimTeam(candidates, ctx({ teamFilter: ['t-beta'] }))).toBe('t-beta')
  })

  it('ignores a filter that names a team that is not a candidate', () => {
    expect(
      preselectClaimTeam(candidates, ctx({ teamFilter: ['t-gamma'], lastActingTeamId: 't-beta' })),
    ).toBe('t-beta')
  })

  it('ignores a filter that names more than one team', () => {
    expect(preselectClaimTeam(candidates, ctx({ teamFilter: ['t-alpha', 't-beta'] }))).toBe(
      't-alpha',
    )
  })

  it('falls back to the last-used team only when it is a candidate', () => {
    expect(preselectClaimTeam(candidates, ctx({ lastActingTeamId: 't-gamma' }))).toBe('t-alpha')
  })
})
