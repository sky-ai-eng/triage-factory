import type { Conversation, Task } from '../../types'
import { ACTIVE_STATUSES, isFailedStatus, workStartedAt } from '../../lib/conversationStatus'
import type { ActivityDay } from '../../hooks/useTeamActivity'
import type { RunLifecycle, RunRowItem, RunSource } from './RunRows'

// The Overview's pure derivations: conversation + task rows into RunRowItems,
// activity days into window sums, spend into donut arcs, the away lead's
// wording. No fetching here — hooks.ts owns the reads, this file owns what
// they mean on screen, and being pure is what lets the tests pin the wording.

/** Elapsed as the row wears it: `40s`, `11m`, `18h`, `3d`. The row itself does
 *  no time math. A future timestamp (clock skew) reads as `0s`. */
export function compactAge(iso: string, now: number = Date.now()): string {
  const ms = now - Date.parse(iso)
  if (!Number.isFinite(ms)) return ''
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

/** Dollars with cents, the day-figure way: `$41.20`. */
export function money(v: number): string {
  return '$' + v.toFixed(2)
}

/** Today's UTC midnight, RFC3339 — the convergence's SINCE MIDNIGHT anchor,
 *  in the calendar the backend windows and caps by. */
export function utcMidnightISO(now: number = Date.now()): string {
  const d = new Date(now)
  return (
    new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()))
      .toISOString()
      .slice(0, 19) + 'Z'
  )
}

/** The glyph names the SOURCE, not the event — a review request and a merge
 *  conflict both draw a pull request. Failure is the exception and takes its
 *  own mark, because it should not have to be read to be seen. A run with no
 *  task (or a conversation-shaped source like Slack) is a conversation. */
export function sourceOf(conv: Conversation, task: Task | undefined): RunSource {
  if (isFailedStatus(conv.Status)) return 'alert'
  if (task?.source === 'github') return 'pull'
  if (task?.source === 'jira') return 'ticket'
  return 'manual'
}

/** The source's own reference, written the source's way. GitHub's source_id is
 *  `owner/repo#N`; the owner goes — the rail already names the org, and the
 *  repo half is what a reader recognizes. Jira keys are already short. A
 *  source with no compact spelling gets none: the activity carries the row. */
export function refOf(task: Task | undefined): string | null {
  if (!task) return null
  if (task.source === 'github') {
    const slash = task.source_id.indexOf('/')
    return slash >= 0 ? task.source_id.slice(slash + 1) : task.source_id
  }
  if (task.source === 'jira') return task.source_id
  return null
}

export function lifecycleOf(conv: Conversation): RunLifecycle {
  if (conv.Status === 'queued') return 'queued'
  if ((ACTIVE_STATUSES as readonly string[]).includes(conv.Status)) return 'working'
  if (isFailedStatus(conv.Status)) return 'failed'
  return 'done'
}

/** What a setting-up run is doing, in the words the state actually means.
 *  `running` deliberately has no entry: a running row prefers the server's
 *  derived current_action, and the honest fallback when it had nothing to say
 *  is the bare word — never a fabricated action. */
const PHASE_PROSE: Record<string, string> = {
  fetching: 'Fetching the workspace',
  cloning: 'Cloning the repository',
  agent_starting: 'Starting the agent',
  awaiting_credentials: 'Waiting on credentials',
}

/** A working (or queued) row's one prose string. Working rows lead with the
 *  agent's own action; setup phases have real labels; a queued run's prose
 *  names the work it will do — the wait itself is the hourglass mark's job. */
export function workingProse(conv: Conversation, task: Task | undefined): string {
  if (conv.Status === 'queued') return task?.title || 'Waiting for a slot'
  if (conv.current_action) return conv.current_action
  return PHASE_PROSE[conv.Status] ?? 'Working'
}

/** Why a failed run failed, from the machine-readable kind — never parsed out
 *  of message text. Unclassified failures say only what is known. */
const FAILURE_PROSE: Record<string, string> = {
  memory_limit: 'Killed at the memory limit',
  crash: 'The agent process crashed',
  no_result: 'Ended without a result',
  agent_error: 'Failed with an error',
  executor_lost: 'Lost its executor',
  session_lost: 'Lost its session transcript',
}

/** A NEEDS YOU row's prose is the ask. Failure states name their failure;
 *  unresolved artifacts name what to approve; the remainder of the attention
 *  set is a conversation holding a permission prompt. */
export function askProse(conv: Conversation): string {
  if (isFailedStatus(conv.Status)) {
    return FAILURE_PROSE[conv.FailureKind ?? ''] ?? 'Failed'
  }
  const prs = conv.unresolved_pr_count ?? 0
  const reviews = conv.unresolved_review_count ?? 0
  if (prs > 0 && reviews > 0) return `Approve ${prs + reviews} items`
  if (prs > 0)
    return prs === 1 ? 'Approve the draft pull request' : `Approve ${prs} draft pull requests`
  if (reviews > 0)
    return reviews === 1 ? 'Approve the pending review' : `Approve ${reviews} pending reviews`
  return 'Waiting on your approval'
}

/** One RUNNING row. Age ticks from when work actually began (the claim stamp)
 *  so queue dwell never inflates working time; a queued row's age is its wait
 *  so far, beside the hourglass that says how many sit ahead. */
export function runningRow(
  conv: Conversation,
  task: Task | undefined,
  href: string,
  now: number = Date.now(),
): RunRowItem {
  const queued = conv.Status === 'queued'
  return {
    id: conv.ID,
    source: sourceOf(conv, task),
    lifecycle: lifecycleOf(conv),
    activity: workingProse(conv, task),
    ref: refOf(task),
    age: compactAge(queued ? (conv.QueuedAt ?? conv.StartedAt) : workStartedAt(conv), now),
    queue: queued ? (conv.queue_position ?? null) : null,
    href,
  }
}

/** One NEEDS YOU row: the ask leads, the warm tick is the constant, and the
 *  age is how long it has been waiting — from settled time for a terminal row,
 *  from mint otherwise. */
export function needsRow(
  conv: Conversation,
  task: Task | undefined,
  href: string,
  now: number = Date.now(),
): RunRowItem {
  return {
    id: conv.ID,
    source: sourceOf(conv, task),
    lifecycle: lifecycleOf(conv),
    activity: askProse(conv),
    ref: refOf(task),
    age: compactAge(conv.CompletedAt ?? conv.StartedAt, now),
    asks: true,
    href,
  }
}

/** The convergence's figures from one activity window: merged and failed
 *  summed, filtered = events that became no task.
 *  Clamped at zero because the two counts are windowed independently and a
 *  task minted just past a boundary must not read as negative absorption. */
export function windowSums(days: ActivityDay[]): {
  events: number
  merged: number
  failed: number
  filtered: number
} {
  let events = 0
  let tasks = 0
  let merged = 0
  let failed = 0
  for (const d of days) {
    events += d.events
    tasks += d.tasks
    merged += d.merged
    failed += d.failed
  }
  return { events, merged, failed, filtered: Math.max(0, events - tasks) }
}

const DAY_MS = 86400_000

/** The away line's lead. The anchor is the viewer's own marker; before one
 *  exists the page anchors to midnight and the copy says so — honest, and it
 *  costs the sentence its point exactly once. */
export function seenLead(priorISO: string | null, now: number = Date.now()): string {
  if (!priorISO) return 'Your first look since midnight.'
  const t = Date.parse(priorISO)
  if (!Number.isFinite(t)) return 'Your first look since midnight.'
  const then = new Date(t)
  const clock = then.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
  const startOfDay = (ms: number) => {
    const d = new Date(ms)
    return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
  }
  const dayDiff = Math.round((startOfDay(now) - startOfDay(t)) / DAY_MS)
  if (dayDiff <= 0) return `You were last here at ${clock}.`
  if (dayDiff === 1) return `You were last here at ${clock} yesterday.`
  if (dayDiff < 7)
    return `You were last here on ${then.toLocaleDateString([], { weekday: 'long' })}.`
  return `You were last here on ${then.toLocaleDateString([], { day: 'numeric', month: 'short' })}.`
}

/** r=40 → the ring's circumference, shared by every segment's dash math. */
export const DONUT_C = 251.33

/** The donut's shade for slice i of n — one warm ramp stepping down, so the
 *  ring stays a single voice rather than a palette. */
export function donutShade(i: number, n: number): string {
  return `color-mix(in srgb, var(--color-warm) ${Math.round(100 - (i / Math.max(1, n)) * 58)}%, transparent)`
}

export type DonutSegment = { dash: string; offset: string; color: string; delay: string }

/** The ring's segments from the by_model cut, largest first. The 2-unit
 *  deduction is the gap between segments; segments are shares of the
 *  model-attributed spend, while the hole shows the day's whole figure —
 *  system overhead has no model and belongs to the total alone. */
export function donutSegments(byModel: Array<{ model: string; cost: number }>): DonutSegment[] {
  const sorted = [...byModel].sort((a, b) => b.cost - a.cost)
  const total = sorted.reduce((a, m) => a + m.cost, 0)
  if (total <= 0) return []
  let acc = 0
  return sorted.map((m, i) => {
    const len = (m.cost / total) * DONUT_C
    const seg: DonutSegment = {
      dash: `${Math.max(0, len - 2).toFixed(2)} ${DONUT_C}`,
      offset: (-acc).toFixed(2),
      color: donutShade(i, sorted.length),
      delay: (0.15 + i * 0.09).toFixed(2) + 's',
    }
    acc += len
    return seg
  })
}
