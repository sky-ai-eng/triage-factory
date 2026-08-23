import { useState, useEffect, useRef } from 'react'
import type { CSSProperties } from 'react'
import { useReducedMotion } from '../shared/useReducedMotion'

export type CountdownProps = {
  /** Controlled remaining fraction, 1 → 0. Omit to let the component run its own clock. */
  frac?: number | null
  /** Self-driven window length. Ignored when `frac` is given. */
  seconds?: number
  /** Pause/resume a self-driven window. */
  running?: boolean
  /** Change to restart a self-driven window without remounting. */
  startKey?: string | number
  /** Fired once when a self-driven window reaches zero. */
  onDone?: () => void
  /** Rendered size in px. Draws from a 24×24 box, so any size holds. */
  size?: number
  /** Number of crates. Three reads at 14px; more only above ~24px. */
  steps?: number
  /** 'warm' | 'alarm' | 'ink', or any CSS color. */
  tone?: 'warm' | 'alarm' | 'ink' | string
  /** Accessible name. */
  label?: string
  /** Native tooltip. */
  title?: string
  className?: string
  style?: CSSProperties
}

// Countdown — the shape a timed window takes in this system.
//
// Three stacked crates emptying top down, each draining left to right. The
// value is `frac`: 1 when the window opens, 0 when it closes. It drains rather
// than fills because a window is time being spent, and it drains smoothly
// rather than ticking — a per-second jump reads as a stutter at fourteen
// pixels, and the window is a duration, not a count of seconds.
//
// The mark keeps its footprint the whole way: empty crates stay drawn as
// outlines, so the countdown never collapses to nothing and never shifts the
// layout around it.
//
// Controlled (`frac`) when the caller owns the clock — a table holding an undo
// snapshot has to own it, since the commit fires from the same timer. Give it
// `seconds` instead and it runs its own, calling `onDone` at zero.

const TONES: Record<string, string> = {
  warm: 'var(--color-warm)',
  alarm: 'var(--color-alarm)',
  ink: 'var(--color-ink-2)',
}

export function Countdown({
  frac: fracProp,
  seconds = 10,
  running = true,
  startKey,
  onDone,
  size = 14,
  steps = 3,
  tone = 'warm',
  label = 'time remaining',
  title,
  className = '',
  style,
}: CountdownProps) {
  const [own, setOwn] = useState(1)
  const raf = useRef<number | null>(null)
  const done = useRef(false)
  const driven = fracProp == null

  // The drain is rAF-driven, so the reduced-motion blanket in tokens/motion.css
  // cannot reach it. Here the motion is not decoration — it is the remaining
  // time — so it is not removed, it is COARSENED: one step per second instead of
  // one per frame. The dial still tells the truth, and nothing moves smoothly.
  const reduced = useReducedMotion()

  // Restarting is a render-time adjustment: the effect below owns the clock and
  // nothing else, so a new window never paints the previous one's remainder for
  // a frame on its way in.
  const [ranWith, setRanWith] = useState({ driven, running, seconds, startKey, reduced })
  if (
    ranWith.driven !== driven ||
    ranWith.running !== running ||
    ranWith.seconds !== seconds ||
    ranWith.startKey !== startKey ||
    ranWith.reduced !== reduced
  ) {
    setRanWith({ driven, running, seconds, startKey, reduced })
    if (driven && running) setOwn(1)
  }

  // onDone through a ref: it is usually an inline arrow, and depending on it
  // would restart the window on every render of the parent.
  const doneCb = useRef(onDone)
  useEffect(() => {
    doneCb.current = onDone
  })

  useEffect(() => {
    if (!driven || !running) return
    done.current = false
    const total = seconds * 1000
    const t0 = performance.now()
    const finish = () => {
      if (done.current) return
      done.current = true
      doneCb.current?.()
    }

    if (reduced) {
      const tick = setInterval(() => {
        const left = Math.max(0, 1 - (performance.now() - t0) / total)
        setOwn(left)
        if (left <= 0) {
          clearInterval(tick)
          finish()
        }
      }, 1000)
      return () => clearInterval(tick)
    }

    const step = (now: number) => {
      const left = Math.max(0, 1 - (now - t0) / total)
      setOwn(left)
      if (left <= 0) return finish()
      raf.current = requestAnimationFrame(step)
    }
    raf.current = requestAnimationFrame(step)
    return () => {
      if (raf.current != null) cancelAnimationFrame(raf.current)
    }
  }, [driven, running, seconds, startKey, reduced])

  const f = Math.max(0, Math.min(1, driven ? own : (fracProp as number)))
  const color = TONES[tone] || tone
  const faint = 'color-mix(in srgb, ' + color + ' 26%, transparent)'

  // 24x24 box scaled to whatever size is asked for, so one set of coordinates
  // serves a 14px dial and a 56px one.
  const gap = 0.6
  const h = (17.8 - gap * (steps - 1)) / steps

  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      role="img"
      aria-label={label}
      style={{ display: 'block', flex: 'none', ...style }}
    >
      {title ? <title>{title}</title> : null}
      {Array.from({ length: steps }, (_, i) => {
        const y = 3.1 + i * (h + gap)
        // Rows are counted from the bottom, so the top crate holds the last
        // slice of the window and goes first.
        const fill = Math.max(0, Math.min(1, f * steps - (steps - 1 - i)))
        const r = Math.min(0.9, h / 4)
        return (
          <g key={i}>
            <rect
              x={3.5}
              y={y}
              width={17}
              height={h}
              rx={r}
              fill="none"
              stroke={faint}
              strokeWidth={1.3}
            />
            {fill > 0.002 ? (
              <rect x={3.5} y={y} width={17 * fill} height={h} rx={r} fill={color} />
            ) : null}
          </g>
        )
      })}
    </svg>
  )
}

export default Countdown
