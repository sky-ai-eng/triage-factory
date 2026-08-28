import { useCallback, useEffect, useRef, useState } from 'react'
import { apiList } from '../lib/apiClient'
import { queueCountBody, TASK_LIST_PATH } from '../lib/taskList'
import { ACTIVE_STATUSES } from '../lib/conversationStatus'
import { useApiOrgId } from './useApiOrgId'
import { useWebSocket } from './useWebSocket'
import type { WSEvent } from '../types'

// The shell rail's three live counts. Each is a COUNT-ONLY LIST READ —
// `page_size: 0` on the resource's own list route, which returns `total_count`
// under the same filters and no rows — because a count of rows is a list read
// with the page removed, never a bespoke aggregate. There is deliberately no
// /api/shell and no /counts: three cheap reads that reuse the filters the
// surfaces already use cannot disagree with those surfaces, and an aggregate
// would be a fourth definition of each set.
//
// Freshness is the invalidation-ping tier: a websocket hint says something
// changed, this refetches through REST, and REST is what carries the scoping.
// So a client that misses a hint is STALE, never wrong — and there is no
// polling loop, because a timer would be a second, worse hint that never stops
// firing.

/** The conversations list route. Its filters are a body, so the read is a POST. */
const CONVERSATION_LIST_PATH = '/api/agent/conversations/list'

/** How long a burst of hints is allowed to coalesce before the reads go out.
 *  One delegation fires several frames in a second (a status flip, its task's
 *  column move, the first phase write); a round of counts per frame would be
 *  three times the work for one answer. */
const HINT_DEBOUNCE_MS = 1000

/**
 * The hints that can move one of the three sets. Each is an invalidation
 * signal, not a value — none of them is read for its payload:
 *
 *  - `task_updated` / `tasks_updated` — the queue's status axis: a mint, a
 *    close, a snooze, a board column move.
 *  - `task_claimed` — the queue's CLAIM axis. The queue count is the unclaimed
 *    set, and a claim leaves the status alone, so this is the only hint that
 *    a task was picked up.
 *  - `conversation_update` — a conversation's display status moved, which is
 *    the whole of `running` and half of `needs`.
 *  - `permission_request` / `permission_resolved` — the other half of `needs`.
 *  - `conversations_updated` — a write that moved a conversation between the
 *    sets without touching its status (an artifact resolved by a person or
 *    retired by the reconciler).
 */
const HINTS = new Set<WSEvent['type']>([
  'task_updated',
  'tasks_updated',
  'task_claimed',
  'conversation_update',
  'permission_request',
  'permission_resolved',
  'conversations_updated',
])

export interface RailCounts {
  /** Conversations waiting on a person, counted per conversation. */
  needs: number | null
  /** Conversations with a live engagement — running, or any claim phase. */
  running: number | null
  /** The pickable queue's depth: the Queued column's own filter set. */
  queued: number | null
}

/** Nothing known yet — what a cold load and an org switch both render. Frozen
 *  and shared: it is only ever spread from, never written. */
const BLANK: RailCounts = { needs: null, running: null, queued: null }

/** The rail's `running`: the live display statuses, which is what the run view
 *  lights as WORKING. ACTIVE_STATUSES is derived from the status vocabulary a
 *  Go test pins to the backend's, so a new claim phase joins this count on
 *  both sides at once. */
function runningCountBody() {
  return { statuses: [...ACTIVE_STATUSES], page_size: 0 }
}

/** The rail's `needs`: the server-side "YOUR MOVE" predicate — an unanswered
 *  permission prompt, or a not-live conversation still holding an unresolved
 *  artifact. Derived there rather than here because it is a question about
 *  every conversation the viewer can see, and the client holds none of them. */
function needsCountBody() {
  return { attention: true, page_size: 0 }
}

/**
 * useRailCounts reads the shell rail's three live counts and keeps them fresh
 * off the websocket.
 *
 * Every count starts `null` and stays there until its OWN first answer. Null is
 * "not known yet" and renders as `--`; it is not 0, because "nothing needs you"
 * is a claim, and a rail that makes it before the read lands tells the reader
 * they are done when it has no idea. A count that has answered once then keeps
 * its last value through a failed refetch: that is the hint tier's ordinary
 * stale-until-the-next-hint, and the case where holding a number really is
 * worse than showing none — the stream being down — is the shell's `offline`,
 * which inerts all three readouts on its own.
 */
export function useRailCounts(): RailCounts {
  const orgID = useApiOrgId() ?? ''

  // The answers are stored WITH the org they answer for, so an org switch reads
  // `--` until the new answers land — no synchronous reset, stale numbers
  // simply stop matching. Same shape as useTeamActivity's (team, window) key,
  // and for the same reason: a reset is a render in which the rail asserts
  // something nobody asked it.
  const [answered, setAnswered] = useState<{ org: string; counts: RailCounts }>({
    org: '',
    counts: BLANK,
  })

  // The org the rail is showing RIGHT NOW, for the write below to compare a
  // resolved read against. A ref because the comparison happens inside a
  // promise callback, which holds whatever org it was created under.
  const orgRef = useRef(orgID)
  useEffect(() => {
    orgRef.current = orgID
  }, [orgID])

  const read = useCallback((org: string) => {
    if (!org) return
    const one = async (key: keyof RailCounts, path: string, body: Record<string, unknown>) => {
      try {
        const page = await apiList<unknown>(path, body)
        // total_count is null only on a proxy list that cannot count itself;
        // neither of these is one, but the envelope's type admits it and a
        // null would be a worse claim than no answer.
        if (page.total_count === null) return
        setAnswered((prev) => {
          // A read that resolved for an org this rail has already left belongs
          // to a tenant it is no longer showing: drop it rather than let it
          // overwrite the answers the current one has collected.
          if (orgRef.current !== org) return prev
          const counts = prev.org === org ? prev.counts : BLANK
          return { org, counts: { ...counts, [key]: page.total_count } }
        })
      } catch {
        // Stale-until-the-next-hint. A count that has never answered stays
        // `--`; one that has keeps what it had.
      }
    }
    void one('needs', CONVERSATION_LIST_PATH, needsCountBody())
    void one('running', CONVERSATION_LIST_PATH, runningCountBody())
    void one('queued', TASK_LIST_PATH, queueCountBody())
  }, [])

  // Cold load, and a fresh set on every org change. Declared after the orgRef
  // sync above so that effect has already run when this one reads it.
  useEffect(() => {
    read(orgID)
  }, [orgID, read])

  // Hint → debounced refetch. The timer is a coalescing window, not a poll: it
  // is armed by a hint and never by its own expiry.
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current)
    },
    [],
  )

  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (!HINTS.has(event.type)) return
        if (timer.current) return
        timer.current = setTimeout(() => {
          timer.current = null
          read(orgRef.current)
        }, HINT_DEBOUNCE_MS)
      },
      [read],
    ),
  )

  return answered.org === orgID ? answered.counts : BLANK
}
