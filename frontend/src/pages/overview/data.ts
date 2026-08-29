import type { Conversation, Task } from '../../types'
import { ACTIVE_STATUSES, isFailedStatus, workStartedAt } from '../../lib/conversationStatus'
import type { ActivityDay } from '../../hooks/useTeamActivity'
import type { RunLifecycle, RunRowItem, RunSource } from '../../ui/runrow/RunRows'

// The Overview's pure derivations: conversation + task rows into RunRowItems,
// activity days into window sums. No fetching here — hooks.ts owns the reads,
// this file owns what they mean on screen, and being pure is what lets the
// tests pin the wording.

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
    // queue_position is the 1-based place in line; the mark wears the places
    // AHEAD (position − 1), which is the component's contract — position 1 is
    // nobody ahead, and the mark says so rather than showing a misleading 1.
    queue: queued && conv.queue_position != null ? conv.queue_position - 1 : null,
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

/** The ring's models from the by_model cut, largest first, so the first band
 *  takes the fullest-warm shade. The ring is the MODEL-ATTRIBUTED spend — its
 *  hole totals exactly the bands it draws, so the hover figures always sum to
 *  the standing one; spend with no model behind it (system overhead) belongs
 *  to the usage page the ring links to. */
export function ringModels(
  byModel: Array<{ model: string; cost: number }>,
): Array<{ name: string; v: number }> {
  return [...byModel].sort((a, b) => b.cost - a.cost).map((m) => ({ name: m.model, v: m.cost }))
}
