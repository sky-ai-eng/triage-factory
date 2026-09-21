import { useCallback, useEffect, useRef, useState } from 'react'
import { apiJSON, apiList, httpErrorMessage, HttpError } from '../lib/apiClient'
import type { ListPage } from '../lib/apiClient'

// One registered work kind, as the catalogue describes it: a table of
// durable, leased work on the shared work-item contract, with the operator
// controls it offers and the age its oldest ready row may reach.
export interface WorkKind {
  kind: string
  label: string
  controls: { redrive: boolean; supersede: boolean; cancel: boolean }
  objective: { oldest_ready_age_seconds: number }
}

// A kind's depth for the org: numbers about its rows, on their own node.
export interface WorkDepth {
  ready: number
  leased: number
  parked: number
  deferred: number
  oldest_ready_age_seconds: number
  oldest_deferred_age_seconds: number
}

// One row's shared block plus the kind's description of it. A parked row is
// work that is durably recorded and will never run on its own: nothing
// re-drives it, so an operator is the only path back. Every timestamp is
// RFC 3339; nullable fields are absent when null.
export interface WorkItem {
  kind: string
  id: number
  status: 'ready' | 'leased' | 'done' | 'parked' | 'cancelled'
  attempt: number
  max_attempts: number
  last_outcome?: string
  last_error?: string
  unique_key?: string
  first_enqueued_at: string
  created_at: string
  done_at?: string
  next_attempt_at?: string
  superseded_by?: number
  lease?: {
    generation: number
    owner: string
    epoch?: number
    leased_at?: string
    expires_at?: string
  }
  cancel?: { requested_at: string; by: string; reason: string }
  subject?: { label: string; detail?: string; fields: Record<string, string> }
}

// The statuses the panel filters by. `deferred` is the ready rows whose retry
// time has not come, the same partition the depth node reports.
export type WorkStatusFilter = 'parked' | 'ready' | 'leased' | 'deferred'

/** One kind's share of a bulk control: ids are per kind, so a selection that
 *  spans the table is a list of these. */
export interface WorkSelection {
  kind: string
  ids: number[]
}

export interface UseParkedWork {
  kinds: WorkKind[]
  /** Each kind's depth for the org, keyed by kind name. A kind whose depth
   *  read failed on the last load is absent. */
  depths: Record<string, WorkDepth>
  /** The sum of every loaded kind's parked depth — the badge's number, which
   *  is the org's whole parked population rather than the length of a page. */
  parkedTotal: number
  /** The rows loaded so far under `status`, in catalogue order by kind. */
  items: WorkItem[]
  /** What `status` matches across every loaded kind, summed. */
  total: number
  status: WorkStatusFilter
  /** Change the filter. Every kind refetches from its first page, so a page
   *  token never crosses a filter change. */
  setStatus: (status: WorkStatusFilter) => void
  loading: boolean
  /** The last load's failure, or null. A failure of one kind's reads leaves
   *  the other kinds' data in place and names the kind here, so a broken
   *  kind never blanks a healthy one's rows. */
  error: string | null
  /** True when at least one kind has a further page. */
  hasMore: boolean
  loadMore: () => Promise<void>
  reload: () => Promise<void>
  /** Return the named parked rows of one kind to ready. Resolves to the number
   *  that actually moved — ids that are no longer parked are counted out by
   *  the backend, so a stale selection is a partial no-op, not a failure.
   *  Only that kind refetches afterwards; the other kinds' loaded pages stay. */
  redrive: (kind: string, ids: number[]) => Promise<number>
  /** Redrive across kinds with one refresh of the touched kinds at the end,
   *  for a selection that spans the table. Resolves to the total moved. */
  redriveMany: (selections: WorkSelection[]) => Promise<number>
  /** Record a cancellation request against the named ready or leased rows of
   *  one kind. Resolves to the number recorded; the row settles at its next
   *  claim or its holder's next renewal, not here. */
  cancel: (kind: string, ids: number[], reason: string) => Promise<number>
}

// What one kind's load produced: its depth and its first page, or the reason
// it could not be read.
type KindLoad =
  | { kind: WorkKind; depth: WorkDepth; page: ListPage<WorkItem> }
  | { kind: WorkKind; failure: unknown }

// One kind's loaded rows under the current filter, with the filtered total
// the list envelope reported.
interface KindPage {
  items: WorkItem[]
  total: number
}

// useParkedWork owns the parked-work operator surface: the catalogue of kinds,
// each kind's depth, and one page per kind of the rows under the status
// filter. Org-admin surface: every route 403s a plain member, so the hook is
// gated on `enabled` (the caller's admin bit) and stays inert otherwise. A
// role that changes mid-session turns the gate to 403 on the wire, which is
// rendered as an empty panel rather than an alarming error line.
//
// Org-addressed paths, like /sources: the org is the caller's to name.
//
// Paging is per kind: the registry is a runtime set, and a hook cannot be
// instantiated once per kind, so this threads one token per kind itself,
// against the same list envelope every other paged read answers with. Kinds
// load independently, and one kind's failure is reported beside the others'
// rows rather than in place of them: the panel's job is to show what is
// parked, and a transient fault on one queue must not hide the backlog on
// another. A control refetches only the kinds it acted on, so an operator
// three pages into one queue does not lose their place by redriving a row in
// another.
//
// No websocket wiring. A row parks at most once every few thousand events in
// a healthy deployment, so a live channel would be a subscription that never
// fires; the panel loads when it opens and refreshes after a control.
export function useParkedWork(orgId: string | null, enabled: boolean): UseParkedWork {
  const [kinds, setKinds] = useState<WorkKind[]>([])
  const [depths, setDepths] = useState<Record<string, WorkDepth>>({})
  const [pages, setPages] = useState<Record<string, KindPage>>({})
  const [status, setStatusState] = useState<WorkStatusFilter>('parked')
  const [loading, setLoading] = useState(false)
  // A failure of the whole surface (the catalogue, a further page), and the
  // per-kind failures of the last load that touched each kind. Kept apart so
  // a refresh of one kind neither clears nor restates another's.
  const [loadError, setLoadError] = useState<string | null>(null)
  const [failures, setFailures] = useState<Record<string, string>>({})
  const [hasMore, setHasMore] = useState(false)

  // The next-page token per kind, valid only for the filter the items were
  // fetched under. Refs rather than state: loadMore reads the latest without
  // being re-created per page, and a filter change clears them before it
  // fetches so a token never pairs with another status.
  const tokensRef = useRef<Record<string, string>>({})
  const statusRef = useRef<WorkStatusFilter>('parked')
  // The catalogue as last loaded, for a refresh to look kinds up by name.
  const kindsRef = useRef<WorkKind[]>([])
  // Bumped per load, so a response applies only if it is still the latest.
  const requestIdRef = useRef(0)
  const inFlight = useRef(false)

  const base = orgId ? `/api/orgs/${encodeURIComponent(orgId)}/work` : null

  const clear = useCallback(() => {
    setKinds([])
    setDepths({})
    setPages({})
    setFailures({})
    setHasMore(false)
    tokensRef.current = {}
    kindsRef.current = []
  }, [])

  const anyMore = () => Object.values(tokensRef.current).some((tok) => tok !== '')

  // loadKinds reads each kind's depth and first page under the current
  // filter, independently, so one kind's fault is one entry of the result.
  const loadKinds = useCallback(
    (ks: WorkKind[]): Promise<KindLoad[]> => {
      const filter = statusRef.current
      return Promise.all(
        ks.map(async (k): Promise<KindLoad> => {
          try {
            const [depth, page] = await Promise.all([
              apiJSON<WorkDepth>(`${base}/${encodeURIComponent(k.kind)}/depth`),
              apiList<WorkItem>(`${base}/${encodeURIComponent(k.kind)}/items/list`, {
                status: filter,
              }),
            ])
            return { kind: k, depth, page }
          } catch (failure) {
            return { kind: k, failure }
          }
        }),
      )
    },
    [base],
  )

  // applyLoads merges a batch of kind loads into the loaded state, replacing
  // exactly the kinds it carries. Returns false when the gate has closed
  // (a 403 on any kind), after clearing the surface: that is the whole
  // surface gone, not one kind of it.
  const applyLoads = useCallback(
    (loads: KindLoad[]): boolean => {
      for (const load of loads) {
        if ('failure' in load && load.failure instanceof HttpError && load.failure.status === 403) {
          clear()
          setLoadError(null)
          return false
        }
      }
      const nextPages: Record<string, KindPage> = {}
      const nextFailures: Record<string, string> = {}
      for (const load of loads) {
        const name = load.kind.kind
        if ('failure' in load) {
          nextFailures[name] =
            `${load.kind.label}: ${httpErrorMessage(load.failure, 'could not be read')}`
          continue
        }
        nextPages[name] = {
          items: load.page.items,
          total: load.page.total_count ?? load.page.items.length,
        }
        tokensRef.current[name] = load.page.next_page_token
      }
      setDepths((prev) => {
        const out = { ...prev }
        for (const load of loads) {
          if ('failure' in load) continue
          out[load.kind.kind] = load.depth
        }
        return out
      })
      setPages((prev) => ({ ...prev, ...nextPages }))
      setFailures((prev) => {
        const out = { ...prev }
        for (const load of loads) delete out[load.kind.kind]
        return { ...out, ...nextFailures }
      })
      setHasMore(anyMore())
      return true
    },
    [clear],
  )

  const reload = useCallback(async () => {
    if (!enabled || !base) {
      clear()
      return
    }
    const requestId = ++requestIdRef.current
    inFlight.current = true
    setLoading(true)
    try {
      const catalogue = await apiJSON<{ kinds: WorkKind[] }>(base)
      const perKind = await loadKinds(catalogue.kinds)
      if (requestId !== requestIdRef.current) return
      // A full load starts from nothing: a kind that left the catalogue
      // leaves the state with it.
      kindsRef.current = catalogue.kinds
      tokensRef.current = {}
      setKinds(catalogue.kinds)
      setDepths({})
      setPages({})
      setFailures({})
      if (applyLoads(perKind)) setLoadError(null)
    } catch (err) {
      if (requestId !== requestIdRef.current) return
      if (err instanceof HttpError && err.status === 403) {
        clear()
        setLoadError(null)
      } else {
        setLoadError(httpErrorMessage(err, 'Could not load parked work.'))
      }
    } finally {
      if (requestId === requestIdRef.current) {
        inFlight.current = false
        setLoading(false)
      }
    }
  }, [enabled, base, clear, loadKinds, applyLoads])

  // refreshKinds refetches the named kinds from their first page and leaves
  // every other kind's loaded pages and place in place. A name the catalogue
  // does not carry is skipped.
  const refreshKinds = useCallback(
    async (names: string[]) => {
      if (!enabled || !base) return
      const wanted = new Set(names)
      const ks = kindsRef.current.filter((k) => wanted.has(k.kind))
      if (ks.length === 0) return
      const requestId = ++requestIdRef.current
      inFlight.current = true
      setLoading(true)
      try {
        const loads = await loadKinds(ks)
        if (requestId !== requestIdRef.current) return
        if (applyLoads(loads)) setLoadError(null)
      } finally {
        if (requestId === requestIdRef.current) {
          inFlight.current = false
          setLoading(false)
        }
      }
    },
    [enabled, base, loadKinds, applyLoads],
  )

  useEffect(() => {
    void reload()
  }, [reload])

  const setStatus = useCallback(
    (next: WorkStatusFilter) => {
      statusRef.current = next
      tokensRef.current = {}
      setStatusState(next)
      void reload()
    },
    [reload],
  )

  const loadMore = useCallback(async () => {
    if (!enabled || !base || inFlight.current) return
    const pending = Object.entries(tokensRef.current).filter(([, tok]) => tok !== '')
    if (pending.length === 0) return
    const requestId = requestIdRef.current
    inFlight.current = true
    setLoading(true)
    try {
      const filter = statusRef.current
      const fetched = await Promise.all(
        pending.map(async ([kind, tok]) => {
          const page: ListPage<WorkItem> = await apiList<WorkItem>(
            `${base}/${encodeURIComponent(kind)}/items/list`,
            { status: filter, page_token: tok },
          )
          return { kind, page }
        }),
      )
      if (requestId !== requestIdRef.current) return
      for (const { kind, page } of fetched) {
        tokensRef.current[kind] = page.next_page_token
      }
      setPages((prev) => {
        const out = { ...prev }
        for (const { kind, page } of fetched) {
          const cur = out[kind] ?? { items: [], total: 0 }
          out[kind] = { ...cur, items: cur.items.concat(page.items) }
        }
        return out
      })
      setHasMore(anyMore())
      setLoadError(null)
    } catch (err) {
      if (requestId !== requestIdRef.current) return
      setLoadError(httpErrorMessage(err, 'Could not load more parked work.'))
    } finally {
      if (requestId === requestIdRef.current) {
        inFlight.current = false
        setLoading(false)
      }
    }
  }, [enabled, base])

  // redriveMany runs one call per kind and refreshes the touched kinds once
  // afterwards, so a selection spanning N kinds costs N controls and one
  // refetch of each rather than N reloads of everything. A failure part-way
  // still refreshes: whatever moved before it has to leave the table.
  const redriveMany = useCallback(
    async (selections: WorkSelection[]): Promise<number> => {
      if (!base) return 0
      let moved = 0
      const touched: string[] = []
      try {
        for (const { kind, ids } of selections) {
          if (ids.length === 0) continue
          touched.push(kind)
          const res = await apiJSON<{ redriven: number }>(
            `${base}/${encodeURIComponent(kind)}/items/redrive`,
            {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ ids }),
            },
          )
          moved += res.redriven
        }
      } finally {
        await refreshKinds(touched)
      }
      return moved
    },
    [base, refreshKinds],
  )

  const redrive = useCallback(
    (kind: string, ids: number[]): Promise<number> => redriveMany([{ kind, ids }]),
    [redriveMany],
  )

  const cancel = useCallback(
    async (kind: string, ids: number[], reason: string): Promise<number> => {
      if (!base) return 0
      const res = await apiJSON<{ requested: number }>(
        `${base}/${encodeURIComponent(kind)}/items/cancel`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ids, reason }),
        },
      )
      await refreshKinds([kind])
      return res.requested
    },
    [base, refreshKinds],
  )

  const parkedTotal = Object.values(depths).reduce((sum, d) => sum + d.parked, 0)
  const items = kinds.flatMap((k) => pages[k.kind]?.items ?? [])
  const total = kinds.reduce((sum, k) => sum + (pages[k.kind]?.total ?? 0), 0)
  const failed = kinds.map((k) => failures[k.kind]).filter((f): f is string => f !== undefined)
  const error = loadError ?? (failed.length === 0 ? null : `Could not load ${failed.join('; ')}.`)

  return {
    kinds,
    depths,
    parkedTotal,
    items,
    total,
    status,
    setStatus,
    loading,
    error,
    hasMore,
    loadMore,
    reload,
    redrive,
    redriveMany,
    cancel,
  }
}
