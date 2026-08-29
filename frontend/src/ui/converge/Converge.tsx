import { useCallback, useEffect, useRef, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import { allocate, decodeInto, tones } from './convergeParts'
import './converge.css'

// Converge — many to few. One instrument for the question "what happened to
// everything that arrived?": events fan in from the left, resolve into a small
// number of named outcomes, and the counts are the data.
//
// Two honesty rules make it an instrument rather than a picture:
//   1. STRANDS ARE STRUCTURE, NOT DATA. Twenty-eight filaments stand for 312
//      events; only the counts on the right are read. Allocation lives in
//      ./convergeParts.ts — square-root scaled, floored — and nobody is meant
//      to count the strands.
//   2. THE BUILD PLAYS ON ARRIVAL, NEVER ON DATA CHANGE. Scaffold, allocate,
//      resolve, radiate — the same four beats as the build vocabulary. A value
//      that ticks moves in place.
//
// The `height` prop is a viewBox unit, NOT pixels. By default the CSS is
// width:100% / height:auto with preserveAspectRatio="none" on a 740-wide
// viewBox, so what renders is width × (height / 740) — a caller wanting a
// fixed pixel height must measure and convert. `fill` is the other regime:
// the CONTAINER owns the height and the SVG stretches into it.

export interface ConvergeOutcome {
  /** Sentence-case name, e.g. "filtered by rules". Never a badge word. */
  name: string
  /** The count. This is the data; the strands are not. */
  v: number
  /** quiet = absorbed by the system · warm = settled work · ask = wants a
   *  person · cool = live right now. Only one outcome should ever be cool. */
  tone?: 'quiet' | 'warm' | 'ask' | 'cool'
}

export interface ConvergeProps {
  outcomes: ConvergeOutcome[]
  /** Total that arrived, shown as a mono footer. Omit to hide the footer. */
  total?: number
  /** Filaments drawn. Structure, not data — 20 to 34 reads well. */
  strands?: number
  /** Optional headline over the fan. Off by default: it sits where there is no
   *  data, and it must never overlap the endpoints. */
  title?: string
  /**
   * Rendered before the decoded words, inside the same headline. The decode
   * scrambles a string, so a figure that changes on its own cannot live in
   * `title`: every increment would re-garble the whole sentence, and an
   * increment is not a reveal. Anything animating its own value goes here and
   * is left alone — and it should carry the headline's accessible label.
   */
  titleNode?: ReactNode
  /** Mono kicker above the title. */
  kicker?: string
  height?: number
  /**
   * Where the endpoints sit, as a [start, end] fraction of the padded plot.
   * Defaults to the full span, which is arithmetically identical to having no
   * band at all. Narrow it only when the caller needs the vacated area for
   * something else — the fan then sweeps toward one side rather than splaying
   * evenly, a different drawing worth looking at before shipping. It
   * compresses rather than translating: the last lane already sits near the
   * plot floor, so there is nowhere to shift the whole set down to.
   */
  endpointBand?: [number, number]
  /**
   * Fill the container's height instead of deriving it from width.
   *
   * Off by default: the SVG is `height: auto`, so its rendered height is
   * width × (height / 740) and a caller wanting a fixed pixel height must
   * measure the width and solve for the viewBox — JS in the resize path, and a
   * height that can only land a frame after the width settles.
   *
   * On, the SVG stretches into the container and width and height resolve in
   * one layout pass with no JS at all, so a rail animating open reshapes the
   * fan continuously rather than snapping at the end. THE CALLER'S CONTAINER
   * MUST BE `position: relative` WITH A DEFINITE HEIGHT — fill positions
   * itself absolutely into it, because `height: 100%` cannot resolve through a
   * mount wrapper and an SVG at auto height takes its intrinsic ratio instead,
   * scaling as a rectangle.
   */
  fill?: boolean
  replayOnClick?: boolean
  style?: CSSProperties
}

const W = 740
const XC = 430
const XB = 548
const XL = 596
const BARW = 36

export function Converge({
  outcomes = [],
  total,
  strands = 28,
  title,
  titleNode,
  kicker,
  height = 190,
  endpointBand = [0, 1],
  fill = false,
  replayOnClick = true,
  style,
}: ConvergeProps) {
  const ref = useRef<HTMLDivElement | null>(null)
  const [hot, setHot] = useState<number | null>(null)
  // The build gate is STATE, not an imperative class: hover re-renders this
  // component, and a React-owned className would wipe an imperatively added
  // one — the instrument would erase itself the first time it was touched.
  // With no IntersectionObserver (jsdom), the gate opens at mount: every
  // element rests at opacity 0, so an ungated instrument is an invisible one.
  const [built, setBuilt] = useState(() => !('IntersectionObserver' in window))
  const [gen, setGen] = useState(0)
  const titleRef = useRef<HTMLSpanElement | null>(null)

  // Replay restarts the running CSS animations in place — cancel + play on the
  // real Animation objects — rather than remounting or dropping the .charting
  // gate. Every element rests at opacity 0, so any frame without the class, or
  // any frame between unmount and paint, is a frame with no instrument in it.
  const play = useCallback(() => {
    const el = ref.current
    if (!el) return
    if (!built) {
      setBuilt(true)
      return
    }
    if (el.getAnimations) {
      for (const a of el.getAnimations({ subtree: true })) {
        a.cancel()
        a.play()
      }
    }
    setGen((g) => g + 1)
  }, [built])

  useEffect(() => {
    const el = ref.current
    if (!el) return
    if (!('IntersectionObserver' in window)) return
    const io = new IntersectionObserver(
      (es) => {
        for (const e of es) if (e.isIntersecting) play()
      },
      { threshold: 0.2 },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [play])

  useEffect(() => {
    if (!built || !title) return
    let stop = () => {}
    const t = setTimeout(() => {
      stop = decodeInto(titleRef.current, title, 680)
    }, 480)
    return () => {
      clearTimeout(t)
      stop()
    }
  }, [built, gen, title])

  const pad = 24
  const n = outcomes.length || 1
  // ONE place computes a lane, and the strands, the bars and the labels all
  // read it — so a band change moves the whole drawing consistently and there
  // is no second copy of this to fall out of step. At the default [0, 1] this
  // is arithmetically identical to top = pad, span = height - pad*2.
  const bandStart = Math.max(0, Math.min(1, endpointBand[0]))
  const bandEnd = Math.max(bandStart, Math.min(1, endpointBand[1]))
  const inner = height - pad * 2
  const laneTop = pad + inner * bandStart
  const laneSpan = inner * (bandEnd - bandStart)
  const yFor = (i: number) => laneTop + ((i + 0.5) * laneSpan) / n

  // Endpoints radiate in a scattered order, not top to bottom: they resolved
  // independently, and a tidy downward cascade would imply a sequence.
  const slot: number[] = []
  outcomes
    .map((_, i) => i)
    .sort((a, b) => ((a * 7919) % n) - ((b * 7919) % n))
    .forEach((oi, k) => {
      slot[oi] = k
    })

  const counts = allocate(outcomes, strands)
  const list: number[] = []
  counts.forEach((c, oi) => {
    for (let k = 0; k < c; k++) list.push(oi)
  })
  // A deterministic scatter, so the filaments cross rather than reading as
  // four combed bundles. Same input, same picture, every render.
  const seats = list
    .map((oi, i) => ({ oi, key: (i * 7919) % list.length }))
    .sort((a, b) => a.key - b.key)
  const step = list.length > 1 ? (height - 12) / (list.length - 1) : 0

  const d = (y0: number, yt: number) =>
    `M0,${y0.toFixed(1)} C ${XC * 0.35},${y0.toFixed(1)} ${XC * 0.62},${yt.toFixed(1)} ${XC},${yt.toFixed(1)} H ${XB}`

  const iVar = (i: number) => ({ '--i': i }) as CSSProperties

  return (
    <div
      ref={ref}
      className={
        'conv' +
        (fill ? ' conv-fill' : '') +
        (built ? ' charting' : '') +
        (hot !== null ? ' dimmed' : '')
      }
      onClick={replayOnClick ? play : undefined}
      title={replayOnClick ? 'Replay the build' : undefined}
      style={{ cursor: replayOnClick ? 'pointer' : undefined, ...style }}
      onPointerLeave={() => setHot(null)}
    >
      {(title || titleNode) && (
        <div className="convtitle">
          {kicker && (
            <span className="k" aria-hidden="true">
              {kicker}
            </span>
          )}
          <span
            className="t"
            style={{ display: 'flex', alignItems: 'baseline', gap: 9, flexWrap: 'wrap' }}
          >
            {titleNode}
            {/* Scrambling glyphs, hidden from a reader who cannot watch them
                resolve. When a titleNode is present it carries the label for
                the whole headline; the chart's own role=img names the data
                either way. */}
            {title && (
              <span ref={titleRef} aria-hidden="true">
                {title}
              </span>
            )}
          </span>
        </div>
      )}
      <div className="convplot" style={{ '--cvr': height / W } as CSSProperties}>
        <svg
          viewBox={`0 0 ${W} ${height}`}
          preserveAspectRatio="none"
          role="img"
          aria-label={`${total != null ? total : ''} resolving into ${outcomes.map((o) => o.v + ' ' + o.name).join(', ')}`}
        >
          <g className="crule">
            <line x1="120" y1="0" x2="120" y2={height} />
            <line x1="280" y1="0" x2="280" y2={height} />
            <line x1={XC} y1="0" x2={XC} y2={height} />
          </g>
          <g className="cstrands">
            {seats.map((s, i) => (
              <path
                key={'s' + i}
                className={'cstrand ' + tones(outcomes[s.oi].tone) + (hot === s.oi ? ' hot' : '')}
                d={d(6 + i * step, yFor(s.oi))}
                pathLength="1"
                style={iVar(i)}
              />
            ))}
            {seats.map((s, i) => (
              <path
                key={'h' + i}
                className="chit"
                d={d(6 + i * step, yFor(s.oi))}
                onPointerEnter={() => setHot(s.oi)}
              />
            ))}
          </g>
          <g className="cends">
            {outcomes.map((o, i) => (
              <g
                key={o.name}
                className={'cend ' + tones(o.tone) + (hot === i ? ' hot' : '')}
                style={iVar(slot[i])}
                onPointerEnter={() => setHot(i)}
              >
                <rect className="cbar" x={XB} y={yFor(i) - 1.4} width={BARW} height="2.8" />
              </g>
            ))}
          </g>
        </svg>
        <div className="cendlabs">
          {outcomes.map((o, i) => (
            <div
              key={o.name}
              className={'cendlab ' + tones(o.tone) + (hot === i ? ' hot' : '')}
              style={{
                ...iVar(slot[i]),
                top: ((yFor(i) / height) * 100).toFixed(3) + '%',
                left: ((XL / W) * 100).toFixed(3) + '%',
              }}
              onPointerEnter={() => setHot(i)}
            >
              <span className="cval">{o.v}</span>
              <span className="cname">{o.name}</span>
            </div>
          ))}
        </div>
      </div>
      {total != null && (
        <div className="convfoot">
          <span className="n">{total}</span> in · {outcomes.length} outcomes
        </div>
      )}
    </div>
  )
}

export default Converge
