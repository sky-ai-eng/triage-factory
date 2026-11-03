import { useEffect, useState } from 'react'

/** How long typing has to hold still before the read goes out. */
export const SEARCH_DELAY_MS = 150

/** A list route's answer to a search, and the needle it answered. */
export interface SearchAnswer<T> {
  /** The needle `value` answers. It trails the one being typed while the read
   *  for that one is still out. */
  needle: string
  /** The route's answer: null until one has arrived for this read, and when
   *  the read failed. */
  value: T | null
  /** True while the answer on hand is for an earlier needle than the one
   *  typed, or there is none yet. */
  pending: boolean
}

interface Held<T> {
  read: unknown
  needle: string
  value: T | null
}

/** useSearchAnswer reads a list route's answer to what a reader is typing.
 *
 *  A surface with a search box narrows on the server: it sends the needle as
 *  the route's `search` and renders the rows that come back, in their order.
 *  Filtering the rows it already holds would answer a different question — a
 *  match on a page it never read is invisible to a filter of its own. This is
 *  the typing half of that. The needle is trimmed; a read goes out once it has
 *  held still for SEARCH_DELAY_MS; a read still in flight when the needle moves
 *  on is aborted; and the last answer stays on hand, marked `pending`, until the
 *  next one lands, so a list does not blank between keystrokes.
 *
 *  Returns null while the trimmed needle is empty: there is no search, and the
 *  surface shows what it shows unsearched.
 *
 *  `read` must be referentially stable (wrap it in useCallback). A new `read`
 *  is a new list, and an answer from the old one is never returned for it. */
export function useSearchAnswer<T>(
  needle: string,
  read: (needle: string, signal: AbortSignal) => Promise<T>,
): SearchAnswer<T> | null {
  const q = needle.trim()
  const [held, setHeld] = useState<Held<T> | null>(null)

  useEffect(() => {
    if (!q) return
    const controller = new AbortController()
    const timer = setTimeout(() => {
      read(q, controller.signal).then(
        (value) => {
          if (!controller.signal.aborted) setHeld({ read, needle: q, value })
        },
        () => {
          if (!controller.signal.aborted) setHeld({ read, needle: q, value: null })
        },
      )
    }, SEARCH_DELAY_MS)
    return () => {
      clearTimeout(timer)
      controller.abort()
    }
  }, [q, read])

  if (!q) return null
  if (!held || held.read !== read) return { needle: q, value: null, pending: true }
  return { needle: held.needle, value: held.value, pending: held.needle !== q }
}
