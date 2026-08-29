import { useEffect, useRef, useState } from 'react'
import { apiJSON, apiList } from '../../lib/apiClient'
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
// — and there is no polling loop, because the page's own rule is that nothing
// on it ticks.

/**
 * The hints that can move a reading on this page. The rail's curated set, plus
 * `event` (every recorded source event — the convergence's and OPEN PRS' feed;
 * poll cycles burst these, which the debounce collapses to one refetch) and
 * `artifact_updated` (a PR merging moves both MERGED and OPEN PRS without any
 * task necessarily changing).
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

/** A counter that bumps (debounced) when a hint lands. Data hooks put it in
 *  their effect deps, so one burst of hints is one round of refetches. */
export function useOverviewTick(): number {
  const [tick, setTick] = useState(0)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useWebSocket((ev) => {
    if (!HINTS.has(ev.type)) return
    if (timer.current) return
    timer.current = setTimeout(() => {
      timer.current = null
      setTick((t) => t + 1)
    }, HINT_DEBOUNCE_MS)
  })
  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current)
    },
    [],
  )
  return tick
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
 */
export function useConversationSets(
  teamId: string,
  tick: number,
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
  }, [teamId, tick])
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
 *  convergence asks since midnight; the away line asks since the viewer's
 *  marker. Null while unanswered: unknown is not zero. */
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

/** Today's spend — the usage node since UTC midnight, the same calendar the
 *  backend windows and caps by. Member-visible: the donut needs the total and
 *  the by-model split, neither of which names a person. */
export function useSpendToday(teamId: string, tick: number): TeamUsage | null {
  const [got, setGot] = useState<{ team: string; usage: TeamUsage } | null>(null)
  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiJSON<TeamUsage>(
      `/api/teams/${encodeURIComponent(teamId)}/usage?since=${encodeURIComponent(utcMidnightISO())}`,
    ).then(
      (u) => {
        if (live) setGot({ team: teamId, usage: u })
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, tick])
  return got && got.team === teamId ? got.usage : null
}

/** The team's standing work: open PRs in its tracked repos that it authored or
 *  commissioned — the count-only read of the team PRs list, so this figure and
 *  the PR page's team view are the same number by construction. */
export function useOpenPRCount(teamId: string, tick: number): number | null {
  const [got, setGot] = useState<{ team: string; count: number } | null>(null)
  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiList<unknown>(`/api/teams/${encodeURIComponent(teamId)}/prs/list`, {
      states: ['open'],
      page_size: 0,
    }).then(
      (page) => {
        if (live && page.total_count !== null) setGot({ team: teamId, count: page.total_count })
      },
      () => {},
    )
    return () => {
      live = false
    }
  }, [teamId, tick])
  return got && got.team === teamId ? got.count : null
}

type MeSettings = { user_settings?: { overview_seen_at?: string | null } }

/**
 * The away line's anchor: the viewer's own last-visit marker. On mount it
 * reads the PRIOR value (that is the anchor this visit renders against), then
 * stamps now — an explicit write, because reads are side-effect-free on this
 * API. One stamp per mount: revisiting the page is a new visit; a hint-driven
 * refetch is not.
 *
 * `undefined` while the read is in flight, `null` when no marker exists yet
 * (the page anchors to midnight and the copy says so).
 */
export function useOverviewSeen(): string | null | undefined {
  const [anchor, setAnchor] = useState<string | null | undefined>(undefined)
  const stamped = useRef(false)
  useEffect(() => {
    if (stamped.current) return
    stamped.current = true
    let live = true
    void apiJSON<MeSettings>('/api/me/settings')
      .then((s) => {
        if (live) setAnchor(s.user_settings?.overview_seen_at ?? null)
        return apiJSON<MeSettings>('/api/me/settings', {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ user_settings: { overview_seen_at: new Date().toISOString() } }),
        })
      })
      .then(
        () => {},
        () => {
          // The stamp is best-effort; a failed write costs the NEXT visit's
          // anchor, not this render. A failed READ leaves the midnight anchor.
          if (live) setAnchor((a) => (a === undefined ? null : a))
        },
      )
    return () => {
      live = false
    }
  }, [])
  return anchor
}
