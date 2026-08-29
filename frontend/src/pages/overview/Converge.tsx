import { useCallback, useEffect, useRef, useState } from 'react'
import type { CSSProperties } from 'react'
import { allocate, decodeInto, tones } from './convergeParts'
import './converge.css'

// Converge — many to few. One instrument for the question "what happened to
// everything that arrived?": events fan in from the left, resolve into a small
// number of named outcomes, and the counts are the data.
//
// Two honesty rules make it an instrument rather than a picture:
//   1. STRANDS ARE STRUCTURE, NOT DATA. Twenty-eight filaments stand for 312
//      events; only the counts on the right are read. Allocation lives in
//      ./converge.ts — square-root scaled, floored — and nobody is meant to
//      count the strands.
//   2. THE BUILD PLAYS ON ARRIVAL, NEVER ON DATA CHANGE. Scaffold, allocate,
//      resolve, radiate — the same four beats as the build vocabulary. A value
//      that ticks moves in place.
//
// The `height` prop is a viewBox unit, NOT pixels: the CSS is width:100% /
// height:auto with preserveAspectRatio="none" on a 740-wide viewBox, so what
// renders is width × (height / 740). The page owns that conversion — it
// measures its pixel room, clamps it, and converts back.

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
  /** Mono kicker above the title. */
  kicker?: string
  height?: number
  replayOnClick?: boolean
  style?: CSSProperties
}

const W = 740
const XC = 430
const XB = 548
const XL = 596
const BARW = 36

export default function Converge({
  outcomes = [],
  total,
  strands = 28,
  title,
  kicker,
  height = 190,
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
  const yFor = (i: number) => pad + ((i + 0.5) * (height - pad * 2)) / n

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
      className={'conv' + (built ? ' charting' : '') + (hot !== null ? ' dimmed' : '')}
      onClick={replayOnClick ? play : undefined}
      title={replayOnClick ? 'Replay the build' : undefined}
      style={{ cursor: replayOnClick ? 'pointer' : undefined, ...style }}
      onPointerLeave={() => setHot(null)}
    >
      {title && (
        <div className="convtitle" aria-hidden="true">
          {kicker && <span className="k">{kicker}</span>}
          <span className="t" ref={titleRef}>
            {title}
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
