/** The tasks resource's list read. Filters travel in the body, so every task
 *  surface is one filter set over this single address — there is no second
 *  spelling that answers the same question differently. */
export const TASK_LIST_PATH = '/api/tasks/list'

/** The keys the server will order a lane by. There is deliberately no
 *  'default' member: the default order is what you get by sending no
 *  `sort_key` at all, so naming it would be a second spelling of absent. */
export type TaskSortKey = 'title' | 'created' | 'event_type' | 'claimee'
export type TaskSortDir = 'asc' | 'desc'

/** The filter and sort fields a lane adds to its list body.
 *
 *  Every one of them runs SERVER-side, and that is the point: a lane holds a
 *  page, not the column, so a client-side pass can only narrow what it already
 *  fetched — a match sitting in an unfetched page is invisible, and the `N of
 *  M` tail would count an unfiltered lane above filtered rows. */
export type TaskListFilters = {
  /** Case-insensitive substring over the four fields the card shows: the
   *  entity's title and source id, the task's summary and event type. Matched
   *  literally — a '%' is a percent sign. Capped at 200 characters. */
  search?: string
  sources?: string[]
  event_types?: string[]
  created_since?: string
  created_before?: string
  sort_key?: TaskSortKey
  /** Only meaningful alongside `sort_key` — the default order has no
   *  direction, and the server refuses a lone `sort_dir` rather than ignoring
   *  it. Absent with a key means 'desc'. */
  sort_dir?: TaskSortDir
}

/** The body of POST /api/tasks/list, named field by field so a lane's query is
 *  checkable at the call site rather than at the server. */
export type TaskListRequest = TaskListFilters & {
  statuses?: string[]
  team_ids?: string[]
  only_unclaimed?: boolean
  include_snoozed?: boolean
  closed_since?: string
  page_size?: number
  page_token?: string
}

/** How many rows a task surface asks for at once. 200 is the server's cap; a
 *  larger page is a 400, not a silent truncation. The board and the deck both
 *  take the whole cap because their columns are meant to be scanned, and page
 *  past it through the token when a column is genuinely deeper. */
export const TASK_PAGE_SIZE = 200

/** How far back the board's Done column looks. The server used to apply this
 *  window itself, invisibly; now the surface that wants it asks for it. */
const DONE_WINDOW_DAYS = 7

/** The pickable-right-now projection: the triage deck, and the board's Queued
 *  column. Unclaimed, still queued, not sleeping — the exact set the queue
 *  view has always shown.
 *
 *  `showSnoozed` is the board's "show snoozed" toggle: it widens the lanes to
 *  include the snoozed one and keeps rows that are still inside their snooze
 *  window, which the server orders behind the live ones. */
export function queueListBody(
  teamIds: string[],
  showSnoozed = false,
  filters: TaskListFilters = {},
): TaskListRequest {
  return {
    statuses: showSnoozed ? ['queued', 'snoozed'] : ['queued'],
    team_ids: teamIds,
    only_unclaimed: true,
    include_snoozed: showSnoozed,
    page_size: TASK_PAGE_SIZE,
    ...filters,
  }
}

/** The queue's DEPTH: the same filter set the Queued column renders, with the
 *  page removed. `page_size: 0` is the count-only read — no items, the total
 *  under those filters — which is what a count of rows is on this API, and
 *  building it from queueListBody is what keeps the shell rail's number and
 *  the column it names from drifting apart.
 *
 *  No team narrowing, matching the board's own default: the viewer's whole
 *  visible set under RLS. */
export function queueCountBody(): TaskListRequest {
  return { ...queueListBody([]), page_size: 0 }
}

/** One lane of the board by status. `claimed` is the claim axis rather than a
 *  lifecycle status — a queued task someone has taken — and is why the board's
 *  Claimed column can't be spelled as a plain status. */
export function statusListBody(
  status: string,
  teamIds: string[],
  filters: TaskListFilters = {},
): TaskListRequest {
  return {
    statuses: [status],
    team_ids: teamIds,
    include_snoozed: true,
    page_size: TASK_PAGE_SIZE,
    ...filters,
  }
}

/** The board's Done column: closed work, windowed to the last week. */
export function doneListBody(teamIds: string[], filters: TaskListFilters = {}): TaskListRequest {
  const since = new Date(Date.now() - DONE_WINDOW_DAYS * 24 * 60 * 60 * 1000)
  return {
    ...statusListBody('done', teamIds, filters),
    closed_since: since.toISOString(),
  }
}
