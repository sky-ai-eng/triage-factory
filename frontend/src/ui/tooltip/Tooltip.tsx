import { useEffect, useId, useRef, useState } from 'react'
import type { KeyboardEvent, MouseEvent, ReactNode } from 'react'
import './tooltip.css'

// Tooltip — the definition of a label, never the value of a datum.
//
// This is the SHIPPED RAIL's treatment (`.sh-tip` in shell.css). The project
// had three tooltips and no decision between them: the rail's, the product's
// Radix content spread across seven pages, and the proposal layer's, which was
// the Radix look expressed in tokens. Three is the actual defect — the rail's
// is the least chrome that still reads as a surface, so it wins and the other
// two are what this replaces.
//
// Radius 4, 11px sans, ink-1, no arrow. An arrow is a pointer to the thing the
// tooltip is already touching.
//
// ONE delay for the whole project, exported as TOOLTIP_DELAY. A product that
// ranges from 150ms to 650ms depending on which file you open is three
// different products to anyone who uses more than one page.
//
// On the leader-line rule: the design language says a hover readout should be
// a leader line, and that stands for a VALUE. The test is not the mark's size,
// it is what is missing: if the number is absent from the page, it needs a
// leader line; if the number is on the page and its UNIT is missing — a warm
// `3` beside an hourglass, where "ahead of this one" is the part nobody can
// see — that is a label wanting its definition, and this is the instrument.
//
// TWO MODES, and picking the wrong one is an accessibility bug rather than a
// styling one.
//
//   focusable (default) — the trigger is the interactive thing. It takes a tab
//     stop and `aria-describedby`, and the tooltip is a real `role="tooltip"`.
//     Use it on a badge, a bare glyph, a label in running text.
//
//   focusable={false} — the trigger sits INSIDE something already interactive,
//     which is the case on a run row: the row is an `<a>`. Then a tab stop
//     here is illegal (an anchor may not contain interactive content) and
//     pointless (the row already has focus), so the host takes neither and the
//     tooltip becomes `aria-hidden` scenery. The caller owes the same words to
//     assistive technology by another route — an `aria-label` on the mark is
//     the usual one. That is the answer Table.md already gives for its
//     clipped-cell tooltip: a visual echo of text that is present anyway
//     should not be announced twice.
//
// Either way the host swallows its own click. On touch there is no hover, so a
// tap is the only route to the hint; without this the tap navigates the row
// and the tooltip is unreachable on a phone.

/** The project's one tooltip delay. */
export const TOOLTIP_DELAY = 200

const SIDES = {
  top: { bottom: 'calc(100% + 7px)', left: '50%', transform: 'translateX(-50%)' },
  bottom: { top: 'calc(100% + 7px)', left: '50%', transform: 'translateX(-50%)' },
  // The rail's own direction. Clear of the trigger rather than touching it: a
  // hint that touches the thing it names reads as attached to it.
  right: { left: 'calc(100% + 9px)', top: '50%', transform: 'translateY(-50%)' },
  left: { right: 'calc(100% + 9px)', top: '50%', transform: 'translateY(-50%)' },
} as const

export type TooltipProps = {
  /** The trigger. Made focusable so the hint has a keyboard route. */
  children?: ReactNode
  /** Omit or pass nothing and the trigger stays inert, focus included. */
  content?: ReactNode
  /** `right` is the shipped rail's own direction. */
  side?: keyof typeof SIDES
  /**
   * Leave this alone. It exists for the rare trigger that needs a longer
   * guard, not for per-page taste — one delay across the project is the point.
   */
  delay?: number
  /**
   * Allow two lines, capped at 220px. Off by default: a hint that needs a
   * paragraph is documentation, and a hint that wraps unpredictably in a dense
   * row changes the row's height on hover.
   */
  wrap?: boolean
  /** Suppresses the tooltip and the trigger's focusability together. */
  disabled?: boolean
  /** Whether the trigger is the interactive thing. See the note above. */
  focusable?: boolean
}

export function Tooltip({
  children,
  content,
  side = 'top',
  delay = TOOLTIP_DELAY,
  wrap = false,
  disabled = false,
  focusable = true,
}: TooltipProps) {
  const [open, setOpen] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const id = useId()
  const live = !disabled && content != null && content !== ''

  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current)
    },
    [],
  )

  const show = () => {
    if (!live) return
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => setOpen(true), delay)
  }
  const hide = () => {
    if (timer.current) clearTimeout(timer.current)
    setOpen(false)
  }

  // Focus opens with no delay. A delay keeps a tooltip from flickering under a
  // pointer crossing the screen; a keyboard never crosses anything, so waiting
  // only makes the hint feel broken.
  const focus = () => {
    if (!live) return
    if (timer.current) clearTimeout(timer.current)
    setOpen(true)
  }

  // A tap toggles. Stopping the event is the load-bearing half: this mark is
  // often inside a link, and a tap that navigates instead of explaining leaves
  // touch with no route to the hint at all.
  const tap = (e: MouseEvent) => {
    if (!live) return
    e.preventDefault()
    e.stopPropagation()
    if (timer.current) clearTimeout(timer.current)
    setOpen((v) => !v)
  }

  const keyboard = live && focusable

  return (
    <span
      className="tip-host"
      onMouseEnter={show}
      onMouseLeave={hide}
      onClick={live ? tap : undefined}
      onFocus={keyboard ? focus : undefined}
      onBlur={keyboard ? hide : undefined}
      tabIndex={keyboard ? 0 : undefined}
      aria-describedby={keyboard && open ? id : undefined}
      onKeyDown={
        keyboard
          ? (e: KeyboardEvent) => {
              if (e.key === 'Escape') hide()
            }
          : undefined
      }
    >
      {children}
      {open ? (
        <span
          id={keyboard ? id : undefined}
          role={keyboard ? 'tooltip' : undefined}
          aria-hidden={keyboard ? undefined : 'true'}
          className="tip"
          data-side={side}
          style={{
            ...(SIDES[side] ?? SIDES.top),
            ...(wrap ? { whiteSpace: 'normal', width: 'max-content', maxWidth: 220 } : null),
          }}
        >
          {content}
        </span>
      ) : null}
    </span>
  )
}

export default Tooltip
