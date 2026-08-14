import { useState, useEffect, useRef } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import { prefersReducedMotion } from '../shared/useReducedMotion'
import './acquire.css'

export type AcquirePhase = 'frame' | 'flash' | 'gap' | 'cross' | 'wait' | 'done' | 'failed'

export type AcquireProps = {
  /** The resolved value, or null while the call is out. */
  value?: ReactNode | null
  /** Reserved box width in px. Default 54. */
  width?: number
  /** Reserved box height in px. Default 12. */
  height?: number
  /** Which edge the value settles against. Default 'right'. */
  align?: 'left' | 'right'
  /** Hairline box during the sequence, dropped on resolve. Default true. */
  outline?: boolean
  /**
   * What waiting looks like once the sequence has run: the shape of the answer,
   * field by field — '$--.--' for money, '--' for a count. A bare bar if omitted.
   */
  skeleton?: ReactNode
  /**
   * The answer cannot arrive at all. Distinct from waiting: waiting breathes,
   * because something is still happening; this is inert, because nothing is.
   */
  failed?: boolean
  /** Force a phase. Documentation and tests only — never in a screen. */
  phase?: AcquirePhase | null
  className?: string
  style?: CSSProperties
}

// Acquire — a value arriving under attention.
//
// A fixed sequence that draws the eye to one figure. It is NOT a loading mask:
// the sequence runs for the same length of time whether the data is already in
// hand or still in flight, because its job is to say LOOK HERE, not to fill
// time. Four beats:
//
//   frame     the box is reserved and empty
//   flash     the box fills once — the space is claimed
//   gap       empty again, so the flash reads as a single strike
//   cross     the box takes a wireframe X, corner to corner
//
// Then, and only then, one of two things happens. If the value is there, it
// lands. If it is not, the box hands off to a skeleton — and that skeleton is
// the SHAPE OF THE ANSWER, not the reserved box carried over: a currency field
// waits as $--.--, a count as --. A rectangle that persists past the sequence
// reads as the sequence stalling, when in fact the sequence finished and the
// call did not. The sequence never stretches to cover a slow response, and
// never cuts short for a fast one.
//
// Phases are timers and classes, not CSS delays. A delayed animation with
// `both` fills BACKWARDS through its delay, so "wait, then flash" quietly means
// "flash immediately" — and a stray brace elsewhere in a stylesheet silently
// drops whichever rule came after it. Neither can happen here, and the current
// phase is readable off the element as data-phase.
//
// See Acquire.md before changing any of it.

// 2 : 1.2 : 4.3 : 3.15 on a 70ms unit. Even beats read as a metronome; uneven ones
// read as something happening — the flash is the shortest thing in the sequence,
// and the pause after it is the longest.
const UNIT = 70
const BEATS: Array<[AcquirePhase, number]> = [
  ['frame', UNIT * 2],
  ['flash', UNIT * 1.2],
  ['gap', UNIT * 4.3],
  ['cross', UNIT * 3.15],
]

/** The phase the sequence would have ended on, given what is known now. */
function settled(failed: boolean, has: boolean): AcquirePhase {
  return failed ? 'failed' : has ? 'done' : 'wait'
}

export function Acquire({
  value = null,
  width = 54,
  height = 12,
  align = 'right',
  outline = true,
  skeleton = null,
  failed = false,
  phase: forced = null,
  className = '',
  style,
}: AcquireProps) {
  const has = value !== null && value !== undefined

  // Read once per render rather than subscribed: a sequence that restarts
  // because the OS preference changed mid-flight would be the ambient loop this
  // component exists to avoid. Under the preference there are no beats at all —
  // the end state is the information, the beats are only the attention, and
  // attention is the part being asked for less of.
  const reduced = prefersReducedMotion()

  const [phase, setPhase] = useState<AcquirePhase>(
    () => forced || (reduced ? settled(failed, has) : 'frame'),
  )

  // Has the sequence finished? Under reduced motion it never starts, so it is
  // finished from the first frame.
  const [done, setDone] = useState(reduced)

  // Restarting is a render-time adjustment, not something the sequence effect
  // resets on its way in. The effect bails early under reduced motion, so if it
  // reset `done` from inside it could never do so on the one transition that
  // matters: turning the preference ON mid-sequence stops the timers, and
  // without this the component would sit on whichever beat it had reached, with
  // nothing left running to move it off.
  const [ranWith, setRanWith] = useState({ forced, reduced })
  if (ranWith.forced !== forced || ranWith.reduced !== reduced) {
    setRanWith({ forced, reduced })
    setDone(reduced)
    setPhase(forced || (reduced ? settled(failed, has) : 'frame'))
  }

  // Latest-value refs. The sequence runs on its own clock and reads these when
  // it lands, so a value that arrives mid-sequence is neither dropped nor
  // allowed to cut in.
  const valueRef = useRef(has)
  const failRef = useRef(failed)
  // Written in an effect, never during render: a render can be thrown away and
  // re-run, and a ref write inside one is not undone with it.
  useEffect(() => {
    valueRef.current = has
    failRef.current = failed
  })

  // The sequence. Runs once per mount, on its own clock, ignoring the value.
  useEffect(() => {
    if (forced || reduced) return
    let t = 0
    const timers = BEATS.slice(1).map(([name], i) => {
      t += BEATS[i][1]
      return setTimeout(() => setPhase(name), t)
    })
    timers.push(
      setTimeout(
        () => {
          setDone(true)
          setPhase(settled(failRef.current, valueRef.current))
        },
        t + BEATS[BEATS.length - 1][1],
      ),
    )
    return () => timers.forEach(clearTimeout)
  }, [forced, reduced])

  // The value, whenever it turns up — before the sequence ends it simply waits.
  // Adjusted during render, not in an effect: the sequence has already
  // finished, so there is nothing to wait for and no reason to paint the
  // skeleton one more frame before replacing it.
  if (!forced && done) {
    const want = settled(failed, has)
    if (want !== phase) setPhase(want)
  }

  // A forced phase simply wins at render. It is a documentation and test hatch,
  // and letting it override here rather than syncing it into state means it can
  // never disagree with what is on screen.
  const shown = forced || phase
  const resolved = shown === 'done' || shown === 'failed'

  return (
    <span
      className={'acq' + (outline ? ' acq-box' : '') + (className ? ' ' + className : '')}
      data-phase={shown}
      data-align={align}
      // Honest about the one thing assistive tech can act on: whether this
      // figure is still resolving. Deliberately NOT a live region — the .md
      // rules out using Acquire for anything that changes on its own, so there
      // is nothing here worth interrupting a reader to announce.
      aria-busy={resolved ? undefined : true}
      style={{ minWidth: width, height: Math.max(10, height), ...style }}
    >
      {shown === 'cross' && (
        <svg className="acq-x" viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true">
          <path d="M0 0 L100 100" pathLength={1} />
          <path d="M100 0 L0 100" pathLength={1} />
        </svg>
      )}
      {(shown === 'wait' || shown === 'failed') && (
        <span className={shown === 'failed' ? 'acq-sk acq-dead' : 'acq-sk'}>
          {skeleton || <i className="acq-bar" />}
        </span>
      )}
      {shown === 'done' && <b className="acq-v">{value}</b>}
    </span>
  )
}

export default Acquire
