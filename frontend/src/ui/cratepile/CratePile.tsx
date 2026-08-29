import { useEffect, useState } from 'react'
import type { CSSProperties } from 'react'
import { FlapCount } from '../flapcount/FlapCount'
import { compactCount } from './compactCount'
import './cratepile.css'

// CratePile — a standing count, drawn as a 2.5D pile.
//
// For a quantity that ACCUMULATES rather than resets: open pull requests,
// items in a backlog, anything that was there yesterday and will be there
// tomorrow. Put a standing figure among event figures and it gets read as an
// event count — "23 pull requests happened today" is wrong and quietly
// alarming. The pile is what says this one is a different kind of figure, and
// it says it without a word.
//
// The pile is texture with a floor, not a tally. Nobody is meant to count 23
// crates; they are meant to see a pile bigger than Friday's. That is the
// difference from one tile per item, which invites counting and then breaks at
// thirty.
//
// Projection is the Pie's, not a new one: tilt 0.44 (about 26 degrees), the
// top face lightest and the two side faces stepped down, so this and the spend
// ring look like they were drawn by the same hand. There is no fourth face and
// no back — at this angle they are never seen.

const TILT = 0.44

const SHAPE = { nx: 3, ny: 3, hw: 15, ch: 12 }

// The pile SATURATES rather than growing forever, and this is the number that
// says so. Height comes from the occupied cells, so an uncapped pile keeps
// climbing while its width stays fixed — at twelve layers it draws about 181px
// tall in a 98px-wide box, which would outgrow a live band and shove
// everything under it down the page.
//
// Four layers is the cap because the drawing was never the count: it is
// texture with a floor, and "a lot" is the most a pile can honestly say. Past
// 36 the figure carries the number alone, which it was always doing anyway.
const MAX_LAYERS = 4

function faces(cx: number, cy: number, hw: number, ch: number) {
  const hd = hw * TILT
  const f = (n: number) => Math.round(n * 10) / 10
  return {
    top: `M${f(cx)} ${f(cy - hd)}L${f(cx + hw)} ${f(cy)}L${f(cx)} ${f(cy + hd)}L${f(cx - hw)} ${f(cy)}Z`,
    left: `M${f(cx - hw)} ${f(cy)}L${f(cx)} ${f(cy + hd)}L${f(cx)} ${f(cy + hd + ch)}L${f(cx - hw)} ${f(cy + ch)}Z`,
    right: `M${f(cx)} ${f(cy + hd)}L${f(cx + hw)} ${f(cy)}L${f(cx + hw)} ${f(cy + ch)}L${f(cx)} ${f(cy + hd + ch)}Z`,
  }
}

export type CratePileProps = {
  /** The standing total. The figure abbreviates past 999 (`1.2k`, `12k`) to
   *  stay inside four glyphs; the pile saturates at 36 crates. */
  count?: number
  /** Plural noun beside the figure. */
  caption?: string
  /** Singular, used at exactly 1. Omit and `caption` is used at every count. */
  captionOne?: string | null
  /** Draws an em dash and an empty pallet: a readout with nothing behind it. */
  offline?: boolean
  /** False renders the drawing alone, with no figure and no link. */
  figure?: boolean
  /** Rendered width of the pile in px. */
  width?: number
  /** Figure size in px. Drives FlapCount's window and roll together. */
  figureSize?: number
  onOpen?: (() => void) | null
  href?: string
}

export function CratePile({
  count = 0,
  caption = 'open pull requests',
  captionOne = null,
  offline = false,
  figure = true,
  width = 98,
  figureSize = 27,
  onOpen = null,
  href = '#',
}: CratePileProps) {
  // True only while the first pile is landing — the entrance stagger belongs
  // to the first build, and a crate arriving after it is one event, not a
  // sequence.
  const [staggered, setStaggered] = useState(true)
  useEffect(() => {
    const t = setTimeout(() => setStaggered(false), 900)
    return () => clearTimeout(t)
  }, [])

  const n = offline ? 0 : Math.max(count || 0, 0)
  const hd = SHAPE.hw * TILT
  const per = SHAPE.nx * SHAPE.ny

  const cells: Array<{ i: number; j: number; k: number }> = []
  const capacity = per * MAX_LAYERS
  for (let i = 0; i < Math.min(n, capacity); i++) {
    cells.push({
      i: (i % per) % SHAPE.nx,
      j: Math.floor((i % per) / SHAPE.nx),
      k: Math.floor(i / per),
    })
  }
  // Painter order for axis-aligned boxes in isometric is (i + j + k)
  // ascending: a cube with a lower sum can never occlude one with a higher
  // sum, so one sort covers every overlap with no depth testing.
  cells.sort((a, b) => a.i + a.j + a.k - (b.i + b.j + b.k))
  const layers = Math.max(Math.ceil(cells.length / per), 1)

  // The frame is the PALLET, whatever the count. Measuring a tight box around
  // the occupied cells instead made the drawing zoom rather than grow: one
  // crate filled the whole width — near 3x the size it has in a full pile —
  // and shrank as crates arrived, which reads as the backlog getting lighter.
  // Seeded with the full footprint, a crate is the same size at every count,
  // the pile visibly fills the pallet, and the union with the cells' own
  // extents only adds the height a stacked layer earns.
  const xs: number[] = [-SHAPE.ny * SHAPE.hw, SHAPE.nx * SHAPE.hw]
  const ys: number[] = [-hd, (SHAPE.nx + SHAPE.ny - 1) * hd]
  for (const c of cells) {
    const cx = (c.i - c.j) * SHAPE.hw
    const cy = (c.i + c.j) * hd - c.k * SHAPE.ch
    xs.push(cx - SHAPE.hw, cx + SHAPE.hw)
    ys.push(cy - hd, cy + hd + SHAPE.ch)
  }
  const x0 = Math.min(...xs)
  const x1 = Math.max(...xs)
  const y0 = Math.min(...ys)
  const y1 = Math.max(...ys)

  const ghost =
    `M0 ${-hd}L${SHAPE.nx * SHAPE.hw} ${(SHAPE.nx - 1) * hd}` +
    `L${(SHAPE.nx - SHAPE.ny) * SHAPE.hw} ${(SHAPE.nx + SHAPE.ny - 1) * hd}L${-SHAPE.ny * SHAPE.hw} ${(SHAPE.ny - 1) * hd}Z`

  const pile = (
    <svg
      className="cp-svg"
      style={{ width }}
      viewBox={`${x0 - 2} ${y0 - 2} ${x1 - x0 + 4} ${y1 - y0 + 4}`}
      aria-hidden="true"
    >
      {cells.length ? null : <path className="cp-ghost" d={ghost} />}
      {cells.map((c, order) => {
        const cx = (c.i - c.j) * SHAPE.hw
        const cy = (c.i + c.j) * hd - c.k * SHAPE.ch
        const F = faces(cx, cy, SHAPE.hw, SHAPE.ch)
        // Higher crates read lighter, so the pile has a light source rather
        // than being a flat field of one colour. Mixed toward the GROUND,
        // never toward transparent: a crate has to be opaque or the pile
        // behind shows through and the whole stack reads as one translucent
        // smear.
        const lift = layers > 1 ? (c.k / (layers - 1)) * 14 : 0
        const mix = (pct: number) =>
          `color-mix(in srgb, var(--color-warm) ${Math.round(pct)}%, var(--color-ground))`
        return (
          // KEYED ON THE CELL, not on array position, and this is
          // load-bearing. A crate added at the top of the pile has a LOW
          // painter sum — (0,0,2) sums to 2 — so it sorts near the FRONT.
          // Keyed by index, every crate after it shifted one slot, React
          // re-mounted them all, and the entrance re-ran from opacity 0 behind
          // its stagger delay: a count going UP made an existing crate vanish
          // for a beat, while going down was clean because removal takes the
          // highest sum, already last.
          <g
            key={c.i + '-' + c.j + '-' + c.k}
            className="cp-crate"
            // The stagger belongs to the first build only. A crate arriving
            // later is one event, not a sequence, and inheriting a 0.4s delay
            // would make a single increment look like a pause before it
            // landed.
            style={{
              animationDelay: staggered ? `${Math.min(order * 0.018, 0.5)}s` : '0s',
            }}
          >
            <path d={F.left} fill={mix(32 + lift * 0.4)} />
            <path d={F.right} fill={mix(50 + lift * 0.6)} />
            <path d={F.top} fill={mix(74 + lift)} />
          </g>
        )
      })}
    </svg>
  )

  if (!figure) return <span className="cp cp-bare">{pile}</span>

  const word = n === 1 && captionOne ? captionOne : caption
  const linked = Boolean(onOpen) || href !== '#'
  const fig = (
    <span
      className="cp-fig"
      style={
        {
          '--flap-ink': n && !offline ? 'var(--color-ink-1)' : 'var(--color-ink-4)',
        } as CSSProperties
      }
    >
      <FlapCount
        value={offline ? null : n}
        size={figureSize}
        format={compactCount}
        label={offline ? 'not available' : n + ' ' + word}
      />
      <span className="cp-cap">{word}</span>
    </span>
  )
  return linked ? (
    <a
      className="cp"
      href={href}
      onClick={(e) => {
        // Plain primary click goes through onOpen (in-app navigation);
        // modified clicks and non-primary buttons keep the anchor's own
        // behavior, so the block still opens in a new tab like any link.
        if (!onOpen || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
        e.preventDefault()
        onOpen()
      }}
    >
      {fig}
      {pile}
    </a>
  ) : (
    <span className="cp">
      {fig}
      {pile}
    </span>
  )
}

export default CratePile
