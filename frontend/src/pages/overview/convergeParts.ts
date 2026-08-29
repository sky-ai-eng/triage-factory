// The Converge instrument's pure parts — allocation, tone mapping, and the
// headline decode — split from the component file because react-refresh
// objects to a component file that also exports functions, and because the
// allocation is the piece worth pinning with unit tests.

/** Tone → the single-letter class the stylesheet keys on. */
export function tones(tone?: 'quiet' | 'warm' | 'ask' | 'cool'): 'q' | 'w' | 'a' | 'c' {
  return tone === 'ask' ? 'a' : tone === 'cool' ? 'c' : tone === 'warm' ? 'w' : 'q'
}

/**
 * How many of the fan's filaments each outcome draws. STRANDS ARE STRUCTURE,
 * NOT DATA — the counts beside them are the data — so the allocation's only job
 * is a legible picture, and it is on a SQUARE-ROOT scale for that reason: this
 * product's healthy day is 95% filtered, and proportional allocation hands that
 * outcome twenty-six of twenty-eight strands with no picture left. The sqrt
 * compresses the tail while keeping the order of magnitudes readable.
 *
 * A floor of two per non-empty outcome keeps a small one reading as a bundle
 * rather than a stray line (dropping to one only when the outcome count is too
 * large for the strand budget to afford two each).
 */
export function allocate(outcomes: Array<{ v: number }>, strands: number): number[] {
  const scaled = outcomes.map((o) => Math.sqrt(Math.max(0, o.v)))
  const sum = scaled.reduce((a, s) => a + s, 0) || 1
  const raw = scaled.map((s) => (s / sum) * strands)
  const floor = outcomes.length * 2 <= strands ? 2 : 1
  const n = raw.map((r) => Math.max(floor, Math.floor(r)))
  let left = strands - n.reduce((a, b) => a + b, 0)
  const order = raw.map((r, i) => [r - Math.floor(r), i] as const).sort((a, b) => b[0] - a[0])
  for (let k = 0; left > 0; k++, left--) n[order[k % order.length][1]]++
  while (left < 0) {
    const i = n.indexOf(Math.max(...n))
    if (n[i] <= floor) break
    n[i]--
    left++
  }
  return n
}

export const DECODE_GLYPHS = 'abcdefghijklmnopqrstuvwxyz0123456789'

/**
 * The headline decode: characters churn and settle left to right. Decode stays
 * rationed to one per view — it is the headline or nothing, never the endpoint
 * labels, which are read rather than watched.
 */
export function decodeInto(
  el: HTMLElement | null,
  final: string,
  ms: number,
  glyphs: string = DECODE_GLYPHS,
): () => void {
  if (!el) return () => {}
  const t0 = performance.now()
  let raf = 0
  const tick = (now: number) => {
    const p = Math.min(1, (now - t0) / ms)
    const settled = Math.floor(p * final.length)
    let out = ''
    for (let i = 0; i < final.length; i++) {
      out +=
        i < settled || final[i] === ' '
          ? final[i]
          : glyphs[(Math.floor(now / 40) + i * 7) % glyphs.length]
    }
    el.textContent = out
    if (p < 1) raf = requestAnimationFrame(tick)
    else el.textContent = final
  }
  raf = requestAnimationFrame(tick)
  return () => cancelAnimationFrame(raf)
}
