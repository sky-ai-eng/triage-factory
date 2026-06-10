import { useCallback, useEffect, useRef, useState } from 'react'
import type { AgentMessage, AgentRun, Task, WSEvent } from '../types'
import { readError } from '../lib/api'
import { toast } from '../components/Toast/toastStore'
import { useWebSocket } from './useWebSocket'

// PendingPermission is one in-flight tool-approval prompt surfaced by a run
// (the `permission_request` WS payload). Parallel tool calls can yield more
// than one at a time, so the hook keeps a queue keyed by request_id.
// timeout_ms is the prompt's server-side deadline (relative), used to derive
// the client dismiss TTL; older payloads may lack it.
export interface PendingPermission {
  request_id: string
  tool_name: string
  input: Record<string, unknown>
  timeout_ms?: number
}

// PermissionDecisionInput is the user's answer to a prompt — the body the
// resolve endpoint accepts (message is an optional deny reason / note).
export interface PermissionDecisionInput {
  behavior: 'allow' | 'deny'
  message?: string
}

// Fallback client-side TTL for a payload that carries no timeout_ms,
// mirroring the backend's default permTimeout() (= 5m/2). The backend denies
// an unanswered prompt at its bound but emits no "expired" event, so without
// a timer a timed-out prompt would linger in the dock forever.
const PERMISSION_TTL_FALLBACK_MS = 150_000

// How long the prompt outlives the server deadline before the client drops
// it. Deliberately AFTER the deadline, not racing it: the broker guarantees a
// late answer 404s (never a dropped 200), and the resolver drops the prompt
// on 404 — so a user clicking into the grace window gets a clean dismiss
// instead of a prompt that vanished from under the cursor at t-0.
const PERMISSION_TTL_GRACE_MS = 5_000

export interface RunDetailState {
  run: AgentRun | null
  task: Task | null
  messages: AgentMessage[]
  loading: boolean
  notFound: boolean
  error: string | null
  refetch: () => void
  /** Queue of unanswered tool-permission prompts for this run, head-first. */
  pendingPermissions: PendingPermission[]
  /** Answer a pending prompt; clears it from the queue on a definitive
   *  response (200 resolved, or 404 already-resolved / timed-out). The
   *  promise settles when the POST round-trip finishes — callers may await
   *  it (e.g. to disable buttons) or fire-and-forget. */
  resolvePermission: (requestID: string, decision: PermissionDecisionInput) => Promise<void>
}

// useRunDetail loads a single agent run, its messages, and the parent
// task, then subscribes to live websocket updates so the page stays
// fresh while the agent works. We fetch the task separately because
// AgentRun only carries TaskID, and the detail page wants the title +
// source badge in its header.
export function useRunDetail(runID: string | undefined): RunDetailState {
  const [run, setRun] = useState<AgentRun | null>(null)
  const [task, setTask] = useState<Task | null>(null)
  const [messages, setMessages] = useState<AgentMessage[]>([])
  const [loading, setLoading] = useState(true)
  const [notFound, setNotFound] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refetchTick, setRefetchTick] = useState(0)
  const [pendingPermissions, setPendingPermissions] = useState<PendingPermission[]>([])

  const refetch = useCallback(() => setRefetchTick((n) => n + 1), [])

  // Track the runID the current state belongs to so we can distinguish a
  // same-run refetch (merge messages) from a navigation to a different
  // run (reset, otherwise message IDs from two runs would interleave).
  const lastRunIDRef = useRef<string | undefined>(runID)

  // Per-request TTL timers for pending permission prompts, keyed by request_id.
  // Cleared on resolve and on unmount so a fired timer never touches a stale
  // queue. The ref object is stable, so the unmount cleanup below can capture it.
  const permTimers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())

  // dropPermission removes one prompt from the queue and cancels its TTL timer.
  // Stable so the WS handler and the resolver can share it without re-subscribing.
  const dropPermission = useCallback((requestID: string) => {
    const t = permTimers.current.get(requestID)
    if (t) {
      clearTimeout(t)
      permTimers.current.delete(requestID)
    }
    setPendingPermissions((prev) => prev.filter((p) => p.request_id !== requestID))
  }, [])

  // resolvePermission answers a pending prompt: POST the decision, then drop it
  // on a definitive response — 200 (resolved) or 404 (already answered, or the
  // backend timed out and deregistered it). A transient failure (5xx / network)
  // keeps the prompt up so the user can retry, with a toast.
  const resolvePermission = useCallback(
    async (requestID: string, decision: PermissionDecisionInput) => {
      if (!runID) return
      try {
        const res = await fetch(`/api/agent/runs/${runID}/permissions/${requestID}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(decision),
        })
        if (res.ok || res.status === 404) {
          dropPermission(requestID)
          return
        }
        toast.error(await readError(res, 'Failed to answer permission request'))
      } catch (err) {
        toast.error(`Failed to answer permission request: ${(err as Error).message}`)
      }
    },
    [runID, dropPermission],
  )

  useEffect(() => {
    if (lastRunIDRef.current !== runID) {
      lastRunIDRef.current = runID
      setRun(null)
      setTask(null)
      setMessages([])
      // A new run starts with no prompts; drop the prior run's queue + timers.
      for (const t of permTimers.current.values()) clearTimeout(t)
      permTimers.current.clear()
      setPendingPermissions([])
    }
    if (!runID) {
      setLoading(false)
      setNotFound(true)
      return
    }
    let cancelled = false
    setLoading(true)
    setNotFound(false)
    setError(null)
    ;(async () => {
      try {
        const runRes = await fetch(`/api/agent/runs/${runID}`)
        if (runRes.status === 404) {
          if (!cancelled) {
            setNotFound(true)
            setLoading(false)
          }
          return
        }
        if (!runRes.ok) {
          if (!cancelled) setError(await readError(runRes, 'Failed to load run'))
          return
        }
        const runData = (await runRes.json()) as AgentRun
        if (cancelled) return
        setRun(runData)

        // Parallel: messages + task.
        const [msgsRes, taskRes] = await Promise.all([
          fetch(`/api/agent/runs/${runID}/messages`),
          runData.TaskID ? fetch(`/api/tasks/${runData.TaskID}`) : Promise.resolve(null),
        ])
        if (cancelled) return

        if (msgsRes.ok) {
          const msgs = (await msgsRes.json()) as AgentMessage[]
          if (!cancelled) {
            // Merge by ID rather than replacing. If a websocket
            // agent_message arrived between the run fetch starting and
            // the messages fetch resolving, a wholesale replace would
            // erase that newer row until the next refetch.
            setMessages((prev) => {
              if (prev.length === 0) return msgs
              const byID = new Map<number, AgentMessage>()
              for (const m of msgs) byID.set(m.ID, m)
              for (const m of prev) byID.set(m.ID, m)
              return Array.from(byID.values()).sort((a, b) => a.ID - b.ID)
            })
          }
        } else if (!cancelled) {
          setError(await readError(msgsRes, 'Failed to load messages'))
        }
        if (taskRes && taskRes.ok) {
          const t = (await taskRes.json()) as Task
          if (!cancelled) setTask(t)
        } else if (taskRes && !cancelled) {
          setError(await readError(taskRes, 'Failed to load task'))
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message)
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
  }, [runID, refetchTick])

  // Live updates. agent_message appends; agent_run_update refetches the
  // run row so status/duration/cost flip without a full reload.
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (!runID) return
        if (event.type === 'agent_message' && event.run_id === runID) {
          setMessages((prev) => {
            // Dedup: a refetch + ws race can replay the same row. Match
            // on ID, which is set server-side by the time the row hits
            // the wire.
            if (prev.some((m) => m.ID === event.data.ID)) return prev
            return [...prev, event.data]
          })
        }
        if (event.type === 'agent_run_update' && event.run_id === runID) {
          fetch(`/api/agent/runs/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((data: AgentRun | null) => {
              if (data) setRun(data)
            })
            .catch(() => {})
        }
        if (event.type === 'permission_request' && event.run_id === runID) {
          const req = event.data
          setPendingPermissions((prev) => {
            // Dedup: a request id is unique within a run process, so a replayed
            // event (e.g. a reconnect) must not double-queue the same prompt.
            if (prev.some((p) => p.request_id === req.request_id)) return prev
            return [...prev, req]
          })
          // Arm the client-side TTL once per request (a backstop for the
          // backend's silent timeout-deny), derived from the payload's
          // server-side deadline plus a small grace.
          if (!permTimers.current.has(req.request_id)) {
            const ttl =
              (typeof req.timeout_ms === 'number' && req.timeout_ms > 0
                ? req.timeout_ms
                : PERMISSION_TTL_FALLBACK_MS) + PERMISSION_TTL_GRACE_MS
            permTimers.current.set(
              req.request_id,
              setTimeout(() => dropPermission(req.request_id), ttl),
            )
          }
        }
      },
      [runID, dropPermission],
    ),
  )

  // Clear all TTL timers on unmount so a fired timer can't touch a torn-down
  // component. The ref object is stable, so capturing it here is safe.
  useEffect(() => {
    const timers = permTimers.current
    return () => {
      for (const t of timers.values()) clearTimeout(t)
      timers.clear()
    }
  }, [])

  return {
    run,
    task,
    messages,
    loading,
    notFound,
    error,
    refetch,
    pendingPermissions,
    resolvePermission,
  }
}
