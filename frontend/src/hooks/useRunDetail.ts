import { useCallback, useEffect, useRef, useState } from 'react'
import type { Message, Conversation, Artifact, Task, WSEvent } from '../types'
import { readError } from '../lib/api'
import { isActiveRun, isPermissionTerminalStatus } from '../lib/runStatus'
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
  resolvePermission: (toolCallID: string, decision: PermissionDecisionInput) => Promise<void>
  /** Silently re-pull the run row + its artifact set (no loading flash, unlike
   *  refetch). Used after a per-item approve/dismiss so the derived approval
   *  surface (has_unresolved_artifacts + the list) updates in place without
   *  blanking the whole station to a spinner mid-resolve. */
  softRefresh: () => void
}

// mergeMessages folds rows into the held transcript, keeping it ordered by id
// and dropping any id already present — the held copy wins, since it may carry
// a stamp the incoming one lacks. Every path that adds rows goes through it
// (the initial fetch, the websocket append, the reconcile poll), so a row
// delivered twice — a refetch racing a frame, or a repair of a frame that
// turned out not to have been dropped — can never render twice.
function mergeMessages(prev: Message[], incoming: Message[]): Message[] {
  if (prev.length === 0) return incoming
  const held = new Set(prev.map((m) => m.id))
  const fresh = incoming.filter((m) => !held.has(m.id))
  if (fresh.length === 0) return prev
  return [...prev, ...fresh].sort((a, b) => a.id - b.id)
}

// maxMessageID is the last id in a read, or 0 for an empty one.
function maxMessageID(msgs: Message[]): number {
  return msgs.reduce((max, m) => (m.id > max ? m.id : max), 0)
}

interface TokenDelta {
  input: number
  output: number
  cacheRead: number
  cacheWrite: number
}

// tokenDelta reads a row's usage, or null when it carries none — the token
// analogue of an unstamped cost row, and the same no-op. Unlike the cost stamp
// (a lump settled onto an existing row at completion), usage is written when
// the row is inserted, so a streamed row carries its own turn's counts and a
// fold needs nothing more than the frame.
function tokenDelta(msg: Message): TokenDelta | null {
  const input = msg.input_tokens ?? 0
  const output = msg.output_tokens ?? 0
  const cacheRead = msg.cache_read_tokens ?? 0
  const cacheWrite = msg.cache_creation_tokens ?? 0
  if (input === 0 && output === 0 && cacheRead === 0 && cacheWrite === 0) return null
  return { input, output, cacheRead, cacheWrite }
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

  // Mirrors of the two states the reconcile tick reads. That interval is
  // created once per run, so it cannot close over `run` / `messages` directly:
  // a dep on either would tear the timer down and rebuild it on every streamed
  // row (foldMessageUsage writes `run` per stamped message), and on a busy run
  // the 5s tick would never come due. Written after commit, so by the time a
  // tick fires they hold what the view is showing.
  const runRef = useRef<Conversation | null>(null)
  const messagesRef = useRef<Message[]>([])

  // Whether this run has been seen live since the last repair, which is what
  // buys the settled run one closing read instead of none.
  //
  // Gating the repair on "live right now" leaves the last row of a run
  // unreachable: status and transcript arrive as separate frames, so the hub
  // can drop the final `message` and deliver the `conversation_update` that
  // settles the run, and by the next tick the gate is shut on a transcript
  // that is still missing a row. Set here rather than at tick time so it
  // reflects every run row observed — a run that settles between two ticks
  // still leaves the flag up for the tick that follows.
  const sawActiveRef = useRef(false)
  useEffect(() => {
    runRef.current = run
    if (run && isActiveRun(run)) sawActiveRef.current = true
  }, [run])
  useEffect(() => {
    messagesRef.current = messages
  }, [messages])

  // The id through which the held transcript is known complete, and the
  // watermark every repair reads from. It is the high-water mark of a *whole*
  // read — the initial fetch, or a repair response — and never of a websocket
  // append. That distinction is the whole of the repair.
  //
  // The tempting version, "highest id held", is unsound: ids come from one
  // table shared by every conversation and the display read hides withdrawn
  // rows, so a gap in what the client holds is indistinguishable from the
  // ordinary spacing between two ids and there is nothing to detect it with.
  // Frames go missing with the socket still up (the hub drops for a slow
  // client rather than disconnecting), so a row that lands above a gap is the
  // expected case, not a corner one — and a watermark taken from it steps
  // over the dropped rows permanently, which is the bug rather than the fix.
  //
  // The price of the sound watermark is that a repair re-sends whatever the
  // socket delivered since the previous one, so an open station on a live run
  // carries each row twice. That is what verifying instead of assuming costs,
  // and it is bounded by a single tick of streaming.
  const completeThroughRef = useRef(0)

  // The ids whose cost stamp the displayed total already counts: seeded from the
  // fetched transcript (those stamps are inside the run row's SUM) and extended
  // by every row foldMessageUsage adds. An id enters it only once a non-null
  // stamp has been seen for it, so a row that streamed unstamped can still be
  // folded if a stamp arrives later.
  //
  // null means the baseline is unknown — no load has established which ids the
  // displayed SUM covers — and nothing may be folded against it.
  const costBaseline = useRef<Set<number> | null>(null)

  // The same baseline for the token rollups, kept as its own set rather than
  // shared with cost: the two stamps land on different rows. Usage rides nearly
  // every assistant row as it streams, while cost settles as a single lump at
  // the end of an engagement — so one set gated on either would either strand
  // most token deltas or re-count them.
  const tokenBaseline = useRef<Set<number> | null>(null)

  // foldMessageUsage accumulates a streamed row's settled cost and its token
  // usage into the held run's rollups. The run row is re-read only on a status
  // flip or an artifact transition, so on a long engagement with neither, both
  // readouts would otherwise sit at whatever the SUMs were when the page loaded
  // — a runtime that stamps every assistant row as it streams reads badly low
  // for most of the run. Each id is folded at most once per rollup (a
  // refetch/websocket race replays rows), and the next run refetch replaces the
  // accumulation with the server's authoritative SUMs, so drift self-corrects
  // rather than compounding. A row carrying neither figure — every SDK-runtime
  // row's cost until it settles at terminal time — is a no-op for that rollup.
  //
  // A fold needs both halves of the baseline in hand — the run row's SUM and the
  // ids inside it — so it holds while a load is in flight. Both windows are one
  // round trip wide and each fails a different way: before the run row lands
  // there is no object to fold into, and marking the id anyway would strand its
  // figures for good (counted, never added); between the run row and the
  // transcript the ids inside the SUM aren't known yet, so a row already counted
  // there would be folded a second time. Holding costs at worst under-reporting
  // until the next read, which is the bounded drift the rest of this accepts.
  const foldMessageUsage = useCallback((msg: Message) => {
    const cost = msg.cost_usd
    const costSeen = costBaseline.current
    if (cost != null && costSeen !== null && !costSeen.has(msg.id)) {
      costSeen.add(msg.id)
      setRun((prev) => (prev ? { ...prev, TotalCostUSD: (prev.TotalCostUSD ?? 0) + cost } : prev))
    }
    const usage = tokenDelta(msg)
    const tokensSeen = tokenBaseline.current
    if (usage !== null && tokensSeen !== null && !tokensSeen.has(msg.id)) {
      tokensSeen.add(msg.id)
      setRun((prev) =>
        prev
          ? {
              ...prev,
              input_tokens: (prev.input_tokens ?? 0) + usage.input,
              output_tokens: (prev.output_tokens ?? 0) + usage.output,
              cache_read_tokens: (prev.cache_read_tokens ?? 0) + usage.cacheRead,
              cache_creation_tokens: (prev.cache_creation_tokens ?? 0) + usage.cacheWrite,
            }
          : prev,
      )
    }
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
  // on fetch/TTL behavior. A run with no prompts is absent from the map.
  const { queues, refresh: refreshPermissions, resolve, dropRun } = usePermissionQueues()
  const pendingPermissions = runID ? (queues[runID] ?? []) : []

  // Read the pending set on mount and on every navigation to a different run.
  // This is the case a fire-once frame structurally could not serve: open (or
  // reload) a run whose agent is already parked on a prompt and the prompt
  // renders, instead of the page looking idle until the server-side timeout
  // silently denied it.
  useEffect(() => {
    if (runID) refreshPermissions(runID)
  }, [runID, refreshPermissions])

  // resolvePermission answers a prompt for this run via the shared resolver,
  // which drops it on a definitive response (200/404) and toasts a transient
  // failure (prompt stays up to retry).
  const resolvePermission = useCallback(
    (toolCallID: string, decision: PermissionDecisionInput) => {
      if (!runID) return Promise.resolve()
      return resolve(runID, toolCallID, decision)
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
      // Nothing has been read for this run yet, so it is complete through
      // nothing — the load below re-establishes the watermark, and the run
      // row it fetches decides whether this one is owed any repair at all.
      completeThroughRef.current = 0
      messagesRef.current = []
      sawActiveRef.current = false
      // A new run starts with no prompts; drop the prior run's queue + timers.
      if (prevRunID) dropRun(prevRunID)
    }
    // A load in flight invalidates both baselines until the seed below
    // re-establishes them — for a same-run refetch as much as a navigation,
    // since either way the totals are about to be replaced by SUMs whose
    // covered ids aren't known yet.
    costBaseline.current = null
    tokenBaseline.current = null
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
            // The fetched transcript's stamped rows become the baselines the
            // fold counts from: a websocket replay of any of them can't be
            // folded in a second time, and folding resumes from here.
            //
            // Treating the whole transcript as already covered is an
            // approximation, not an identity. The run row is read first and the
            // transcript second, so a row written between the two reads is in
            // the transcript but outside the SUM the run row carries — seeding
            // it here means its figures are never folded, and the readouts sit
            // that row low until the next run refetch replaces them with a
            // fresh SUM. That is the deliberate direction: reading the
            // transcript first would invert the race into a row the SUM covers
            // but the baseline doesn't, and a replay of it would fold dollars
            // and tokens that are already counted. Under-reporting for one
            // round trip self-corrects on the next read; over-reporting
            // compounds. Closing it properly needs the run row to say which
            // message id its SUM runs through, which the wire doesn't carry.
            const seededCost = new Set<number>()
            const seededTokens = new Set<number>()
            for (const m of msgs) {
              if (m.cost_usd != null) seededCost.add(m.id)
              if (tokenDelta(m) !== null) seededTokens.add(m.id)
            }
            costBaseline.current = seededCost
            tokenBaseline.current = seededTokens
            // A whole read: the transcript is complete through its last row,
            // so repairs start asking from there.
            completeThroughRef.current = maxMessageID(msgs)
            // Merge by id rather than replacing. If a websocket
            // `message` event arrived between the run fetch starting and
            // the messages fetch resolving, a wholesale replace would
            // erase that newer row until the next refetch.
            setMessages((prev) => mergeMessages(prev, msgs))
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
  //
  // The same tick also repairs the transcript. A `message` frame is the only
  // thing that appends a row between page loads, so anything emitted while the
  // socket was down — a reconnect, a suspended tab, a control-plane restart, or
  // a hub drop with the socket still up — would be missing until a full reload,
  // with nothing on screen to say so. Re-reading from the watermark turns the
  // frame back into what it should be: a hint that arrives faster than the
  // poll, rather than the sole path to the row. Bounded by construction — one
  // request per tick while the run is live, answering with the rows written
  // since the last one, plus a single closing read once it settles.
  useEffect(() => {
    if (!runID) return
    let cancelled = false
    const reconcileMessages = () => {
      const current = runRef.current
      if (!current) return
      // A live run is repaired every tick. A settled one is repaired once —
      // its transcript is final, so a single read closes it out and there is
      // never a reason to ask again. A run already settled when the station
      // opened never asks at all: the load read it whole.
      const settled = !isActiveRun(current)
      if (settled && !sawActiveRef.current) return
      const sinceID = completeThroughRef.current
      fetch(`/api/agent/conversations/${runID}/messages?since_id=${sinceID}`)
        .then((r) => (r.ok ? r.json() : null))
        .then((rows: Message[] | null) => {
          if (cancelled || rows === null || runID !== lastRunIDRef.current) return
          // The closing read landed, so stop asking. Only a real response
          // counts — a failed one leaves the flag up so the next tick retries,
          // which is the difference between closing the transcript out and
          // merely having tried to.
          if (settled) sawActiveRef.current = false
          if (rows.length === 0) return
          // Another whole read landed, so the watermark moves up to its last
          // row — but only forward, since a response that raced a fresher one
          // (or a refetch) would otherwise walk it back over rows already
          // reconciled and re-ask for them every tick from then on.
          completeThroughRef.current = Math.max(completeThroughRef.current, maxMessageID(rows))
          // Most of what comes back is already held — the socket delivered it
          // in the seconds since the last repair. What is left is what the
          // socket lost, and it folds its cost exactly as the frame that
          // never arrived would have.
          const seen = new Set(messagesRef.current.map((m) => m.id))
          const repaired = rows.filter((m) => !seen.has(m.id))
          if (repaired.length === 0) return
          setMessages((prev) => mergeMessages(prev, repaired))
          for (const m of repaired) foldMessageUsage(m)
        })
        .catch(() => {})
    }
    const poll = () => {
      reconcileMessages()
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
  }, [runID, refetchArtifacts, foldMessageUsage])

  // Live updates. A `message` event appends, and folds whatever cost and token
  // usage the row carries into the held run's rollups so the spend and token
  // readouts track a long engagement; `conversation_update` refetches the
  // conversation row so status/duration and the authoritative SUMs flip
  // without a full reload. Permission prompts
  // route into the shared queue (ingest on request, forget on a
  // resolved-elsewhere / timeout broadcast).
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (!runID) return
        if (event.type === 'message' && event.conversation_id === runID) {
          // Dedup by id (set server-side by the time the row hits the wire):
          // a refetch, or the reconcile poll, can replay the same row.
          setMessages((prev) => mergeMessages(prev, [event.data]))
          foldMessageUsage(event.data)
        }
        if (event.type === 'conversation_update' && event.conversation_id === runID) {
          // A run that left the running state can't act on a parked prompt —
          // drop the queue so the dock doesn't keep a stale Allow/Deny up until
          // the client TTL fires (mirrors the board's terminal drop).
          if (isPermissionTerminalStatus(event.data.status ?? '')) {
            dropRun(runID)
          }
          // `resumable` rides a status the row may already have — a workspace
          // snapshot landing after the park was announced, or (later) a
          // retention sweep collecting one. Apply it to the held row before the
          // refetch below returns, so the composer enables (or disables) on the
          // frame rather than on the round-trip. The refetch is still the
          // authority and overwrites this.
          const resumable = event.data.resumable
          if (resumable !== undefined) {
            setRun((prev) => (prev ? { ...prev, resumable } : prev))
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
        // Both permission frames are refetch triggers, matching artifact_updated
        // above: the socket says this run's pending set changed, the endpoint
        // says what it changed to.
        if (
          (event.type === 'permission_request' || event.type === 'permission_resolved') &&
          event.conversation_id === runID
        ) {
          refreshPermissions(runID)
        }
      },
      [runID, refreshPermissions, dropRun, refetchArtifacts, foldMessageUsage],
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
