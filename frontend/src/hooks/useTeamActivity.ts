import { useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'

// The team activity node — GET /api/teams/{id}/activity?since= — is the one
// read behind every flow figure on the team surface: the header's task count,
// the sources band, and each source page's figures and chart. It is windowed
// by the caller, so a surface fetches the window its chart needs rather than
// re-deriving one client-side from another window's answer.

// merged / failed are plain ints with real zeros: merged counts the tracked
// set's github:pr:merged events, failed the team's terminally-failed runs —
// both windowed like events/tasks, so sums over any [since, until) are honest.
export type ActivityDay = {
  date: string
  events: number
  tasks: number
  merged: number
  failed: number
}
export type ActivityDaySource = { date: string; source: string; events: number; tasks: number }
export type ActivitySourceRow = {
  source: string
  /** null when the count is undefined for this source (Slack has no tracked
   * set), which renders as a dash — never as a zero claiming a quiet week. */
  events: number | null
  tasks: number
  runs: number
  last_poll_at: string | null
}
export type TeamActivity = {
  team_id: string
  by_day: ActivityDay[]
  by_day_source: ActivityDaySource[]
  by_source: ActivitySourceRow[]
}

/** The window's opening day, `days` back, in the node's own date grammar
 * (UTC calendar date — the node buckets by UTC day, so the client must ask
 * in the same zone or the two disagree around midnight). */
function sinceParam(days: number): string {
  return new Date(Date.now() - days * 86400_000).toISOString().slice(0, 10)
}

/** Fetches the node once per (team, window). Null while unanswered — every
 * consumer renders that as dashes, never as zeros. */
export function useTeamActivity(teamId: string, days: number): TeamActivity | null {
  // The answer is stored with the ask it answers, so a team or window switch
  // reads null (dashes) until the new answer lands — no synchronous reset,
  // stale data simply stops matching.
  const key = `${teamId}:${days}`
  const [got, setGot] = useState<{ key: string; data: TeamActivity } | null>(null)
  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiJSON<TeamActivity>(
      `/api/teams/${encodeURIComponent(teamId)}/activity?since=${sinceParam(days)}`,
    )
      .then((a) => {
        if (live) setGot({ key: `${teamId}:${days}`, data: a })
      })
      .catch(() => {
        // The figures hold their dashes; the page around them still works.
      })
    return () => {
      live = false
    }
  }, [teamId, days])
  return got && got.key === key ? got.data : null
}

/** One source's totals row, or null while the node hasn't answered. */
export function activitySource(a: TeamActivity | null, source: string): ActivitySourceRow | null {
  return a?.by_source.find((r) => r.source === source) ?? null
}

/**
 * The chart series for one source: `days` continuous UTC days ending today,
 * zero-filled where the node reported nothing — a day with no rows is a real
 * zero once the node has answered. Null while it hasn't, because a flat line
 * at zero is a claim about the window.
 */
export function activitySeries(
  a: TeamActivity | null,
  source: string,
  days: number,
): Array<{ events: number; tasks: number }> | null {
  if (!a) return null
  const byDate = new Map(a.by_day_source.filter((d) => d.source === source).map((d) => [d.date, d]))
  const out: Array<{ events: number; tasks: number }> = []
  for (let i = days - 1; i >= 0; i--) {
    const date = new Date(Date.now() - i * 86400_000).toISOString().slice(0, 10)
    const d = byDate.get(date)
    out.push({ events: d?.events ?? 0, tasks: d?.tasks ?? 0 })
  }
  return out
}

/** "40s" split for an Odometer: the number and the unit it rides with.
 * Null when there is no timestamp — no poller, or none completed yet. */
export function sinceParts(iso: string | null): { value: number; unit: string } | null {
  if (!iso) return null
  const ms = Date.now() - Date.parse(iso)
  if (!Number.isFinite(ms) || ms < 0) return null
  const s = Math.floor(ms / 1000)
  if (s < 60) return { value: s, unit: 's' }
  if (s < 3600) return { value: Math.floor(s / 60), unit: 'm' }
  if (s < 86400) return { value: Math.floor(s / 3600), unit: 'h' }
  return { value: Math.floor(s / 86400), unit: 'd' }
}

/** The same reading as one string — "40s" — for the frame's plain figure. */
export function sinceLabel(iso: string | null): string | null {
  const p = sinceParts(iso)
  return p ? `${p.value}${p.unit}` : null
}
