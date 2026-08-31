import { useEffect, useRef, useState } from 'react'
import { HttpError, apiJSON, apiList } from '../../lib/apiClient'
import { ACTIVE_STATUSES } from '../../lib/conversationStatus'
import { TASK_LIST_PATH, TASK_PAGE_SIZE } from '../../lib/taskList'
import { useWebSocket } from '../../hooks/useWebSocket'
import type { ActivityDay } from '../../hooks/useTeamActivity'
import type { TeamUsage } from '../../hooks/useTeamUsage'
import type { Conversation, Task, WSEvent } from '../../types'
import { utcMidnightISO } from './data'

// The Overview's reads. Every hook here follows the keyed-answer shape the
// team surface established: the answer is stored WITH the ask it answers, so a
// team switch reads null (dashes) until the new answer lands — no synchronous
// reset, stale data simply stops matching. And every failure holds the dash:
// nothing on this page invents a number.
//
// Freshness is the invalidation-ping tier, same machinery as the shell rail:
// a websocket hint says something moved, the page refetches through REST, and
// REST carries the scoping. A client that misses a hint is stale, never wrong
// — and there is no polling loop. The page's rule is that a tick never
// fetches: the two wall-clock readouts (the masthead clock here, the row ages
// via ui/shared/LiveText) recompute locally from time the page already holds.

/**
 * The hints that can move a reading on this page. The rail's curated set, plus
 * `event` (every recorded source event — the convergence's feed; poll cycles
 * burst these, which the debounce collapses to one refetch) and
 * `artifact_updated` (a PR merging moves the MERGED count without any task
 * necessarily changing).
 *
 * `message` is deliberately NOT here: an agent streams transcript rows
 * continuously, and a full round of this page's reads per row would be a
 * polling loop wearing a push hat. The one reading a transcript row moves —
 * a RUNNING row's `current_action` — has its own tick (useTranscriptTick),
 * consumed by the one read that carries it.
 */
const HINTS = new Set<WSEvent['type']>([
  'event',
  'task_updated',
  'tasks_updated',
  'task_claimed',
  'conversation_update',
  'conversations_updated',
  'permission_request',
  'permission_resolved',
  'artifact_updated',
])

const HINT_DEBOUNCE_MS = 1000

/** One debounced counter over the stream: bumps once per coalescing window in
 *  which at least one frame matched. Both ticks below are this with a
 *  different question and a different window. The timer is armed by a match
 *  and never by its own expiry — a coalescing window, not a poll. */
function useDebouncedTick(match: (ev: WSEvent) => boolean, windowMs: number): number {
  const [tick, setTick] = useState(0)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useWebSocket((ev) => {
    if (!match(ev)) return
    if (timer.current) return
    timer.current = setTimeout(() => {
      timer.current = null
      setTick((t) => t + 1)
    }, windowMs)
  })
  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current)
    },
    [],
  )
  return tick
}

/** A counter that bumps (debounced) when a hint lands. Data hooks put it in
 *  their effect deps, so one burst of hints is one round of refetches. */
export function useOverviewTick(): number {
  return useDebouncedTick((ev) => HINTS.has(ev.type), HINT_DEBOUNCE_MS)
}

/** How long transcript frames coalesce before the running read goes out. Its
 *  own knob even though it currently matches the hint window: the two windows
 *  pace different streams, and this one bounds the cost to one bounded list
 *  read per window while an agent works. */
const TRANSCRIPT_DEBOUNCE_MS = 1000

/**
 * The transcript's own tick. A RUNNING row's prose is `current_action`,
 * derived server-side from the newest assistant message's tool calls — and the
 * only frame that says that message moved is `message`, which the page hints
 * above exclude. Without this tick the line only refreshed when some other
 * hint happened to land (a poll cycle's event burst, a status flip), which
 * read as frozen for the whole middle of a run.
 *
 * Only an assistant row can move the derivation — the server's pick is the
 * newest assistant message, tool calls or none, so a prose turn honestly drops
 * the line back to "Working" — and every other role is skipped unread. The
 * frames are already on this org-scoped socket either way; consuming them
 * spends a refetch, not wire.
 */
export function useTranscriptTick(): number {
  return useDebouncedTick(
    (ev) => ev.type === 'message' && ev.data.role === 'assistant',
    TRANSCRIPT_DEBOUNCE_MS,
  )
}

/** The grouped conversations-list envelope, flattened: rows in the server's
 *  own order (task-contiguous, newest first within a task) plus the filtered
 *  total, which can exceed the page. */
export type ConversationSet = { rows: Conversation[]; total: number }

type ConversationListEnvelope = {
  runs?: Record<string, Conversation[]>
  total_count?: number
}

async function readConversationSet(body: Record<string, unknown>): Promise<ConversationSet> {
  const page = await apiJSON<ConversationListEnvelope>('/api/agent/conversations/list', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const rows = Object.values(page.runs ?? {}).flat()
  return { rows, total: page.total_count ?? rows.length }
}

/**
 * The page's two conversation sets, one filter each over the same route the
 * Board and the rail read — which is what keeps this page's numbers and
 * theirs the same numbers:
 *
 *   needs   — the server-side YOUR MOVE predicate, team-narrowed.
 *   running — the live display statuses plus `queued`, because a queued run
 *             belongs in RUNNING with its hourglass mark rather than in a
 *             section of its own saying "waiting" four times.
 *
 * The two reads ride different ticks, which is why they are two effects: both
 * refetch on the page tick, but only `running` also refetches on the
 * transcript tick — a transcript row moves `current_action` and nothing in
 * the attention set, so a working agent must not buy the needs read a
 * refetch per window.
 */
export function useConversationSets(
  teamId: string,
  tick: number,
  transcriptTick: number,
): { needs: ConversationSet | null; running: ConversationSet | null } {
  const [got, setGot] = useState<{
    team: string
    needs: ConversationSet | null
    running: ConversationSet | null
  }>({ team: '', needs: null, running: null })
  useEffect(() => {
    if (!teamId) return
    let live = true
    void readConversationSet({ team_ids: [teamId], attention: true, page_size: 20 }).then(
      (needs) => {
        if (live)
          setGot((prev) => ({
            ...(prev.team === teamId ? prev : { running: null }),
            team: teamId,
            needs,
          }))
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, tick])
  useEffect(() => {
    if (!teamId) return
    let live = true
    void readConversationSet({
      team_ids: [teamId],
      statuses: [...ACTIVE_STATUSES, 'queued'],
      page_size: 20,
    }).then(
      (running) => {
        if (live)
          setGot((prev) => ({
            ...(prev.team === teamId ? prev : { needs: null }),
            team: teamId,
            running,
          }))
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, tick, transcriptTick])
  return got.team === teamId ? got : { needs: null, running: null }
}

/**
 * The rows' tasks, for the reference and the glyph. One list read over the
 * statuses that can carry a conversation — `claimed` is the claim axis
 * (a queued task someone took), the three working columns, and recent `done`
 * (a settled run still asking) — keyed by task id for the join.
 */
export function useTasksIndex(teamId: string, tick: number): Map<string, Task> | null {
  const [got, setGot] = useState<{ team: string; index: Map<string, Task> } | null>(null)
  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiList<Task>(TASK_LIST_PATH, {
      statuses: ['claimed', 'in_progress', 'in_review', 'done'],
      team_ids: [teamId],
      closed_since: new Date(Date.now() - 7 * 86400_000).toISOString(),
      page_size: TASK_PAGE_SIZE,
    }).then(
      (page) => {
        if (live) setGot({ team: teamId, index: new Map(page.items.map((t) => [t.id, t])) })
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, tick])
  return got && got.team === teamId ? got.index : null
}

/** One activity window — GET /api/teams/{id}/activity?since=<RFC3339>. The
 *  convergence asks since midnight. Null while unanswered: unknown is not
 *  zero. */
export function useActivityWindow(
  teamId: string,
  sinceISO: string | null,
  tick: number,
): ActivityDay[] | null {
  const key = `${teamId}:${sinceISO ?? ''}`
  const [got, setGot] = useState<{ key: string; days: ActivityDay[] } | null>(null)
  useEffect(() => {
    if (!teamId || !sinceISO) return
    let live = true
    void apiJSON<{ by_day: ActivityDay[] }>(
      `/api/teams/${encodeURIComponent(teamId)}/activity?since=${encodeURIComponent(sinceISO)}`,
    ).then(
      (a) => {
        if (live) setGot({ key: `${teamId}:${sinceISO}`, days: a.by_day })
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, sinceISO, tick])
  return got && got.key === key ? got.days : null
}

/**
 * Today's spend — the usage node since UTC midnight, the same calendar the
 * backend windows and caps by. Member-visible (the ring needs the total and
 * the by-model split, neither of which names a person), but a deployment
 * whose grant disagrees answers 403, and that is `'forbidden'` — a different
 * claim from unanswered: the page removes the ring entirely and the rows take
 * the full width, one fewer answer rather than a dash that never resolves.
 */
export function useSpendToday(teamId: string, tick: number): TeamUsage | 'forbidden' | null {
  const [got, setGot] = useState<{ team: string; usage: TeamUsage | 'forbidden' } | null>(null)
  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiJSON<TeamUsage>(
      `/api/teams/${encodeURIComponent(teamId)}/usage?since=${encodeURIComponent(utcMidnightISO())}`,
    ).then(
      (u) => {
        if (live) setGot({ team: teamId, usage: u })
      },
      (e) => {
        if (live && e instanceof HttpError && e.status === 403)
          setGot({ team: teamId, usage: 'forbidden' })
      },
    )
    return () => {
      live = false
    }
  }, [teamId, tick])
  return got && got.team === teamId ? got.usage : null
}

/**
 * The standing open pull-request backlog for the pile — the team PRs list's
 * count-only read (`page_size: 0` is the count under the same filters). The
 * population is the team's: entities in the repos this team tracks that
 * members authored or that carry the team as their structural owner.
 */
export function useOpenPRCount(teamId: string, tick: number): number | null {
  const [got, setGot] = useState<{ team: string; count: number } | null>(null)
  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiJSON<{ total_count?: number }>(`/api/teams/${encodeURIComponent(teamId)}/prs/list`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ states: ['open'], page_size: 0 }),
    }).then(
      (page) => {
        if (live) setGot({ team: teamId, count: page.total_count ?? 0 })
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, tick])
  return got && got.team === teamId ? got.count : null
}

/**
 * The masthead's clock, updated on the minute. A wall-clock readout, like the
 * row ages: the "nothing ticks" rule is about metrics performing, and a clock
 * that holds a stale minute is simply wrong. No animation rides the change —
 * FlapCount is for counts, not clocks.
 */
export function useClock(): { clock: string; date: string } {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    let interval: ReturnType<typeof setInterval> | null = null
    // Align to the minute boundary first, so the readout never shows a minute
    // that ended up to 59 seconds ago.
    const t = setTimeout(
      () => {
        setNow(new Date())
        interval = setInterval(() => setNow(new Date()), 60_000)
      },
      60_000 - (Date.now() % 60_000),
    )
    return () => {
      clearTimeout(t)
      if (interval) clearInterval(interval)
    }
  }, [])
  return {
    clock: now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false }),
    date: [
      now.toLocaleDateString([], { weekday: 'short' }),
      now.getDate(),
      now.toLocaleDateString([], { month: 'short' }),
    ]
      .join(' ')
      .toUpperCase(),
  }
}
