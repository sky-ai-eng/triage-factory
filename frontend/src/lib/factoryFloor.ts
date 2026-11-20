import { apiJSON, apiList } from './apiClient'
import { ACTIVE_STATUSES } from './conversationStatus'
import { TASK_LIST_PATH } from './taskList'
import type {
  ActivityEventType,
  Conversation,
  EntityResource,
  EventResource,
  FactoryEntity,
  FactoryFloor,
  FactoryRecentEvent,
  FactoryRun,
  Task,
} from '../types'

// The factory floor's reads. Nothing here is factory-shaped on the server:
// the floor is one page of the entity list, the work on those entities from
// the task and conversation lists, the recent events of the entities that
// need them from the event list, and the station counters from the activity
// node's by_event_type cut over the last REACHED_WINDOW_DAYS days. What the floor makes of them — stations, chips,
// rides — is the scene's.

export const ENTITY_LIST_PATH = '/api/entities/list'
export const EVENT_LIST_PATH = '/api/events/list'
const CONVERSATION_LIST_PATH = '/api/agent/conversations/list'
const ACTIVITY_PATH = '/api/me/activity'

/** How many pull requests the floor holds: one full page. */
export const FLOOR_PAGE_SIZE = 200

/** How long a closed pull request stays on the floor, so its chip can ride
 *  the last bridge into its terminal station. */
const CLOSED_GRACE_MS = 60_000

/** How recently an entity's latest event must have landed for its chip to be
 *  possibly mid-ride. Longer than any chain of hops takes to play. */
const RIDE_HORIZON_MS = 10 * 60_000

/** How far back the floor reads an entity's history, to find the station it
 *  rode in from or the last station it stood at. */
const HISTORY_LOOKBACK_MS = 7 * 24 * 60 * 60_000

/** How many entities one history read names. A read is one page of events,
 *  newest first, so a smaller group leaves each entity more of the page. */
const HISTORY_GROUP = 20

/** How many events of an entity's history the chain animation keeps. */
const HISTORY_TAIL = 10

/** The task statuses of a task still open at its station. */
const OPEN_TASK_STATUSES = ['queued', 'in_progress', 'snoozed']

/** One entity's history as last read, with the latest event time it was read
 *  at. An entity's history changes only when a new event lands, which moves
 *  its latest event, so an entry whose time still matches is current. */
export type FloorHistoryCache = Map<string, { latestEventAt: string; events: FactoryRecentEvent[] }>

type ConversationListEnvelope = {
  runs?: Record<string, Conversation[]>
}

type ActivityNode = {
  by_event_type?: ActivityEventType[]
}

/** The floor's page of pull requests: the open ones and those closed in the
 *  last minute, in the teams' tracked repositories. */
export function readFloorEntities(teamIds: string[], now: number) {
  return apiList<EntityResource>(ENTITY_LIST_PATH, {
    sources: ['github'],
    kinds: ['pr'],
    closed_since: new Date(now - CLOSED_GRACE_MS).toISOString(),
    team_ids: teamIds,
    page_size: FLOOR_PAGE_SIZE,
  })
}

/** How far back a station's counter looks. A window rather than all history
 *  keeps the activity read bounded as an org ages, and says what is moving
 *  now rather than what ever moved. */
export const REACHED_WINDOW_DAYS = 30

/** Distinct entities that reached each event type in the last
 *  REACHED_WINDOW_DAYS days, over the viewer's teams. */
export async function readReachedCounts(now: number): Promise<Record<string, number>> {
  const since = new Date(now - REACHED_WINDOW_DAYS * 24 * 60 * 60_000).toISOString()
  const node = await apiJSON<ActivityNode>(`${ACTIVITY_PATH}?since=${encodeURIComponent(since)}`)
  const out: Record<string, number> = {}
  for (const row of node.by_event_type ?? []) out[row.event_type] = row.entities
  return out
}

/** Whether the floor reads an entity's history: when its chip may be
 *  mid-ride, or when its latest event has no station, so the station it
 *  stands at is an earlier event's. */
function needsHistory(e: EntityResource, stations: ReadonlySet<string>, now: number): boolean {
  if (!e.latest_event_type || !e.latest_event_at) return false
  if (!stations.has(e.latest_event_type)) return true
  return now - Date.parse(e.latest_event_at) < RIDE_HORIZON_MS
}

/** Reads the histories the floor needs and the cache does not hold, and
 *  answers each needed entity's history. */
async function readHistories(
  entities: EntityResource[],
  stations: ReadonlySet<string>,
  cache: FloorHistoryCache,
  now: number,
): Promise<Map<string, FactoryRecentEvent[]>> {
  const needed = entities.filter((e) => needsHistory(e, stations, now))
  const stale = needed.filter((e) => cache.get(e.id)?.latestEventAt !== e.latest_event_at)
  const since = new Date(now - HISTORY_LOOKBACK_MS).toISOString()
  const groups: EntityResource[][] = []
  for (let i = 0; i < stale.length; i += HISTORY_GROUP)
    groups.push(stale.slice(i, i + HISTORY_GROUP))
  const pages = await Promise.all(
    groups.map((group) =>
      apiList<EventResource>(EVENT_LIST_PATH, {
        entity_ids: group.map((e) => e.id),
        since,
        page_size: FLOOR_PAGE_SIZE,
      }),
    ),
  )
  groups.forEach((group, i) => {
    const byEntity = new Map<string, FactoryRecentEvent[]>()
    // The page is newest recorded first; reversed, ties on a source time
    // keep the order the events were recorded in.
    for (const ev of [...pages[i].items].reverse()) {
      const list = byEntity.get(ev.entity_id) ?? []
      list.push({
        event_type: ev.event_type,
        at: ev.occurred_at ?? ev.created_at,
        detected_at: ev.created_at,
      })
      byEntity.set(ev.entity_id, list)
    }
    for (const e of group) {
      const events = (byEntity.get(e.id) ?? [])
        .map((ev, order) => ({ ev, order }))
        .sort((a, b) => Date.parse(a.ev.at) - Date.parse(b.ev.at) || a.order - b.order)
        .map(({ ev }) => ev)
        .slice(-HISTORY_TAIL)
      cache.set(e.id, { latestEventAt: e.latest_event_at ?? '', events })
    }
  })
  const keep = new Set(entities.map((e) => e.id))
  for (const id of cache.keys()) if (!keep.has(id)) cache.delete(id)
  const out = new Map<string, FactoryRecentEvent[]>()
  for (const e of needed) out.set(e.id, cache.get(e.id)?.events ?? [])
  return out
}

/** The conversations in flight on the entities, with their tasks, and the
 *  open tasks on them. */
async function readWork(entityIds: string[]): Promise<{ runs: FactoryRun[]; openTasks: Task[] }> {
  const [convs, open] = await Promise.all([
    apiJSON<ConversationListEnvelope>(CONVERSATION_LIST_PATH, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        entity_ids: entityIds,
        statuses: ACTIVE_STATUSES,
        page_size: FLOOR_PAGE_SIZE,
      }),
    }),
    apiList<Task>(TASK_LIST_PATH, {
      entity_ids: entityIds,
      statuses: OPEN_TASK_STATUSES,
      include_snoozed: true,
      page_size: FLOOR_PAGE_SIZE,
    }),
  ])
  const tasks = new Map(open.items.map((t) => [t.id, t]))
  // A run goes on after its task closes, so a run's task may not be open.
  const missing = Object.keys(convs.runs ?? {}).filter((id) => !tasks.has(id))
  if (missing.length > 0) {
    const closed = await apiList<Task>(TASK_LIST_PATH, {
      ids: missing,
      include_snoozed: true,
      page_size: FLOOR_PAGE_SIZE,
    })
    for (const t of closed.items) tasks.set(t.id, t)
  }
  const runs: FactoryRun[] = []
  for (const [taskId, rows] of Object.entries(convs.runs ?? {})) {
    const task = tasks.get(taskId)
    if (!task) continue
    for (const run of rows) runs.push({ run, task })
  }
  return { runs, openTasks: open.items }
}

/** Reads the floor for the team filter. `stations` is the set of event types
 *  the scene has a station for; `reached` is the station counters last read,
 *  which this read does not refresh. */
export async function readFactoryFloor(opts: {
  teamIds: string[]
  stations: ReadonlySet<string>
  cache: FloorHistoryCache
  reached: Record<string, number>
  now: number
}): Promise<FactoryFloor> {
  const page = await readFloorEntities(opts.teamIds, opts.now)
  const ids = page.items.map((e) => e.id)
  // An empty entity_ids narrows nothing, so an empty floor reads no work.
  const [work, histories] = await Promise.all([
    ids.length > 0 ? readWork(ids) : Promise.resolve({ runs: [], openTasks: [] }),
    readHistories(page.items, opts.stations, opts.cache, opts.now),
  ])

  const pending = new Map<string, FactoryEntity['pending_tasks']>()
  for (const t of work.openTasks) {
    const byType = pending.get(t.entity_id) ?? {}
    ;(byType[t.event_type] ??= []).push({ task_id: t.id, dedup_key: t.dedup_key ?? '' })
    pending.set(t.entity_id, byType)
  }
  const runs: FactoryFloor['runs'] = {}
  for (const r of work.runs) (runs[r.task.event_type] ??= []).push(r)

  return {
    entities: page.items.map((e) => ({
      ...e,
      recent_events: histories.get(e.id) ?? [],
      pending_tasks: pending.get(e.id) ?? {},
    })),
    total: page.total_count ?? page.items.length,
    runs,
    reached: opts.reached,
  }
}
