import { useId, useState } from 'react'
import type { MouseEvent } from 'react'
import useReducedMotion from '../shared/useReducedMotion'
import './source-chart.css'

// A fortnight of events, with the portion that became tasks stacked inside it.
// The GAP BETWEEN THE TWO LINES is the subject: events nothing was done about.
//
// One scale, not two. Tasks are drawn against the EVENTS maximum, because the
// whole reading is that distance — normalising each series to its own peak
// would put a quiet week's tasks level with a busy week's events, and the
// chart would say the opposite of what is true.
//
// The build is one gesture: the baseline draws, then the area and both lines
// are revealed under a single mask wipe. Two wipes would read as two charts.

/** One day. `events` is the outer quantity, `tasks` the portion inside it. */
export type SourcePoint = { events: number; tasks: number }

/** The drawn box. The baseline sits at 84 and the tallest point at 8. */
const W = 320
const H = 96
const BASE = 84
const TOP = 8

export type SourceChartProps = {
  /** The section label — WHAT THE REPOSITORIES SENT. */
  title: string
  /** The two legend keys, outer then inner. */
  keys: [string, string]
  /** How the hover names them: "27 events · 3 tasks". */
  units: [string, string]
  /**
   * The days, oldest first — or null while there is no series to draw. Never
   * an empty array standing in for "not answered": a flat line at zero is a
   * claim about a fortnight.
   */
  series: SourcePoint[] | null
  /** What the empty state says. */
  emptyLabel?: string
}

export function SourceChart({
  title,
  keys,
  units,
  series,
  emptyLabel = 'no daily series yet',
}: SourceChartProps) {
  // Gradient ids are document-global, so two charts on one page would share
  // whichever painted last.
  const uid = useId().replace(/:/g, '')
  const still = useReducedMotion()
  const [at, setAt] = useState<number | null>(null)

  const n = series ? series.length : 0
  // Guarded, so a fortnight of nothing draws a flat line on the baseline
  // rather than dividing by zero.
  const max = series ? Math.max(1, ...series.map((p) => p.events)) : 1
  const x = (i: number) => (n > 1 ? (i / (n - 1)) * W : 0)
  const y = (v: number) => BASE - (v / max) * (BASE - TOP)

  const points = (pick: (p: SourcePoint) => number) =>
    (series ?? []).map((p, i) => x(i).toFixed(1) + ',' + y(pick(p)).toFixed(1)).join(' ')
  const area = (pick: (p: SourcePoint) => number) =>
    `M0,${BASE} L${points(pick).split(' ').join(' L')} L${W},${BASE} Z`

  const move = (e: MouseEvent<HTMLDivElement>) => {
    if (!series || n < 2) return
    const box = e.currentTarget.getBoundingClientRect()
    const t = Math.min(1, Math.max(0, (e.clientX - box.left) / box.width))
    setAt(Math.round(t * (n - 1)))
  }

  const hovered = at !== null && series ? series[at] : null
  const ago = at === null ? 0 : n - 1 - at
  const left = at === null || n < 2 ? '0%' : ((at / (n - 1)) * 100).toFixed(3) + '%'

  return (
    <div className="sc" data-still={still || undefined}>
      <div className="sc-legend">
        <span className="sc-legend-t">{title}</span>
        <span className="sc-grow" />
        {/* The keys describe lines. With no series there are none, so naming
            them would be a legend for an empty box. */}
        {series ? (
          <>
            <span className="sc-key">
              <i data-line="outer" />
              {keys[0]}
            </span>
            <span className="sc-key">
              <i data-line="inner" />
              {keys[1]}
            </span>
          </>
        ) : null}
      </div>

      <div
        className="sc-plot"
        data-live={series ? '' : undefined}
        onMouseMove={move}
        onMouseLeave={() => setAt(null)}
      >
        <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
          <defs>
            <linearGradient id={'ev' + uid} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-ink-1)" stopOpacity=".08" />
              <stop offset="100%" stopColor="var(--color-ink-1)" stopOpacity="0" />
            </linearGradient>
            <linearGradient id={'tk' + uid} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-warm)" stopOpacity=".22" />
              <stop offset="100%" stopColor="var(--color-warm)" stopOpacity=".04" />
            </linearGradient>
          </defs>
          {/* Drawn even with no series, so the block reads as a chart waiting
              for its answer rather than as a hole in the page. */}
          <line className="sc-base" x1="0" y1={BASE} x2={W} y2={BASE} />
          {series ? (
            <g className="sc-mask">
              <path d={area((p) => p.events)} fill={`url(#ev${uid})`} />
              <polyline className="sc-line" data-line="outer" points={points((p) => p.events)} />
              <path d={area((p) => p.tasks)} fill={`url(#tk${uid})`} />
              <polyline className="sc-line" data-line="inner" points={points((p) => p.tasks)} />
            </g>
          ) : null}
        </svg>

        {hovered ? (
          <>
            <span className="sc-cross" style={{ left }} aria-hidden="true" />
            <span
              className="sc-node"
              data-line="outer"
              style={{ left, top: y(hovered.events).toFixed(1) + 'px' }}
              aria-hidden="true"
            />
            <span
              className="sc-node"
              data-line="inner"
              style={{ left, top: y(hovered.tasks).toFixed(1) + 'px' }}
              aria-hidden="true"
            />
          </>
        ) : null}

        {series ? null : <span className="sc-none">{emptyLabel}</span>}
      </div>

      {series ? (
        <div className="sc-axis">
          <span>{n} DAYS AGO</span>
          {/* The readout names the day and both figures, so a hover answers
              rather than pointing. It sits between the two ends because that is
              the only place it can appear without moving either. */}
          {hovered ? (
            <span className="sc-read">
              {(ago === 0 ? 'today' : ago + 'd ago') +
                ' · ' +
                hovered.events +
                ' ' +
                units[0] +
                ' · ' +
                hovered.tasks +
                ' ' +
                units[1]}
            </span>
          ) : null}
          <span>TODAY</span>
        </div>
      ) : null}
    </div>
  )
}

export default SourceChart
