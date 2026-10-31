// Notify rules API: the event-handler surface's notify kind. Reads, PATCH and
// DELETE are the shared /api/event-handlers routes; create has its own. The
// callers render failures through httpErrorMessage, so an HttpError passes
// through unchanged.

import type { NotifyAudience, NotifyHandler } from '../types'
import { apiFetch, apiListAll } from './apiClient'
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

export function listNotifyRules(): Promise<NotifyHandler[]> {
  return apiListAll<NotifyHandler>(`${handlersBase()}/list`, { kind: 'notify' })
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
