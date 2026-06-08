import { useMemo, useState } from 'react'
import { useDroppable } from '@dnd-kit/core'
import { motion, useReducedMotion } from 'motion/react'
import { ArrowDown, ArrowUp, Search, SlidersHorizontal, X } from 'lucide-react'
import EventBadge from '../EventBadge'
import { bodyEase } from '../../pages/setup/glassStyle'
import {
  emptyFilter,
  filterIsActive,
  SORT_LABEL,
  type ColumnFilterState,
  type SortKey,
  type SourceFilter,
} from './columnFilter'
import type { Task } from '../../types'

// BoardColumn is the board's column shell, redressed in the liquid-glass
// material the setup/settings flows established: a frosted slab (surface-overlay
// over a glass edge, backdrop-blurred) that fills to the bottom of the page and
// floats on the ambient GlassBackdrop. Two glow layers ride over it —
//   • an ambient breathing glow when an autonomous run is live in this lane
//     (you see *where* the factory is working at a glance), and
//   • the drag-over receive glow that ramps in under bodyEase.
// Both are non-clipping overlay siblings so their bloom spills past the slab.
//
// Search + structured filters live in a sticky frosted header *inside* the
// scroll body (cards slide under it). The search field is always visible; the
// sliders button *melts the header downward* — the filter criteria (sort,
// source, event-type, created-date range) animate open in an expanding panel
// glued beneath the search row, and clicking again shrinks it back. No detached
// popover. Filter state is owned by the parent (Board.tsx) and persisted per
// column; the apply/sort pass lives in ./columnFilter.ts so this file exports
// only a component (React Fast Refresh constraint).

const searchInputClass =
  'w-full rounded-xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 py-1.5 pl-8 pr-7 text-[12px] text-text-primary placeholder:text-text-tertiary backdrop-blur-md outline-none transition-[border-color,background-color,box-shadow] focus:border-accent/40 focus:bg-[var(--color-surface-overlay)] focus:shadow-[0_0_0_3px_var(--color-accent-soft)]'

const dateInputClass =
  'w-full rounded-lg border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 px-2 py-1 text-[11px] text-text-primary outline-none transition-colors focus:border-accent/40'

interface Props {
  id: string
  title: string
  totalCount: number
  filteredCount: number
  tasks: Task[] // raw list, used to populate the event-type chips
  filter: ColumnFilterState
  onFilterChange: (next: ColumnFilterState) => void
  // Optional header slot for column-specific affordances (e.g. the
  // snoozed toggle on Queued, "see more" on Done). Renders to the
  // right of the title/count.
  headerExtra?: React.ReactNode
  // Position in the row (0-based) — drives the staggered mount reveal.
  index?: number
  // True when ≥1 autonomous run is actively turning in this lane — lights the
  // ambient breathing glow.
  active?: boolean
  children: React.ReactNode
}

export default function BoardColumn({
  id,
  title,
  totalCount,
  filteredCount,
  tasks,
  filter,
  onFilterChange,
  headerExtra,
  index = 0,
  active = false,
  children,
}: Props) {
  const { setNodeRef, isOver } = useDroppable({ id, data: { type: 'column' } })
  const reduce = !!useReducedMotion()

  const [controlsOpen, setControlsOpen] = useState(false)

  // Event-type chips reflect the unfiltered set so the user always sees what
  // types exist in this column, even after filtering them out.
  const eventTypes = useMemo(() => {
    const seen = new Set<string>()
    for (const t of tasks) {
      if (t.event_type) seen.add(t.event_type)
    }
    return Array.from(seen).sort()
  }, [tasks])

  const showFilteredCount = filteredCount !== totalCount
  const hasFilters = filterIsActive(filter)

  // SKY-330: fixed 520px width — the board is horizontally scrollable, so a
  // viewport-relative width would compress cards as columns scroll into view.
  return (
    <motion.div
      className="flex h-full w-[520px] shrink-0 flex-col"
      initial={reduce ? false : { opacity: 0, y: 14 }}
      animate={{ opacity: 1, y: 0 }}
      transition={reduce ? { duration: 0 } : { ...bodyEase, delay: index * 0.05 }}
    >
      <div className="mb-3 flex items-center justify-between px-1">
        <div className="flex items-center gap-2">
          <h2 className="text-[13px] font-medium tracking-tight text-text-secondary">{title}</h2>
          <span className="rounded-full border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 px-2 py-0.5 text-[11px] text-text-tertiary backdrop-blur-md">
            {showFilteredCount ? `${filteredCount}/${totalCount}` : totalCount}
          </span>
        </div>
        {headerExtra}
      </div>

      {/* Droppable wrapper does NOT clip — so the glow overlays' bloom spills
          past the slab edge. */}
      <div ref={setNodeRef} className="relative min-h-0 flex-1">
        <div className="h-full overflow-y-auto rounded-3xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/50 backdrop-blur-xl">
          {/* Sticky frosted control header — cards scroll beneath it. The
              filter panel melts down from here on demand. */}
          <div className="sticky top-0 z-10 border-b border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/70 px-3 pb-2.5 pt-3 backdrop-blur-md">
            <div className="flex items-center gap-2">
              <div className="relative flex-1">
                <Search
                  size={13}
                  aria-hidden
                  className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-text-tertiary"
                />
                <input
                  type="text"
                  placeholder="Search…"
                  value={filter.search}
                  onChange={(e) => onFilterChange({ ...filter, search: e.target.value })}
                  className={searchInputClass}
                />
                {filter.search && (
                  <button
                    type="button"
                    aria-label="Clear search"
                    onClick={() => onFilterChange({ ...filter, search: '' })}
                    className="absolute right-2 top-1/2 -translate-y-1/2 text-text-tertiary transition-colors hover:text-text-secondary"
                  >
                    <X size={12} />
                  </button>
                )}
              </div>
              <button
                type="button"
                aria-label="Sort & filter"
                aria-expanded={controlsOpen}
                onClick={() => setControlsOpen((v) => !v)}
                className={`relative shrink-0 rounded-xl border bg-[var(--color-surface-overlay)]/60 p-1.5 backdrop-blur-md transition-colors ${
                  controlsOpen || hasFilters
                    ? 'border-accent/30 text-accent'
                    : 'border-[var(--color-border-glass)] text-text-tertiary hover:text-text-secondary'
                }`}
              >
                <SlidersHorizontal size={14} />
                {hasFilters && (
                  <span
                    aria-hidden
                    className="absolute -right-0.5 -top-0.5 h-1.5 w-1.5 rounded-full bg-accent"
                  />
                )}
              </button>
            </div>

            {/* The melt-down: height + opacity animate from the search row,
                concentrating the filter criteria in the extended bar. */}
            <motion.div
              initial={false}
              animate={{ height: controlsOpen ? 'auto' : 0, opacity: controlsOpen ? 1 : 0 }}
              transition={reduce ? { duration: 0 } : bodyEase}
              className="overflow-hidden"
            >
              <div className="space-y-3 pt-3">
                <FilterControls filter={filter} onChange={onFilterChange} eventTypes={eventTypes} />
              </div>
            </motion.div>
          </div>

          <div className="space-y-3 px-3 pb-3 pt-2">{children}</div>
        </div>

        {/* Ambient "work is live here" glow — a slow breathing ring, suppressed
            while the receive glow is showing. Ring/bloom only (no fill), so it
            never washes the cards. */}
        <motion.div
          aria-hidden
          className="pointer-events-none absolute inset-0 rounded-3xl"
          initial={false}
          animate={{ opacity: isOver ? 0 : active ? (reduce ? 0.22 : [0, 0.45, 0]) : 0 }}
          transition={
            isOver || !active || reduce
              ? bodyEase
              : { duration: 4.5, repeat: Infinity, ease: 'easeInOut' }
          }
          style={{
            boxShadow: 'inset 0 0 0 1px var(--color-accent), 0 0 38px -14px var(--color-accent)',
          }}
        />

        {/* Drag-over receive glow — the target lane breathes toward you. */}
        <motion.div
          aria-hidden
          className="pointer-events-none absolute inset-0 rounded-3xl"
          initial={false}
          animate={{ opacity: isOver ? 1 : 0 }}
          transition={reduce ? { duration: 0 } : bodyEase}
          style={{
            background: 'var(--color-accent-soft)',
            boxShadow: 'inset 0 0 0 1px var(--color-accent), 0 24px 60px -24px var(--color-accent)',
          }}
        />
      </div>
    </motion.div>
  )
}

const SORT_KEYS: SortKey[] = ['default', 'created', 'title', 'event_type', 'claimee']
const SOURCES: { value: SourceFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'github', label: 'GitHub' },
  { value: 'jira', label: 'Jira' },
]

function pill(selected: boolean): string {
  return `rounded-full px-2.5 py-1 text-[11px] font-medium transition-colors ${
    selected
      ? 'bg-accent/[0.14] text-accent'
      : 'border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 text-text-tertiary hover:text-text-secondary'
  }`
}

function Group({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <p className="text-[10px] font-medium uppercase tracking-wide text-text-tertiary">{label}</p>
      <div className="flex flex-wrap items-center gap-1.5">{children}</div>
    </div>
  )
}

// FilterControls is the sort+filter body that melts open inside the column
// header. Controlled — every mutation flows straight back through onChange.
function FilterControls({
  filter,
  onChange,
  eventTypes,
}: {
  filter: ColumnFilterState
  onChange: (next: ColumnFilterState) => void
  eventTypes: string[]
}) {
  // Empty eventTypes = "all on". The first click off the all-on state switches
  // to an explicit set of everything-but-this; re-selecting every type collapses
  // back to the empty (all) sentinel.
  const allOn = filter.eventTypes.length === 0
  const isOn = (et: string) => allOn || filter.eventTypes.includes(et)
  const toggleEvent = (et: string) => {
    if (allOn) {
      onChange({ ...filter, eventTypes: eventTypes.filter((x) => x !== et) })
      return
    }
    const has = filter.eventTypes.includes(et)
    const next = has ? filter.eventTypes.filter((x) => x !== et) : [...filter.eventTypes, et]
    onChange({ ...filter, eventTypes: next.length === eventTypes.length ? [] : next })
  }

  return (
    <>
      <Group label="Sort">
        {SORT_KEYS.map((k) => (
          <button
            key={k}
            type="button"
            onClick={() => onChange({ ...filter, sortKey: k })}
            className={pill(filter.sortKey === k)}
          >
            {SORT_LABEL[k]}
          </button>
        ))}
        {filter.sortKey !== 'default' && (
          <button
            type="button"
            aria-label={`Direction: ${filter.sortDir === 'asc' ? 'ascending' : 'descending'}`}
            onClick={() =>
              onChange({ ...filter, sortDir: filter.sortDir === 'asc' ? 'desc' : 'asc' })
            }
            className={`inline-flex items-center gap-1 ${pill(false)}`}
          >
            {filter.sortDir === 'asc' ? <ArrowUp size={11} /> : <ArrowDown size={11} />}
            {filter.sortDir === 'asc' ? 'Asc' : 'Desc'}
          </button>
        )}
      </Group>

      <Group label="Source">
        {SOURCES.map((s) => (
          <button
            key={s.value}
            type="button"
            onClick={() => onChange({ ...filter, source: s.value })}
            className={pill(filter.source === s.value)}
          >
            {s.label}
          </button>
        ))}
      </Group>

      {eventTypes.length > 0 && (
        <Group label="Event type">
          {eventTypes.map((et) => (
            <button
              key={et}
              type="button"
              aria-pressed={isOn(et)}
              onClick={() => toggleEvent(et)}
              title={et}
              // Selection is an adaptation of the badge itself — off types dim
              // and desaturate rather than wearing a ring.
              className={`rounded transition-[opacity,filter] duration-200 ${
                isOn(et) ? '' : 'opacity-40 grayscale'
              }`}
            >
              <EventBadge eventType={et} compact />
            </button>
          ))}
        </Group>
      )}

      <Group label="Created">
        <div className="flex w-full items-end gap-2">
          <label className="flex-1">
            <span className="mb-1 block text-[10px] text-text-tertiary">After</span>
            <input
              type="datetime-local"
              value={filter.after}
              max={filter.before || undefined}
              onChange={(e) => onChange({ ...filter, after: e.target.value })}
              style={{ colorScheme: 'light dark' }}
              className={dateInputClass}
            />
          </label>
          <label className="flex-1">
            <span className="mb-1 block text-[10px] text-text-tertiary">Before</span>
            <input
              type="datetime-local"
              value={filter.before}
              min={filter.after || undefined}
              onChange={(e) => onChange({ ...filter, before: e.target.value })}
              style={{ colorScheme: 'light dark' }}
              className={dateInputClass}
            />
          </label>
        </div>
        {(filter.after || filter.before) && (
          <button
            type="button"
            onClick={() => onChange({ ...filter, after: '', before: '' })}
            className="text-[11px] text-text-tertiary transition-colors hover:text-text-secondary"
          >
            Clear dates
          </button>
        )}
      </Group>

      <div className="flex justify-end pt-1">
        <button
          type="button"
          disabled={!filterIsActive(filter)}
          onClick={() => onChange({ ...emptyFilter, search: filter.search })}
          className="text-[11px] text-text-tertiary transition-colors hover:text-text-secondary disabled:opacity-40"
        >
          Reset filters
        </button>
      </div>
    </>
  )
}
