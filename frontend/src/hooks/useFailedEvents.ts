import { useCallback, useEffect, useState } from 'react'
import { apiJSON, httpErrorMessage, HttpError } from '../lib/apiClient'

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
  loading: boolean
  error: string | null
  reload: () => Promise<void>
  /** Put the named rows back on the queue. Resolves to the number of rows
   *  that actually moved — ids that are no longer parked are counted out by
   *  the backend, so a stale selection is a partial no-op, not a failure. */
  requeue: (ids: number[]) => Promise<number>
}

// useFailedEvents owns the parked-row diagnostics list. Org-admin surface:
// both endpoints 403 a plain member, so the hook is gated on `enabled` (the
// caller's admin bit) and stays inert — empty list, no fetch — otherwise.
//
// Session-org-scoped paths (no apiClient `org` prefix), like /api/invites: the
// backend reads the active org from the session.
//
// No websocket wiring. A row parks at most once every few thousand events in a
// healthy deployment, so a live channel would be a subscription that never
// fires; the list loads when the panel opens and refreshes after a requeue.
export function useFailedEvents(enabled: boolean): UseFailedEvents {
  const [events, setEvents] = useState<FailedEvent[]>([])
  const [loading, setLoading] = useState(enabled)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    if (!enabled) {
      setEvents([])
      setLoading(false)
      setError(null)
      return
    }
    setLoading(true)
    setError(null)
    try {
      const data = await apiJSON<{ events: FailedEvent[] | null }>('/api/events/failed')
      setEvents(data.events ?? [])
    } catch (e) {
      // A role change mid-session turns the gate to 403. Render that as an
      // empty panel rather than an alarming line about a surface the viewer
      // is simply no longer entitled to.
      if (e instanceof HttpError && e.status === 403) {
        setEvents([])
      } else {
        setError(httpErrorMessage(e, 'Could not load parked events.'))
      }
    } finally {
      setLoading(false)
    }
  }, [enabled])

  useEffect(() => {
    void load()
  }, [load])

  const requeue = useCallback(
    async (ids: number[]): Promise<number> => {
      const res = await apiJSON<{ requeued: number }>('/api/events/failed/requeue', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ids }),
      })
      await load()
      return res.requeued
    },
    [load],
  )

  return { events, loading, error, reload: load, requeue }
}
