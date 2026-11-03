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
  /** Whether this viewer is in the row's audience. False only on a team's row
   *  an org admin reads without being sent it. */
  sent: boolean
}

/** The inbox's filters. An absent field does not filter. */
export interface NotificationFilter {
  status?: 'open' | 'resolved'
  dismissed?: boolean
  unread?: boolean
  priority_in?: NotificationPriority[]
  scope_in?: NotificationScope[]
}

/** A list's order. Absent, a list orders by priority, then the latest
 *  escalation. Sorted by resolved_at, open rows trail in either direction. */
export interface NotificationSort {
  sort_key: 'resolved_at'
  /** Absent means desc. */
  sort_dir?: 'asc' | 'desc'
}

/** The viewer's inbox. */
export const NOTIFICATIONS_LIST_PATH = '/api/me/notifications/list'

/** Every notification in an org, team rows included: an org admin's All
 *  teams view. */
export function orgNotificationsListPath(orgID: string): string {
  return `/api/orgs/${encodeURIComponent(orgID)}/notifications/list`
}

/** The actions this viewer can use, in the row's order: a link with somewhere
 *  to go, or an invoke they may run. One they may not is left out rather than
 *  offered and refused. */
export function usableActions(n: NotificationItem): NotificationAction[] {
  return n.actions.filter((a) => (a.type === 'link' ? !!a.href : a.allowed === true))
}

/** Go where a link action points. An external destination (a GitHub or Jira
 *  page) opens beside the app rather than in place of it. */
export function followNotificationLink(href: string, navigate: (to: string) => void): void {
  if (isExternalHref(href)) window.open(href, '_blank', 'noopener')
  else navigate(href)
}

export function isExternalHref(href: string): boolean {
  return /^https?:\/\//i.test(href)
}

function itemPath(id: string): string {
  return `/api/me/notifications/${encodeURIComponent(id)}`
}

/** Where an item opens in the app: the notifications page, on that item. The
 *  org prefix is the caller's (useOrgHref). */
export function notificationPath(id: string): string {
  return `/notifications?id=${encodeURIComponent(id)}`
}

/** One page of the inbox; `pageSize` 0 is the count-only read. */
export function listNotifications(
  filter: NotificationFilter,
  pageSize: number,
  sort?: NotificationSort,
): Promise<ListPage<NotificationItem>> {
  return apiList<NotificationItem>(NOTIFICATIONS_LIST_PATH, {
    ...filter,
    ...sort,
    page_size: pageSize,
  })
}

/** One page of the inbox, with the server's clock when it answered. */
export function listNotificationsAt(
  filter: NotificationFilter,
  pageSize: number,
): Promise<{ page: ListPage<NotificationItem>; at: number }> {
  return apiListAt<NotificationItem>(NOTIFICATIONS_LIST_PATH, { ...filter, page_size: pageSize })
}

/** Every notification in the org this org admin may read, team rows
 *  included: the All teams view. */
export function listOrgNotifications(
  orgID: string,
  filter: NotificationFilter,
  pageSize: number,
  sort?: NotificationSort,
): Promise<ListPage<NotificationItem>> {
  return apiList<NotificationItem>(orgNotificationsListPath(orgID), {
    ...filter,
    ...sort,
    page_size: pageSize,
  })
}

/** One item this viewer may read. A 404 is an HttpError: gone, or not theirs. */
export function getNotification(id: string): Promise<NotificationItem> {
  return apiJSON<NotificationItem>(itemPath(id))
}

/** This viewer's marks. An absent mark is kept. Answers the item as they now
 *  see it; dismissing one that is not dismissible is a 422. */
export function setNotificationMarks(
  id: string,
  marks: { read?: boolean; dismissed?: boolean },
  init: { keepalive?: boolean } = {},
): Promise<NotificationItem> {
  return apiJSON<NotificationItem>(itemPath(id), {
    ...init,
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(marks),
  })
}

/** Mark one item read for this viewer. Answers the item as they now see it. */
export function markNotificationRead(id: string): Promise<NotificationItem> {
  return setNotificationMarks(id, { read: true })
}

/** The most ids one mark-read or dismiss call takes: the largest page a
 *  client renders. */
export const BULK_MARKS_MAX = 200

function postIDs(
  path: string,
  ids: string[],
  init: { keepalive?: boolean },
): Promise<{ ids: string[] }> {
  return apiJSON<{ ids: string[] }>(path, {
    ...init,
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  })
}

/** Mark the rendered items read, at most BULK_MARKS_MAX of them. Answers the
 *  ids it marked; one the viewer cannot read is left out. */
export function markNotificationsRead(
  ids: string[],
  init: { keepalive?: boolean } = {},
): Promise<{ ids: string[] }> {
  return postIDs('/api/me/notifications/mark-read', ids, init)
}

/** Dismiss the selected items for this viewer, at most BULK_MARKS_MAX of
 *  them. Answers the ids it dismissed; one the viewer cannot read is left out.
 *  One that is not dismissible refuses the whole call with a 422. */
export function dismissNotifications(
  ids: string[],
  init: { keepalive?: boolean } = {},
): Promise<{ ids: string[] }> {
  return postIDs('/api/me/notifications/dismiss', ids, init)
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
