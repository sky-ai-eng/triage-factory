import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { CSSProperties, KeyboardEvent } from 'react'
import './segmented.css'

export type SegmentedOption =
  | string
  | {
      value: string
      label: string
      /** Struck and unpickable, with the reason on `note` — a preset a policy rules out is information. */
      disabled?: boolean
      note?: string
    }

export type SegmentedProps = {
  options: SegmentedOption[]
  value: string
  onChange?: (value: string) => void
  /**
   * How the chosen segment is marked. 'spine' is the default; the rest exist
   * for a page whose neighbours already use one of the system's other marks.
   */
  variant?: 'spine' | 'tick' | 'housed' | 'plate'
  size?: 'sm' | 'md' | 'lg'
  /** Mono by default — the options are values the system reports back. `false` for words a person wrote. */
  mono?: boolean
  /** Accessible name for the group. */
  label?: string
  disabled?: boolean
  className?: string
  style?: CSSProperties
}

// Segmented — one choice from two to five, all of them visible. The sibling
// of a radio group for the case where the options are short words and the
// choice takes effect at once: appearance, an expiry preset, a scope, a lens.
//
// The mark moves; the labels do not. Whichever variant, the thing that says
// "this one" is a single element that travels from the old choice to the new
// on the content curve, so the eye reads a change of state and not a redraw.
// Its position is measured (a label's width is the font's business) and
// handed to the stylesheet as two custom properties; the stylesheet owns
// everything else. See Segmented.md.
export function Segmented({
  options,
  value,
  onChange,
  variant = 'spine',
  size = 'md',
  mono = true,
  label,
  disabled = false,
  className = '',
  style,
}: SegmentedProps) {
  const opts = options.map((o) => (typeof o === 'string' ? { value: o, label: o } : o))
  const rootRef = useRef<HTMLDivElement | null>(null)
  const refs = useRef<Record<string, HTMLSpanElement | null>>({})
  const [mark, setMark] = useState<{ x: number; w: number } | null>(null)
  // The mark's first placement is a jump, not a slide: a control that arrives
  // with its mark travelling in from zero reads as a change nobody made.
  const [settled, setSettled] = useState(false)

  const measure = useCallback(() => {
    const root = rootRef.current
    const el = refs.current[value]
    if (!root || !el) {
      setMark(null)
      return
    }
    const r = root.getBoundingClientRect()
    const e = el.getBoundingClientRect()
    setMark({ x: e.left - r.left, w: e.width })
  }, [value])

  useLayoutEffect(() => {
    // A measurement: the mark's place is the chosen label's box, which only
    // layout knows, so it is read here and written back before paint.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    measure()
  }, [measure, opts.length, size, variant])

  useEffect(() => {
    const t = setTimeout(() => setSettled(true), 0)
    return () => clearTimeout(t)
  }, [])

  useEffect(() => {
    if (typeof ResizeObserver === 'undefined' || !rootRef.current) return
    const ro = new ResizeObserver(measure)
    ro.observe(rootRef.current)
    return () => ro.disconnect()
  }, [measure])

  const pick = (v: string) => {
    if (disabled) return
    const o = opts.find((x) => x.value === v)
    if (!o || o.disabled || v === value) return
    onChange?.(v)
  }

  // Arrows move the choice and the focus together, wrapping, and skip a
  // struck option — it is information, not a stop.
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const live = opts.filter((o) => !o.disabled)
    if (!live.length) return
    const i = live.findIndex((o) => o.value === value)
    let n: (typeof live)[number] | null = null
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') n = live[(i + 1) % live.length]
    if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') n = live[(i - 1 + live.length) % live.length]
    if (e.key === 'Home') n = live[0]
    if (e.key === 'End') n = live[live.length - 1]
    if (!n) return
    e.preventDefault()
    pick(n.value)
    refs.current[n.value]?.focus()
  }

  const vars = {
    '--seg-x': mark ? mark.x + 'px' : '0px',
    '--seg-w': mark ? mark.w + 'px' : '0px',
    ...style,
  } as CSSProperties

  return (
    <div
      ref={rootRef}
      className={('seg ' + className).trim()}
      role="radiogroup"
      aria-label={label}
      aria-disabled={disabled || undefined}
      data-variant={variant}
      data-size={size}
      data-mono={mono ? '' : undefined}
      data-settled={settled ? '' : undefined}
      onKeyDown={onKey}
      style={vars}
    >
      {mark ? <span className="seg-mark" aria-hidden="true" /> : null}
      {opts.map((o) => {
        const on = o.value === value
        const off = disabled || !!o.disabled
        return (
          <span
            key={o.value}
            ref={(el) => {
              refs.current[o.value] = el
            }}
            className="seg-opt"
            role="radio"
            aria-checked={on}
            aria-disabled={off || undefined}
            data-on={on || undefined}
            data-off={off || undefined}
            data-struck={o.disabled || undefined}
            tabIndex={on ? 0 : -1}
            title={o.note}
            onClick={() => !off && pick(o.value)}
          >
            {o.label}
          </span>
        )
      })}
    </div>
  )
}

export default Segmented
