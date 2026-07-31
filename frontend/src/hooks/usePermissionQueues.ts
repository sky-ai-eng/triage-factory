import { useCallback, useEffect, useRef, useState } from 'react'
import {
  fetchPendingPermissions,
  resolvePermission as postResolvePermission,
  ttlForPrompt,
  type PendingPermission,
  type PermissionDecisionInput,
} from '../lib/permissions'
import { toast } from '../components/Toast/toastStore'

export interface PermissionQueues {
  /** runID → its unanswered prompts, head-first. A run with no prompts is
   *  absent from the map (not an empty array), so `queues[runID] ?? []` is the
   *  canonical read. */
  queues: Record<string, PendingPermission[]>
  /** Re-read a run's pending set from the server and replace its queue with it.
   *  The single entry point for both WS triggers and cold load — a
   *  `permission_request` / `permission_resolved` frame is a hint that this
   *  run's set changed, never the set itself. */
  refresh: (runID: string) => void
  /** Answer a prompt; drops it from the queue on a definitive response (200
   *  resolved / 404 already-resolved). A transient failure keeps it up + toasts. */
  resolve: (runID: string, toolCallID: string, decision: PermissionDecisionInput) => Promise<void>
  /** Drop a whole run's queue (its run finished or left the board). */
  dropRun: (runID: string) => void
}

// timerKey scopes a TTL timer by (runID, toolCallID). A tool_use id is unique
// in practice, but keying by both makes two runs raising the same id a
// non-event rather than a collision. The NUL separator can't appear in a run
// uuid or a tool_use id (mirrors the backend's permKey).
function timerKey(runID: string, toolCallID: string): string {
  return `${runID}\x00${toolCallID}`
}

// usePermissionQueues manages permission prompts for many runs at once: a
// runID→queue map sourced from the server, plus per-(runID,toolCallID) TTL
// timers (arm-once, clear-on-resolve/drop/unmount). It's the single
// implementation behind both the board (all visible runs) and useRunDetail
// (filtered to one run), so neither the fetch nor the TTL behavior can diverge
// between the two surfaces.
//
// The queue is a projection of the pending-set endpoint, not an accumulation of
// websocket frames. That inversion is the whole point: a frame is fire-once, so
// anything that reconstructs state from frames alone is blind after a refresh,
// in a second tab, and on a cold load. The client-side TTL stays on as a
// backstop for a dropped trigger.
export function usePermissionQueues(): PermissionQueues {
  const [queues, setQueues] = useState<Record<string, PendingPermission[]>>({})

  // TTL timers keyed by timerKey(runID, toolCallID). The ref object is stable, so
  // the unmount cleanup below captures it once; entries are cleared on resolve,
  // drop, and unmount so a fired timer never touches a stale queue.
  const timers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())

  // Per-run refresh generation. Every local drop and every new fetch bumps it,
  // and only the newest fetch is allowed to write — so an in-flight refetch
  // that resolves late can never resurrect a prompt the user just answered or a
  // run that just left the board.
  const refreshSeq = useRef<Map<string, number>>(new Map())
  const bumpRefreshSeq = useCallback((runID: string) => {
    const next = (refreshSeq.current.get(runID) ?? 0) + 1
    refreshSeq.current.set(runID, next)
    return next
  }, [])

  const clearTimer = useCallback((runID: string, toolCallID: string) => {
    const key = timerKey(runID, toolCallID)
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
    (runID: string, toolCallID: string) => {
      clearTimer(runID, toolCallID)
      bumpRefreshSeq(runID)
      setQueues((prev) => {
        const q = prev[runID]
        if (!q) return prev
        const next = q.filter((p) => p.tool_call_id !== toolCallID)
        if (next.length === q.length) return prev
        const out = { ...prev }
        if (next.length === 0) delete out[runID]
        else out[runID] = next
        return out
      })
    },
    [clearTimer, bumpRefreshSeq],
  )

  const dropRun = useCallback(
    (runID: string) => {
      // Cancel every timer for the run. Deleting the current entry mid-iteration
      // is safe for a Map.
      const prefix = `${runID}\x00`
      for (const [key, t] of timers.current) {
        if (key.startsWith(prefix)) {
          clearTimeout(t)
          timers.current.delete(key)
        }
      }
      bumpRefreshSeq(runID)
      setQueues((prev) => {
        if (!(runID in prev)) return prev
        const out = { ...prev }
        delete out[runID]
        return out
      })
    },
    [bumpRefreshSeq],
  )

  const refresh = useCallback(
    (runID: string) => {
      if (!runID) return
      const seq = bumpRefreshSeq(runID)
      void fetchPendingPermissions(runID).then((pending) => {
        if (refreshSeq.current.get(runID) !== seq) return
        setQueues((prev) => {
          const before = prev[runID] ?? []
          // Skip the state write when nothing moved, so an unchanged refetch
          // doesn't repaint every card that renders a queue.
          if (
            before.length === pending.length &&
            before.every((p, i) => p.tool_call_id === pending[i].tool_call_id)
          ) {
            return prev
          }
          const out = { ...prev }
          if (pending.length === 0) delete out[runID]
          else out[runID] = pending
          return out
        })
        // Cancel timers for prompts the server no longer lists, and arm one
        // per newly-seen prompt. The TTL is a backstop for a missed trigger,
        // so it is derived from the deadline the server says is REMAINING —
        // which is what makes a prompt reconstructed mid-window expire on
        // time rather than getting a fresh full one.
        const live = new Set(pending.map((p) => p.tool_call_id))
        const prefix = `${runID}\x00`
        for (const [key, t] of timers.current) {
          if (key.startsWith(prefix) && !live.has(key.slice(prefix.length))) {
            clearTimeout(t)
            timers.current.delete(key)
          }
        }
        for (const p of pending) {
          const key = timerKey(runID, p.tool_call_id)
          if (timers.current.has(key)) continue
          timers.current.set(
            key,
            setTimeout(() => dropPermission(runID, p.tool_call_id), ttlForPrompt(p)),
          )
        }
      })
    },
    [dropPermission, bumpRefreshSeq],
  )

  const resolve = useCallback(
    async (runID: string, toolCallID: string, decision: PermissionDecisionInput) => {
      const res = await postResolvePermission(runID, toolCallID, decision)
      if (res.kind === 'resolved' || res.kind === 'gone') {
        dropPermission(runID, toolCallID)
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

  return { queues, refresh, resolve, dropRun }
}
