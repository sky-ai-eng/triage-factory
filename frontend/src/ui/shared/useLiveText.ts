import { useSyncExternalStore } from 'react'

// One wall clock for every live readout in the app. A page that shows elapsed
// time has a problem shaped like this: the DISPLAY is coarse — `40s` moves
// every second, but `11m` moves once a minute and `18h` once an hour — so a
// naive per-second re-render does sixty wasted paints for every visible
// change, and a per-page interval re-renders the whole tree to move one span.
//
// The shape here inverts both costs. The store snapshot is the FORMATTED
// STRING, not the timestamp: `useSyncExternalStore` compares snapshots with
// `Object.is`, strings compare by value, so a subscriber whose text did not
// change is not re-rendered at all. A row reading `18h` costs one string
// build and one comparison per second — no React work, no DOM work — until
// the hour actually turns. And the interval is module-level and shared: the
// first subscriber starts it, the last one stops it, and a hundred rows cost
// one timer, not a hundred.
//
// Always derived, never incremented: every tick recomputes from Date.now(),
// so a background tab whose timers the browser throttles snaps back to the
// truth on its first tick after waking, for free.

type Listener = () => void

const listeners = new Set<Listener>()
let now = Date.now()
let timer: ReturnType<typeof setInterval> | null = null

// The clock only advances while the ticker runs. A component mounting after
// an idle stretch (last subscriber left, timer stopped) would otherwise read
// a stale `now` for its first paint — up to a full second of visible lie.
// Refreshing on read while stopped fixes that without breaking getSnapshot's
// stability contract: consecutive reads inside one render see a delta under
// the tick period and return the same value.
function readNow(): number {
  if (timer == null && Date.now() - now >= 1000) now = Date.now()
  return now
}

function subscribe(listener: Listener): () => void {
  listeners.add(listener)
  if (listeners.size === 1) {
    // Re-stamp on waking: the first paint may have read a clock stamped at
    // module load, and React re-checks the snapshot right after subscribing,
    // so a fresh stamp here heals that paint in the same commit.
    now = Date.now()
    timer = setInterval(() => {
      now = Date.now()
      listeners.forEach((l) => l())
    }, 1000)
  }
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0 && timer != null) {
      clearInterval(timer)
      timer = null
    }
  }
}

/**
 * The string `compute` makes of the current wall clock, kept current. The
 * subscriber re-renders only when the STRING changes, so a coarse format is
 * nearly free however often the clock ticks — call it from a leaf component
 * (ui/shared/LiveText is the ready-made one) so the re-render, when it does
 * come, is scoped to the text and not the page.
 */
export function useLiveText(compute: (now: number) => string): string {
  return useSyncExternalStore(subscribe, () => compute(readNow()))
}
