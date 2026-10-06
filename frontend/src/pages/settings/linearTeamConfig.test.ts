// The Linear half of the team config helpers: the pre-fill a watched team's
// rules start from, the watched-or-mapped rule the server stores, and the
// write path that turns the form's state refs into ids.
import { describe, it, expect, vi } from 'vitest'

import {
  dropLinearState,
  linearTeamFromWire,
  linearTeamIsArmed,
  linearTeamRulesValid,
  linearDraftAfterSave,
  linearTeamsBlocked,
  linearTeamsEqual,
  prefillLinearRules,
  saveTeamLinearTeams,
  unmappedLinearTeam,
  unresolvableLinearStates,
  type LinearTeamConfig,
} from './teamConfig'
import type { LinearStateOption } from '../../lib/linearTeams'
import { jsonBody } from '../../test/apiResponse'

const state = (id: string, name: string, type: string, position: number): LinearStateOption => ({
  id,
  name,
  type,
  position,
})

// A workflow listed out of board order, with two started and two completed
// states, so a canonical picked by position shows it.
const WORKFLOW = [
  state('s-review', 'In Review', 'started', 4),
  state('s-done', 'Done', 'completed', 6),
  state('s-triage', 'Triage', 'triage', 0),
  state('s-todo', 'Todo', 'unstarted', 2),
  state('s-doing', 'In Progress', 'started', 3),
  state('s-shipped', 'Shipped', 'completed', 5),
  state('s-backlog', 'Backlog', 'backlog', 1),
  state('s-canceled', 'Canceled', 'canceled', 7),
]

const ids = (refs: { id: string }[]) => refs.map((r) => r.id)

describe('prefillLinearRules', () => {
  it('maps each rule from the state types, in board order', () => {
    const rules = prefillLinearRules(WORKFLOW)!
    expect(ids(rules.pickup.members)).toEqual(['s-triage', 's-backlog', 's-todo'])
    expect(ids(rules.in_progress.members)).toEqual(['s-doing', 's-review'])
    expect(ids(rules.done.members)).toEqual(['s-shipped', 's-done', 's-canceled'])
  })

  it('takes the lowest-positioned started and completed states as the write targets', () => {
    const rules = prefillLinearRules(WORKFLOW)!
    expect(rules.in_progress.canonical?.id).toBe('s-doing')
    // Shipped comes before Done on the board, and a canceled state is never
    // the done target even though it counts as done.
    expect(rules.done.canonical?.id).toBe('s-shipped')
  })

  it('carries each state’s type, without its position', () => {
    const rules = prefillLinearRules(WORKFLOW)!
    expect(rules.in_progress.canonical).toEqual({
      id: 's-doing',
      name: 'In Progress',
      type: 'started',
    })
  })

  it('declines a workflow it cannot map whole', () => {
    const noStarted = WORKFLOW.filter((s) => s.type !== 'started')
    const noCompleted = WORKFLOW.filter((s) => s.type !== 'completed')
    const nothingToPickUp = WORKFLOW.filter(
      (s) => !['triage', 'backlog', 'unstarted'].includes(s.type),
    )
    expect(prefillLinearRules(noStarted)).toBeNull()
    expect(prefillLinearRules(noCompleted)).toBeNull()
    expect(prefillLinearRules(nothingToPickUp)).toBeNull()
  })
})

const mapped = (): LinearTeamConfig => ({
  ...unmappedLinearTeam('team-eng', 'ENG', 'Engineering'),
  ...prefillLinearRules(WORKFLOW)!,
})

describe('the watched-or-mapped rule', () => {
  it('lets a team be watched with nothing mapped, or mapped whole', () => {
    expect(linearTeamRulesValid(unmappedLinearTeam('team-eng', 'ENG', 'Engineering'))).toBe(true)
    expect(linearTeamIsArmed(unmappedLinearTeam('team-eng', 'ENG', 'Engineering'))).toBe(false)
    expect(linearTeamRulesValid(mapped())).toBe(true)
    expect(linearTeamIsArmed(mapped())).toBe(true)
  })

  it('blocks half a mapping', () => {
    const noDone: LinearTeamConfig = { ...mapped(), done: { members: [], canonical: null } }
    const noTarget: LinearTeamConfig = {
      ...mapped(),
      in_progress: { ...mapped().in_progress, canonical: null },
    }
    expect(linearTeamRulesValid(noDone)).toBe(false)
    expect(linearTeamRulesValid(noTarget)).toBe(false)
    expect(linearTeamsBlocked([mapped(), noDone])).toBe(true)
    expect(linearTeamsBlocked([mapped(), unmappedLinearTeam('t2', 'OPS', 'Operations')])).toBe(
      false,
    )
  })

  it('blocks a write target outside its rule', () => {
    const outside: LinearTeamConfig = {
      ...mapped(),
      done: {
        members: [{ id: 's-done', name: 'Done', type: 'completed' }],
        canonical: { id: 's-shipped', name: 'Shipped', type: 'completed' },
      },
    }
    expect(linearTeamRulesValid(outside)).toBe(false)
  })
})

describe('states gone from the workflow', () => {
  it('names each one a rule still holds, once', () => {
    const live = WORKFLOW.filter((s) => s.id !== 's-doing')
    expect(ids(unresolvableLinearStates(mapped(), live))).toEqual(['s-doing'])
  })

  it('says nothing before the workflow is read', () => {
    expect(unresolvableLinearStates(mapped(), [])).toEqual([])
  })

  it('drops one from every rule, clearing a write target that named it', () => {
    const dropped = dropLinearState(mapped(), {
      id: 's-doing',
      name: 'In Progress',
      type: 'started',
    })
    expect(ids(dropped.in_progress.members)).toEqual(['s-review'])
    expect(dropped.in_progress.canonical).toBeNull()
    expect(linearTeamRulesValid(dropped)).toBe(false)
  })
})

describe('linearTeamFromWire', () => {
  it('keeps only what the form edits', () => {
    const wire = { ...mapped(), armed: true }
    expect(linearTeamFromWire(wire)).toEqual(mapped())
  })
})

describe('linearTeamsEqual', () => {
  // A stored team as the wire renders it: refs carry id, name and type only.
  const stored: LinearTeamConfig = {
    ...unmappedLinearTeam('team-eng', 'ENG', 'Engineering'),
    ...prefillLinearRules(WORKFLOW)!,
  }

  it('ignores what an edit adds to a ref and the order a rule was picked in', () => {
    // What the editor hands back after a state is toggled off and on again:
    // the live options, with their positions, in a different order.
    const edited: LinearTeamConfig = {
      ...stored,
      pickup: { members: [...stored.pickup.members].reverse().map((m) => ({ ...m, position: 9 })) },
      done: {
        members: stored.done.members,
        canonical: { ...stored.done.canonical!, position: 6 } as LinearStateOption,
      },
    }
    expect(linearTeamsEqual([edited], [stored])).toBe(true)
  })

  it('sees a member, a write target, or the team order change', () => {
    const other = unmappedLinearTeam('team-ops', 'OPS', 'Operations')
    expect(
      linearTeamsEqual(
        [{ ...stored, pickup: { members: stored.pickup.members.slice(1) } }],
        [stored],
      ),
    ).toBe(false)
    expect(
      linearTeamsEqual(
        [{ ...stored, done: { members: stored.done.members, canonical: stored.done.members[1] } }],
        [stored],
      ),
    ).toBe(false)
    expect(linearTeamsEqual([other, stored], [stored, other])).toBe(false)
    expect(linearTeamsEqual([stored], [stored, other])).toBe(false)
  })
})

describe('linearDraftAfterSave', () => {
  const eng = unmappedLinearTeam('team-eng', 'ENG', '')
  const ops = unmappedLinearTeam('team-ops', 'OPS', '')
  // What the server stored for `eng`: the same team, with the name it resolved.
  const stored = [{ ...eng, name: 'Engineering' }]

  it('takes the stored set when nothing changed while the save was out', () => {
    expect(linearDraftAfterSave([eng], [eng], stored)).toBe(stored)
  })

  it('keeps an edit made while the save was out', () => {
    const current = [eng, ops]
    expect(linearDraftAfterSave(current, [eng], stored)).toBe(current)
  })
})

describe('saveTeamLinearTeams', () => {
  const stubPut = (answer: unknown = { linear_teams: [] }) => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, ...jsonBody(answer) })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }
  const sentBody = (fetchMock: ReturnType<typeof vi.fn>) =>
    JSON.parse(fetchMock.mock.calls[0][1].body)

  it('sends every rule as state ids, in display order, and never a name', async () => {
    const fetchMock = stubPut()
    await saveTeamLinearTeams('team-1', [
      mapped(),
      unmappedLinearTeam('team-ops', 'OPS', 'Operations'),
    ])

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/teams/team-1/linear-teams',
      expect.objectContaining({ method: 'PUT' }),
    )
    const sent = sentBody(fetchMock).linear_teams
    expect(sent).toEqual([
      {
        id: 'team-eng',
        pickup: { member_ids: ['s-triage', 's-backlog', 's-todo'] },
        in_progress: { member_ids: ['s-doing', 's-review'], canonical_id: 's-doing' },
        done: { member_ids: ['s-shipped', 's-done', 's-canceled'], canonical_id: 's-shipped' },
      },
      {
        id: 'team-ops',
        pickup: { member_ids: [] },
        in_progress: { member_ids: [], canonical_id: '' },
        done: { member_ids: [], canonical_id: '' },
      },
    ])
    expect(JSON.stringify(sent)).not.toContain('In Progress')
  })

  it('adopts the set as stored', async () => {
    stubPut({ linear_teams: [{ ...mapped(), key: 'PLAT', armed: true }] })
    const res = await saveTeamLinearTeams('team-1', [mapped()])
    expect(res).toEqual({ ok: true, teams: [{ ...mapped(), key: 'PLAT' }] })
  })

  it('surfaces the server’s reason on a refusal', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 422,
        ...jsonBody({
          errors: [
            {
              reason: 'INVALID_FIELD',
              message: 'no Linear team is visible',
              field: 'linear_teams[0].id',
            },
          ],
        }),
      }),
    )
    const res = await saveTeamLinearTeams('team-1', [mapped()])
    expect(res).toEqual({ ok: false, error: 'no Linear team is visible' })
  })
})
