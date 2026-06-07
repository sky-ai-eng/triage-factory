import { useMemo } from 'react'
import { useDroppable } from '@dnd-kit/core'
import { motion, useReducedMotion } from 'motion/react'
import EventBadge from '../EventBadge'
import { bodyEase } from '../../pages/setup/glassStyle'
import { segmentedWrap, segmentedBtn } from '../../pages/settings/primitives'
import type { Task } from '../../types'
import type { ColumnFilterState } from './columnFilter'

// BoardColumn is SKY-330's reusable column shell, redressed in the liquid-glass
// material the setup/settings flows established: a frosted slab
// (surface-overlay over a glass edge, backdrop-blurred) floating on the page's
// ambient GlassBackdrop, with the header + filter lifted out of the scroll body
// so they read as chrome above the glass rather than content inside it. The
// drag-over affordance is a soft accent glow that ramps in under bodyEase (an
// overlay sibling, so its outer bloom isn't clipped by the slab's scroll) — the
// column breathes toward you instead of flipping a hard border.
//
// Filter state is owned by the parent so each column has its own independent
// filter context. localStorage persistence happens at the parent level
// (Board.tsx) keyed by columnId. Filter helpers (emptyFilter, applyColumnFilter)
// live in ./columnFilter.ts so this file exports only components (React Fast
// Refresh constraint).

// Compact glass field for the per-column search — the same frosted material as
// glassInputClass (settings/primitives), dialled down to the column header's
// tighter type scale rather than the setup flow's roomy py-3 inputs.
const columnSearchClass =
  'w-full rounded-xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 px-3 py-1.5 text-[12px] text-text-primary placeholder:text-text-tertiary backdrop-blur-md outline-none transition-[border-color,background-color,box-shadow] focus:border-accent/40 focus:bg-[var(--color-surface-overlay)] focus:shadow-[0_0_0_3px_var(--color-accent-soft)]'

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
  // Position in the row (0-based) — drives the staggered mount reveal so the
  // five columns settle in sequence rather than popping in together.
  index?: number
  // Column width is hard-coded in the JSX below (520px) so all five
  // columns line up under the horizontal-scroll layout; expose a
  // width prop later if a column needs to breathe differently.
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
  children,
}: Props) {
  const { setNodeRef, isOver } = useDroppable({ id, data: { type: 'column' } })
  const reduce = !!useReducedMotion()

  // Event-type chips reflect the unfiltered set so the user always
  // sees what types exist in this column, even after filtering them
  // out. Matches the old queue sidebar's chip behavior.
  const eventTypes = useMemo(() => {
    const seen = new Set<string>()
    for (const t of tasks) {
      if (t.event_type) seen.add(t.event_type)
    }
    return Array.from(seen)
  }, [tasks])

  const showFilteredCount = filteredCount !== totalCount

  // SKY-330: column width matches the pre-redesign grid-cols-3
  // column. Fixed width here rather than fluid because the board
  // is horizontally scrollable — a viewport-relative width would
  // compress cards as the user scrolls to reveal additional columns.
  // 520px gives AgentCard log content room to breathe (it was the
  // workhorse card pre-redesign at ~480-520px on typical viewports).
  return (
    <motion.div
      className="flex flex-col w-[520px] shrink-0"
      initial={reduce ? false : { opacity: 0, y: 14 }}
      animate={{ opacity: 1, y: 0 }}
      transition={reduce ? { duration: 0 } : { ...bodyEase, delay: index * 0.05 }}
    >
      <div className="flex items-center justify-between mb-3 px-1">
        <div className="flex items-center gap-2">
          <h2 className="text-[13px] font-medium tracking-tight text-text-secondary">{title}</h2>
          <span className="rounded-full border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 px-2 py-0.5 text-[11px] text-text-tertiary backdrop-blur-md">
            {showFilteredCount ? `${filteredCount}/${totalCount}` : totalCount}
          </span>
        </div>
        {headerExtra}
      </div>

      {/* Filter strip — collapses into a one-line search by default. */}
      <ColumnFilter filter={filter} onChange={onFilterChange} eventTypes={eventTypes} />

      {/* Droppable wrapper does NOT clip — so the drag-over glow's outer bloom
          spills past the slab edge. The frosted scroll slab and the glow
          overlay are siblings inside it, sharing the same rounded bounds. */}
      <div ref={setNodeRef} className="relative flex-1">
        <div className="h-full max-h-[calc(100vh-220px)] space-y-3 overflow-y-auto rounded-3xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/50 p-3 backdrop-blur-xl">
          {children}
        </div>
        <motion.div
          aria-hidden
          // Match the slab's exact computed height (same h-full + max-h) so the
          // glow tracks it on every viewport rather than overrunning when the
          // slab is height-capped.
          className="pointer-events-none absolute left-0 right-0 top-0 h-full max-h-[calc(100vh-220px)] rounded-3xl"
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

function ColumnFilter({
  filter,
  onChange,
  eventTypes,
}: {
  filter: ColumnFilterState
  onChange: (next: ColumnFilterState) => void
  eventTypes: string[]
}) {
  return (
    <div className="mb-2 px-1 space-y-1.5">
      <input
        type="text"
        placeholder="Search…"
        value={filter.search}
        onChange={(e) => onChange({ ...filter, search: e.target.value })}
        className={columnSearchClass}
      />
      <div className="flex items-center gap-1.5 flex-wrap">
        <div className={segmentedWrap(true)}>
          {(['all', 'github', 'jira'] as const).map((f) => (
            <button
              key={f}
              type="button"
              onClick={() => onChange({ ...filter, source: f })}
              className={segmentedBtn(filter.source === f, true)}
            >
              {f === 'all' ? 'All' : f === 'github' ? 'GH' : 'Jira'}
            </button>
          ))}
        </div>
        {eventTypes.length > 0 && eventTypes.length <= 6 && (
          <span className="text-text-tertiary/40 text-[10px] mx-0.5">·</span>
        )}
        {eventTypes.length > 0 &&
          eventTypes.length <= 6 &&
          eventTypes.map((et) => (
            <button
              key={et}
              type="button"
              onClick={() => onChange({ ...filter, search: et })}
              className="opacity-70 hover:opacity-100 transition-opacity"
              title={`Filter by ${et}`}
            >
              <EventBadge eventType={et} compact />
            </button>
          ))}
      </div>
    </div>
  )
}
