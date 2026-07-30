import { useCallback, useEffect, useRef, useState } from 'react'
import type { Message, Conversation, Artifact, Task, WSEvent } from '../types'
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
  run: Conversation | null
  task: Task | null
  messages: Message[]
  /** Every artifact this run produced (branch / PR / review / issue / comment),
   *  newest first — the same set GET /api/agent/conversations/{id}/artifacts returns.
   *  Kept live alongside `run` so the approval list + resolve-all confirmation
   *  repaint when an approve/dismiss in another tab changes the set (TFAC-384 §6).
   *  Cross-reference run.pending_artifact_ids to get the *unresolved* subset
   *  (the ready-review predicate needs the run projection, not just artifact.state). */
  artifacts: Artifact[]
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
  /** Silently re-pull the run row + its artifact set (no loading flash, unlike
   *  refetch). Used after a per-item approve/dismiss so the derived approval
   *  surface (has_unresolved_artifacts + the list) updates in place without
   *  blanking the whole station to a spinner mid-resolve. */
  softRefresh: () => void
}

// useRunDetail loads a single agent run, its messages, and the parent
// task, then subscribes to live websocket updates so the page stays
// fresh while the agent works. We fetch the task separately because
// Conversation only carries TaskID, and the detail page wants the title +
// source badge in its header.
export function useRunDetail(runID: string | undefined): RunDetailState {
  const [run, setRun] = useState<Conversation | null>(null)
  const [task, setTask] = useState<Task | null>(null)
  const [messages, setMessages] = useState<Message[]>([])
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [loading, setLoading] = useState(true)
  const [notFound, setNotFound] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refetchTick, setRefetchTick] = useState(0)

  const refetch = useCallback(() => setRefetchTick((n) => n + 1), [])

  // Track the runID the current state belongs to so we can distinguish a
  // same-run refetch (merge messages) from a navigation to a different
  // run (reset, otherwise message IDs from two runs would interleave). It also
  // gates the async setters below so a fetch for the previous run can't land
  // after navigation and overwrite the new run's state.
  const lastRunIDRef = useRef<string | undefined>(runID)

  // Message ids whose cost stamp is already inside the displayed total —
  // either folded from a websocket row (foldMessageCost) or seeded from a
  // fetched transcript, whose stamps the run's SUM already counted. A row
  // enters the set only once a non-null stamp has been seen for it, so a row
  // that streamed unstamped can still be folded if a stamp arrives later.
  const costedMessageIDs = useRef<Set<number>>(new Set())

  // foldMessageCost accumulates a streamed row's settled cost into the held
  // run's total. The run row is re-read only on a status flip or an artifact
  // transition, so on a long engagement with neither, the cost readout would
  // otherwise sit at whatever the SUM was when the page loaded — a runtime that
  // stamps every assistant row as it streams reads badly low for most of the
  // run. Each id is folded at most once (a refetch/websocket race replays
  // rows), and the next run refetch replaces the accumulation with the server's
  // authoritative SUM, so drift self-corrects rather than compounding. Rows with
  // no stamp — every SDK-runtime row until it settles at terminal time — are
  // no-ops, leaving that path exactly as it was.
  const foldMessageCost = useCallback((msg: Message) => {
    const cost = msg.cost_usd
    if (cost == null || costedMessageIDs.current.has(msg.id)) return
    costedMessageIDs.current.add(msg.id)
    setRun((prev) => (prev ? { ...prev, TotalCostUSD: (prev.TotalCostUSD ?? 0) + cost } : prev))
  }, [])

  // Pull the run's artifact set fresh. Shared by the initial load, the WS
  // handlers, and the reconcile poll so an approve/dismiss anywhere (this tab or
  // another) repaints the approval list. Best-effort: a transient failure leaves
  // the prior set in place rather than blanking the surface mid-resolve. Guards
  // on lastRunIDRef so a slow fetch for a since-navigated-away run is discarded
  // rather than clobbering the current run's artifacts.
  const refetchArtifacts = useCallback((id: string) => {
    fetch(`/api/agent/conversations/${id}/artifacts`)
      .then((r) => (r.ok ? r.json() : null))
      .then((data: Artifact[] | null) => {
        if (data && id === lastRunIDRef.current) setArtifacts(data)
      })
      .catch(() => {})
  }, [])

  // softRefresh re-pulls the run row + artifact set without the loading toggle
  // (refetch sets loading=true → the full-screen spinner). A per-item resolve
  // must update the derived approval surface in place, not flash the station.
  // Same stale-navigation guard as refetchArtifacts.
  const softRefresh = useCallback(() => {
    if (!runID) return
    const id = runID
    fetch(`/api/agent/conversations/${id}`)
      .then((r) => (r.ok ? r.json() : null))
      .then((data: Conversation | null) => {
        if (data && id === lastRunIDRef.current) setRun(data)
      })
      .catch(() => {})
    refetchArtifacts(id)
  }, [runID, refetchArtifacts])

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
      setArtifacts([])
      costedMessageIDs.current = new Set()
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
        const runRes = await fetch(`/api/agent/conversations/${runID}`)
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
        const runData = (await runRes.json()) as Conversation
        if (cancelled) return
        setRun(runData)
        // The artifact set drives the approval list; pull it in the same load so
        // the cards are ready when the run paints (best-effort, non-blocking).
        refetchArtifacts(runID)

        // Parallel: messages + task.
        const [msgsRes, taskRes] = await Promise.all([
          fetch(`/api/agent/conversations/${runID}/messages`),
          runData.TaskID ? fetch(`/api/tasks/${runData.TaskID}`) : Promise.resolve(null),
        ])
        if (cancelled) return

        if (msgsRes.ok) {
          const msgs = (await msgsRes.json()) as Message[]
          if (!cancelled) {
            // Every stamped row in the fetched transcript is already inside the
            // run row's SUM, so record them before any of them can be replayed
            // over the websocket and folded in a second time.
            for (const m of msgs) {
              if (m.cost_usd != null) costedMessageIDs.current.add(m.id)
            }
            // Merge by id rather than replacing. If a websocket
            // `message` event arrived between the run fetch starting and
            // the messages fetch resolving, a wholesale replace would
            // erase that newer row until the next refetch.
            setMessages((prev) => {
              if (prev.length === 0) return msgs
              const byID = new Map<number, Message>()
              for (const m of msgs) byID.set(m.id, m)
              for (const m of prev) byID.set(m.id, m)
              return Array.from(byID.values()).sort((a, b) => a.id - b.id)
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
  }, [runID, refetchTick, dropRun, refetchArtifacts])

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
      fetch(`/api/agent/conversations/${runID}/artifacts/refresh`, { method: 'POST' })
        .then((r) => (r.ok ? r.json() : null))
        .then((data: { updated?: number } | null) => {
          if (cancelled || !data?.updated) return
          // A transition landed — pull the run row + its artifact set fresh in
          // case the websocket broadcast was missed (the WS handler does the same
          // on its event), so the derived approval surface can't go stale.
          fetch(`/api/agent/conversations/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((run: Conversation | null) => {
              if (!cancelled && run) setRun(run)
            })
            .catch(() => {})
          if (!cancelled) refetchArtifacts(runID)
        })
        .catch(() => {})
    }
    const id = setInterval(poll, 5000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [runID, refetchArtifacts])

  // Live updates. A `message` event appends, and folds any cost the row carries
  // into the held run's total so the spend readout tracks a long engagement;
  // `conversation_update` refetches the conversation row so status/duration and
  // the authoritative cost SUM flip without a full reload. Permission prompts
  // route into the shared queue (ingest on request, forget on a
  // resolved-elsewhere / timeout broadcast).
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (!runID) return
        if (event.type === 'message' && event.conversation_id === runID) {
          setMessages((prev) => {
            // Dedup: a refetch + ws race can replay the same row. Match
            // on id, which is set server-side by the time the row hits
            // the wire.
            if (prev.some((m) => m.id === event.data.id)) return prev
            return [...prev, event.data]
          })
          foldMessageCost(event.data)
        }
        if (event.type === 'conversation_update' && event.conversation_id === runID) {
          // A run that left the running state can't act on a parked prompt —
          // drop the queue so the dock doesn't keep a stale Allow/Deny up until
          // the client TTL fires (mirrors the board's terminal drop).
          if (isPermissionTerminalStatus(event.data.status ?? '')) {
            dropRun(runID)
          }
          fetch(`/api/agent/conversations/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((data: Conversation | null) => {
              // Guard the async write against a navigation that landed while the
              // fetch was in flight (same guard refetchArtifacts uses), so an
              // old run's row can't clobber the new run's state.
              if (data && runID === lastRunIDRef.current) setRun(data)
            })
            .catch(() => {})
          // A status flip can resolve the last artifact (terminal-on-last) or
          // surface a freshly-staged one — pull the set so the list repaints.
          refetchArtifacts(runID)
        }
        if (event.type === 'artifact_updated' && event.conversation_id === runID) {
          // Reconciler (TFAC-464): an artifact this run produced changed state
          // on GitHub (or another tab approved/dismissed it). Refetch the run so
          // its derived approval signal (has_unresolved_artifacts + counts)
          // updates, and the artifact set so the approval list repaints. The
          // run's own status is unchanged, so no permission-queue drop here.
          fetch(`/api/agent/conversations/${runID}`)
            .then((r) => (r.ok ? r.json() : null))
            .then((data: Conversation | null) => {
              if (data && runID === lastRunIDRef.current) setRun(data)
            })
            .catch(() => {})
          refetchArtifacts(runID)
        }
        if (event.type === 'permission_request' && event.conversation_id === runID) {
          ingest(event)
        }
        if (event.type === 'permission_resolved' && event.conversation_id === runID) {
          forget(event)
        }
      },
      [runID, ingest, forget, dropRun, refetchArtifacts, foldMessageCost],
    ),
  )

  return {
    run,
    task,
    messages,
    artifacts,
    loading,
    notFound,
    error,
    refetch,
    pendingPermissions,
    resolvePermission,
    softRefresh,
  }
}
