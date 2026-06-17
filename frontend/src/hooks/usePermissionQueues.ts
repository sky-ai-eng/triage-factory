import { useCallback, useEffect, useRef, useState } from 'react'
import type { WSEvent } from '../types'
import { toast } from '../components/Toast/toastStore'
import {
  resolvePermission as postResolvePermission,
  ttlForPrompt,
  type PendingPermission,
  type PermissionDecisionInput,
} from '../lib/permissions'

// permission_request / permission_resolved are the two run-scoped WS events the
// queue reacts to. Narrowing here keeps `ingest`/`forget` honest about their
// payloads without re-walking the WSEvent union at every call site.
type PermissionRequestEvent = Extract<WSEvent, { type: 'permission_request' }>
type PermissionResolvedEvent = Extract<WSEvent, { type: 'permission_resolved' }>

export interface PermissionQueues {
  /** runID → its unanswered prompts, head-first. A run with no prompts is
   *  absent from the map (not an empty array), so `queues[runID] ?? []` is the
   *  canonical read. */
  queues: Record<string, PendingPermission[]>
  /** Enqueue a prompt from a `permission_request` event (dedup + arm-once TTL). */
  ingest: (event: PermissionRequestEvent) => void
  /** Drop a prompt a `permission_resolved` event reports answered elsewhere /
   *  timed out, so sibling surfaces clear it before their client TTL fires. */
  forget: (event: PermissionResolvedEvent) => void
  /** Answer a prompt; drops it from the queue on a definitive response (200
   *  resolved / 404 already-resolved). A transient failure keeps it up + toasts. */
  resolve: (runID: string, requestID: string, decision: PermissionDecisionInput) => Promise<void>
  /** Drop a whole run's queue (its run finished or left the board). */
  dropRun: (runID: string) => void
}

// timerKey scopes a TTL timer by (runID, requestID). A wrapper's request_id is
// only unique within one run process (`perm-N`), so two concurrent runs can
// collide on the same id — keying by both keeps each run's timers isolated. The
// NUL separator can't appear in a run uuid or a `perm-N` id (mirrors the
// backend's permKey).
function timerKey(runID: string, requestID: string): string {
  return `${runID}\x00${requestID}`
}

// usePermissionQueues manages permission prompts for many runs at once: a
// runID→queue map plus per-(runID,requestID) TTL timers (dedupe, arm-once,
// clear-on-resolve/drop/unmount). It's the single implementation behind both the
// board (all visible runs) and useRunDetail (filtered to one run), so the TTL
// behavior can't diverge between the two surfaces.
export function usePermissionQueues(): PermissionQueues {
  const [queues, setQueues] = useState<Record<string, PendingPermission[]>>({})

  // TTL timers keyed by timerKey(runID, requestID). The ref object is stable, so
  // the unmount cleanup below captures it once; entries are cleared on resolve,
  // drop, and unmount so a fired timer never touches a stale queue.
  const timers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())

  const clearTimer = useCallback((runID: string, requestID: string) => {
    const key = timerKey(runID, requestID)
    const t = timers.current.get(key)
    if (t) {
      clearTimeout(t)
      timers.current.delete(key)
    }
  }, [])

  // dropPermission removes one prompt from a run's queue and cancels its timer.
  // The run's key is removed entirely when its last prompt clears, so the map
  // only ever holds runs with live prompts (and `runID in queues` reads true
  // exactly when a card should light up).
  const dropPermission = useCallback(
    (runID: string, requestID: string) => {
      clearTimer(runID, requestID)
      setQueues((prev) => {
        const q = prev[runID]
        if (!q) return prev
        const next = q.filter((p) => p.request_id !== requestID)
        if (next.length === q.length) return prev
        const out = { ...prev }
        if (next.length === 0) delete out[runID]
        else out[runID] = next
        return out
      })
    },
    [clearTimer],
  )

  const dropRun = useCallback((runID: string) => {
    // Cancel every timer for the run. Deleting the current entry mid-iteration
    // is safe for a Map.
    const prefix = `${runID}\x00`
    for (const [key, t] of timers.current) {
      if (key.startsWith(prefix)) {
        clearTimeout(t)
        timers.current.delete(key)
      }
    }
    setQueues((prev) => {
      if (!(runID in prev)) return prev
      const out = { ...prev }
      delete out[runID]
      return out
    })
  }, [])

  const ingest = useCallback(
    (event: PermissionRequestEvent) => {
      const runID = event.run_id
      const req = event.data
      setQueues((prev) => {
        const q = prev[runID] ?? []
        // Dedup: a request id is unique within a run process, so a replayed
        // event (e.g. a reconnect) must not double-queue the same prompt.
        if (q.some((p) => p.request_id === req.request_id)) return prev
        return { ...prev, [runID]: [...q, req] }
      })
      // Arm the client-side TTL once per (run, request) — a backstop for the
      // backend's silent timeout-deny, derived from the payload's deadline.
      const key = timerKey(runID, req.request_id)
      if (!timers.current.has(key)) {
        timers.current.set(
          key,
          setTimeout(() => dropPermission(runID, req.request_id), ttlForPrompt(req)),
        )
      }
    },
    [dropPermission],
  )

  const forget = useCallback(
    (event: PermissionResolvedEvent) => {
      dropPermission(event.run_id, event.data.request_id)
    },
    [dropPermission],
  )

  const resolve = useCallback(
    async (runID: string, requestID: string, decision: PermissionDecisionInput) => {
      const res = await postResolvePermission(runID, requestID, decision)
      if (res.kind === 'resolved' || res.kind === 'gone') {
        dropPermission(runID, requestID)
        return
      }
      toast.error(res.message)
    },
    [dropPermission],
  )

  // Clear all timers on unmount so a fired timer can't touch a torn-down
  // component. The ref object is stable, so capturing it here is safe.
  useEffect(() => {
    const t = timers.current
    return () => {
      for (const timer of t.values()) clearTimeout(timer)
      t.clear()
    }
  }, [])

  return { queues, ingest, forget, resolve, dropRun }
}
