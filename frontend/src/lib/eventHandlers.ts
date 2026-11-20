// Event handlers API: one resource, read through POST /api/event-handlers/list
// and GET /api/event-handlers/{id}, created with one POST and changed with one
// PATCH. The callers render failures through httpErrorMessage or
// toast.failed, so an HttpError passes through unchanged.

import type { EventHandler, EventHandlerMode } from '../types'
import { apiFetch, apiJSON, apiList } from './apiClient'
import type { ListRequest } from './apiClient'
import { handlersBase } from './scope'

/** The modes in the order the product lists them. */
export const EVENT_HANDLER_MODES: { value: EventHandlerMode; label: string }[] = [
  { value: 'off', label: 'Off' },
  { value: 'notify', label: 'Notify' },
  { value: 'watch', label: 'Watch' },
  { value: 'automate', label: 'Automate' },
]

/** makesTask mirrors domain.EventHandler.MakesTask: watch and automate. */
export function makesTask(mode: EventHandlerMode): boolean {
  return mode === 'watch' || mode === 'automate'
}

/** hasBlueprint is whether h can be moved to automate. */
export function hasBlueprint(h: Pick<EventHandler, 'blueprint_id'>): boolean {
  return h.blueprint_id !== null && h.blueprint_id !== ''
}

/** notifySource mirrors domain.NotifyRuleSource: a handler may notify only on
 *  a GitHub, Jira or Linear event. */
export function notifySource(eventType: string): boolean {
  return (
    eventType.startsWith('github:') ||
    eventType.startsWith('jira:') ||
    eventType.startsWith('linear:')
  )
}

/** The writable fields of a handler. On a create, an absent field takes the
 *  server's default; on a PATCH it keeps the stored value. null clears
 *  scope_predicate and blueprint_id. */
export interface EventHandlerFields {
  mode?: EventHandlerMode
  name?: string
  scope_predicate?: Record<string, unknown> | null
  default_priority?: number
  sort_order?: number
  blueprint_id?: string | null
  breaker_threshold?: number
  min_autonomy_suitability?: number
  applies_to_unowned?: boolean
}

export interface EventHandlerCreate extends EventHandlerFields {
  event_type: string
  // team_id is the acting team; '' lets the server resolve the sole team.
  team_id: string
}

/** detachPatch is the PATCH that takes a handler off its blueprint: the
 *  blueprint is cleared, and a handler in automate moves to watch in the same
 *  write, because automate needs a blueprint. */
export function detachPatch(h: Pick<EventHandler, 'mode'>): EventHandlerFields {
  return h.mode === 'automate' ? { blueprint_id: null, mode: 'watch' } : { blueprint_id: null }
}

function handlerPath(id: string): string {
  return `${handlersBase()}/${encodeURIComponent(id)}`
}

/** The list route. */
export function eventHandlersListPath(): string {
  return `${handlersBase()}/list`
}

export function getEventHandler(id: string): Promise<EventHandler> {
  return apiJSON<EventHandler>(handlerPath(id))
}

export function createEventHandler(body: EventHandlerCreate): Promise<EventHandler> {
  return apiJSON<EventHandler>(handlersBase(), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

export function updateEventHandler(id: string, patch: EventHandlerFields): Promise<EventHandler> {
  return apiJSON<EventHandler>(handlerPath(id), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
}

export async function deleteEventHandler(id: string): Promise<void> {
  await apiFetch(handlerPath(id), { method: 'DELETE' })
}

/** countEventHandlers answers how many handlers a list body matches, with the
 *  count-only read (page_size 0). */
export async function countEventHandlers(body: ListRequest): Promise<number> {
  const page = await apiList<EventHandler>(eventHandlersListPath(), { ...body, page_size: 0 })
  // The handler list always counts itself.
  return page.total_count ?? 0
}
