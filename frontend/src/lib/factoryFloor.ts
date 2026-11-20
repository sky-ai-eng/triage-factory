import { apiJSON, apiList, apiListAll } from './apiClient'
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
// the task and conversation lists, each entity's recent events from the event
// list, and the station counters from the activity node's by_event_type cut
// over the last REACHED_WINDOW_DAYS days. What the floor makes of them —
// stations, chips, rides — is the scene's.

export const ENTITY_LIST_PATH = '/api/entities/list'
export const EVENT_LIST_PATH = '/api/events/list'
const CONVERSATION_LIST_PATH = '/api/agent/conversations/list'
const ACTIVITY_PATH = '/api/me/activity'

/** How many pull requests the floor holds: one full page. */
export const FLOOR_PAGE_SIZE = 200

/** How long a closed pull request stays on the floor, so its chip can ride
 *  the last bridge into its terminal station. */
const CLOSED_GRACE_MS = 60_000

/** How many events of an entity's history the chain animation keeps, and how
 *  many the event list returns for each entity. */
const HISTORY_TAIL = 10

/** How many entities one history read names: as many as fill one page with a
 *  full tail each. */
const HISTORY_GROUP = FLOOR_PAGE_SIZE / HISTORY_TAIL

/** The task statuses of a task still open at its station. */
const OPEN_TASK_STATUSES = ['queued', 'in_progress', 'snoozed']

/** One entity's history as last read, with the latest event time it was read
 *  at. An entity's history changes only when a new event lands, which moves
 *  its latest event, so an entry whose time still matches is current. */
export type FloorHistoryCache = Map<
  string,
  { latestEventAt: string | null; events: FactoryRecentEvent[] }
>

/** A floor as the reads build it, without the station counters, which are
 *  read on their own schedule (readReachedCounts). */
export type FloorRead = Omit<FactoryFloor, 'reached'>

type ConversationListEnvelope = {
  runs?: Record<string, Conversation[]>
  next_page_token?: string
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

/** Reads the histories the cache does not hold, and answers each entity's
 *  history. */
async function readHistories(
  entities: EntityResource[],
  cache: FloorHistoryCache,
): Promise<Map<string, FactoryRecentEvent[]>> {
  const stale = entities.filter(
    (e) => e.latest_event_at && cache.get(e.id)?.latestEventAt !== e.latest_event_at,
  )
  const groups: EntityResource[][] = []
  for (let i = 0; i < stale.length; i += HISTORY_GROUP)
    groups.push(stale.slice(i, i + HISTORY_GROUP))
  const pages = await Promise.all(
    groups.map((group) =>
      apiListAll<EventResource>(EVENT_LIST_PATH, {
        entity_ids: group.map((e) => e.id),
        per_entity: HISTORY_TAIL,
      }),
    ),
  )
  groups.forEach((group, i) => {
    const byEntity = new Map<string, FactoryRecentEvent[]>()
    // The page is newest recorded first; reversed, ties on a source time
    // keep the order the events were recorded in.
    for (const ev of [...pages[i]].reverse()) {
      const list = byEntity.get(ev.entity_id) ?? []
      list.push({
        event_type: ev.event_type,
        at: ev.occurred_at ?? ev.created_at,
        detected_at: ev.created_at,
      })
      byEntity.set(ev.entity_id, list)
    }
    for (const e of group) {
      // The chain plays in source-time order, which the list does not sort
      // by: it orders by recording, the one order a walk can resume in.
      const events = (byEntity.get(e.id) ?? [])
        .map((ev, order) => ({ ev, order }))
        .sort((a, b) => Date.parse(a.ev.at) - Date.parse(b.ev.at) || a.order - b.order)
        .map(({ ev }) => ev)
      cache.set(e.id, { latestEventAt: e.latest_event_at, events })
    }
  })
  const keep = new Set(entities.map((e) => e.id))
  for (const id of cache.keys()) if (!keep.has(id)) cache.delete(id)
  const out = new Map<string, FactoryRecentEvent[]>()
  for (const e of entities) out.set(e.id, cache.get(e.id)?.events ?? [])
  return out
}

/** Every in-flight conversation on the entities, by task id, across every
 *  page of the list. */
async function readActiveRuns(entityIds: string[]): Promise<Record<string, Conversation[]>> {
  const runs: Record<string, Conversation[]> = {}
  let token = ''
  do {
    const page = await apiJSON<ConversationListEnvelope>(CONVERSATION_LIST_PATH, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        entity_ids: entityIds,
        statuses: ACTIVE_STATUSES,
        page_size: FLOOR_PAGE_SIZE,
        ...(token ? { page_token: token } : {}),
      }),
    })
    for (const [taskId, rows] of Object.entries(page.runs ?? {}))
      (runs[taskId] ??= []).push(...rows)
    token = page.next_page_token ?? ''
  } while (token)
  return runs
}

/** The conversations in flight on the entities, with their tasks, and the
 *  open tasks on them in the filtered teams. Every page of each list is read:
 *  an entity whose task sat on an unread page would show none. */
async function readWork(
  entityIds: string[],
  teamIds: string[],
): Promise<{ runs: FactoryRun[]; openTasks: Task[] }> {
  const [convs, openTasks] = await Promise.all([
    readActiveRuns(entityIds),
    apiListAll<Task>(TASK_LIST_PATH, {
      entity_ids: entityIds,
      team_ids: teamIds,
      statuses: OPEN_TASK_STATUSES,
      include_snoozed: true,
    }),
  ])
  const tasks = new Map(openTasks.map((t) => [t.id, t]))
  // A run goes on after its task closes, and a run on a shared entity may
  // work another team's task, so a run's task may not be among those read.
  const missing = Object.keys(convs).filter((id) => !tasks.has(id))
  const chunks: string[][] = []
  for (let i = 0; i < missing.length; i += FLOOR_PAGE_SIZE)
    chunks.push(missing.slice(i, i + FLOOR_PAGE_SIZE))
  const found = await Promise.all(
    chunks.map((ids) => apiListAll<Task>(TASK_LIST_PATH, { ids, include_snoozed: true })),
  )
  for (const t of found.flat()) tasks.set(t.id, t)
  const runs: FactoryRun[] = []
  for (const [taskId, rows] of Object.entries(convs)) {
    const task = tasks.get(taskId)
    if (!task) continue
    for (const run of rows) runs.push({ run, task })
  }
  return { runs, openTasks }
}

/** Reads the floor for the team filter. `cache` holds the histories of the
 *  last read, which this one reuses for every entity with no newer event. */
export async function readFactoryFloor(opts: {
  teamIds: string[]
  cache: FloorHistoryCache
  now: number
}): Promise<FloorRead> {
  const page = await readFloorEntities(opts.teamIds, opts.now)
  const ids = page.items.map((e) => e.id)
  // An empty entity_ids narrows nothing, so an empty floor reads no work.
  const [work, histories] = await Promise.all([
    ids.length > 0 ? readWork(ids, opts.teamIds) : Promise.resolve({ runs: [], openTasks: [] }),
    readHistories(page.items, opts.cache),
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
  }
}
