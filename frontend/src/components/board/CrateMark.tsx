import type { CSSProperties } from 'react'
import './crate-mark.css'

// CrateMark — what a working run looks like on a card.
//
// A pallet takes crates one at a time. When the floor row is complete it goes
// to full warm and clears, and the crate that was stacked on it falls into the
// gap. The floor is rebuilt around that survivor, cleared the same way, and the
// pallet stands empty until the cycle starts again.
//
// Four decisions carry it:
//
//   IT BORROWS CratePile's LANGUAGE, NOT A NEW ONE. Tilt 0.44, three faces and
//   no back, the top lightest and the sides stepped down, every face mixed
//   toward the GROUND so a crate is opaque. A pile already means accumulation
//   on this board, so a filling pallet reads as work arriving without being
//   taught, and an empty pallet is a rest state a spinner cannot have.
//
//   A CLEAR IS ONE EVENT, NOT FOUR DEPARTURES. The finished row is redrawn at
//   the top of the warm ramp and then every crate in it — and the light with
//   it — goes out on the SAME frame. A solid silhouette laid over the row would
//   erase the boxes for the length of the flash; this keeps every facet and
//   changes only the light.
//
//   ARRIVALS FALL, CLEARS STEP. A crate enters from 3px above its cell and
//   lands over 4% of the cycle, fading up across the first half of the drop and
//   accelerating into the last — it arrives out of the air rather than
//   switching on above the pile. Only the two clears step hard, which is what
//   separates a thing happening from a thing arriving.
//
//   PAINTER ORDER IS THE WHOLE DRAWING. Cells paint back to front and each one
//   is trailed by its own flash copy, because a flash belongs to ONE layer: the
//   crate standing on the back cell must cover that cell's light, and the front
//   crate must cover the standing crate. Grouping the flash as a single shape
//   over the finished row makes those two requirements contradict each other.
//
// The pile is drawn in a 22 × 21 user-unit box and scaled by `size`. Hairlines
// are authored for the default 20px and are not rescaled — a 0.7 edge that
// scales to 0.3 vanishes on a standard display.
//
// Warm is spent here. Warm elsewhere on a card means a human is needed; on
// this mark it means work landing. Do not add a second warm element to a
// working card.

const WARM = 'var(--color-warm)'
const GROUND = 'var(--color-ground)'
const LINE = 'var(--color-line-2)'

const TILT = 0.44
const HW = 5 // half-width of a crate, in user units
const CH = 4 // crate height
const HD = HW * TILT // half-depth, the isometric foreshortening

const r1 = (n: number) => Math.round(n * 10) / 10
const mix = (pct: number) => `color-mix(in srgb, ${WARM} ${pct}%, ${GROUND})`

// The hairline between crates is drawn in the GROUND colour, as cratepile.css
// draws it, and it is load-bearing: without it two adjacent top faces at the
// same shade merge into one plateau and the pile stops reading as discrete
// boxes.
const EDGE = { stroke: GROUND, strokeWidth: 0.7, strokeLinejoin: 'round' as const }

function faces(cx: number, cy: number) {
  const hw = HW
  const ch = CH
  const hd = HD
  return {
    top: `M${r1(cx)} ${r1(cy - hd)}L${r1(cx + hw)} ${r1(cy)}L${r1(cx)} ${r1(cy + hd)}L${r1(cx - hw)} ${r1(cy)}Z`,
    left: `M${r1(cx - hw)} ${r1(cy)}L${r1(cx)} ${r1(cy + hd)}L${r1(cx)} ${r1(cy + hd + ch)}L${r1(cx - hw)} ${r1(cy + ch)}Z`,
    right: `M${r1(cx)} ${r1(cy + hd)}L${r1(cx + hw)} ${r1(cy)}L${r1(cx + hw)} ${r1(cy + ch)}L${r1(cx)} ${r1(cy + hd + ch)}Z`,
  }
}

// A crate SITS ON the pallet: its bottom face is the pallet plane, so the box is
// lifted one crate-height above its grid point rather than hanging below it.
const cellAt = (i: number, j: number, k: number) => ({
  cx: (i - j) * HW,
  cy: (i + j) * HD - (k + 1) * CH,
})
// The four floor cells, in painter order.
const FLOOR = [cellAt(0, 0, 0), cellAt(1, 0, 0), cellAt(0, 1, 0), cellAt(1, 1, 0)]

const REST = [32, 50, 74] // left, right, top — the pile's own light model
const LIT = [70, 86, 100] // the same crate at the top of the warm ramp

function Crate({
  cls,
  pos,
  anim,
  shade,
}: {
  cls: string
  pos: { cx: number; cy: number }
  /** The crate's keyframe animation, or none for a still. Inline, so it has
   *  to be absent rather than overridden: an element's own style beats any
   *  stylesheet rule, whatever the selector. */
  anim?: string
  shade: number[]
}) {
  const F = faces(pos.cx, pos.cy)
  return (
    <g className={cls} style={anim ? { animation: anim } : undefined}>
      <path d={F.left} fill={mix(shade[0])} {...EDGE} />
      <path d={F.right} fill={mix(shade[1])} {...EDGE} />
      <path d={F.top} fill={shade[2] === 100 ? WARM : mix(shade[2])} {...EDGE} />
    </g>
  )
}

export type CrateMarkProps = {
  /** Rendered width in px. The pile is drawn in a 22 × 21 box; height follows. Default 20. */
  size?: number
  /** Seconds for one full cycle — build, clear, fall, rebuild, clear, rest. Default 7.2. */
  duration?: number
  /** `idle` holds the pile built and unlit rather than freezing it mid-cycle,
   *  where it could be an empty pallet. A stopped factory looks stopped. */
  state?: 'running' | 'idle'
  /** Give it a title only where the mark is the sole statement of liveness.
   *  Otherwise it is decorative. */
  title?: string
  className?: string
  style?: CSSProperties
}

export function CrateMark({
  size = 20,
  duration = 7.2,
  state = 'running',
  title,
  className = '',
  style,
}: CrateMarkProps) {
  const cycle = `${duration}s`
  // Idle holds the pile built and unlit: no crate animates, and the flash and
  // rebuild copies are hidden by the stylesheet, which leaves the floor row
  // and the crate stacked on the back cell at rest.
  const idle = state === 'idle'
  const a = (name: string) => (idle ? undefined : `tf-crate-${name} ${cycle} linear infinite both`)
  // Row A's crates and Row B's crates share the two flash keyframes, so the
  // whole row lights and leaves on one frame however many crates are in it.
  // Each cell is trailed by its own flash copy — painter order, see above.
  const cells: Array<{
    key: string
    cls: string
    pos: { cx: number; cy: number }
    anim?: string
    flash?: string
    flashPos: { cx: number; cy: number }
  }> = [
    {
      key: 'a',
      cls: 'cm-crate',
      pos: FLOOR[0],
      anim: a('1'),
      flash: a('rowa'),
      flashPos: FLOOR[0],
    },
    {
      key: 'b',
      cls: 'cm-crate',
      pos: FLOOR[1],
      anim: a('2'),
      flash: a('rowa'),
      flashPos: FLOOR[1],
    },
    {
      key: 'c',
      cls: 'cm-crate',
      pos: FLOOR[2],
      anim: a('3'),
      flash: a('rowa'),
      flashPos: FLOOR[2],
    },
    // The survivor, and the back cell's SECOND-row light: by the time row B
    // lights, this crate has fallen into that cell.
    {
      key: 'top',
      cls: 'cm-crate',
      pos: cellAt(0, 0, 1),
      anim: a('top'),
      flash: a('rowb'),
      flashPos: FLOOR[0],
    },
    {
      key: 'e',
      cls: 'cm-crate',
      pos: FLOOR[3],
      anim: a('4'),
      flash: a('rowa'),
      flashPos: FLOOR[3],
    },
    {
      key: 'f',
      cls: 'cm-crate cm-rebuild',
      pos: FLOOR[1],
      anim: a('5'),
      flash: a('rowb'),
      flashPos: FLOOR[1],
    },
    {
      key: 'g',
      cls: 'cm-crate cm-rebuild',
      pos: FLOOR[2],
      anim: a('6'),
      flash: a('rowb'),
      flashPos: FLOOR[2],
    },
    {
      key: 'h',
      cls: 'cm-crate cm-rebuild',
      pos: FLOOR[3],
      anim: a('7'),
      flash: a('rowb'),
      flashPos: FLOOR[3],
    },
  ]

  return (
    <svg
      className={`tf-crates ${className}`.trim()}
      data-state={state}
      width={size}
      height={Math.round((size * 21) / 22)}
      viewBox="-11 -13.7 22 21"
      role={title ? 'img' : 'presentation'}
      aria-hidden={title ? undefined : true}
      style={{ display: 'block', overflow: 'visible', ...style }}
    >
      {title ? <title>{title}</title> : null}
      {/* The pallet exactly as cratepile.css draws its empty state: a DASHED
          structural hairline. Solid, it reads as a stray dark line over the
          pile rather than as the floor the crates land on. */}
      <path
        d={`M0 ${r1(-HD)}L${2 * HW} ${r1(HD)}L0 ${r1(3 * HD)}L${-2 * HW} ${r1(HD)}Z`}
        fill="none"
        stroke={LINE}
        strokeWidth={1}
        strokeDasharray="3 3"
      />
      {cells.map((c) => (
        <g key={c.key}>
          <Crate cls={c.cls} pos={c.pos} anim={c.anim} shade={REST} />
          <Crate cls="cm-flash" pos={c.flashPos} anim={c.flash} shade={LIT} />
        </g>
      ))}
    </svg>
  )
}

export default CrateMark
