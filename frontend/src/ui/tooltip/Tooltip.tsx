import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import type { CSSProperties, KeyboardEvent, MouseEvent, ReactNode } from 'react'
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

/** Clearance kept between the bubble and the edge of the page. */
const EDGE = 8
const FLIP = { left: 'right', right: 'left', top: 'top', bottom: 'bottom' } as const

/** The measured correction keeping an open bubble on the page. One of three
 *  shapes, never combined — see the layout effect below for the order. */
type Adjust = { dx?: number; side?: keyof typeof SIDES; wrap?: number }

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
   * Allow wrapping, capped at 220px — or at the given width, for the rare
   * hint that is a small panel rather than a phrase. Off by default: a hint
   * that needs a paragraph is documentation, and a hint that wraps
   * unpredictably in a dense row changes the row's height on hover.
   */
  wrap?: boolean | number
  /** Suppresses the tooltip and the trigger's focusability together. */
  disabled?: boolean
  /** Whether the trigger is the interactive thing. See the note above. */
  focusable?: boolean
  /** Merged onto the host, for a trigger that has to participate in its
   *  parent's layout (a flex row's filling cell, say). Layout only — the
   *  hint's own surface is not restylable. */
  className?: string
}

export function Tooltip({
  children,
  content,
  side = 'top',
  delay = TOOLTIP_DELAY,
  wrap = false,
  disabled = false,
  focusable = true,
  className = '',
}: TooltipProps) {
  const [open, setOpen] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const pop = useRef<HTMLSpanElement | null>(null)
  const id = useId()
  const live = !disabled && content != null && content !== ''

  // Kept inside the page.
  //
  // A centered bubble on a trigger near either edge hangs off the page, and
  // the half that hangs off is unreadable — on a run row the trigger is a 14px
  // glyph 39px from the left of a list that may itself be at the left of the
  // window, so a 300px reference centered on it starts well outside. Measured
  // rather than guessed: the widths involved are content widths, and the
  // trigger's distance from the edge is not knowable from CSS.
  //
  // Three corrections, in order of how much they change the tooltip:
  //   - a SHIFT along the page for a top/bottom bubble, which keeps the bubble
  //     where it was pointing and only slides it clear;
  //   - a FLIP to the opposite side for a left/right bubble, which cannot
  //     shift sideways without covering the thing it names;
  //   - a WRAP, if it is wider than the page can hold at any position — then
  //     there is nowhere to slide it to and the line has to break instead.
  // Reset at close (hide/tap), not here: opening therefore mounts a fresh
  // bubble with no correction on it, so the measurement is always of the
  // uncorrected position; once a correction is applied it stands until close,
  // and this cannot oscillate.
  const [adj, setAdj] = useState<Adjust | null>(null)
  useLayoutEffect(() => {
    if (!open || adj) return
    const el = pop.current
    const vw = document.documentElement.clientWidth
    // No page to measure against (an environment without layout): fits.
    if (!el || !vw) return
    const r = el.getBoundingClientRect()
    const room = vw - EDGE * 2
    const fix: Adjust | null =
      r.width > room
        ? { wrap: room }
        : r.left >= EDGE && r.right <= vw - EDGE
          ? null
          : side === 'left' || side === 'right'
            ? { side: FLIP[side] }
            : { dx: Math.round(r.left < EDGE ? EDGE - r.left : vw - EDGE - r.right) }
    if (!fix) return
    // Measure-then-correct is what a layout effect is for: the write happens
    // before paint, so the reader never sees the uncorrected position, and the
    // `adj` guard above means it runs once per open — no cascade.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setAdj(fix)
  }, [open, content, side, adj])

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
    setAdj(null)
  }

  // Focus opens with no delay. A delay keeps a tooltip from flickering under a
  // pointer crossing the screen; a keyboard never crosses anything, so waiting
  // only makes the hint feel broken.
  //
  // KEYBOARD focus only, though — marked by the absence of a preceding
  // pointerdown. A pointer's focus is a side effect of its press, and the
  // pointer already has its own routes in (hover, and the tap below); opening
  // on it anyway made the press a flash, focus opening the hint and the click
  // half of the same press toggling it straight shut.
  const pointer = useRef(false)
  const press = () => {
    pointer.current = true
  }
  const focus = () => {
    const fromPointer = pointer.current
    pointer.current = false
    if (!live || fromPointer) return
    if (timer.current) clearTimeout(timer.current)
    setOpen(true)
  }

  // A tap toggles. Stopping the event is the load-bearing half: this mark is
  // often inside a link, and a tap that navigates instead of explaining leaves
  // touch with no route to the hint at all.
  const tap = (e: MouseEvent) => {
    // The press flag is consumed by focus on a fresh press and cleared here
    // for a press on an already-focused host, which fires no focus event.
    pointer.current = false
    if (!live) return
    e.preventDefault()
    e.stopPropagation()
    if (timer.current) clearTimeout(timer.current)
    setOpen((v) => !v)
    // Closing discards the correction; opening starts from none anyway.
    setAdj(null)
  }

  const keyboard = live && focusable
  const placed = adj?.side ?? side

  return (
    <span
      className={('tip-host ' + className).trim()}
      onMouseEnter={show}
      onMouseLeave={hide}
      onClick={live ? tap : undefined}
      onPointerDown={keyboard ? press : undefined}
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
          ref={pop}
          id={keyboard ? id : undefined}
          role={keyboard ? 'tooltip' : undefined}
          aria-hidden={keyboard ? undefined : 'true'}
          className="tip"
          data-side={placed}
          style={
            {
              ...(SIDES[placed] ?? SIDES.top),
              ...(wrap
                ? {
                    whiteSpace: 'normal',
                    width: 'max-content',
                    maxWidth: typeof wrap === 'number' ? wrap : 220,
                  }
                : null),
              // The shift is a variable as well as a transform: the entrance
              // keyframes restate the resting transform and outrank an inline
              // one for the whole run, so a shifted bubble written inline
              // alone would animate in centered and jump sideways at the end.
              // The keyframes read the variable; the inline transform is the
              // resting position after they finish.
              ...(adj?.dx
                ? {
                    '--tip-dx': adj.dx + 'px',
                    transform: 'translateX(calc(-50% + ' + adj.dx + 'px))',
                  }
                : null),
              ...(adj?.wrap
                ? { whiteSpace: 'normal', maxWidth: adj.wrap, overflowWrap: 'anywhere' }
                : null),
            } as CSSProperties
          }
        >
          {content}
        </span>
      ) : null}
    </span>
  )
}

export default Tooltip
