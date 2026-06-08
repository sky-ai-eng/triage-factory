import type { Task } from '../../types'

// The per-column sort + filter engine. Lives in its own file (not
// BoardColumn.tsx) so React Fast Refresh stays happy — the rule is "a
// component file exports only components." The board's controls (the sticky
// in-column search + the filter popover) read and write ColumnFilterState;
// applyColumnFilter is the single pass every column runs so behaviour stays
// uniform.

export type SourceFilter = 'all' | 'github' | 'jira'
// Sort keys the column popover offers. 'default' preserves the order the board
// hands us (priority for the queue, run-attention for in-progress/in-review) —
// the smart baseline — so picking a sort is an explicit override, never the
// thing that silently reshuffles a freshly-loaded board.
export type SortKey = 'default' | 'title' | 'created' | 'event_type' | 'claimee'
export type SortDir = 'asc' | 'desc'

export type ColumnFilterState = {
  search: string
  source: SourceFilter
  // Selected event types; empty = all. Replaces the old single-chip search.
  eventTypes: string[]
  // Created-date window. ISO-ish datetime-local strings ("2026-06-01T12:00"),
  // empty = unbounded on that side. `after` keeps tasks created at/after it;
  // `before` keeps tasks created at/before it.
  after: string
  before: string
  sortKey: SortKey
  sortDir: SortDir
}

export const emptyFilter: ColumnFilterState = {
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
export function filterIsActive(f: ColumnFilterState): boolean {
  return (
    f.source !== 'all' ||
    f.eventTypes.length > 0 ||
    f.after !== '' ||
    f.before !== '' ||
    f.sortKey !== 'default'
  )
}

export const SORT_LABEL: Record<SortKey, string> = {
  default: 'Smart',
  created: 'Newest',
  title: 'Title',
  event_type: 'Event type',
  claimee: 'Claimee',
}

export interface ApplyOpts {
  // Resolve a task's claimee to a display string for the claimee sort. Returns
  // '' for unclaimed; those are pushed to the end regardless of direction.
  resolveClaimee?: (t: Task) => string
}

// applyColumnFilter runs the filter predicates, then (only when the user has
// picked a non-default sort) reorders. Returns the input array untouched when
// nothing applies, so the caller's useMemo can stay cheap.
export function applyColumnFilter(
  tasks: Task[],
  f: ColumnFilterState,
  opts: ApplyOpts = {},
): Task[] {
  let items = tasks

  if (f.source !== 'all') {
    items = items.filter((t) => t.source === f.source)
  }

  if (f.eventTypes.length > 0) {
    const set = new Set(f.eventTypes)
    items = items.filter((t) => set.has(t.event_type))
  }

  if (f.after) {
    const a = Date.parse(f.after)
    if (!Number.isNaN(a)) {
      items = items.filter((t) => {
        const ts = Date.parse(t.created_at)
        return !Number.isNaN(ts) && ts >= a
      })
    }
  }

  if (f.before) {
    const b = Date.parse(f.before)
    if (!Number.isNaN(b)) {
      items = items.filter((t) => {
        const ts = Date.parse(t.created_at)
        return !Number.isNaN(ts) && ts <= b
      })
    }
  }

  const q = f.search.trim().toLowerCase()
  if (q) {
    items = items.filter(
      (t) =>
        t.title.toLowerCase().includes(q) ||
        t.source_id.toLowerCase().includes(q) ||
        (t.ai_summary ?? '').toLowerCase().includes(q) ||
        t.event_type.toLowerCase().includes(q),
    )
  }

  if (f.sortKey !== 'default') {
    const dir = f.sortDir === 'asc' ? 1 : -1
    const resolve = opts.resolveClaimee ?? (() => '')
    items = [...items].sort((a, b) => {
      switch (f.sortKey) {
        case 'title':
          return a.title.localeCompare(b.title) * dir
        case 'created':
          return (Date.parse(a.created_at) - Date.parse(b.created_at)) * dir
        case 'event_type':
          return a.event_type.localeCompare(b.event_type) * dir
        case 'claimee': {
          const an = resolve(a)
          const bn = resolve(b)
          // Unclaimed always sinks to the bottom, whichever way we're sorting —
          // an empty claimee is "nobody", not a name that should lead asc.
          if (!an && !bn) return 0
          if (!an) return 1
          if (!bn) return -1
          return an.localeCompare(bn) * dir
        }
        default:
          return 0
      }
    })
  }

  return items
}
