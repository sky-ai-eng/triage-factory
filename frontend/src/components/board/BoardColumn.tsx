import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useDroppable } from '@dnd-kit/core'
import { Tooltip } from '../../ui/tooltip/Tooltip'
import {
  animate,
  motion,
  useMotionValue,
  useReducedMotion,
  useTransform,
  type MotionValue,
} from 'motion/react'
import {
  ArrowDown,
  ArrowUp,
  ChevronDown,
  ChevronsRightLeft,
  ChevronUp,
  Search,
  SlidersHorizontal,
  X,
} from 'lucide-react'
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

// BoardColumn is the board's column shell in the borderless liquid-glass
// idiom: no outlines anywhere — panes separate by depth and light, not lines.
// Each column is a frosted slab (surface-overlay, backdrop-blurred) that floats
// on the ambient GlassBackdrop via a soft drop-shadow + a 1px specular top edge,
// with a slow living sheen catching the top. Several overlay layers ride over
// the slab (all non-clipping siblings so their bloom spills past it):
//   • a top specular sheen that slowly breathes (the pane catches light),
//   • a depth-of-field veil (backdrop-blur + dim) that recedes the column when
//     another lane has focus — a camera focal plane across the board, and
//   • the drag-over receive glow.
//
// The lane no longer carries a "work is live" glow — that signal moved onto the
// card that's actually working (CardPlane's status glow in cardChrome.tsx), so
// the light rides the work, not the column around it.
//
// IMPORTANT: the recede blur is a `backdrop-filter` overlay *in front* of the
// column, never a CSS `filter`/`transform` on an ancestor — either of those
// would establish a new backdrop root and silently kill the slab's own frost at
// rest. Opacity dimming is safe (only active while receding).
//
// Search + structured filters live in a sticky frosted header inside the scroll
// body; the sliders button melts the header downward into an expanding filter
// panel. State is owned by Board.tsx; the apply/sort pass lives in
// ./columnFilter.ts so this file exports only a component (Fast Refresh).

// The search field floats — no box, no border, just the icon + text on the
// column glass (the Her move). A whisper of fill appears only on focus so
// typing has some ground.
// Border-trace timing — a smooth ease-in-out lap, one clear loop.
const TRACE_EASE = [0.65, 0, 0.35, 1] as const
const TRACE_DUR = 1.4
// Resting opacity of the L-bracket (brightens on hover / drag-over).
const BRACKET_REST = 0.55
// The L: each arm is ARM units long (% of the pathLength=100 perimeter — equal
// path units render as equal pixel lengths). The lit dash is both arms; at rest
// it straddles the top-left corner (path origin), so the offset that parks it
// there is +ARM. Sliding the offset by −100 is exactly one lap.
const ARM = 10
const BRACKET_REST_OFFSET = ARM
// The fade: instead of one solid dash, stack FADE_N concentric dashes, all
// centred on the same point. Shorter layers cover only the middle, longer ones
// reach the tips — and the longer (tip-reaching) layers are the faintest, so
// the stack is bright at the centre (the corner, at rest) and dissolves toward
// the arm ends, like the old gradient bracket. The parent <svg> opacity scales
// the whole thing. They share one offset, so the soft-edged segment travels
// intact and rests as a faded L.
const FADE_N = 6
const FADE_LAYERS = Array.from({ length: FADE_N }, (_, k) => {
  const t = (k + 1) / FADE_N // longest (reaches the tips) at t = 1
  return { length: 2 * ARM * t, opacity: 0.46 * (1 - t) + 0.07 }
})

// The card scroll area dissolves its cards into the page at top + bottom rather
// than hard-cutting them — a tall column melts its overflow into the field the
// same way the board's horizontal scroll fades at its edges. A small top fade
// (cards emerge from under the masthead) + a deeper bottom fade.
const CARD_FADE_MASK =
  'linear-gradient(to bottom, transparent 0, #000 8px, #000 calc(100% - 44px), transparent 100%)'

const searchInputClass =
  'w-full rounded-lg bg-transparent py-1.5 pl-7 pr-7 text-body text-ink-1 placeholder:text-ink-3 outline-none transition-colors focus:bg-[var(--color-raised)]/40'

const dateInputClass =
  'flex-1 rounded-lg bg-[var(--color-raised)]/70 px-2 py-1 text-reported text-ink-1 outline-none transition-[box-shadow] focus:shadow-[0_0_0_2px_var(--color-warm-2)]'
const timeInputClass =
  'w-[5.5rem] rounded-lg bg-[var(--color-raised)]/70 px-2 py-1 text-reported text-ink-1 outline-none transition-[box-shadow] focus:shadow-[0_0_0_2px_var(--color-warm-2)] disabled:opacity-40'

// Split / join the stored datetime-local string ("YYYY-MM-DDTHH:mm") into its
// date and time halves. The time is optional in the UI — a bare date stores
// midnight, so the filter still has a valid instant to compare against.
function splitDT(v: string): { date: string; time: string } {
  if (!v) return { date: '', time: '' }
  const [date, time = ''] = v.split('T')
  return { date, time }
}
function joinDT(date: string, time: string): string {
  if (!date) return '' // no date → the bound is cleared; a lone time means nothing
  return `${date}T${time || '00:00'}`
}

interface Props {
  id: string
  title: string
  tasks: Task[] // raw list, used to populate the event-type chips
  filter: ColumnFilterState
  onFilterChange: (next: ColumnFilterState) => void
  // Optional header slot for a column-specific note (e.g. Done's "last 7
  // days"). Renders to the right of the title, inside the bracket.
  headerExtra?: React.ReactNode
  // Optional snoozed toggle (Queued only) — surfaced inside the filter panel
  // rather than as a button stacked on the column.
  snooze?: { shown: boolean; onToggle: () => void }
  // Position in the row (0-based) — drives the staggered mount reveal.
  index?: number
  // True when a drag is currently over this column or any of its cards
  // (resolved at the board level). Fires the border-trace + brightens it.
  dragOver?: boolean
  // When provided, renders a faint collapse affordance left of the title.
  onCollapse?: () => void
  children: React.ReactNode
}

export default function BoardColumn({
  id,
  title,
  tasks,
  filter,
  onFilterChange,
  headerExtra,
  snooze,
  index = 0,
  dragOver = false,
  onCollapse,
  children,
}: Props) {
  // setNodeRef registers the column as a drop target; the "is the drag over
  // me" signal comes from the `dragOver` prop (board-resolved) so it counts
  // hovering the column's cards too, not just its empty area.
  const { setNodeRef } = useDroppable({ id, data: { type: 'column' } })
  const reduce = !!useReducedMotion()

  const [controlsOpen, setControlsOpen] = useState(false)
  // Hovering a column brightens its own rust L-bracket — a quiet "this one has
  // focus" cue, no effect on the other lanes.
  const [hovered, setHovered] = useState(false)

  // The L-bracket IS the animation. It's a single SVG stroke (a dash on the
  // column's rect path, pathLength-normalized to 100 so the math is
  // size-independent). At rest the dash straddles the top-left corner — ARM
  // units along the top edge + ARM units up the left edge = the L. We just
  // slide its dash-offset one full lap (REST → REST−100, same shape) and back,
  // so the L unfolds into the travelling segment and folds back into the L.
  // One element, no cross-fade, ends exactly where it rests. lapOnce drives
  // every case — it plays on mount (page load + post-expand remount) and again
  // each time a card is dragged over (a single lap, not a continuous loop).
  const offset = useMotionValue(BRACKET_REST_OFFSET)
  const anim = useRef<ReturnType<typeof animate> | null>(null)

  const lapOnce = useCallback(() => {
    if (reduce) return
    anim.current?.stop()
    offset.set(BRACKET_REST_OFFSET)
    anim.current = animate(offset, BRACKET_REST_OFFSET - 100, {
      duration: TRACE_DUR,
      ease: TRACE_EASE,
      onComplete: () => offset.set(BRACKET_REST_OFFSET),
    })
  }, [reduce, offset])

  // Play once on mount (page load + post-expand remount).
  useEffect(() => {
    lapOnce()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // One lap each time a card enters this column (not a continuous loop). It
  // self-resets to the resting L; leaving mid-lap just lets it finish.
  const wasOver = useRef(false)
  useEffect(() => {
    if (dragOver && !wasOver.current) lapOnce()
    wasOver.current = dragOver
  }, [dragOver, lapOnce])

  // Event-type chips reflect the unfiltered set so the user always sees what
  // types exist in this column, even after filtering them out.
  const eventTypes = useMemo(() => {
    const seen = new Set<string>()
    for (const t of tasks) {
      if (t.event_type) seen.add(t.event_type)
    }
    return Array.from(seen).sort()
  }, [tasks])

  const hasFilters = filterIsActive(filter)

  // Fixed 430px width (keep in sync with COL_W in Board.tsx, which
  // computes the centered-lane layout). Fixed, not viewport-relative, so cards
  // don't compress as columns scroll into view.
  return (
    <motion.div
      className="flex h-full w-[430px] shrink-0 flex-col"
      initial={reduce ? false : { opacity: 0, y: 14 }}
      animate={{ opacity: 1, y: 0 }}
      transition={reduce ? { duration: 0 } : { ...bodyEase, delay: index * 0.05 }}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      {/* The column is not a surface — no fill, no blur. It melts into the page;
          only the rust L-bracket and the cards themselves mark it out. Flex-col
          so the masthead sits above its own scrolling card area (no sticky
          overlap, nothing to frost). */}
      <div ref={setNodeRef} className="relative flex min-h-0 flex-1 flex-col">
        {/* Masthead — title + search + filter floating directly on the page.
            The filter panel melts down from here, pushing the cards down. */}
        <div className="px-3 pb-2 pt-3">
          <div className="mb-1.5 flex items-center justify-between px-0.5">
            <div className="flex items-center gap-1.5">
              {onCollapse && (
                // focusable={false}: the button is the interactive thing and
                // names itself via aria-label — the hint is scenery, and a
                // second tab stop around a button would be redundant.
                <Tooltip content="Collapse" focusable={false}>
                  <button
                    type="button"
                    aria-label={`Collapse ${title}`}
                    onClick={onCollapse}
                    className="text-ink-3/50 transition-colors hover:text-ink-2"
                  >
                    <ChevronsRightLeft size={14} />
                  </button>
                </Tooltip>
              )}
              <h2 className="text-column font-semibold tracking-tight text-ink-1">{title}</h2>
            </div>
            {headerExtra}
          </div>

          <div className="flex items-center gap-2">
            <div className="relative flex-1">
              <Search
                size={14}
                aria-hidden
                className="pointer-events-none absolute left-0 top-1/2 -translate-y-1/2 text-ink-3"
              />
              <input
                type="text"
                aria-label={`Search ${title}`}
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
                  className="absolute right-1 top-1/2 -translate-y-1/2 text-ink-3 transition-colors hover:text-ink-2"
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
              className={`relative shrink-0 rounded-lg p-1.5 transition-colors ${
                controlsOpen ? 'text-warm' : 'text-ink-3 hover:text-ink-2'
              }`}
            >
              <SlidersHorizontal size={15} />
              {hasFilters && (
                <span
                  aria-hidden
                  className="absolute -right-0.5 -top-0.5 h-1.5 w-1.5 rounded-full bg-warm"
                />
              )}
            </button>
          </div>

          {/* The melt-down: height + opacity animate from the search row. */}
          <motion.div
            initial={false}
            animate={{ height: controlsOpen ? 'auto' : 0, opacity: controlsOpen ? 1 : 0 }}
            transition={reduce ? { duration: 0 } : bodyEase}
            className="overflow-hidden"
          >
            <div className="space-y-3 pt-3">
              <FilterControls
                filter={filter}
                onChange={onFilterChange}
                eventTypes={eventTypes}
                snooze={snooze}
              />
            </div>
          </motion.div>
        </div>

        {/* Cards scroll independently below the masthead; the mask melts the
            overflow into the page at top + bottom instead of a hard cutoff. */}
        <div
          className="min-h-0 flex-1 space-y-3 overflow-y-auto px-3 pb-3 pt-1"
          style={{ maskImage: CARD_FADE_MASK, WebkitMaskImage: CARD_FADE_MASK }}
        >
          {children}
        </div>

        {/* The rust L-bracket — concentric fading dashes (see TraceLayer). At
            rest they straddle the top-left corner as a soft-edged L; the lap
            slides their shared offset one full turn and back, so the L unfolds
            into the travelling segment and folds back. Brightens on hover /
            while a card is over. */}
        <svg
          aria-hidden
          className="pointer-events-none absolute inset-0 h-full w-full overflow-visible"
          style={{
            opacity: dragOver ? 0.9 : hovered ? 1 : BRACKET_REST,
            transition: 'opacity 0.3s ease',
          }}
        >
          {FADE_LAYERS.map((layer, i) => (
            <TraceLayer key={i} base={offset} length={layer.length} opacity={layer.opacity} />
          ))}
        </svg>
      </div>
    </motion.div>
  )
}

// TraceLayer is one concentric dash of the L-bracket's fade. `length` is its lit
// span (path units); it's centred on the same point as every other layer, so a
// shorter layer covers only the middle and a longer one reaches the tips —
// stacked, they read as a soft fade. Its offset tracks the shared `base` value
// (rest = ARM), shifted by (length/2 − ARM) so all layers stay co-centred as
// the base slides one lap.
function TraceLayer({
  base,
  length,
  opacity,
}: {
  base: MotionValue<number>
  length: number
  opacity: number
}) {
  const off = useTransform(base, (v) => v + length / 2 - ARM)
  return (
    <motion.rect
      x="0"
      y="0"
      width="100%"
      height="100%"
      pathLength={100}
      fill="none"
      stroke="var(--color-warm)"
      strokeWidth={1}
      strokeLinecap="round"
      strokeDasharray={`${length} ${100 - length}`}
      strokeDashoffset={off}
      style={{ opacity }}
    />
  )
}

// CollapsedColumn is the thin rail a collapsed lane shrinks to — a gray vertical
// label flanked by inward chevrons (the "I'm collapsed, click to open me" cue),
// with the task count. Hovering underlines the title and gives it a slow pulse.
// Clicking expands the lane back to a full BoardColumn.
export function CollapsedColumn({
  title,
  count,
  index = 0,
  onExpand,
}: {
  title: string
  count: number
  index?: number
  onExpand: () => void
}) {
  const reduce = !!useReducedMotion()
  const [hovered, setHovered] = useState(false)
  return (
    <motion.button
      type="button"
      aria-label={`Expand ${title}`}
      title={`Expand ${title}`}
      onClick={onExpand}
      onHoverStart={() => setHovered(true)}
      onHoverEnd={() => setHovered(false)}
      className="group flex h-full w-5 shrink-0 cursor-pointer flex-col items-center justify-center gap-3 text-ink-3"
      initial={reduce ? false : { opacity: 0, y: 14 }}
      animate={{ opacity: 1, y: 0 }}
      transition={reduce ? { duration: 0 } : { ...bodyEase, delay: index * 0.05 }}
    >
      <ChevronDown size={15} className="opacity-60 transition-opacity group-hover:opacity-100" />
      <motion.div
        className="flex flex-col items-center gap-2"
        animate={hovered && !reduce ? { scale: [1, 1.06, 1] } : { scale: 1 }}
        transition={
          hovered && !reduce
            ? { duration: 1.4, repeat: Infinity, ease: 'easeInOut' }
            : { duration: 0.2 }
        }
      >
        {count > 0 && (
          <span
            className="text-reported font-medium tabular-nums text-ink-3 [writing-mode:vertical-rl]"
            style={{ textOrientation: 'mixed' }}
          >
            {count}
          </span>
        )}
        <span
          className="text-body font-medium tracking-wide text-ink-2 decoration-ink-3 underline-offset-4 [writing-mode:vertical-rl] group-hover:underline"
          style={{ textOrientation: 'mixed' }}
        >
          {title}
        </span>
      </motion.div>
      <ChevronUp size={15} className="opacity-60 transition-opacity group-hover:opacity-100" />
    </motion.button>
  )
}

const SORT_KEYS: SortKey[] = ['default', 'created', 'title', 'event_type', 'claimee']
const SOURCES: { value: SourceFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'github', label: 'GitHub' },
  { value: 'jira', label: 'Jira' },
]

function pill(selected: boolean): string {
  return `rounded-full px-2.5 py-1 text-reported font-medium transition-colors ${
    selected
      ? 'bg-warm/[0.14] text-warm'
      : 'bg-[var(--color-raised)]/70 text-ink-3 hover:text-ink-2'
  }`
}

function Group({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <p className="text-label font-medium uppercase tracking-wide text-ink-3">{label}</p>
      <div className="flex flex-wrap items-center gap-1.5">{children}</div>
    </div>
  )
}

// DateTimeField is one bound of the created-date window: a required date plus
// an optional time. Picking a date alone stores midnight; the time input is
// enabled only once a date exists and edits the same combined value.
function DateTimeField({
  label,
  value,
  minDate,
  maxDate,
  onChange,
}: {
  label: string
  value: string
  minDate?: string
  maxDate?: string
  onChange: (v: string) => void
}) {
  const { date, time } = splitDT(value)
  return (
    <div className="space-y-1">
      <span className="block text-label text-ink-3">{label}</span>
      <div className="flex items-center gap-1.5">
        <input
          type="date"
          value={date}
          min={minDate}
          max={maxDate}
          onChange={(e) => onChange(joinDT(e.target.value, time))}
          style={{ colorScheme: 'light dark' }}
          className={dateInputClass}
        />
        <input
          type="time"
          value={time}
          disabled={!date}
          aria-label={`${label} time (optional)`}
          onChange={(e) => onChange(joinDT(date, e.target.value))}
          style={{ colorScheme: 'light dark' }}
          className={timeInputClass}
        />
      </div>
    </div>
  )
}

// FilterControls is the sort+filter body that melts open inside the column
// header. Controlled — every mutation flows straight back through onChange.
function FilterControls({
  filter,
  onChange,
  eventTypes,
  snooze,
}: {
  filter: ColumnFilterState
  onChange: (next: ColumnFilterState) => void
  eventTypes: string[]
  snooze?: { shown: boolean; onToggle: () => void }
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
      {snooze && (
        <Group label="Snoozed">
          <button type="button" onClick={snooze.onToggle} className={pill(snooze.shown)}>
            {snooze.shown ? 'Showing snoozed' : 'Show snoozed'}
          </button>
        </Group>
      )}

      <Group label="Sort">
        {SORT_KEYS.map((k) => (
          <button
            key={k}
            type="button"
            // Returning to 'default' (Smart) also resets the direction, so a
            // stale asc/desc from a prior key can't silently carry into the
            // next sort the user picks.
            onClick={() =>
              onChange({
                ...filter,
                sortKey: k,
                sortDir: k === 'default' ? emptyFilter.sortDir : filter.sortDir,
              })
            }
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
        <div className="w-full space-y-2">
          <DateTimeField
            label="After"
            value={filter.after}
            maxDate={splitDT(filter.before).date || undefined}
            onChange={(v) => onChange({ ...filter, after: v })}
          />
          <DateTimeField
            label="Before"
            value={filter.before}
            minDate={splitDT(filter.after).date || undefined}
            onChange={(v) => onChange({ ...filter, before: v })}
          />
        </div>
        {(filter.after || filter.before) && (
          <button
            type="button"
            onClick={() => onChange({ ...filter, after: '', before: '' })}
            className="text-reported text-ink-3 transition-colors hover:text-ink-2"
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
          className="text-reported text-ink-3 transition-colors hover:text-ink-2 disabled:opacity-40"
        >
          Reset filters
        </button>
      </div>
    </>
  )
}
