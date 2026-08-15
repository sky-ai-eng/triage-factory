import { useState, useRef, useEffect, useCallback } from 'react'
import type { CSSProperties, KeyboardEvent, PointerEvent, ReactNode } from 'react'
import './hold.css'

export type HoldProps = {
  /**
   * Name used when this control is itself the reachable one. In surface mode
   * with a `trigger` it is NOT used: the triggers carry their own names.
   */
  label?: string
  holdingLabel?: string
  doneLabel?: string
  /** Gesture length. 0 fires at once, still with the rings, as one acknowledgement. */
  ms?: number
  /** Ring count. Discrete stops read as counting; a smooth fill reads as waiting. */
  steps?: number
  tone?: 'warm' | 'alarm' | 'ink' | string
  disabled?: boolean
  /** Draw `n/steps` while holding. */
  showCount?: boolean
  /** Fired once the gesture completes. In surface mode, receives the trigger held. */
  onConfirm?: (trigger: HTMLElement | null) => void
  /**
   * SURFACE MODE. The measurement is drawn across whatever the control wraps
   * rather than across a button of its own. Use it when the thing being
   * confirmed already has a container — a selection bar, a row, a card — and a
   * second button inside that container would be one affordance too many.
   */
  variant?: 'button' | 'surface'
  /**
   * A selector: only a press landing on a matching descendant starts the hold,
   * so a bar can carry several verbs and only one of them is the dangerous one.
   * A trigger may carry `data-hold-ms` to set its own pressure.
   */
  trigger?: string
  children?: ReactNode
  className?: string
  style?: CSSProperties
}

// Hold — a commit you have to mean.
//
// The gesture replaces a confirmation dialog: instead of reading a sentence and
// clicking Yes, you hold the control down and watch the commit close in on you.
//
// Two ideas carry it, both borrowed from the build vocabulary rather than from a
// download's progress bar:
//
//   HOVER EMPTIES THE BUTTON. At rest it is filled, like any other commit control.
//   Point at it and the fill drains — then, half a second later, a cross draws
//   itself corner to corner. That is Acquire's wireframe beat: the region is
//   reserved and its contents are not there yet. The button stops looking like a
//   thing you click and starts looking like a thing you fill.
//
//   PRESSING EXPANDS FROM YOUR FINGER. The progress is not a bar sweeping in from
//   an edge you did not choose; it grows outward from the exact point you pressed,
//   in discrete rings, with a bright ring at the frontier. The commit is measured
//   FROM you, so the geometry starts at you.
//
// The rings are stepped, not smooth: a smooth fill reads as waiting, discrete
// stops read as counting. They are driven from an interval in state, not from CSS
// keyframes, so the count is real, releasing is instant, and a re-press cannot
// inherit a part-elapsed clock. That also means the gesture needs no
// reduced-motion branch — see Hold.md.
//
// Pointer capture is mandatory — the label changes width mid-hold, so the button
// resizes under the cursor; without capture a press near the edge falls outside
// the element and the hold dies. pointerleave must NOT cancel.
export function Hold({
  label = 'Hold to save',
  holdingLabel = 'Hold…',
  doneLabel = 'Saved',
  ms = 850,
  steps = 9,
  tone = 'warm',
  disabled = false,
  showCount = true,
  onConfirm,
  variant = 'button',
  trigger,
  children,
  className = '',
  style,
}: HoldProps) {
  const [step, setStep] = useState(0)
  const [done, setDone] = useState(false)
  const [at, setAt] = useState({ x: 50, y: 50, max: 100 })
  const tick = useRef<ReturnType<typeof setInterval> | null>(null)
  const settle = useRef<ReturnType<typeof setTimeout> | null>(null)

  // Which trigger started the gesture, so a surface carrying several verbs can
  // tell onConfirm which one was leaned on.
  const fired = useRef<HTMLElement | null>(null)

  const stop = useCallback(() => {
    if (tick.current) clearInterval(tick.current)
    tick.current = null
    setStep(0)
  }, [])

  useEffect(
    () => () => {
      if (tick.current) clearInterval(tick.current)
      if (settle.current) clearTimeout(settle.current)
    },
    [],
  )

  // One engine, two inputs. The gesture is a sustained commit, so it must have a
  // sustained KEY path too — a control that can only be held with a mouse means a
  // keyboard user can stage changes and never save them.
  const begin = (
    el: HTMLElement,
    px: number | null,
    py: number | null,
    over: { ms: number } | null,
  ) => {
    if (disabled || tick.current) return
    // The trigger may carry its own duration (data-hold-ms), so one surface can
    // hold several verbs at different pressures: a removal that must be leaned
    // on, an ordinary verb at zero.
    const MS = over && over.ms != null ? over.ms : ms
    // The farthest corner from the origin, so the last ring covers the whole
    // control wherever it started — a hold begun at an edge must not finish early
    // on one side.
    const b = el.getBoundingClientRect()
    const x = px == null ? b.width / 2 : px
    const y = py == null ? b.height / 2 : py
    const max = Math.max(
      Math.hypot(x, y),
      Math.hypot(b.width - x, y),
      Math.hypot(x, b.height - y),
      Math.hypot(b.width - x, b.height - y),
    )
    setAt({ x: (x / b.width) * 100, y: (y / b.height) * 100, max: Math.ceil(max) })

    const finish = () => {
      setStep(steps)
      setDone(true)
      settle.current = setTimeout(() => {
        setDone(false)
        setStep(0)
        const held = fired.current
        fired.current = null
        if (!trigger || held) onConfirm?.(held)
      }, 220)
    }

    // Zero is not "no gesture": the rings arrive at once and settle, then the
    // verb runs. The same confirmation, played in one frame — an action that
    // fires with no acknowledgement at all reads as a mis-click either way.
    if (MS <= 0) return finish()

    let n = 0
    setStep(0)
    tick.current = setInterval(() => {
      n += 1
      if (n >= steps) {
        if (tick.current) clearInterval(tick.current)
        tick.current = null
        finish()
      } else {
        setStep(n)
      }
    }, MS / steps)
  }

  const start = (e: PointerEvent<HTMLElement>) => {
    if (e.button) return
    const target = e.target as HTMLElement
    const hit = trigger && target.closest ? (target.closest(trigger) as HTMLElement | null) : null
    if (trigger && !hit) return
    fired.current = hit
    const over = hit && hit.dataset.holdMs != null ? { ms: +hit.dataset.holdMs } : null
    try {
      e.currentTarget.setPointerCapture(e.pointerId)
    } catch {
      /* no capture available — the hold still runs, it is just fragile at the edge */
    }
    const b = e.currentTarget.getBoundingClientRect()
    begin(e.currentTarget, e.clientX - b.left, e.clientY - b.top, over)
  }

  // Space and Enter both hold: Space is the button convention, Enter is what
  // people try. e.repeat guards auto-repeat from restarting the count, and Space
  // is prevented so the page does not scroll under the gesture. Keyboard holds
  // start from the centre — there is no cursor to measure from.
  //
  // In surface mode the key event arrives from whichever verb has focus, so the
  // gesture is resolved the same way a press is: from the trigger under it. A
  // surface carrying several verbs at different prices cannot guess which one a
  // keypress meant, so with nothing focused and more than one trigger the hold
  // does not start at all — a completed gesture that fires nothing is worse
  // than one that never began.
  const key = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key !== ' ' && e.key !== 'Enter') return
    let hit: HTMLElement | null = null
    if (trigger) {
      const host = e.currentTarget
      const active =
        typeof document !== 'undefined' ? (document.activeElement as HTMLElement | null) : null
      hit = active && active.closest ? (active.closest(trigger) as HTMLElement | null) : null
      if (!hit && active && host.contains(active)) {
        const all = host.querySelectorAll(trigger)
        if (all.length === 1) hit = all[0] as HTMLElement
      }
      if (!hit) return
    }
    e.preventDefault()
    if (e.repeat) return
    fired.current = hit
    begin(
      e.currentTarget,
      null,
      null,
      hit && hit.dataset.holdMs != null ? { ms: +hit.dataset.holdMs } : null,
    )
  }

  const holding = step > 0 && !done
  const r = ((done ? steps : step) / steps) * at.max

  // In surface mode with a trigger the WRAPPER IS NOT THE CONTROL — each trigger
  // is, and each carries its own name and tab stop. So the wrapper renders as a
  // plain div and claims no name: as a tabIndex={-1} <button aria-label> it was
  // an unreachable control announcing "Hold to save" over a bar whose visible
  // verb read "Hold to remove", which is WCAG 2.5.3 and, on an irreversible
  // verb, a lie about what is about to happen. It also nested focusable
  // role="button" spans inside a button.
  const delegated = variant === 'surface' && !!trigger

  // The element type is decided at runtime, so it is aliased rather than
  // branched: two nearly-identical JSX trees would drift, and every prop below
  // applies to both.
  const Root = (delegated ? 'div' : 'button') as 'button'

  return (
    <Root
      className={('hld' + (holding ? ' on' : '') + (done ? ' done' : '') + ' ' + className).trim()}
      data-tone={tone}
      data-variant={variant}
      disabled={delegated ? undefined : disabled}
      aria-disabled={delegated && disabled ? true : undefined}
      type={delegated ? undefined : 'button'}
      tabIndex={delegated ? -1 : undefined}
      aria-label={delegated ? undefined : label + ' — press and hold to confirm'}
      onPointerDown={start}
      onPointerUp={stop}
      onPointerCancel={stop}
      onKeyDown={key}
      onKeyUp={stop}
      onBlur={stop}
      style={
        {
          '--hx': at.x + '%',
          '--hy': at.y + '%',
          '--r': r.toFixed(1) + 'px',
          ...style,
        } as CSSProperties
      }
    >
      <span className="hld-x1" />
      <span className="hld-x2" />
      <span className="hld-wave" />
      <span className="hld-front" />
      {children ? (
        <span className="hld-face">{children}</span>
      ) : (
        <span className="hld-face">
          <span className="hld-lbl">{done ? doneLabel : holding ? holdingLabel : label}</span>
          {showCount && holding ? <span className="hld-count">{step + '/' + steps}</span> : null}
        </span>
      )}
    </Root>
  )
}

export default Hold
