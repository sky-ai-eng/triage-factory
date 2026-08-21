// The Jira write path's half of the id contract: the PUT takes status IDS and
// resolves display names itself, so this is where the form's refs become ids —
// and where a rule that has no ids to send is left out instead of cleared.
import { describe, it, expect, vi, afterEach } from 'vitest'

import {
  dropStatus,
  emptyProject,
  projectIsArmed,
  projectRulesValid,
  ruleIsIdentified,
  saveTeamJiraProjects,
  teamProjectsBlocked,
  unresolvableStatuses,
  type JiraProjectConfig,
} from './teamConfig'
import type { JiraStatusRef } from '../../components/JiraStatusRule'
import { jsonBody } from '../../test/apiResponse'

const toDo = { id: '10000', name: 'To Do' }
const inProgress = { id: '10001', name: 'In Progress' }
const done = { id: '10002', name: 'Done' }

const armed = (key: string): JiraProjectConfig => ({
  key,
  pickup: { members: [toDo] },
  in_progress: { members: [inProgress], canonical: inProgress },
  done: { members: [done], canonical: done },
})

// legacy is a project stored before statuses were identified: names, no ids.
const legacy = (key: string): JiraProjectConfig => ({
  key,
  pickup: { members: [{ id: '', name: 'To Do' }] },
  in_progress: {
    members: [{ id: '', name: 'In Progress' }],
    canonical: { id: '', name: 'In Progress' },
  },
  done: { members: [{ id: '', name: 'Done' }], canonical: { id: '', name: 'Done' } },
})

function stubPut(stored: JiraProjectConfig[] = []) {
  const fetchMock = vi.fn(async () => ({
    ok: true,
    status: 200,
    ...jsonBody({ jira_projects: stored }),
  }))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function sentBody(fetchMock: ReturnType<typeof stubPut>): {
  jira_projects: Record<string, unknown>[]
} {
  const call = fetchMock.mock.calls[0] as unknown as [string, { body: string }]
  return JSON.parse(call[1].body)
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('saveTeamJiraProjects', () => {
  it('sends status ids and never names', async () => {
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [armed('SKY')])

    const [project] = sentBody(fetchMock).jira_projects
    expect(project).toEqual({
      key: 'SKY',
      pickup: { member_ids: ['10000'] },
      in_progress: { member_ids: ['10001'], canonical_id: '10001' },
      done: { member_ids: ['10002'], canonical_id: '10002' },
    })
    expect(JSON.stringify(project)).not.toContain('In Progress')
  })

  it('omits a rule it cannot express in ids, rather than clearing it', async () => {
    // Absent means "keep the stored rule"; an explicit empty would wipe it. A
    // project stored before statuses were identified has no ids to send, so
    // saving an unrelated project must not take its rules down with it.
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [legacy('OLD'), armed('SKY')])

    const [old, sky] = sentBody(fetchMock).jira_projects
    expect(old).toEqual({ key: 'OLD' })
    expect(sky).toHaveProperty('pickup')
  })

  it('sends an explicit empty rule to clear one', async () => {
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [emptyProject('SKY')])

    expect(sentBody(fetchMock).jira_projects[0]).toEqual({
      key: 'SKY',
      pickup: { member_ids: [] },
      in_progress: { member_ids: [], canonical_id: '' },
      done: { member_ids: [], canonical_id: '' },
    })
  })

  it('returns the set as stored, which is where refreshed names come from', async () => {
    const renamed: JiraProjectConfig = {
      ...armed('SKY'),
      pickup: { members: [{ id: '10000', name: 'Selected for work' }] },
    }
    stubPut([renamed])
    const res = await saveTeamJiraProjects('team-1', [armed('SKY')])

    expect(res.ok).toBe(true)
    if (res.ok) {
      expect(res.projects[0].pickup.members[0].name).toBe('Selected for work')
    }
  })
})

describe('project rule predicates', () => {
  it('reads a watched-but-unmapped project as valid and unarmed', () => {
    const watched = emptyProject('SKY')
    expect(projectRulesValid(watched)).toBe(true)
    expect(projectIsArmed(watched)).toBe(false)
    // Unarmed does not block a save — arming is the step after watching.
    expect(teamProjectsBlocked([watched], true)).toBe(false)
  })

  it('blocks a save on a rule with members and no write target', () => {
    const half = { ...emptyProject('SKY'), in_progress: { members: [inProgress] } }
    expect(projectRulesValid(half)).toBe(false)
    expect(teamProjectsBlocked([half], true)).toBe(true)
  })

  it('reads a legacy name-only project as armed and identified-less', () => {
    expect(projectIsArmed(legacy('OLD'))).toBe(true)
    expect(ruleIsIdentified(legacy('OLD').pickup)).toBe(false)
    expect(ruleIsIdentified(armed('SKY').pickup)).toBe(true)
  })
})

describe('unresolvableStatuses', () => {
  const workflow: JiraStatusRef[] = [
    { id: '10001', name: 'To Do' },
    { id: '10002', name: 'In Progress' },
    { id: '10003', name: 'Done' },
  ]

  const project = (over: Partial<JiraProjectConfig> = {}): JiraProjectConfig => ({
    ...emptyProject('SKY'),
    ...over,
  })

  it('reports a member whose status is gone from the workflow', () => {
    const p = project({
      pickup: {
        members: [
          { id: '10001', name: 'To Do' },
          { id: '99999', name: 'Retired' },
        ],
      },
    })
    expect(unresolvableStatuses(p, workflow)).toEqual([{ id: '99999', name: 'Retired' }])
  })

  it('reports a canonical whose status is gone, not just members', () => {
    const p = project({
      done: {
        members: [{ id: '10003', name: 'Done' }],
        canonical: { id: '88888', name: 'Archived' },
      },
    })
    expect(unresolvableStatuses(p, workflow)).toEqual([{ id: '88888', name: 'Archived' }])
  })

  it('resolves a renamed status by id, so a rename is not a missing status', () => {
    const p = project({ pickup: { members: [{ id: '10001', name: 'Backlog' }] } })
    expect(unresolvableStatuses(p, workflow)).toEqual([])
  })

  it('resolves a name-only member against the workflow, for rules with no ids', () => {
    const p = project({ pickup: { members: [{ id: '', name: 'To Do' }] } })
    expect(unresolvableStatuses(p, workflow)).toEqual([])
  })

  it('reports nothing when the workflow has not been fetched', () => {
    const p = project({ pickup: { members: [{ id: '99999', name: 'Retired' }] } })
    expect(unresolvableStatuses(p, [])).toEqual([])
  })

  it('reports one status once even when several rules name it', () => {
    const gone = { id: '99999', name: 'Retired' }
    const p = project({
      pickup: { members: [gone] },
      done: { members: [gone], canonical: gone },
    })
    expect(unresolvableStatuses(p, workflow)).toEqual([gone])
  })
})

describe('dropStatus', () => {
  const gone = { id: '99999', name: 'Retired' }

  it('removes the status from every rule that names it', () => {
    const p: JiraProjectConfig = {
      ...emptyProject('SKY'),
      pickup: {
        members: [{ id: '10001', name: 'To Do' }, gone],
      },
      done: {
        members: [gone, { id: '10003', name: 'Done' }],
        canonical: { id: '10003', name: 'Done' },
      },
    }
    const next = dropStatus(p, gone)
    expect(next.pickup.members).toEqual([{ id: '10001', name: 'To Do' }])
    expect(next.done.members).toEqual([{ id: '10003', name: 'Done' }])
    expect(next.done.canonical).toEqual({ id: '10003', name: 'Done' })
  })

  it('clears a canonical that pointed at the dropped status, leaving the rule incomplete', () => {
    const p: JiraProjectConfig = {
      ...emptyProject('SKY'),
      in_progress: { members: [gone], canonical: gone },
    }
    const next = dropStatus(p, gone)
    expect(next.in_progress.members).toEqual([])
    expect(next.in_progress.canonical).toBeNull()
    expect(projectIsArmed(next)).toBe(false)
  })
})
