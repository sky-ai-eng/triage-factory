import { useState } from 'react'
import type { CSSProperties } from 'react'
import useReducedMotion from '../shared/useReducedMotion'
import './odometer.css'

// Odometer — a figure arriving, and a figure incrementing.
//
// Two instruments, one file, because they are the same mechanism at two
// speeds and a page uses them together: the header band's `became tasks`
// increments while the left column's figures are still arriving.
//
//   Odometer  a value ROLLING INTO PLACE on arrival. Each digit is a 30-step
//             ramp that runs past a one-digit window and stops on the answer,
//             one beat behind the digit to its left.
//   Ticker    a value CHANGING while someone watches. It travels exactly one
//             line inside a hollow warm frame that appears and fades.
//
// Neither is Acquire. Acquire points at one figure as it lands and is
// explicitly ruled out for anything that changes on its own; these are for a
// column of figures resolving together, and for a count that ticks.
//
// Deliberately NOT a loading mask, same as Acquire: mount these when the
// answer is in hand. A ramp that rolls to nothing and then jumps is worse than
// a dash that becomes a number.

/** How many digits the ramp holds: 0–9, three times over. */
const RAMP = Array.from({ length: 30 }, (_, i) => i % 10)
/** One digit's line box. The ramp's offsets are multiples of it. */
const LINE = 22
/**
 * Which step of the ramp a digit stops on. The third pass, so every digit has
 * spun past its own face twice before it lands — a `1` that travelled one step
 * would read as a correction rather than as an arrival.
 */
const LANDING = 20

/** The stagger between one digit and the next, in seconds. */
const STEP = 0.06

export type OdometerProps = {
  /** The resolved figure, or null for a measurement nobody has taken yet. */
  value: number | null
  /**
   * What the wrapper announces. The ramp is 30 stacked digits, so without a
   * label the page's primary statistics read as `0123456789…` to a screen
   * reader and to anyone copying them.
   */
  label?: string
  /** A unit that rides beside the digits at their size — the `s` of `40s`. */
  suffix?: string
  /** When this figure's first digit starts, in seconds from the build. */
  at?: number
  /** The slot the figure sits in, so a column of them shares one left edge. */
  width?: number
  style?: CSSProperties
}

export function Odometer({ value, label, suffix, at = 0, width, style }: OdometerProps) {
  const reduced = useReducedMotion()
  const slot: CSSProperties = { ...(width ? { flex: 'none', width } : null), ...style }

  if (value === null) {
    return (
      <span className="odo" style={slot}>
        <span className="odo-none">—</span>
      </span>
    )
  }

  const digits = String(value).split('')

  return (
    <span
      className="odo"
      // The wrapper is the reading; everything inside it is scenery.
      role="img"
      aria-label={label ?? String(value) + (suffix ?? '')}
      data-still={reduced || undefined}
      style={slot}
    >
      <span className="odo-digits" aria-hidden="true">
        {digits.map((d, i) => (
          <span className="odo-col" key={i}>
            <span
              className="odo-ramp"
              style={
                {
                  '--roll': -(LANDING + Number(d)) * LINE + 'px',
                  '--at': (at + i * STEP).toFixed(2) + 's',
                } as CSSProperties
              }
            >
              {RAMP.map((n, j) => (
                <span className="odo-d" key={j}>
                  {n}
                </span>
              ))}
            </span>
          </span>
        ))}
      </span>
      {suffix ? (
        <span className="odo-suffix" aria-hidden="true">
          {suffix}
        </span>
      ) : null}
    </span>
  )
}

export type TickerProps = {
  /** The live figure, or null while there is nothing to report. */
  value: number | null
  /**
   * head — the header band's reading: it rolls from the old value to the new
   *        one and flashes, on arrival and on every change after it.
   * row  — the same figure in a column of figures. It does not travel, because
   *        the band already showed the movement, and it does not flash on
   *        arrival — only when something happened while you were watching.
   */
  variant?: 'head' | 'row'
  label?: string
}

/** What the strip travels between, and how many times it has travelled. */
type Beat = { value: number | null; from: number; n: number; changed: boolean }

export function Ticker({ value, variant = 'head', label }: TickerProps) {
  const reduced = useReducedMotion()
  // Derived from the prop during render, not in an effect: an effect runs
  // after paint, so the first frame would show the new figure and the roll
  // would then start from underneath it — the movement would read as a
  // correction of something already on screen.
  const [beat, setBeat] = useState<Beat>({ value: null, from: 0, n: 0, changed: false })
  if (value !== null && value !== beat.value) {
    setBeat({
      value,
      // The first arrival rolls up from nothing; every later one from
      // whatever was there.
      from: beat.value ?? 0,
      n: beat.n + 1,
      changed: beat.value !== null,
    })
  }

  if (value === null) {
    return (
      <span className="tick" data-variant={variant}>
        <span className="tick-plain">—</span>
      </span>
    )
  }

  if (variant === 'row') {
    return (
      <span className="tick" data-variant="row">
        {/* Nothing flashes on arrival here: the first frame the reader sees is
            not news. Keyed on the beat, so each change fires its own. */}
        {beat.changed ? <span className="tick-frame" key={beat.n} /> : null}
        <span className="tick-plain">{value}</span>
      </span>
    )
  }

  return (
    <span
      className="tick"
      data-variant="head"
      role="img"
      aria-label={label ?? String(value)}
      data-still={reduced || undefined}
    >
      <span className="tick-frame" key={'f' + beat.n} />
      <span className="tick-win" aria-hidden="true" key={'w' + beat.n}>
        <span className="tick-strip">
          <span className="tick-v">{beat.from}</span>
          <span className="tick-v">{value}</span>
        </span>
      </span>
    </span>
  )
}

export default Odometer
