import { useCallback, useEffect, useRef, useState } from 'react'
import type { Message, CuratorRequestStatus, CuratorRequestWithMessages, WSEvent } from '../types'
import { readError } from '../lib/api'
import { useWebSocket } from './useWebSocket'

// useCuratorChat owns the per-project chat transcript: REST backfill on
// mount, live updates via the websocket bus, optimistic send, and
// cancel. Everything is keyed by `request_id` because that's what
// every emission (REST history, WS message, WS status update) carries.
//
// One non-obvious bit: the WS push for a freshly-queued request can
// arrive *before* the POST response that tells us the request_id. We
// handle that by parking the user's text on a temporary client-id and
// merging when the real id lands — see `commitOptimisticID` below.

const TERMINAL_STATUSES: CuratorRequestStatus[] = ['done', 'cancelled', 'failed']

function isTerminal(status: CuratorRequestStatus): boolean {
  return TERMINAL_STATUSES.includes(status)
}

export interface UseCuratorChatResult {
  requests: CuratorRequestWithMessages[]
  inFlight: CuratorRequestWithMessages | null
  loading: boolean
  loadError: string | null
  sendError: string | null
  totalCostUSD: number
  send: (content: string) => Promise<void>
  cancel: () => Promise<void>
  reset: () => Promise<{ ok: boolean; conflict?: boolean; error?: string }>
  refetch: () => Promise<void>
}

export function useCuratorChat(projectId: string | undefined): UseCuratorChatResult {
  const [requests, setRequests] = useState<CuratorRequestWithMessages[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [sendError, setSendError] = useState<string | null>(null)

  // Track the live project id in a ref so handlers issued for project
  // A don't apply state when the user has navigated to project B.
  // Mirrors the pattern in ProjectDetail's PATCH closures.
  const liveProjectRef = useRef(projectId)
  useEffect(() => {
    liveProjectRef.current = projectId
  }, [projectId])

  // Per-project fetch seq: drop responses whose project no longer
  // matches the live ref, and drop older fetches that resolved after
  // a newer one (visibility-triggered refetch can stack fetches).
  const fetchSeq = useRef(0)

  const refetch = useCallback(async () => {
    if (!projectId) return
    const myID = projectId
    fetchSeq.current += 1
    const mySeq = fetchSeq.current
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(myID)}/curator/messages`)
      if (myID !== liveProjectRef.current || mySeq !== fetchSeq.current) return
      if (!res.ok) {
        setLoadError(await readError(res, 'Failed to load chat history'))
        return
      }
      const data = (await res.json()) as CuratorRequestWithMessages[]
      if (myID !== liveProjectRef.current || mySeq !== fetchSeq.current) return
      setLoadError(null)
      // Merge by request_id rather than user_input. Earlier versions
      // used user_input equality to decide whether an optimistic row
      // had a server counterpart, which silently dropped the second
      // of two identical-text sends — both optimistics matched the
      // first server row's user_input, so the in-flight second one
      // was filtered out.
      //
      // Per-row policy:
      //   - Server returns a row with id X: it's authoritative for
      //     status/cost/etc. Use the longer message list, since a
      //     WS push that arrived after the GET snapshot was taken
      //     would otherwise be silently discarded.
      //   - Local row not in server's response: keep it. Two cases:
      //     optimistic-prefixed (POST hasn't returned yet) and the
      //     race where a real id was committed locally but the GET
      //     snapshot pre-dates the insert. Both want preservation.
      setRequests((prev) => {
        const dataIds = new Set(data.map((d) => d.id))
        const merged = data.map((serverReq) => {
          const local = prev.find((r) => r.id === serverReq.id)
          if (!local) return serverReq
          const messages =
            local.messages.length > serverReq.messages.length ? local.messages : serverReq.messages
          return { ...serverReq, messages }
        })
        const localOnly = prev.filter((r) => !dataIds.has(r.id))
        return [...merged, ...localOnly]
      })
    } catch (err) {
      if (myID !== liveProjectRef.current || mySeq !== fetchSeq.current) return
      setLoadError(
        `Failed to load chat history: ${err instanceof Error ? err.message : String(err)}`,
      )
    } finally {
      if (myID === liveProjectRef.current && mySeq === fetchSeq.current) {
        setLoading(false)
      }
    }
  }, [projectId])

  // Initial load + reset on project change.
  useEffect(() => {
    if (!projectId) {
      setRequests([])
      setLoading(false)
      return
    }
    setRequests([])
    setLoading(true)
    setLoadError(null)
    setSendError(null)
    refetch()
  }, [projectId, refetch])

  // Refetch on tab visibility return — the WS may have dropped events
  // while the page was backgrounded. The 2s reconnect on the singleton
  // socket recovers the connection but not the missed messages.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === 'visible') {
        refetch()
      }
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => document.removeEventListener('visibilitychange', onVisible)
  }, [refetch])

  // WS handler. Filters to the current project. The converged wire uses one
  // vocabulary across surfaces: a `message` event (project_id set for curator)
  // carries the shared Message DTO and is appended to the active in-flight
  // turn — the transcript row no longer carries a turn id, and curator
  // serializes turns so there is exactly one in-flight turn to attach to; a
  // `conversation_update` carries { request_id, status } for a curator turn; a
  // `conversation_reset` clears the transcript.
  const handleWS = useCallback(
    (event: WSEvent) => {
      if (!projectId) return
      if (event.type === 'message') {
        if (event.project_id !== projectId) return
        const msg = event.data
        setRequests((prev) => mergeMessage(prev, msg))
        return
      }
      if (event.type === 'conversation_update') {
        // A delegated-run conversation_update carries conversation_id (no
        // project_id) — filter it out; only the curator turn variant applies.
        if (event.project_id !== projectId) return
        const { request_id, status } = event.data
        if (!request_id || !status) return
        setRequests((prev) => mergeStatus(prev, request_id, status as CuratorRequestStatus))
        return
      }
      if (event.type === 'conversation_reset') {
        // Mirrors the backend wipe: drop the local transcript so a
        // second tab viewing this project clears at the same instant
        // the tab that issued the reset does. The next REST refetch
        // would also produce zero rows, but this gives an immediate
        // UI response without waiting for the round-trip.
        if (event.project_id !== projectId) return
        setRequests([])
        setSendError(null)
        return
      }
    },
    [projectId],
  )
  useWebSocket(handleWS)

  const send = useCallback(
    async (content: string) => {
      if (!projectId) return
      const trimmed = content.trim()
      if (!trimmed) return
      setSendError(null)

      // Optimistic local id. Real id replaces this when POST returns.
      const optimisticID = `optimistic-${Math.random().toString(36).slice(2, 10)}`
      const now = new Date().toISOString()
      const optimistic: CuratorRequestWithMessages = {
        id: optimisticID,
        project_id: projectId,
        status: 'queued',
        user_input: trimmed,
        cost_usd: 0,
        duration_ms: 0,
        num_turns: 0,
        created_at: now,
        messages: [],
      }
      setRequests((prev) => [...prev, optimistic])

      try {
        const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/curator/messages`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: trimmed }),
        })
        if (!res.ok) {
          const msg = await readError(res, 'Failed to send message')
          setSendError(msg)
          // Remove the optimistic bubble — the message never landed.
          setRequests((prev) => prev.filter((r) => r.id !== optimisticID))
          return
        }
        const { request_id } = (await res.json()) as { request_id: string }
        setRequests((prev) => commitOptimisticID(prev, optimisticID, request_id))
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        setSendError(`Failed to send message: ${msg}`)
        setRequests((prev) => prev.filter((r) => r.id !== optimisticID))
      }
    },
    [projectId],
  )

  // Identify the active in-flight request (queued or running). The
  // backend serializes per-project so there's at most one. Cancel and
  // composer-disable derive from this.
  const inFlight = requests.find((r) => !isTerminal(r.status)) ?? null

  const cancel = useCallback(async () => {
    if (!projectId) return
    if (!inFlight) return
    // Optimistic id can't be cancelled server-side — there's no real
    // request_id yet. Window is sub-second; the button stays enabled
    // but the call no-ops until the real id lands.
    if (inFlight.id.startsWith('optimistic-')) return
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/curator/messages/in-flight`,
        { method: 'DELETE' },
      )
      // 404 means the request finished between the user's click and
      // the request landing — treat as success (the row is already
      // terminal locally on the next WS update).
      if (!res.ok && res.status !== 404 && res.status !== 204) {
        setSendError(await readError(res, 'Failed to cancel'))
      }
    } catch (err) {
      setSendError(`Failed to cancel: ${err instanceof Error ? err.message : String(err)}`)
    }
  }, [projectId, inFlight])

  // Sum cost across all terminal requests in this session. The
  // `cost_usd` is stamped on the request row at terminal time, so
  // queued/running rows contribute 0 — that's intentional.
  const totalCostUSD = requests.reduce((sum, r) => sum + (r.cost_usd || 0), 0)

  // reset POSTs to the curator/reset endpoint. The backend broadcasts
  // `conversation_reset` on success, which our WS handler picks up and
  // clears local state — so this function only owns the network call,
  // not the UI mutation. Returns a structured result so the caller can
  // distinguish "in-flight conflict" (409) from a real failure and
  // surface a clear hint.
  const reset = useCallback(async () => {
    if (!projectId) return { ok: false, error: 'no project' as string }
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/curator/reset`, {
        method: 'POST',
      })
      if (res.status === 204) return { ok: true }
      if (res.status === 409) {
        return {
          ok: false,
          conflict: true,
          error: 'A turn is in flight — cancel it before resetting.',
        }
      }
      return { ok: false, error: await readError(res, 'Failed to reset chat') }
    } catch (err) {
      return {
        ok: false,
        error: `Failed to reset chat: ${err instanceof Error ? err.message : String(err)}`,
      }
    }
  }, [projectId])

  return {
    requests,
    inFlight,
    loading,
    loadError,
    sendError,
    totalCostUSD,
    send,
    cancel,
    reset,
    refetch,
  }
}

// mergeMessage appends a Message to its parent request, deduping
// by message id. If the request_id is unknown locally, creates a stub
// so the message lands somewhere — the next REST refetch fills in
// user_input + accounting fields.
// mergeMessage appends a streamed agent row to its turn, deduping by message
// id. The shared Message DTO no longer carries a turn id, so it attaches to
// the active in-flight turn — curator serializes turns per conversation, so
// there is at most one (the last non-terminal request). When none is present
// locally (a second tab that never saw the user's optimistic turn), the row is
// dropped from the live view and the next REST refetch reconstructs the turn.
function mergeMessage(
  requests: CuratorRequestWithMessages[],
  msg: Message,
): CuratorRequestWithMessages[] {
  let idx = -1
  for (let i = requests.length - 1; i >= 0; i--) {
    if (!isTerminal(requests[i].status)) {
      idx = i
      break
    }
  }
  if (idx === -1) return requests
  const existing = requests[idx]
  if (existing.messages.some((m) => m.id === msg.id)) {
    return requests
  }
  const next = [...requests]
  next[idx] = { ...existing, messages: [...existing.messages, msg] }
  return next
}

function mergeStatus(
  requests: CuratorRequestWithMessages[],
  requestID: string,
  status: CuratorRequestStatus,
): CuratorRequestWithMessages[] {
  const idx = requests.findIndex((r) => r.id === requestID)
  if (idx === -1) {
    const stub: CuratorRequestWithMessages = {
      id: requestID,
      project_id: '',
      status,
      user_input: '',
      cost_usd: 0,
      duration_ms: 0,
      num_turns: 0,
      created_at: new Date().toISOString(),
      messages: [],
    }
    return [...requests, stub]
  }
  const next = [...requests]
  next[idx] = { ...next[idx], status }
  return next
}

// commitOptimisticID swaps a temporary client id for the real one
// returned by POST. Two cases:
//   - Real id is unknown locally: rename the optimistic in place.
//   - Real id already exists (WS arrived first with a stub): merge
//     the optimistic's user_input into the real row and drop the
//     optimistic.
function commitOptimisticID(
  requests: CuratorRequestWithMessages[],
  optimisticID: string,
  realID: string,
): CuratorRequestWithMessages[] {
  const optIdx = requests.findIndex((r) => r.id === optimisticID)
  if (optIdx === -1) return requests
  const optimistic = requests[optIdx]
  const realIdx = requests.findIndex((r) => r.id === realID)
  if (realIdx === -1) {
    const next = [...requests]
    next[optIdx] = { ...optimistic, id: realID }
    return next
  }
  // Merge: keep the WS-built row's status + messages, but bring in
  // user_input from the optimistic which the WS event didn't carry.
  const real = requests[realIdx]
  const merged: CuratorRequestWithMessages = {
    ...real,
    user_input: real.user_input || optimistic.user_input,
    created_at: optimistic.created_at < real.created_at ? optimistic.created_at : real.created_at,
  }
  const next = requests.filter((r) => r.id !== optimisticID)
  const mergedIdx = next.findIndex((r) => r.id === realID)
  next[mergedIdx] = merged
  return next
}
