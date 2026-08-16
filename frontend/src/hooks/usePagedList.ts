import { useCallback, useRef, useState } from 'react'
import { apiList, httpErrorMessage } from '../lib/apiClient'
import type { ListPage, ListRequest } from '../lib/apiClient'

/** What a paged list surface gets back from the hook.
 *
 *  `items` accumulates: `load` replaces it with the first page, `loadMore`
 *  appends the next one. `total` is what the filters match, so a caller can
 *  render "12 of 213" without counting anything itself. */
export interface PagedList<T> {
  items: T[]
  total: number
  /** True while a load or loadMore is in flight. */
  loading: boolean
  /** The last failure's user-facing message, or '' — the items are left
   *  untouched on a failure, so a surface can keep painting the stale page
   *  while it shows this. */
  error: string
  /** True when the server minted a token for a further page. */
  hasMore: boolean
  /** Fetch the first page for `filters`, replacing whatever is held. Resolves
   *  to the page, or null if the request failed. */
  load: (filters: ListRequest) => Promise<ListPage<T> | null>
  /** Fetch the next page and append it. A no-op when there is no next page or
   *  a request is already in flight. */
  loadMore: () => Promise<void>
  /** Apply a local edit to the held items — an optimistic removal right after
   *  a gesture, before the next load reconciles. It does not touch `total`:
   *  the server's count is the server's to correct. */
  setItems: (update: (prev: T[]) => T[]) => void
}

/** usePagedList is the one place page tokens are threaded, for every
 *  `POST /api/<resource>/list` surface.
 *
 *  It deliberately does not fetch on mount or on a filter change: which reads
 *  happen when is the page's business (the board fires five columns at once
 *  and enriches the result; the triage deck refetches after a swipe), and a
 *  hook that owned that schedule would need every caller to memoize a body
 *  object just to avoid a fetch loop. `load` and `loadMore` are stable across
 *  renders, so they are safe to call from an effect or a ref.
 *
 *  The filters passed to `load` are remembered and reused for `loadMore`, so a
 *  page token never crosses a filter change — the server rejects a token
 *  minted for a different filter set, and this is what keeps a caller from
 *  ever producing one.
 *
 *  `fallbackError` is the message surfaced when a request fails without a
 *  server-supplied one; write it as a whole sentence. */
export function usePagedList<T>(path: string, fallbackError: string): PagedList<T> {
  const [items, setItems] = useState<T[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [nextToken, setNextToken] = useState('')

  // The filters the current page set was fetched with, and the token that
  // continues it. Refs rather than state: loadMore must read the latest of
  // both without being re-created on every page, since callers hold it in
  // effects and event handlers.
  const filtersRef = useRef<ListRequest>({})
  const tokenRef = useRef('')
  const inFlight = useRef(false)

  const load = useCallback(
    async (filters: ListRequest): Promise<ListPage<T> | null> => {
      filtersRef.current = filters
      inFlight.current = true
      setLoading(true)
      try {
        const page = await apiList<T>(path, filters)
        setItems(page.items)
        setTotal(page.total_count)
        setNextToken(page.next_page_token)
        tokenRef.current = page.next_page_token
        setError('')
        return page
      } catch (err) {
        // Keep the items: blanking a column on one failed poll is worse than
        // showing a slightly stale page, and the next load reconciles.
        setError(httpErrorMessage(err, fallbackError))
        return null
      } finally {
        inFlight.current = false
        setLoading(false)
      }
    },
    [path, fallbackError],
  )

  const loadMore = useCallback(async () => {
    if (!tokenRef.current || inFlight.current) return
    inFlight.current = true
    setLoading(true)
    try {
      const page = await apiList<T>(path, {
        ...filtersRef.current,
        page_token: tokenRef.current,
      })
      setItems((prev) => [...prev, ...page.items])
      setTotal(page.total_count)
      setNextToken(page.next_page_token)
      tokenRef.current = page.next_page_token
      setError('')
    } catch (err) {
      setError(httpErrorMessage(err, fallbackError))
    } finally {
      inFlight.current = false
      setLoading(false)
    }
  }, [path, fallbackError])

  return { items, total, loading, error, hasMore: nextToken !== '', load, loadMore, setItems }
}
