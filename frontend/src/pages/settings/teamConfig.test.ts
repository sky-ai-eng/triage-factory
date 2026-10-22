// The Jira write path's half of the id contract: the PUT takes status IDS and
// resolves display names itself, so this is where the form's refs become ids —
// and where a rule that has no ids to send is left out instead of cleared.
import { describe, it, expect, vi } from 'vitest'

import {
  dropStatus,
  projectIsArmed,
  projectRulesValid,
  ruleIsIdentified,
  saveTeamGitHubGroups,
  saveTeamJiraProjects,
  saveTeamRepos,
  teamProjectsBlocked,
  unresolvableStatuses,
  watchedProject,
  type JiraProjectConfig,
} from './teamConfig'
import type { JiraStatusRef } from '../../components/JiraStatusRule'
import { jsonBody } from '../../test/apiResponse'

const toDo = { id: '10000', name: 'To Do' }
const inProgress = { id: '10001', name: 'In Progress' }
const done = { id: '10002', name: 'Done' }
const codeReview = { id: '10003', name: 'Code Review' }

// SKY is a catalog candidate: a project the team could add, by its Jira id.
const SKY = { jira_id: '10100', key: 'SKY', name: 'Sky' }

// armed leaves in_review empty — the optional rule is not part of armed, and
// this is the shape a team without a review status stores.
const armed = (key: string): JiraProjectConfig => ({
  id: `reg-${key}`,
  key,
  pickup: { members: [toDo] },
  in_progress: { members: [inProgress], canonical: inProgress },
  in_review: { members: [] },
  done: { members: [done], canonical: done },
})

const withReview = (key: string): JiraProjectConfig => ({
  ...armed(key),
  in_review: { members: [codeReview], canonical: codeReview },
})

// legacy is a project stored before statuses were identified: names, no ids.
const legacy = (key: string): JiraProjectConfig => ({
  id: `reg-${key}`,
  key,
  pickup: { members: [{ id: '', name: 'To Do' }] },
  in_progress: {
    members: [{ id: '', name: 'In Progress' }],
    canonical: { id: '', name: 'In Progress' },
  },
  in_review: { members: [] },
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

describe('saveTeamJiraProjects', () => {
  it('names a tracked project by its registry id, and sends status ids and never names', async () => {
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [armed('SKY')])

    const [project] = sentBody(fetchMock).jira_projects
    expect(project).toEqual({
      id: 'reg-SKY',
      pickup: { member_ids: ['10000'] },
      in_progress: { member_ids: ['10001'], canonical_id: '10001' },
      in_review: { member_ids: [], canonical_id: '' },
      done: { member_ids: ['10002'], canonical_id: '10002' },
    })
    expect(JSON.stringify(project)).not.toContain('In Progress')
  })

  it('sends the in-review rule as ids like its siblings', async () => {
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [withReview('SKY')])

    expect(sentBody(fetchMock).jira_projects[0].in_review).toEqual({
      member_ids: ['10003'],
      canonical_id: '10003',
    })
  })

  it('omits a rule it cannot express in ids, rather than clearing it', async () => {
    // Absent means "keep the stored rule"; an explicit empty would wipe it. A
    // project stored before statuses were identified has no ids to send, so
    // saving an unrelated project must not take its rules down with it.
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [legacy('OLD'), armed('SKY')])

    const [old, sky] = sentBody(fetchMock).jira_projects
    // Every rule the legacy project actually names is omitted. Its in-review
    // rule is empty, and an empty rule is always expressible in ids, so it is
    // sent — a clear that clears nothing.
    expect(old).toEqual({ id: 'reg-OLD', in_review: { member_ids: [], canonical_id: '' } })
    expect(sky).toHaveProperty('pickup')
  })

  it('names a project being added by its Jira id, and sends an explicit empty rule to clear one', async () => {
    const fetchMock = stubPut()
    await saveTeamJiraProjects('team-1', [watchedProject(SKY)])

    expect(sentBody(fetchMock).jira_projects[0]).toEqual({
      jira_id: '10100',
      pickup: { member_ids: [] },
      in_progress: { member_ids: [], canonical_id: '' },
      in_review: { member_ids: [], canonical_id: '' },
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
    const watched = watchedProject(SKY)
    expect(projectRulesValid(watched)).toBe(true)
    expect(projectIsArmed(watched)).toBe(false)
    // Unarmed does not block a save — arming is the step after watching.
    expect(teamProjectsBlocked([watched], true)).toBe(false)
  })

  it('blocks a save on a rule with members and no write target', () => {
    const half = { ...watchedProject(SKY), in_progress: { members: [inProgress] } }
    expect(projectRulesValid(half)).toBe(false)
    expect(teamProjectsBlocked([half], true)).toBe(true)
  })

  it('holds the same completeness rule for in-review, which arms nothing', () => {
    const halfReview = { ...armed('SKY'), in_review: { members: [codeReview] } }
    expect(projectRulesValid(halfReview)).toBe(false)
    expect(teamProjectsBlocked([halfReview], true)).toBe(true)
    // Mapped or empty, the optional rule never moves armed.
    expect(projectIsArmed(withReview('SKY'))).toBe(true)
    expect(projectIsArmed(armed('SKY'))).toBe(true)
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
    ...watchedProject(SKY),
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

  it('checks the in-review rule too', () => {
    const gone = { id: '99999', name: 'Retired' }
    const p = project({ in_review: { members: [gone], canonical: gone } })
    expect(unresolvableStatuses(p, workflow)).toEqual([gone])
  })
})

describe('dropStatus', () => {
  const gone = { id: '99999', name: 'Retired' }

  it('removes the status from every rule that names it', () => {
    const p: JiraProjectConfig = {
      ...watchedProject(SKY),
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
      ...watchedProject(SKY),
      in_progress: { members: [gone], canonical: gone },
    }
    const next = dropStatus(p, gone)
    expect(next.in_progress.members).toEqual([])
    expect(next.in_progress.canonical).toBeNull()
    expect(projectIsArmed(next)).toBe(false)
  })

  it('strips the in-review rule as well, back to the unmapped state', () => {
    const p: JiraProjectConfig = {
      ...watchedProject(SKY),
      in_review: { members: [gone], canonical: gone },
    }
    const next = dropStatus(p, gone)
    expect(next.in_review.members).toEqual([])
    expect(next.in_review.canonical).toBeNull()
    expect(projectRulesValid(next)).toBe(true)
  })
})

// The tracked-repo write's half of the id contract: the PUT names repositories
// by registry id, so a candidate picked from the reachable list is registered by
// its GitHub id first, and nothing is ever sent as owner/name.
describe('saveTeamRepos', () => {
  it('registers what has no registry id, then sends registry ids alone', async () => {
    const stored = [
      { id: 'r-api', github_id: '11', owner: 'acme', repo: 'api' },
      { id: 'r-web', github_id: '13', owner: 'acme', repo: 'web' },
    ]
    const fetchMock = vi.fn(async (input: RequestInfo | URL) =>
      String(input) === '/api/repos'
        ? { ok: true, status: 200, ...jsonBody({ id: 'r-web' }) }
        : { ok: true, status: 200, ...jsonBody({ repos: stored }) },
    )
    vi.stubGlobal('fetch', fetchMock)

    const res = await saveTeamRepos('team-1', [
      { id: 'r-api', github_id: '11', owner: 'acme', repo: 'api' },
      { id: '', github_id: '13', owner: 'acme', repo: 'web' },
    ])

    const calls = fetchMock.mock.calls as unknown as [string, { method: string; body: string }][]
    expect(calls.map(([url, init]) => [url, init.method, JSON.parse(init.body)])).toEqual([
      ['/api/repos', 'POST', { github_id: '13' }],
      ['/api/teams/team-1/github-repos', 'PUT', { repository_ids: ['r-api', 'r-web'] }],
    ])
    expect(res).toEqual({ ok: true, repos: stored })
  })

  it('sends a registry id once when a registration resolves to a tracked row', async () => {
    // acme/legacy is tracked with no GitHub id on file; picking it again from
    // the reachable list registers it, which resolves to that same row.
    const fetchMock = vi.fn(async (input: RequestInfo | URL) =>
      String(input) === '/api/repos'
        ? { ok: true, status: 200, ...jsonBody({ id: 'r-legacy' }) }
        : { ok: true, status: 200, ...jsonBody({ repos: [] }) },
    )
    vi.stubGlobal('fetch', fetchMock)

    await saveTeamRepos('team-1', [
      { id: 'r-legacy', github_id: '', owner: 'acme', repo: 'legacy' },
      { id: '', github_id: '12', owner: 'acme', repo: 'legacy' },
    ])

    const put = fetchMock.mock.calls[1] as unknown as [string, { body: string }]
    expect(JSON.parse(put[1].body)).toEqual({ repository_ids: ['r-legacy'] })
  })
})

describe('saveTeamGitHubGroups', () => {
  it('names each GitHub team by its id, once, and returns the stored set', async () => {
    const stored = [
      { github_team_id: '11', github_org_id: '9', org_login: 'acme', team_slug: 'server' },
    ]
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      ...jsonBody({ groups: stored }),
    }))
    vi.stubGlobal('fetch', fetchMock)

    const res = await saveTeamGitHubGroups('team-1', [
      { github_team_id: '11', github_org_id: '9', org_login: 'acme', team_slug: 'backend' },
      { github_team_id: '11', github_org_id: '9', org_login: 'acme', team_slug: 'backend' },
      // Not identified by the server and matched to no candidate: no id to name it by.
      { github_team_id: '', github_org_id: '', org_login: 'acme', team_slug: 'gone' },
    ])

    expect(res).toEqual({ ok: true, groups: stored })
    const put = fetchMock.mock.calls[0] as unknown as [string, { method: string; body: string }]
    expect(put[0]).toBe('/api/teams/team-1/github-groups')
    expect(put[1].method).toBe('PUT')
    expect(JSON.parse(put[1].body)).toEqual({ github_team_ids: ['11'] })
  })
})
