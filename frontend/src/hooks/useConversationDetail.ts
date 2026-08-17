import { useCallback, useEffect, useRef, useState } from 'react'
import type { Message, Conversation, Artifact, Task, TranscriptPage, WSEvent } from '../types'
import { apiJSON, HttpError, httpErrorMessage } from '../lib/apiClient'
import { isActiveConversation, isPermissionTerminalStatus } from '../lib/conversationStatus'
import { useWebSocket } from './useWebSocket'
import { usePermissionQueues } from './usePermissionQueues'
import type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'

// PendingPermission / PermissionDecisionInput now live in lib/permissions (the
// shared core behind both this hook and the board). Re-exported here so existing
// importers of useConversationDetail keep working.
export type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'

export interface ConversationDetailState {
  conversation: Conversation | null
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
  /** True while history remains behind the held transcript. The transcript
   *  read is bounded, so a long run opens on its tail; this is what tells a
   *  surface to offer the way back instead of presenting the tail as the
   *  whole conversation. */
  hasOlderMessages: boolean
  /** True while a back-page is in flight. */
  loadingOlderMessages: boolean
  /** Prepend the next page of older history. A no-op at the beginning of the
   *  transcript or while a page is already loading. */
  loadOlderMessages: () => Promise<void>
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

// useConversationDetail loads a single agent run, its messages, and the parent
// task, then subscribes to live websocket updates so the page stays
// fresh while the agent works. We fetch the task separately because
// Conversation only carries TaskID, and the detail page wants the title +
// source badge in its header.
export function useConversationDetail(conversationID: string | undefined): ConversationDetailState {
  const [conversation, setConversation] = useState<Conversation | null>(null)
  const [task, setTask] = useState<Task | null>(null)
  const [messages, setMessages] = useState<Message[]>([])
  const [artifacts, setArtifacts] = useState<Artifact[]>([])
  const [loading, setLoading] = useState(true)
  const [notFound, setNotFound] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [refetchTick, setRefetchTick] = useState(0)
  // The token addressing the page of history OLDER than the held transcript,
  // or '' when the held copy reaches the beginning. The transcript read is
  // bounded (500 rows), so a long run opens on its tail and this is the only
  // way back — without it everything before the newest page is unreachable,
  // with nothing on screen to say history was cut.
  const [olderToken, setOlderToken] = useState('')
  const [loadingOlder, setLoadingOlder] = useState(false)
  // Mirrored so loadOlder stays stable across renders (the station holds it in
  // a click handler) while still reading the current token.
  const olderTokenRef = useRef('')
  const loadingOlderRef = useRef(false)

  const refetch = useCallback(() => setRefetchTick((n) => n + 1), [])

  // Track the conversationID the current state belongs to so we can distinguish a
  // same-run refetch (merge messages) from a navigation to a different
  // run (reset, otherwise message IDs from two runs would interleave). It also
  // gates the async setters below so a fetch for the previous run can't land
  // after navigation and overwrite the new run's state.
  const lastConversationIDRef = useRef<string | undefined>(conversationID)

  // Mirrors of the two states the reconcile tick reads. That interval is
  // created once per run, so it cannot close over `conversation` / `messages`
  // directly: a dep on either would tear the timer down and rebuild it on every
  // streamed row (foldMessageUsage writes `conversation` per stamped message), and on a busy run
  // the 5s tick would never come due. Written after commit, so by the time a
  // tick fires they hold what the view is showing.
  const conversationRef = useRef<Conversation | null>(null)
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
    conversationRef.current = conversation
    if (conversation && isActiveConversation(conversation)) sawActiveRef.current = true
  }, [conversation])
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
      setConversation((prev) =>
        prev ? { ...prev, TotalCostUSD: (prev.TotalCostUSD ?? 0) + cost } : prev,
      )
    }
    const usage = tokenDelta(msg)
    const tokensSeen = tokenBaseline.current
    if (usage !== null && tokensSeen !== null && !tokensSeen.has(msg.id)) {
      tokensSeen.add(msg.id)
      setConversation((prev) =>
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
  // on lastConversationIDRef so a slow fetch for a since-navigated-away run is discarded
  // rather than clobbering the current run's artifacts.
  const refetchArtifacts = useCallback((id: string) => {
    apiJSON<Artifact[]>(`/api/agent/conversations/${id}/artifacts`)
      .then((data) => {
        if (id === lastConversationIDRef.current) setArtifacts(data)
      })
      .catch(() => {})
  }, [])

  // softRefresh re-pulls the run row + artifact set without the loading toggle
  // (refetch sets loading=true → the full-screen spinner). A per-item resolve
  // must update the derived approval surface in place, not flash the station.
  // Same stale-navigation guard as refetchArtifacts.
  const softRefresh = useCallback(() => {
    if (!conversationID) return
    const id = conversationID
    apiJSON<Conversation>(`/api/agent/conversations/${id}`)
      .then((data) => {
        if (id === lastConversationIDRef.current) setConversation(data)
      })
      .catch(() => {})
    refetchArtifacts(id)
  }, [conversationID, refetchArtifacts])

  // Permission prompts run through the shared queue core (the same one the
  // board uses), filtered to this single run so the two surfaces can't diverge
  // on fetch/TTL behavior. A run with no prompts is absent from the map.
  const { queues, refresh: refreshPermissions, resolve, dropConversation } = usePermissionQueues()
  const pendingPermissions = conversationID ? (queues[conversationID] ?? []) : []

  // Read the pending set on mount and on every navigation to a different run.
  // This is the case a fire-once frame structurally could not serve: open (or
  // reload) a run whose agent is already parked on a prompt and the prompt
  // renders, instead of the page looking idle until the server-side timeout
  // silently denied it.
  useEffect(() => {
    if (conversationID) refreshPermissions(conversationID)
  }, [conversationID, refreshPermissions])

  // loadOlderMessages walks one page further back through history.
  //
  // The rows it brings back are OLDER than everything held, so they carry no
  // usage the run row's SUM doesn't already count — they are seeded into the
  // baselines exactly as the first page's rows are, and deliberately not
  // folded. Folding them would add dollars to a total that already includes
  // them, and over-reporting compounds where the under-reporting the load
  // comment describes self-corrects.
  //
  // completeThroughRef is untouched: it is the tail watermark, and reading
  // backward tells the repair poll nothing about the newest row.
  const loadOlderMessages = useCallback(async () => {
    if (!conversationID || !olderTokenRef.current || loadingOlderRef.current) return
    loadingOlderRef.current = true
    setLoadingOlder(true)
    const token = olderTokenRef.current
    try {
      const page = await apiJSON<TranscriptPage>(
        `/api/agent/conversations/${conversationID}/messages?page_token=${encodeURIComponent(token)}`,
      )
      // A run switch (or a refetch) while the page was in flight retires this
      // answer: its rows belong to a transcript the hook no longer holds, and
      // the token it carries was minted against that read.
      if (conversationID !== lastConversationIDRef.current || olderTokenRef.current !== token)
        return
      for (const m of page.items) {
        if (m.cost_usd != null) costBaseline.current?.add(m.id)
        if (tokenDelta(m) !== null) tokenBaseline.current?.add(m.id)
      }
      olderTokenRef.current = page.next_page_token ?? ''
      setOlderToken(olderTokenRef.current)
      setMessages((prev) => mergeMessages(prev, page.items))
    } catch (err) {
      // Non-fatal: the held transcript stays exactly as it was, and the
      // control stays up to retry. Blanking the station because a page of
      // history failed would lose what is already on screen.
      setError(httpErrorMessage(err, 'Could not load earlier messages.'))
    } finally {
      loadingOlderRef.current = false
      setLoadingOlder(false)
    }
  }, [conversationID])

  // resolvePermission answers a prompt for this run via the shared resolver,
  // which drops it on a definitive response (200/404) and toasts a transient
  // failure (prompt stays up to retry).
  const resolvePermission = useCallback(
    (toolCallID: string, decision: PermissionDecisionInput) => {
      if (!conversationID) return Promise.resolve()
      return resolve(conversationID, toolCallID, decision)
    },
    [conversationID, resolve],
  )

  useEffect(() => {
    const prevConversationID = lastConversationIDRef.current
    if (prevConversationID !== conversationID) {
      lastConversationIDRef.current = conversationID
      setConversation(null)
      setTask(null)
      setMessages([])
      setArtifacts([])
      // Nothing has been read for this run yet, so it is complete through
      // nothing — the load below re-establishes the watermark, and the run
      // row it fetches decides whether this one is owed any repair at all.
      completeThroughRef.current = 0
      messagesRef.current = []
      sawActiveRef.current = false
      // The back-page token belongs to the run that minted it, and the server
      // rejects one presented against a different read. Clearing it here is
      // also what hides the control until the new run's load says whether
      // there is any history behind its first page.
      olderTokenRef.current = ''
      setOlderToken('')
      // A new run starts with no prompts; drop the prior run's queue + timers.
      if (prevConversationID) dropConversation(prevConversationID)
    }
    // A load in flight invalidates both baselines until the seed below
    // re-establishes them — for a same-run refetch as much as a navigation,
    // since either way the totals are about to be replaced by SUMs whose
    // covered ids aren't known yet.
    costBaseline.current = null
    tokenBaseline.current = null
    if (!conversationID) {
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
        const conversationData = await apiJSON<Conversation>(
          `/api/agent/conversations/${conversationID}`,
        )
        if (cancelled) return
        setConversation(conversationData)
        // The artifact set drives the approval list; pull it in the same load so
        // the cards are ready when the run paints (best-effort, non-blocking).
        refetchArtifacts(conversationID)

        // Parallel: messages + task. The task read carries its failure rather
        // than throwing it, because the two are independent — a run whose task
        // row has gone still has a transcript worth rendering, and letting the
        // task 404 reject the pair would blank it.
        const [transcript, taskRow] = await Promise.all([
          apiJSON<TranscriptPage>(`/api/agent/conversations/${conversationID}/messages`),
          conversationData.TaskID
            ? apiJSON<Task>(`/api/tasks/${conversationData.TaskID}`).catch((err: unknown) => ({
                taskError: httpErrorMessage(err, 'Could not load the task.'),
              }))
            : null,
        ])
        if (cancelled) return
        const msgs = transcript.items

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
        // The page addressing older history, if the read was bounded. Set
        // before the merge so a station that paints immediately already knows
        // whether to offer the control.
        olderTokenRef.current = transcript.next_page_token ?? ''
        setOlderToken(olderTokenRef.current)
        // Merge by id rather than replacing. If a websocket
        // `message` event arrived between the run fetch starting and
        // the messages fetch resolving, a wholesale replace would
        // erase that newer row until the next refetch.
        setMessages((prev) => mergeMessages(prev, msgs))

        if (taskRow && 'taskError' in taskRow) setError(taskRow.taskError)
        else if (taskRow) setTask(taskRow)
      } catch (err) {
        if (cancelled) return
        // A 404 is this run's own not-found state, not a failed read — the
        // station renders "no such run" rather than an error banner.
        if (err instanceof HttpError && err.status === 404) {
          setNotFound(true)
          return
        }
        setError(httpErrorMessage(err, 'Could not load the run.'))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
  }, [conversationID, refetchTick, dropConversation, refetchArtifacts])

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
    if (!conversationID) return
    let cancelled = false
    const reconcileMessages = () => {
      const current = conversationRef.current
      if (!current) return
      // A live run is repaired every tick. A settled one is repaired once —
      // its transcript is final, so a single read closes it out and there is
      // never a reason to ask again. A run already settled when the station
      // opened never asks at all: the load read it whole.
      const settled = !isActiveConversation(current)
      if (settled && !sawActiveRef.current) return
      const sinceID = completeThroughRef.current
      apiJSON<TranscriptPage>(
        `/api/agent/conversations/${conversationID}/messages?since_id=${sinceID}`,
      )
        .then((page) => {
          const rows = page.items
          if (cancelled || conversationID !== lastConversationIDRef.current) return
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
      apiJSON<{ updated?: number }>(
        `/api/agent/conversations/${conversationID}/artifacts/refresh`,
        {
          method: 'POST',
        },
      )
        .then((data) => {
          if (cancelled || !data?.updated) return
          // A transition landed — pull the run row + its artifact set fresh in
          // case the websocket broadcast was missed (the WS handler does the same
          // on its event), so the derived approval surface can't go stale.
          apiJSON<Conversation>(`/api/agent/conversations/${conversationID}`)
            .then((data) => {
              if (!cancelled) setConversation(data)
            })
            .catch(() => {})
          if (!cancelled) refetchArtifacts(conversationID)
        })
        .catch(() => {})
    }
    const id = setInterval(poll, 5000)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [conversationID, refetchArtifacts, foldMessageUsage])

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
        if (!conversationID) return
        if (event.type === 'message' && event.conversation_id === conversationID) {
          // Dedup by id (set server-side by the time the row hits the wire):
          // a refetch, or the reconcile poll, can replay the same row.
          setMessages((prev) => mergeMessages(prev, [event.data]))
          foldMessageUsage(event.data)
        }
        if (event.type === 'conversation_update' && event.conversation_id === conversationID) {
          // A run that left the running state can't act on a parked prompt —
          // drop the queue so the dock doesn't keep a stale Allow/Deny up until
          // the client TTL fires (mirrors the board's terminal drop).
          if (isPermissionTerminalStatus(event.data.status ?? '')) {
            dropConversation(conversationID)
          }
          // `resumable` rides a status the row may already have — a workspace
          // snapshot landing after the park was announced, or (later) a
          // retention sweep collecting one. Apply it to the held row before the
          // refetch below returns, so the composer enables (or disables) on the
          // frame rather than on the round-trip. The refetch is still the
          // authority and overwrites this.
          const resumable = event.data.resumable
          if (resumable !== undefined) {
            setConversation((prev) => (prev ? { ...prev, resumable } : prev))
          }
          apiJSON<Conversation>(`/api/agent/conversations/${conversationID}`)
            .then((data) => {
              // Guard the async write against a navigation that landed while the
              // fetch was in flight (same guard refetchArtifacts uses), so an
              // old run's row can't clobber the new run's state.
              if (conversationID === lastConversationIDRef.current) setConversation(data)
            })
            .catch(() => {})
          // A status flip can resolve the last artifact (terminal-on-last) or
          // surface a freshly-staged one — pull the set so the list repaints.
          refetchArtifacts(conversationID)
        }
        if (event.type === 'artifact_updated' && event.conversation_id === conversationID) {
          // Reconciler (TFAC-464): an artifact this run produced changed state
          // on GitHub (or another tab approved/dismissed it). Refetch the run so
          // its derived approval signal (has_unresolved_artifacts + counts)
          // updates, and the artifact set so the approval list repaints. The
          // run's own status is unchanged, so no permission-queue drop here.
          apiJSON<Conversation>(`/api/agent/conversations/${conversationID}`)
            .then((data) => {
              if (conversationID === lastConversationIDRef.current) setConversation(data)
            })
            .catch(() => {})
          refetchArtifacts(conversationID)
        }
        // Both permission frames are refetch triggers, matching artifact_updated
        // above: the socket says this run's pending set changed, the endpoint
        // says what it changed to.
        if (
          (event.type === 'permission_request' || event.type === 'permission_resolved') &&
          event.conversation_id === conversationID
        ) {
          refreshPermissions(conversationID)
        }
      },
      [conversationID, refreshPermissions, dropConversation, refetchArtifacts, foldMessageUsage],
    ),
  )

  return {
    conversation,
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
    hasOlderMessages: olderToken !== '',
    loadingOlderMessages: loadingOlder,
    loadOlderMessages,
  }
}
