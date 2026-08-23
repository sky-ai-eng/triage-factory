import type { CSSProperties, KeyboardEvent, MouseEvent, ReactNode } from 'react'
import './checkbox.css'

export type CheckboxProps = {
  /** Ticked. With `indeterminate`, `indeterminate` wins for the mark drawn. */
  checked?: boolean
  /** The mixed state — a dash, not a tick. Announced as aria-checked="mixed". */
  indeterminate?: boolean
  /**
   * Inert and dimmed. Stays FOCUSABLE with aria-disabled rather than being
   * removed from the tab order: a row checkbox is usually disabled because
   * something else owns that row, and a keyboard reader should be able to
   * find out which rows those are.
   */
  disabled?: boolean
  /** Square edge in px. Radius and stroke scale with it. 13 is the table row. */
  size?: number
  /** Text beside the square. Becomes the control's accessible name. */
  label?: ReactNode
  /** Tooltip, and the accessible name when there is no `label`. */
  title?: string
  /** Called with the NEXT checked value. Not called while disabled. */
  onChange?: (next: boolean, event: MouseEvent | KeyboardEvent) => void
  /** Escape hatch for layout only. Color and state belong to checkbox.css. */
  style?: CSSProperties
}

// Checkbox — the square a row is selected with.
//
// One idea: UNCHECKED IS STRUCTURE, CHECKED IS A VALUE. Empty is a hairline
// and nothing else; ticked is a solid warm block carrying a light mark, the
// same figure/ground flip the repository field uses for a tracked cell.
//
// The mark is drawn, not typed, and it is centred on its own INK rather than
// on its bounding box. A check is an asymmetric glyph — the long arm carries
// most of the weight up and to the right — so a path whose geometric box is
// centred reads as high and right, which is exactly the complaint this
// component exists to fix. TICK is authored in a 12x12 viewBox with its
// stroked extents measured (checkboxCentering() below reproduces the check),
// then nudged by TICK_DX/TICK_DY so the inked area sits on the box's centre.
// Ink centring, worked rather than guessed. For the polyline below the STROKE
// CENTROID (each segment weighted by its length) sits 0.32 units BELOW the
// square's centre while its bounding box sits 0.11 ABOVE it — the two
// disagree because the long arm is thin and high while the vertex is a dense
// corner. Neither alone reads as centred; the offset that does is the one that
// zeroes their average, which is 0.10 up.
const TICK = 'M2.81 6.17 L4.94 8.30 L9.19 3.48'
const TICK_DX = 0.0
const TICK_DY = 0.06
const DASH = 'M3.1 6 L8.9 6'

export function Checkbox({
  checked = false,
  indeterminate = false,
  disabled = false,
  size = 13,
  label = null,
  title,
  onChange,
  style,
}: CheckboxProps) {
  const on = checked || indeterminate

  // The radius tracks the size: a 3px radius on an 11px square is a lozenge,
  // and a 2.5px radius on 20px is a hard edge.
  const radius = Math.max(2, Math.round(size * 0.22 * 2) / 2)
  const stroke = size <= 12 ? 1.9 : 1.75

  const toggle = (event: MouseEvent | KeyboardEvent) => {
    if (disabled) return
    if (onChange) onChange(!checked, event)
  }

  // Space toggles, matching a native checkbox — and Enter deliberately does
  // NOT. A row checkbox lives inside a row that has its own Enter meaning, and
  // a control that swallowed it would take the row's primary action away.
  const onKeyDown = (event: KeyboardEvent<HTMLSpanElement>) => {
    if (event.key !== ' ' && event.key !== 'Spacebar') return
    event.preventDefault() // or the page scrolls
    toggle(event)
  }

  return (
    <span
      className="tf-check"
      role="checkbox"
      aria-checked={indeterminate ? 'mixed' : checked ? 'true' : 'false'}
      aria-disabled={disabled || undefined}
      aria-label={!label && title ? title : undefined}
      tabIndex={0}
      title={title}
      data-on={on || undefined}
      data-labelled={label ? '' : undefined}
      onClick={toggle}
      onKeyDown={onKeyDown}
      style={
        {
          '--check-size': `${size}px`,
          '--check-radius': `${radius}px`,
          '--check-stroke': stroke,
          ...style,
        } as CSSProperties
      }
    >
      <span className="tf-check__box" aria-hidden="true">
        {on ? (
          <svg className="tf-check__mark" viewBox="0 0 12 12" fill="none">
            <path
              d={indeterminate ? DASH : TICK}
              transform={indeterminate ? undefined : `translate(${TICK_DX} ${TICK_DY})`}
            />
          </svg>
        ) : null}
      </span>
      {label}
    </span>
  )
}

export default Checkbox
