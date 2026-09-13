import type { TaskListFilters, TaskSortDir, TaskSortKey } from '../../lib/taskList'
import type { TaskSource } from '../../types'

// The per-lane filter state the board's masthead controls read and write —
// the search field and the sort-and-filter panel. It is the reader's own
// narrowing of a lane, and every field of it runs SERVER-side: a lane holds
// one page, so a client-side pass could only narrow what it had already
// fetched, hiding a match that sits on an unfetched page with no affordance
// left to reach it. laneFilterFields is the one translation into the list
// body, so a lane's query is checkable at the call site.
//
// Its own file rather than BoardColumn.tsx so React Fast Refresh stays happy:
// a component file exports only components.

export type SourceFilter = 'all' | TaskSource
// 'default' is the server's own order — attention first for the run lanes,
// priority for the queue — which is what you get by sending no sort_key at
// all. Naming it here is what makes picking a sort an explicit override, never
// the thing that silently reshuffles a freshly-loaded board.
export type SortKey = 'default' | TaskSortKey

export type LaneFilter = {
  search: string
  source: SourceFilter
  // Selected event types; empty = all.
  eventTypes: string[]
  // Created-date window, as the popover's datetime-local strings
  // ("2026-06-01T12:00", read on the reader's own clock); empty = unbounded on
  // that side. `after` keeps tasks created at or after it, `before` at or
  // before.
  after: string
  before: string
  sortKey: SortKey
  sortDir: TaskSortDir
}

export const emptyLaneFilter: LaneFilter = {
  search: '',
  source: 'all',
  eventTypes: [],
  after: '',
  before: '',
  sortKey: 'default',
  sortDir: 'desc',
}

// filterIsActive — true when anything diverges from the pristine defaults.
// Drives the active dot on the filter button. Search is deliberately excluded
// (it has its own always-visible field, so it doesn't need the dot to announce
// it).
export function filterIsActive(f: LaneFilter): boolean {
  return (
    f.source !== 'all' ||
    f.eventTypes.length > 0 ||
    f.after !== '' ||
    f.before !== '' ||
    f.sortKey !== 'default'
  )
}

// narrows — true when the reader has asked the lane any question at all,
// search included. An empty lane under a narrowing has no matches, which is
// not the same thing as an empty lane, and the empty state says which.
export function narrows(f: LaneFilter): boolean {
  return filterIsActive(f) || f.search.trim() !== ''
}

export const SORT_LABEL: Record<SortKey, string> = {
  default: 'Smart',
  created: 'Newest',
  title: 'Title',
  event_type: 'Event type',
  claimee: 'Claimee',
}

export const SOURCE_OPTIONS: { value: SourceFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'github', label: 'GitHub' },
  { value: 'jira', label: 'Jira' },
  { value: 'slack', label: 'Slack' },
]

// The server's cap on `search`. The field stops the reader there so a long
// paste is cut where the server would refuse it, rather than answered with a
// 400 after the fact.
export const SEARCH_MAX_CHARS = 200

// laneFilterFields is the filter as the list body carries it. Absent fields
// are left absent rather than sent empty: the server reads an empty
// `event_types` as no narrowing, but a lone `sort_dir` is refused, and a
// `search` of spaces would fingerprint a page token differently from none.
export function laneFilterFields(f: LaneFilter): TaskListFilters {
  const out: TaskListFilters = {}
  const search = f.search.trim()
  if (search) out.search = search
  if (f.source !== 'all') out.sources = [f.source]
  if (f.eventTypes.length > 0) out.event_types = f.eventTypes
  const since = instantOf(f.after)
  if (since) out.created_since = since
  const before = instantOf(f.before)
  if (before) out.created_before = before
  if (f.sortKey !== 'default') {
    out.sort_key = f.sortKey
    out.sort_dir = f.sortDir
  }
  return out
}

// instantOf turns a datetime-local string into the RFC3339 instant the server
// compares against. The string carries no zone, so it is read on the reader's
// own clock — the bound they meant. Empty or unparseable is no bound.
function instantOf(local: string): string | undefined {
  if (!local) return undefined
  const ms = Date.parse(local)
  if (Number.isNaN(ms)) return undefined
  return new Date(ms).toISOString()
}
