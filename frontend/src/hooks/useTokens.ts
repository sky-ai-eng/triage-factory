import { useCallback, useEffect, useMemo, useState } from 'react'
import { usePagedList } from './usePagedList'
import {
  API_TOKENS_LIST_PATH,
  createApiToken,
  readApiTokenPolicy,
  renameApiToken,
  revokeApiToken,
} from '../lib/apiTokens'
import type { ApiTokenSort } from '../lib/apiTokens'
import type { ApiToken, ApiTokenCreateRequest, ApiTokenCreated, AuthOrg } from '../types'

// useTokens owns the personal-settings token table: the caller's tokens across
// every org, read a page at a time in the order the table asks for, the cap
// each of their orgs sets, and the three mutations.
//
// The order is the server's, so the held rows are never re-sorted here, and a
// mutation does not guess where a row lands: a created token's place in a
// last-used order is the server's answer, and so is the count. Each mutation
// re-reads the list at the depth it holds — which also supersedes any read
// that began before the mutation, whose answer would otherwise land after it
// and undo what it showed. A revoke or a rename is applied to the held rows
// first, so the screen does not wait on that re-read.

/** The order the table opens in: most recently used first. */
export const DEFAULT_TOKEN_SORT: ApiTokenSort = { key: 'last_used_at', dir: 'desc' }

/** One page of the list. Tokens are capped at fifty per org, so one page is
 *  the whole list for anyone in a single org; the table pages what it holds. */
const PAGE_SIZE = 50

export interface UseTokens {
  /** The rows read so far, in the server's order. */
  tokens: ApiToken[]
  /** How many tokens the caller holds across every page. */
  total: number
  /** True when the server holds a page past the ones read. */
  hasMore: boolean
  loadMore: () => void
  sort: ApiTokenSort
  setSort: (sort: ApiTokenSort) => void
  /** Each membership's cap, keyed by org id; null = no cap. An org whose read
   *  failed is absent, which the page treats as "cap unknown" rather than
   *  "no cap" — a struck preset is a promise about the 422. */
  caps: Record<string, number | null>
  /** True until the first list read has answered. */
  loading: boolean
  /** The last failure's message, or '' — the list is left as it was. */
  error: string
  create: (body: ApiTokenCreateRequest) => Promise<ApiTokenCreated>
  revoke: (id: string, opts?: { keepalive?: boolean }) => Promise<void>
  rename: (id: string, name: string) => Promise<ApiToken>
  reload: () => void
}

export function useTokens(enabled: boolean, orgs: AuthOrg[]): UseTokens {
  const list = usePagedList<ApiToken>(API_TOKENS_LIST_PATH, 'Could not load your API tokens.')
  const { load, loadMore, setItems } = list
  const [sort, setSort] = useState<ApiTokenSort>(DEFAULT_TOKEN_SORT)
  const [caps, setCaps] = useState<Record<string, number | null>>({})
  const [answered, setAnswered] = useState(false)
  // Memberships change identity on every /api/me refresh; the ids are what
  // the policy reads depend on.
  const orgKey = orgs.map((o) => o.id).join(',')

  const filters = useMemo(
    () => ({ sort_key: sort.key, sort_dir: sort.dir, page_size: PAGE_SIZE }),
    [sort],
  )

  useEffect(() => {
    if (!enabled) return
    void load(filters).then(() => setAnswered(true))
  }, [enabled, filters, load])

  useEffect(() => {
    if (!enabled) return
    let cancelled = false
    const ids = orgKey ? orgKey.split(',') : []
    void Promise.all(
      ids.map((id) =>
        readApiTokenPolicy(id).then(
          (p) => [id, p.max_age_days] as const,
          () => null,
        ),
      ),
    ).then((policies) => {
      if (cancelled) return
      const next: Record<string, number | null> = {}
      for (const p of policies) if (p) next[p[0]] = p[1]
      setCaps(next)
    })
    return () => {
      cancelled = true
    }
  }, [enabled, orgKey])

  const reread = useCallback(() => {
    void load(filters, { keepDepth: true })
  }, [load, filters])

  const create = useCallback(
    async (body: ApiTokenCreateRequest) => {
      const made = await createApiToken(body)
      reread()
      return made
    },
    [reread],
  )

  const revoke = useCallback(
    async (id: string, opts?: { keepalive?: boolean }) => {
      await revokeApiToken(id, opts)
      setItems((t) => t.filter((r) => r.id !== id))
      reread()
    },
    [setItems, reread],
  )

  const rename = useCallback(
    async (id: string, name: string) => {
      const row = await renameApiToken(id, name)
      setItems((t) => t.map((r) => (r.id === id ? row : r)))
      reread()
      return row
    },
    [setItems, reread],
  )

  return {
    tokens: list.items,
    total: list.total ?? list.items.length,
    hasMore: list.hasMore,
    loadMore: () => void loadMore(),
    sort,
    setSort,
    caps,
    loading: enabled && !answered,
    error: list.error,
    create,
    revoke,
    rename,
    reload: reread,
  }
}
