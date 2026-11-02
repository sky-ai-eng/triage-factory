import { useCallback, useEffect, useRef } from 'react'
import { useAway } from '../../ui/shared/useAway'

// Read on dwell: a row becomes read once it has been at least partly on screen
// for DWELL_MS while the viewer is here (the tab visible and the window
// focused). That is what "the ids the client rendered" means for a page that
// scrolls: a row below the fold was rendered and not seen, and a row passed in
// a fling was on screen and not read.

/** How long a row must be in view before it reads as seen. */
export const DWELL_MS = 2000
/** How often the clock is checked; the dwell is measured to within this. */
const TICK_MS = 250

export interface DwellRead {
  /** The ref for the element that stands for row `id`, stable per id. */
  observe: (id: string) => (el: Element | null) => void
  /** Stop counting row `id` until it has left the screen and come back: the
   *  viewer marked it unread while looking at it. A row already reported is
   *  not reported again in the same stay either. */
  suspend: (id: string) => void
}

/**
 * `root` is the scroller the rows are seen through. `wants` says whether a row
 * is one to mark (unread and open); it is read on every tick, so it need not be
 * stable. `onSeen` gets the rows that just crossed the line, once each.
 */
export function useDwellRead(
  root: HTMLElement | null,
  wants: (id: string) => boolean,
  onSeen: (ids: string[]) => void,
): DwellRead {
  const away = useAway()
  const latest = useRef({ wants, onSeen, away })
  useEffect(() => {
    latest.current = { wants, onSeen, away }
  })

  const els = useRef(new Map<string, Element>())
  const visible = useRef(new Set<string>())
  // When each visible row's current dwell began. Cleared while the viewer is
  // away, so a dwell is one stretch of attention, not a sum of glances.
  const since = useRef(new Map<string, number>())
  const suspended = useRef(new Set<string>())
  // Reported once per stay on screen: the page's answer (the row now read)
  // may not have reached `wants` by the next tick.
  const reported = useRef(new Set<string>())
  const io = useRef<IntersectionObserver | null>(null)

  useEffect(() => {
    if (!root || typeof IntersectionObserver === 'undefined') return
    const shown = visible.current
    const dwelling = since.current
    const obs = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          const id = (e.target as HTMLElement).dataset.nid
          if (!id) continue
          if (e.isIntersecting) {
            visible.current.add(id)
          } else {
            visible.current.delete(id)
            since.current.delete(id)
            suspended.current.delete(id)
            reported.current.delete(id)
          }
        }
      },
      { root },
    )
    io.current = obs
    for (const el of els.current.values()) obs.observe(el)
    return () => {
      obs.disconnect()
      io.current = null
      shown.clear()
      dwelling.clear()
    }
  }, [root])

  useEffect(() => {
    const t = setInterval(() => {
      const { wants, onSeen, away } = latest.current
      if (away) {
        since.current.clear()
        return
      }
      const now = Date.now()
      const seen: string[] = []
      for (const id of visible.current) {
        if (suspended.current.has(id) || reported.current.has(id) || !wants(id)) {
          since.current.delete(id)
          continue
        }
        const start = since.current.get(id)
        if (start === undefined) since.current.set(id, now)
        else if (now - start >= DWELL_MS) {
          seen.push(id)
          since.current.delete(id)
          reported.current.add(id)
        }
      }
      if (seen.length) onSeen(seen)
    }, TICK_MS)
    return () => clearInterval(t)
  }, [])

  const refs = useRef(new Map<string, (el: Element | null) => void>())
  const observe = useCallback((id: string) => {
    let ref = refs.current.get(id)
    if (!ref) {
      ref = (el: Element | null) => {
        const prev = els.current.get(id)
        if (prev && prev !== el) {
          io.current?.unobserve(prev)
          els.current.delete(id)
          visible.current.delete(id)
          since.current.delete(id)
        }
        if (el) {
          els.current.set(id, el)
          io.current?.observe(el)
        }
      }
      refs.current.set(id, ref)
    }
    return ref
  }, [])

  const suspend = useCallback((id: string) => {
    suspended.current.add(id)
    since.current.delete(id)
  }, [])

  return { observe, suspend }
}
