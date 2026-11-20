import { beforeEach, describe, expect, it, vi } from 'vitest'
import { readFactoryFloor, readReachedCounts, type FloorHistoryCache } from './factoryFloor'
import { jsonBody } from '../test/apiResponse'
import type { EntityResource, EventResource } from '../types'

const ENTITIES = '/api/entities/list'
const EVENTS = '/api/events/list'
const TASKS = '/api/tasks/list'
const CONVERSATIONS = '/api/agent/conversations/list'

const NOW = Date.parse('2026-11-20T12:00:00Z')

function entity(id: string, latest: string | null, latestAt: string | null): EntityResource {
  return {
    id,
    source: 'github',
    source_id: `acme/api#${id}`,
    kind: 'pr',
    title: id,
    url: '',
    state: 'active',
    created_at: '2026-11-01T00:00:00Z',
    closed_at: null,
    repository_id: 'repo-1',
    latest_event_type: latest,
    latest_event_at: latestAt,
    mine: false,
    pull_request: null,
  }
}

function event(
  id: string,
  entityId: string,
  type: string,
  createdAt: string,
  occurredAt: string | null = null,
): EventResource {
  return {
    id,
    entity_id: entityId,
    event_type: type,
    dedup_key: '',
    occurred_at: occurredAt,
    created_at: createdAt,
  }
}

type Call = { path: string; body: Record<string, unknown> }
let calls: Call[] = []
let answers: Record<string, (body: Record<string, unknown>) => unknown> = {}

beforeEach(() => {
  calls = []
  answers = {}
  vi.stubGlobal(
    'fetch',
    vi.fn(async (path: string, init?: RequestInit) => {
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {}
      calls.push({ path, body })
      const key = path.split('?')[0]
      const answer = answers[key]
      if (!answer) throw new Error(`unexpected request ${path}`)
      return { ok: true, status: 200, headers: new Headers(), ...jsonBody(answer(body)) }
    }),
  )
})

const list = (items: unknown[], total = items.length) => ({
  items,
  next_page_token: '',
  total_count: total,
})

describe('readFactoryFloor', () => {
  it('asks for one page of GitHub pull requests, open or closed in the last minute, in the filtered teams', async () => {
    answers[ENTITIES] = () => list([], 0)
    const floor = await readFactoryFloor({ teamIds: ['team-a'], cache: new Map(), now: NOW })
    expect(calls).toHaveLength(1)
    expect(calls[0].body).toEqual({
      sources: ['github'],
      kinds: ['pr'],
      closed_since: new Date(NOW - 60_000).toISOString(),
      team_ids: ['team-a'],
      page_size: 200,
    })
    // An empty floor names no entities, and an empty entity_ids narrows
    // nothing, so nothing else is read.
    expect(floor).toEqual({ entities: [], total: 0, runs: {} })
  })

  it('joins every page of the work on the floor and reads each entity a tail of its history', async () => {
    const parked = entity('parked', 'github:pr:ci_check_passed', '2026-11-19T00:00:00Z')
    const riding = entity('riding', 'github:pr:merged', '2026-11-20T11:59:50Z')
    const quiet = entity('quiet', null, null)
    answers[ENTITIES] = () => list([parked, riding, quiet], 340)
    answers[CONVERSATIONS] = (body) =>
      body.page_token
        ? { runs: { 'task-open': [{ id: 'run-3', status: 'running' }] }, total_count: 3 }
        : {
            runs: {
              'task-open': [{ id: 'run-1', status: 'running' }],
              'task-closed': [{ id: 'run-2', status: 'running' }],
            },
            total_count: 3,
            next_page_token: 'page-2',
          }
    answers[TASKS] = (body) =>
      body.ids
        ? list([
            {
              id: 'task-closed',
              entity_id: 'riding',
              event_type: 'github:pr:merged',
              status: 'done',
            },
          ])
        : list([
            {
              id: 'task-open',
              entity_id: 'parked',
              event_type: 'github:pr:ci_check_passed',
              dedup_key: '',
            },
            {
              id: 'task-label',
              entity_id: 'parked',
              event_type: 'github:pr:label_added',
              dedup_key: 'bug',
            },
          ])
    answers[EVENTS] = () =>
      list([
        event('e4', 'riding', 'github:pr:merged', '2026-11-20T11:59:50Z'),
        event('e3', 'parked', 'github:pr:ci_check_passed', '2026-11-19T00:00:00Z'),
        event(
          'e2',
          'riding',
          'github:pr:ci_check_passed',
          '2026-11-19T00:00:00Z',
          '2026-11-18T23:00:00Z',
        ),
        event('e1', 'parked', 'github:pr:opened', '2026-11-01T00:00:00Z'),
      ])

    const floor = await readFactoryFloor({ teamIds: ['team-a'], cache: new Map(), now: NOW })

    const convs = calls.filter((c) => c.path === CONVERSATIONS)
    expect(convs.map((c) => c.body.page_token)).toEqual([undefined, 'page-2'])
    expect(convs[0].body.entity_ids).toEqual(['parked', 'riding', 'quiet'])
    expect(convs[0].body.statuses).toContain('running')
    const open = calls.find((c) => c.path === TASKS && !c.body.ids)
    expect(open?.body.team_ids).toEqual(['team-a'])
    expect(calls.find((c) => c.path === TASKS && c.body.ids)?.body.ids).toEqual(['task-closed'])
    // One read names every entity with an event, and asks for a tail of
    // each rather than a window of time.
    const history = calls.filter((c) => c.path === EVENTS)
    expect(history).toHaveLength(1)
    expect(history[0].body).toEqual({
      entity_ids: ['parked', 'riding'],
      per_entity: 10,
      page_size: 200,
    })

    expect(floor.total).toBe(340)
    expect(floor.runs['github:pr:ci_check_passed'].map((r) => r.run.id)).toEqual(['run-1', 'run-3'])
    expect(floor.runs['github:pr:merged'].map((r) => r.task.id)).toEqual(['task-closed'])

    const byId = Object.fromEntries(floor.entities.map((e) => [e.id, e]))
    expect(byId.parked.pending_tasks).toEqual({
      'github:pr:ci_check_passed': [{ task_id: 'task-open', dedup_key: '' }],
      'github:pr:label_added': [{ task_id: 'task-label', dedup_key: 'bug' }],
    })
    expect(byId.parked.recent_events.map((e) => e.event_type)).toEqual([
      'github:pr:opened',
      'github:pr:ci_check_passed',
    ])
    expect(byId.riding.recent_events).toEqual([
      {
        event_type: 'github:pr:ci_check_passed',
        at: '2026-11-18T23:00:00Z',
        detected_at: '2026-11-19T00:00:00Z',
      },
      {
        event_type: 'github:pr:merged',
        at: '2026-11-20T11:59:50Z',
        detected_at: '2026-11-20T11:59:50Z',
      },
    ])
    expect(byId.quiet.recent_events).toEqual([])
  })

  it('reuses a history until the entity has a newer event', async () => {
    const riding = entity('riding', 'github:pr:merged', '2026-11-20T11:59:50Z')
    answers[ENTITIES] = () => list([riding])
    answers[CONVERSATIONS] = () => ({ runs: {}, total_count: 0 })
    answers[TASKS] = () => list([])
    answers[EVENTS] = () =>
      list([event('e1', 'riding', 'github:pr:merged', '2026-11-20T11:59:50Z')])
    const cache: FloorHistoryCache = new Map()

    await readFactoryFloor({ teamIds: [], cache, now: NOW })
    await readFactoryFloor({ teamIds: [], cache, now: NOW + 1000 })
    expect(calls.filter((c) => c.path === EVENTS)).toHaveLength(1)

    answers[ENTITIES] = () => list([{ ...riding, latest_event_at: '2026-11-20T12:00:01Z' }])
    await readFactoryFloor({ teamIds: [], cache, now: NOW + 2000 })
    expect(calls.filter((c) => c.path === EVENTS)).toHaveLength(2)
  })
})

describe('readReachedCounts', () => {
  it('reads distinct entities per event type from the viewer activity node over the last 30 days', async () => {
    answers['/api/me/activity'] = () => ({
      by_event_type: [{ event_type: 'github:pr:merged', events: 9, tasks: 2, entities: 4 }],
    })
    expect(await readReachedCounts(NOW)).toEqual({ 'github:pr:merged': 4 })
    const since = new Date(NOW - 30 * 24 * 60 * 60_000).toISOString()
    expect(calls[0].path).toBe(`/api/me/activity?since=${encodeURIComponent(since)}`)
  })
})
