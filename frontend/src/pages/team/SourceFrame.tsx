import type { CSSProperties, ReactNode } from 'react'
import { ICONS } from '../../ui/sourcecard/icons'
import { Ticker } from '../../ui/odometer/Odometer'
import useReducedMotion from '../../ui/shared/useReducedMotion'
import './source-pages.css'

// The band every source page hangs off, and the two small parts all three
// share.
//
// The band is the same shape as the team's own: a title on the left, three
// figures on the right. On the team it reads members, spend and tasks; on a
// source it reads that source's events, what became of them, and whether the
// connection is alive. `since last poll` is the one an admin actually comes
// here for — a connection that stopped is otherwise silent.
//
// `became tasks` is the live one. It is not allowed to change quietly, so it
// is a Ticker rather than a printed number, and the same figure appears again
// in the left column where a page has one.

export type SourceKind = 'github' | 'jira' | 'slack'

/** What every source body is handed. The team is already resolved. */
export type SourceBodyProps = {
  teamId: string
  teamName: string
  /** Team admin. A member sees the same page with the verbs absent. */
  isAdmin: boolean
  /** Back to the sources panel — up one level, not out of the team. */
  onBack: () => void
}

export type SourceFrameProps = {
  source: SourceKind
  /** How the source spells itself. */
  name: string
  teamName: string
  onBack: () => void
  /** Events this source sent in seven days, or null where nothing counts them. */
  events: number | null
  /** How many of them became tasks. Live. */
  tasks: number | null
  /** Already formatted — "40s", "2m". Null where no timestamp is recorded. */
  sincePoll: string | null
  children: ReactNode
}

export function SourceFrame({
  source,
  name,
  teamName,
  onBack,
  events,
  tasks,
  sincePoll,
  children,
}: SourceFrameProps) {
  // One attribute for the whole page. Every build here is a CSS animation
  // filling `both` from a hidden or zeroed start, so the blanket rule in
  // tokens/motion.css would leave the labels invisible, the baseline
  // unstretched and the chart masked out entirely. source-pages.css states
  // each end state under [data-still].
  const still = useReducedMotion()

  return (
    <div className="sp" data-source={source} data-still={still || undefined}>
      <div className="sp-head">
        <div className="sp-id">
          <button type="button" className="sp-back" onClick={onBack} title="Back to event sources">
            <span className="sp-back-chev" aria-hidden="true" />
            <span className="sp-mark" aria-hidden="true">
              {ICONS[source]}
            </span>
            <span className="sp-title">{name}</span>
          </button>
          <span className="sp-sub">event source · {teamName}</span>
        </div>

        <div className="sp-figs">
          <div className="sp-fig">
            <span className="sp-fig-v">{events ?? '—'}</span>
            <span className="sp-fig-l">events · 7 days</span>
          </div>
          <span className="sp-div" />
          <div className="sp-fig">
            <Ticker value={tasks} />
            <span className="sp-fig-l">became tasks · 7 days</span>
          </div>
          <span className="sp-div" />
          <div className="sp-fig">
            <span className="sp-fig-v" data-tone="faint">
              {sincePoll ?? '—'}
            </span>
            <span className="sp-fig-l">since last poll</span>
          </div>
        </div>
      </div>

      <div className="sp-main">{children}</div>
    </div>
  )
}

/**
 * A figure and what it counts, on one line of the left column. The odometer
 * rolls in at `at`; the label follows on its own schedule, so a column reads
 * as figures arriving and then being named.
 */
export function FigureRow({
  children,
  label,
  at,
  tone,
}: {
  children: ReactNode
  label: string
  /** When the label lands, in seconds. The figure's own `at` is its business. */
  at: number
  tone?: 'warm' | 'alarm' | 'faint'
}) {
  return (
    <div className="sp-figrow" data-tone={tone}>
      <span className="sp-figrow-v">{children}</span>
      <span className="sp-figrow-l sp-fade" style={{ '--at': at + 's' } as CSSProperties}>
        {label}
      </span>
    </div>
  )
}

/** The one filter every source table carries. */
export function FilterField({
  value,
  onChange,
  label,
}: {
  value: string
  onChange: (next: string) => void
  label: string
}) {
  return (
    <span className="sp-filter">
      <svg
        width="11"
        height="11"
        viewBox="0 0 24 24"
        fill="none"
        stroke="var(--color-ink-4)"
        strokeWidth="1.8"
        strokeLinecap="round"
        aria-hidden="true"
      >
        <circle cx="11" cy="11" r="7" />
        <path d="m20 20-3.5-3.5" />
      </svg>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="filter"
        aria-label={label}
      />
    </span>
  )
}

export default SourceFrame
