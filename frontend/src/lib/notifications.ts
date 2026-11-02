import { apiFetch, apiJSON, apiList, readJSON, type ListPage } from './apiClient'

// The notifications inbox (specs/durable-notifications.md, HTTP API). It is
// viewer-relative, so every route is under /api/me and answers for the org the
// credential carries.

export type NotificationPriority = 'urgent' | 'high' | 'low'
export type NotificationScope = 'org' | 'team' | 'user'

export interface NotificationAction {
  id: string
  type: 'link' | 'invoke'
  label: string
  /** A link's destination: an in-app path, org prefix included, or an external URL. */
  href?: string
  /** An invoke's confirmation copy, when it asks for one. */
  confirm?: string
  /** On an invoke: whether this viewer may run it, here and now. */
  allowed?: boolean
}

export interface NotificationItem {
  id: string
  kind: string
  lifecycle: 'condition' | 'notice'
  priority: NotificationPriority
  audience: {
    scope: NotificationScope
    role: string | null
    team_id: string | null
    user_id: string | null
  }
  title: string
  body: string
  dismissible: boolean
  first_seen_at: string
  last_seen_at: string
  seen_count: number
  resolved_at: string | null
  resolved_reason: string | null
  expires_at: string | null
  actions: NotificationAction[]
  read: boolean
  dismissed: boolean
}

/** The inbox's filters. An absent field does not filter. */
export interface NotificationFilter {
  status?: 'open' | 'resolved'
  dismissed?: boolean
  unread?: boolean
  priority_in?: NotificationPriority[]
  scope_in?: NotificationScope[]
}

const LIST_PATH = '/api/me/notifications/list'

function itemPath(id: string): string {
  return `/api/me/notifications/${encodeURIComponent(id)}`
}

/** One page of the inbox; `pageSize` 0 is the count-only read. */
export function listNotifications(
  filter: NotificationFilter,
  pageSize: number,
): Promise<ListPage<NotificationItem>> {
  return apiList<NotificationItem>(LIST_PATH, { ...filter, page_size: pageSize })
}

/**
 * One page of the inbox, with the server's clock when it answered (its `Date`
 * header), for comparing the items' timestamps against: the viewer's own clock
 * may be minutes off the server's.
 */
export async function listNotificationsAt(
  filter: NotificationFilter,
  pageSize: number,
): Promise<{ items: NotificationItem[]; at: number }> {
  const res = await apiFetch(LIST_PATH, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...filter, page_size: pageSize }),
  })
  const page = await readJSON<Partial<ListPage<NotificationItem>>>(res, LIST_PATH)
  const at = Date.parse(res.headers.get('Date') ?? '')
  return { items: page.items ?? [], at: Number.isNaN(at) ? Date.now() : at }
}

/** Mark one item read for this viewer. Answers the item as they now see it. */
export function markNotificationRead(id: string): Promise<NotificationItem> {
  return apiJSON<NotificationItem>(itemPath(id), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ read: true }),
  })
}

/**
 * Run an invoke action. Answers the item as the viewer now sees it, or null
 * when the row has since left what they may read (the route's 204).
 */
export async function runNotificationAction(
  id: string,
  actionId: string,
): Promise<NotificationItem | null> {
  const path = `${itemPath(id)}/actions/${encodeURIComponent(actionId)}`
  const res = await apiFetch(path, { method: 'POST' })
  return res.status === 204 ? null : readJSON<NotificationItem>(res, path)
}
