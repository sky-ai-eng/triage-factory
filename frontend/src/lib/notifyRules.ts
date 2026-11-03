// Notify rules API: the event-handler surface's notify kind. Reads, PATCH and
// DELETE are the shared /api/event-handlers routes; create has its own. The
// callers render failures through httpErrorMessage, so an HttpError passes
// through unchanged.

import type { EventHandler, NotifyAudience } from '../types'
import { apiFetch, apiJSON } from './apiClient'
import type { ListRequest } from './apiClient'
import { handlersBase, notifyCreatePath } from './scope'

export interface NotifyRuleCreate {
  event_type: string
  name: string
  audience: NotifyAudience
  // team_id is the acting team; '' lets the server resolve the sole team.
  team_id: string
  scope_predicate?: Record<string, unknown>
}

// NotifyRulePatch names only the fields being changed. scope_predicate: null
// clears the filter.
export interface NotifyRulePatch {
  name?: string
  audience?: NotifyAudience
  enabled?: boolean
  scope_predicate?: Record<string, unknown> | null
}

function rulePath(id: string): string {
  return `${handlersBase()}/${encodeURIComponent(id)}`
}

/** The handler list route the panel's reads page through. */
export function notifyRulesListPath(): string {
  return `${handlersBase()}/list`
}

/** The list body for a person's own personal rules: the notify rules telling
 *  their creator, authored by userId. */
export function personalNotifyRulesFilter(userId: string): ListRequest {
  return { kind: 'notify', audience: 'creator', creator_user_id: userId }
}

/** The list body for the team rules of the caller's teams. Another member's
 *  personal rule is readable too, but it tells only its creator, so neither
 *  read lists it. */
export function teamNotifyRulesFilter(): ListRequest {
  return { kind: 'notify', audience: 'team' }
}

/** getNotifyRule reads one handler by id, for a link to a rule that need not
 *  be on a page the panel has read. The caller checks it is a notify rule. */
export function getNotifyRule(id: string): Promise<EventHandler> {
  return apiJSON<EventHandler>(rulePath(id))
}

export async function createNotifyRule(body: NotifyRuleCreate): Promise<void> {
  await apiFetch(notifyCreatePath(), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

export async function updateNotifyRule(id: string, patch: NotifyRulePatch): Promise<void> {
  await apiFetch(rulePath(id), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
}

export async function deleteNotifyRule(id: string): Promise<void> {
  await apiFetch(rulePath(id), { method: 'DELETE' })
}
