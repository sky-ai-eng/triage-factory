import { beforeEach, describe, expect, it, vi } from 'vitest'
import { readFactoryFloor, readReachedCounts, type FloorHistoryCache } from './factoryFloor'
import { jsonBody } from '../test/apiResponse'
import type { EntityResource, EventResource } from '../types'

const ENTITIES = '/api/entities/list'
const EVENTS = '/api/events/list'
const TASKS = '/api/tasks/list'
const CONVERSATIONS = '/api/agent/conversations/list'

const STATIONS = new Set(['github:pr:opened', 'github:pr:ci_check_passed', 'github:pr:merged'])
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
    const floor = await readFactoryFloor({
      teamIds: ['team-a'],
      stations: STATIONS,
      cache: new Map(),
      reached: {},
      now: NOW,
    })
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
    expect(floor).toEqual({ entities: [], total: 0, runs: {}, reached: {} })
  })

  it('joins the work on the floor and reads history only where a chip may move or stands at an earlier event', async () => {
    const parked = entity('parked', 'github:pr:ci_check_passed', '2026-11-19T00:00:00Z')
    const riding = entity('riding', 'github:pr:merged', '2026-11-20T11:59:50Z')
    const commented = entity('commented', 'github:pr:commented', '2026-11-18T00:00:00Z')
    answers[ENTITIES] = () => list([parked, riding, commented], 340)
    answers[CONVERSATIONS] = () => ({
      runs: {
        'task-open': [{ id: 'run-1', status: 'running' }],
        'task-closed': [{ id: 'run-2', status: 'running' }],
      },
      total_count: 2,
    })
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
        event('e3', 'commented', 'github:pr:commented', '2026-11-18T00:00:00Z'),
        event(
          'e2',
          'riding',
          'github:pr:ci_check_passed',
          '2026-11-19T00:00:00Z',
          '2026-11-18T23:00:00Z',
        ),
        event('e1', 'commented', 'github:pr:opened', '2026-11-17T00:00:00Z'),
      ])

    const floor = await readFactoryFloor({
      teamIds: [],
      stations: STATIONS,
      cache: new Map(),
      reached: { x: 1 },
      now: NOW,
    })

    const conv = calls.find((c) => c.path === CONVERSATIONS)
    expect(conv?.body.entity_ids).toEqual(['parked', 'riding', 'commented'])
    expect(conv?.body.statuses).toContain('running')
    expect(calls.find((c) => c.path === TASKS && c.body.ids)?.body.ids).toEqual(['task-closed'])
    const history = calls.filter((c) => c.path === EVENTS)
    expect(history).toHaveLength(1)
    expect(history[0].body.entity_ids).toEqual(['riding', 'commented'])

    expect(floor.total).toBe(340)
    expect(floor.reached).toEqual({ x: 1 })
    expect(floor.runs['github:pr:ci_check_passed'].map((r) => r.run.id)).toEqual(['run-1'])
    expect(floor.runs['github:pr:merged'].map((r) => r.task.id)).toEqual(['task-closed'])

    const byId = Object.fromEntries(floor.entities.map((e) => [e.id, e]))
    expect(byId.parked.recent_events).toEqual([])
    expect(byId.parked.pending_tasks).toEqual({
      'github:pr:ci_check_passed': [{ task_id: 'task-open', dedup_key: '' }],
      'github:pr:label_added': [{ task_id: 'task-label', dedup_key: 'bug' }],
    })
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
    expect(byId.commented.recent_events.map((e) => e.event_type)).toEqual([
      'github:pr:opened',
      'github:pr:commented',
    ])
  })

  it('reuses a history until the entity has a newer event', async () => {
    const riding = entity('riding', 'github:pr:merged', '2026-11-20T11:59:50Z')
    answers[ENTITIES] = () => list([riding])
    answers[CONVERSATIONS] = () => ({ runs: {}, total_count: 0 })
    answers[TASKS] = () => list([])
    answers[EVENTS] = () =>
      list([event('e1', 'riding', 'github:pr:merged', '2026-11-20T11:59:50Z')])
    const cache: FloorHistoryCache = new Map()

    await readFactoryFloor({ teamIds: [], stations: STATIONS, cache, reached: {}, now: NOW })
    await readFactoryFloor({
      teamIds: [],
      stations: STATIONS,
      cache,
      reached: {},
      now: NOW + 1000,
    })
    expect(calls.filter((c) => c.path === EVENTS)).toHaveLength(1)

    answers[ENTITIES] = () => list([{ ...riding, latest_event_at: '2026-11-20T12:00:01Z' }])
    await readFactoryFloor({
      teamIds: [],
      stations: STATIONS,
      cache,
      reached: {},
      now: NOW + 2000,
    })
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
