import { useCallback, useEffect } from 'react'
import { apiJSON } from '../lib/apiClient'
import { usePagedList } from './usePagedList'

// A parked event_queue row — routing work the queue gave up on. The event is
// durably recorded but its obligations (task mint/bump, owner resolution,
// trigger firing, close processing) burned the retry budget and will never
// run: the tracker's snapshot advanced when the event was minted, so the
// transition does not re-emit. An operator requeue is the only path back.
export interface FailedEvent {
  id: number
  event_type: string
  entity_id?: string
  entity_source?: string
  entity_source_id?: string
  entity_title?: string
  attempts: number
  last_error?: string
  enqueued_at: string
}

export interface UseFailedEvents {
  events: FailedEvent[]
  /** The org's whole parked population, which the badge shows — not the
   *  length of the loaded page. */
  total: number | null
  loading: boolean
  error: string | null
  /** True when the server minted a token for a further page. */
  hasMore: boolean
  loadMore: () => Promise<void>
  reload: () => Promise<void>
  /** Put the named rows back on the queue. Resolves to the number of rows
   *  that actually moved — ids that are no longer parked are counted out by
   *  the backend, so a stale selection is a partial no-op, not a failure. */
  requeue: (ids: number[]) => Promise<number>
}

// useFailedEvents owns the parked-row diagnostics list. Org-admin surface:
// both endpoints 403 a plain member, so the hook is gated on `enabled` (the
// caller's admin bit) and stays inert — empty list, no fetch — otherwise. A
// role that changes mid-session turns the gate to 403 on the wire, which the
// shared hook renders as an empty panel rather than an alarming error line.
//
// Session-org-scoped paths (no apiClient `org` prefix), like /api/invites: the
// backend reads the active org from the session.
//
// No websocket wiring. A row parks at most once every few thousand events in a
// healthy deployment, so a live channel would be a subscription that never
// fires; the list loads when the panel opens and refreshes after a requeue.
export function useFailedEvents(enabled: boolean): UseFailedEvents {
  const page = usePagedList<FailedEvent>(
    '/api/events/failed/list',
    'Could not load parked events.',
    FAILED_EVENTS_EMPTY_ON,
  )
  const { load, setItems } = page

  const reload = useCallback(async () => {
    if (!enabled) {
      setItems(() => [])
      return
    }
    await load({})
  }, [enabled, load, setItems])

  useEffect(() => {
    void reload()
  }, [reload])

  const requeue = useCallback(
    async (ids: number[]): Promise<number> => {
      const res = await apiJSON<{ requeued: number }>('/api/events/failed/requeue', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ids }),
      })
      await reload()
      return res.requeued
    },
    [reload],
  )

  return {
    events: page.items,
    total: page.total,
    loading: page.loading,
    error: page.error === '' ? null : page.error,
    hasMore: page.hasMore,
    loadMore: page.loadMore,
    reload,
    requeue,
  }
}

// Hoisted so the option object is referentially stable across renders — the
// hook's callbacks depend on it, and a fresh literal each render would rebuild
// them and re-fire the load effect forever.
const FAILED_EVENTS_EMPTY_ON = { emptyOnStatus: [403] }
