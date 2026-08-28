import type { ListRequest } from './apiClient'

/** The tasks resource's list read. Filters travel in the body, so every task
 *  surface is one filter set over this single address — there is no second
 *  spelling that answers the same question differently. */
export const TASK_LIST_PATH = '/api/tasks/list'

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
export function queueListBody(teamIds: string[], showSnoozed = false): ListRequest {
  return {
    statuses: showSnoozed ? ['queued', 'snoozed'] : ['queued'],
    team_ids: teamIds,
    only_unclaimed: true,
    include_snoozed: showSnoozed,
    page_size: TASK_PAGE_SIZE,
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
export function queueCountBody(): ListRequest {
  return { ...queueListBody([]), page_size: 0 }
}

/** One lane of the board by status. `claimed` is the claim axis rather than a
 *  lifecycle status — a queued task someone has taken — and is why the board's
 *  Claimed column can't be spelled as a plain status. */
export function statusListBody(status: string, teamIds: string[]): ListRequest {
  return {
    statuses: [status],
    team_ids: teamIds,
    include_snoozed: true,
    page_size: TASK_PAGE_SIZE,
  }
}

/** The board's Done column: closed work, windowed to the last week. */
export function doneListBody(teamIds: string[]): ListRequest {
  const since = new Date(Date.now() - DONE_WINDOW_DAYS * 24 * 60 * 60 * 1000)
  return {
    ...statusListBody('done', teamIds),
    closed_since: since.toISOString(),
  }
}
