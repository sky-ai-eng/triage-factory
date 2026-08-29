import { useState } from 'react'
import type { CSSProperties } from 'react'
import useReducedMotion from '../shared/useReducedMotion'
import './flapcount.css'

// FlapCount — a figure changing, one digit at a time.
//
// The shipped Ticker rolls a whole number behind one window and flashes one
// frame around all of it. That is right when the figure is the news. It is
// wrong for a running total, where 312 → 335 reports two digits of movement
// and the leading 3 never moved: framing the whole number claims more change
// than happened, and on a count that updates every few seconds it reads as the
// number being replaced rather than incremented.
//
// So: one window per digit, and only the digits whose face actually changes
// get a window, a roll and a frame. Everything else is static text. A
// departure board with two flaps turning is unmistakably a board where two
// things changed.
//
// Rules kept from Odometer.md rather than reinvented:
//   · The beat is derived from the prop DURING RENDER, not in an effect. An
//     effect runs after paint, so the first frame would show the new digit and
//     the roll would start from underneath it — movement that reads as a
//     correction of something already on screen.
//   · Re-running is a REMOUNT, keyed on the beat count. Same answer Acquire
//     and Ticker give for replaying a sequence.
//   · Digits start 0.06s apart, left to right — the house stagger.
//   · Rolled digits are aria-hidden scenery inside a labeled wrapper, or a
//     screen reader reads the strip as "31" for a digit going 1 → 3.
//   · Reduced motion writes the end state directly. An animation with `both`
//     that never runs leaves the strip on the OLD value, which is a wrong
//     answer rather than absent motion.

const STEP = 0.06

export type FlapCountProps = {
  /** The running total, or null for a measurement nobody has taken — an em
   *  dash, with no role claimed. An offline readout passes null rather than
   *  the last number it happened to hold. */
  value: number | null
  /** The accessible name for the whole figure. Defaults to String(value). */
  label?: string
  /** Px. Drives the window height and the roll distance together — one
   *  variable, because a window a pixel taller than the travel shows a sliver
   *  of the outgoing digit at rest. */
  size?: number
  /** How the number becomes glyphs. The BEAT tracks the number; the glyphs
   *  come from here — which is what lets a caller abbreviate (1249 → "1.2k")
   *  and still get a correct diff, because the comparison that decides which
   *  flaps turn runs on the strings a reader actually sees. */
  format?: (n: number) => string
}

type Beat = { value: number | null; from: number | null; n: number; dir: 'up' | 'down' }

export function FlapCount({ value, label, size = 24, format = String }: FlapCountProps) {
  const still = useReducedMotion()
  const [beat, setBeat] = useState<Beat>({ value: null, from: null, n: 0, dir: 'up' })

  // Derived during render, the sanctioned store-previous-props form: React
  // re-renders immediately with the new beat before painting, so the first
  // visible frame already carries the roll.
  if (value !== null && value !== beat.value) {
    setBeat({
      value,
      from: beat.value,
      n: beat.n + 1,
      // A flap that always turns the same way says "changed" and nothing
      // else. Rolling with the direction of travel makes the motion carry the
      // sign, so a backlog shrinking looks like a backlog shrinking without
      // reading a digit. The whole row turns together — a board does not flap
      // half its flaps one way.
      dir: beat.value !== null && value < beat.value ? 'down' : 'up',
    })
  }

  if (value === null) {
    return (
      <span className="flap" style={{ '--flap-h': size + 'px' } as CSSProperties}>
        <span className="flap-v">—</span>
      </span>
    )
  }

  const to = format(value)
  const from = beat.from === null ? null : format(beat.from)
  // A change in width is a change in every position — 99 → 103 shifts all of
  // them, so there is no honest way to call one digit unmoved. The new leading
  // column rolls up from blank, not from a zero that was never on the board.
  const shifted = from !== null && from.length !== to.length
  const changed = (i: number) => from !== null && (shifted || from[i] !== to[i])

  return (
    <span
      className="flap"
      role="img"
      aria-label={label ?? String(value)}
      data-still={still || undefined}
      style={{ '--flap-h': size + 'px' } as CSSProperties}
    >
      {to.split('').map((d, i) => {
        if (!changed(i))
          return (
            <span className="flap-v" key={i}>
              {d}
            </span>
          )
        return (
          <span className="flap-d" key={i}>
            <span
              className="flap-frame"
              key={'f' + beat.n}
              style={{ animationDelay: (i * STEP).toFixed(2) + 's' }}
            />
            <span className="flap-win" key={'w' + beat.n} aria-hidden="true">
              <span
                className="flap-strip"
                data-dir={beat.dir}
                style={{ animationDelay: (0.1 + i * STEP).toFixed(2) + 's' }}
              >
                {/* Order follows direction so both end on the incoming glyph:
                    rolling up travels off the old one, rolling down arrives
                    onto the new one from above. */}
                {beat.dir === 'down' ? (
                  <>
                    <span className="flap-v">{d}</span>
                    <span className="flap-v">{shifted ? '' : from![i]}</span>
                  </>
                ) : (
                  <>
                    <span className="flap-v">{shifted ? '' : from![i]}</span>
                    <span className="flap-v">{d}</span>
                  </>
                )}
              </span>
            </span>
          </span>
        )
      })}
    </span>
  )
}

export default FlapCount
