import { apiJSON, apiList, apiListAt, type ListPage } from './apiClient'

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
  /** On an invoke: what to show in the label's place while the call is out. */
  running_label?: string
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
  /** Why the row resolved, in a sentence. null while it is open. */
  resolution: string | null
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

/** One page of the inbox, with the server's clock when it answered. */
export function listNotificationsAt(
  filter: NotificationFilter,
  pageSize: number,
): Promise<{ page: ListPage<NotificationItem>; at: number }> {
  return apiListAt<NotificationItem>(LIST_PATH, { ...filter, page_size: pageSize })
}

/** Mark one item read for this viewer. Answers the item as they now see it. */
export function markNotificationRead(id: string): Promise<NotificationItem> {
  return apiJSON<NotificationItem>(itemPath(id), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ read: true }),
  })
}

/** What an invoke action did: its outcome, in a sentence, and the item as the
 *  viewer now sees it, null once the row has left what they may read. */
export interface NotificationActionResult {
  notification: NotificationItem | null
  outcome: string
}

/** Run an invoke action. */
export function runNotificationAction(
  id: string,
  actionId: string,
): Promise<NotificationActionResult> {
  return apiJSON<NotificationActionResult>(
    `${itemPath(id)}/actions/${encodeURIComponent(actionId)}`,
    { method: 'POST' },
  )
}
