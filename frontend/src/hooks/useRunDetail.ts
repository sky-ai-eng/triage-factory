import { useCallback, useEffect, useRef, useState } from 'react'
import type { AgentMessage, AgentRun, Task, WSEvent } from '../types'
import { readError } from '../lib/api'
import { isPermissionTerminalStatus } from '../lib/runStatus'
import { useWebSocket } from './useWebSocket'
import { usePermissionQueues } from './usePermissionQueues'
import type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'

// PendingPermission / PermissionDecisionInput now live in lib/permissions (the
// shared core behind both this hook and the board). Re-exported here so existing
// importers of useRunDetail keep working.
export type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'

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

  const refetch = useCallback(() => setRefetchTick((n) => n + 1), [])

  // Track the runID the current state belongs to so we can distinguish a
  // same-run refetch (merge messages) from a navigation to a different
  // run (reset, otherwise message IDs from two runs would interleave).
  const lastRunIDRef = useRef<string | undefined>(runID)

  // Permission prompts run through the shared queue core (the same one the
  // board uses), filtered to this single run so the two surfaces can't diverge
  // on TTL/dedup behavior. A run with no prompts is absent from the map.
  const { queues, ingest, forget, resolve, dropRun } = usePermissionQueues()
  const pendingPermissions = runID ? (queues[runID] ?? []) : []

  // resolvePermission answers a prompt for this run via the shared resolver,
  // which drops it on a definitive response (200/404) and toasts a transient
  // failure (prompt stays up to retry).
  const resolvePermission = useCallback(
    (requestID: string, decision: PermissionDecisionInput) => {
      if (!runID) return Promise.resolve()
      return resolve(runID, requestID, decision)
    },
    [runID, resolve],
  )

  useEffect(() => {
    const prevRunID = lastRunIDRef.current
    if (prevRunID !== runID) {
      lastRunIDRef.current = runID
      setRun(null)
      setTask(null)
      setMessages([])
      // A new run starts with no prompts; drop the prior run's queue + timers.
      if (prevRunID) dropRun(prevRunID)
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
  }, [runID, refetchTick, dropRun])

  // Tier-2 artifact reconciliation (TFAC-464): while the run view is open, poll
  // the run-scoped refresh endpoint so externally-changed artifacts (a PR
  // merged/closed on GitHub, a branch deleted, a review submitted) reflect
  // without waiting for the background per-org cycle. The backend bounds the
  // work to this run's non-terminal artifacts — a cheap no-op once they're all
  // terminal — and broadcasts any transition as artifact_updated, which the
  // websocket handler below turns into a run refetch. On a dropped frame the
  // {updated} count drives a defensive refetch so the view can't go stale.
  useEffect(() => {
    if (!runID) return
    let cancelled = false
    const poll = () => {
      fetch(`/api/agent/runs/${runID}/artifacts/refresh`, { method: 'POST' })
        .then((r) => (r.ok ? r.json() : null))
        .then((data: { updated?: number } | null) => {
          if (cancelled || !data?.updated) return
          // A transition landed — pull the run row fresh in case the websocket
          // broadcast was missed (the WS handler does the same on its event).
          fetch(`/api/agent/runs/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((run: AgentRun | null) => {
              if (!cancelled && run) setRun(run)
            })
            .catch(() => {})
        })
        .catch(() => {})
    }
    const id = setInterval(poll, 5000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [runID])

  // Live updates. agent_message appends; agent_run_update refetches the
  // run row so status/duration/cost flip without a full reload. Permission
  // prompts route into the shared queue (ingest on request, forget on a
  // resolved-elsewhere / timeout broadcast).
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
          // A run that left the running state can't act on a parked prompt —
          // drop the queue so the dock doesn't keep a stale Allow/Deny up until
          // the client TTL fires (mirrors the board's terminal drop).
          if (isPermissionTerminalStatus(event.data.status)) {
            dropRun(runID)
          }
          fetch(`/api/agent/runs/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((data: AgentRun | null) => {
              if (data) setRun(data)
            })
            .catch(() => {})
        }
        if (event.type === 'artifact_updated' && event.run_id === runID) {
          // Reconciler (TFAC-464): an artifact this run produced changed state
          // on GitHub. Refetch the run so its artifact-derived surface (pending
          // kind / approval overlay) updates. The run's own status is unchanged,
          // so no permission-queue drop here.
          fetch(`/api/agent/runs/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((data: AgentRun | null) => {
              if (data) setRun(data)
            })
            .catch(() => {})
        }
        if (event.type === 'permission_request' && event.run_id === runID) {
          ingest(event)
        }
        if (event.type === 'permission_resolved' && event.run_id === runID) {
          forget(event)
        }
      },
      [runID, ingest, forget, dropRun],
    ),
  )

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
