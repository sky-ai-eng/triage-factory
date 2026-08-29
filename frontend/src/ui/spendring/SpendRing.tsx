import { useEffect, useState } from 'react'
import type { CSSProperties } from 'react'
import { shortModelName } from './shortModelName'
import './spendring.css'

// SpendRing — a total that decomposes on hover, and no legend ever.
//
// The legend is the whole design decision. A permanent one is a PROMISE OF
// CONTENT: on a zero-spend day it renders blank rows and reads as broken
// rather than as quiet. So the split lives in the ring's own hole — hovering a
// band swaps the total for that model's figure and names it. One readout, two
// things it can say, and nothing reserved for a list that may have nothing in
// it.
//
// A flat ring rather than Pie: Pie is an extruded isometric slab with its own
// legend and it wants a whole column's width. This has to sit in a live band
// beside a row list.
//
// Hover is an accelerator, not the only route. The ring is a link to the usage
// page, because there is no hover on touch and none on a keyboard, and a split
// reachable only by pointer is a split most people never see.

const C = 2 * Math.PI * 40 // circumference at r=40, the viewBox's radius

// Cents below a hundred, none above it. That is not a space trick, it is what
// the figure is for: at $4.20 the cents are the number, and at $1,248 they are
// noise nobody reads and two characters the hole does not have. A truncated
// money figure would be a lie, so precision drops rather than the string being
// clipped — and it is ONE rule at every size, or a ring reads differently at
// 104 than at 168 and they stop being the same component.
//
// Past about seven characters the ring needs to be bigger. That is a caller
// decision, and `format` is the override for anyone who disagrees.
const money = (v: number) =>
  v < 100 ? '$' + v.toFixed(2) : '$' + Math.round(v).toLocaleString('en-US')

export type SpendRingModel = {
  /** Shown uppercased in the hole while the band is hovered. */
  name: string
  /** The amount. Bands are proportional to it. */
  v: number
}

export type SpendRingProps = {
  models?: SpendRingModel[]
  /** Draws `--` and an empty track: a readout with nothing behind it. */
  offline?: boolean
  /** Outer size in px. The ring is square. */
  size?: number
  /** The standing caption under the total, replaced by a model name on hover.
   *  It names the timeframe because nothing else does — the component ships
   *  with no section header over it, deliberately. */
  caption?: string
  /** How a model's name is shortened for the hole. See `shortModelName`. */
  shorten?: (name: string) => string
  /** How an amount is written. */
  format?: (v: number) => string
  onOpen?: (() => void) | null
  href?: string
}

export function SpendRing({
  models = [],
  offline = false,
  size = 132,
  caption = 'SPENT TODAY',
  shorten = shortModelName,
  format = money,
  onOpen = null,
  href = '#',
}: SpendRingProps) {
  const [hover, setHover] = useState(-1)
  const [built, setBuilt] = useState(false)

  // Two frames, not one. The first paint has to land with the dasharray at
  // zero for the browser to have a value to transition FROM; setting it in the
  // same frame is indistinguishable from never animating.
  useEffect(() => {
    let b = 0
    const a = requestAnimationFrame(() => {
      b = requestAnimationFrame(() => setBuilt(true))
    })
    return () => {
      cancelAnimationFrame(a)
      cancelAnimationFrame(b)
    }
  }, [])

  const live = !offline && models.length > 0
  const h = live ? hover : -1
  const open = h >= 0
  const total = models.reduce((a, m) => a + m.v, 0) || 1

  const shade = (i: number) =>
    `color-mix(in srgb, var(--color-warm) ${Math.round(100 - (i / Math.max(models.length, 1)) * 58)}%, var(--color-ground))`

  let acc = 0
  const segs = live
    ? models.map((m, i) => {
        const len = (m.v / total) * C
        const off = -acc
        acc += len
        return { i, len, off, color: shade(i) }
      })
    : []

  const label = open ? shorten(models[h].name).toUpperCase() : caption
  const body = (
    <>
      <svg className="sr-svg" viewBox="0 0 104 104">
        <circle className="sr-track" cx="52" cy="52" r="40" />
        {segs.map((s) => (
          <circle
            key={s.i}
            className="sr-seg"
            cx="52"
            cy="52"
            r="40"
            stroke={s.color}
            opacity={open && h !== s.i ? 0.38 : 1}
            style={{
              // The hovered band THICKENS rather than changing colour: colour
              // already carries which model it is, so weight is the only
              // channel left free.
              strokeWidth: h === s.i ? 17 : 14,
              strokeDasharray: `${built ? Math.max(s.len - 2, 0) : 0} ${C}`,
              strokeDashoffset: s.off,
              // A transition, not a keyframe: the target length is the same
              // value hover re-renders, so a keyframe would restart every time
              // a pointer crossed a band. A transition just stays put.
              transitionDelay: `${(0.06 + s.i * 0.13).toFixed(2)}s, 0s, 0s`,
            }}
            onMouseEnter={() => setHover(s.i)}
            onMouseLeave={() => setHover(-1)}
          />
        ))}
      </svg>
      <span className="sr-hole">
        {/* Offline is not zero: a readout with nothing behind it shows a dash
            rather than the last figure it happened to hold. */}
        <span className="sr-total">
          {offline ? '--' : open ? format(models[h].v) : format(live ? total : 0)}
        </span>
        <span className="sr-cap" data-open={open || undefined}>
          {label}
        </span>
      </span>
    </>
  )

  // Both readouts scale with the ring. They were fixed at 16 and 8.5, so a
  // 104px ring carried a 168px ring's type and a three-digit total ran through
  // the band on both sides.
  //
  // Two ratios, not one. The caption is already near the floor of legible
  // mono, so scaling it at the figure's rate would make a small ring
  // unreadable before the figure was. It floors at 7.5 rather than 8: at 8 the
  // caption came out proportionally LARGER on a small ring, and "SPENT TODAY"
  // — the standing label, which must always fit — ellipsed at 104px.
  const style = {
    width: size,
    '--sr-fig': Math.max(11, Math.round(size / 8.25)) + 'px',
    '--sr-cap': Math.max(7.5, Math.round((size / 8.25) * 0.53 * 10) / 10) + 'px',
  } as CSSProperties

  return onOpen || href !== '#' ? (
    <a
      className="sr"
      style={style}
      href={href}
      onClick={(e) => {
        // Plain primary click goes through onOpen (in-app navigation);
        // modified clicks and non-primary buttons keep the anchor's own
        // behavior, so the ring still opens in a new tab like any link.
        if (!onOpen || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
        e.preventDefault()
        onOpen()
      }}
    >
      {body}
    </a>
  ) : (
    <span className="sr" style={style}>
      {body}
    </span>
  )
}

export default SpendRing
