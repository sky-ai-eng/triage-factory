import type { Conversation, Task } from '../../types'
import {
  activeProse,
  completionKind,
  formatDurationMs,
  formatElapsed,
  isActiveStatus,
  isFailedStatus,
  workStartedAt,
} from '../../lib/conversationStatus'

// cardModel — what a task and the conversation it carries say about the card,
// derived in one place so the board and its tests read the same rules.
//
// A task is the thing that persists; a run is one attempt at it. There is no
// kind on the data: a card HAS A RUN when its task is being worked or is done
// and a conversation is attached, and what it shows follows from that. A
// queued task renders as a task whatever conversation a requeue handed back
// with it — nobody is working on it, so no elapsed, no activity and no result
// — while that conversation's artifacts are the task's now and stay on the
// card. The one thing to watch is two times on one card: age belongs to a
// task and elapsed to a run, and a card showing both has mixed them.

/** Totals per kind the artifact strip states, in creation order. Omit or
 *  zero a kind to hide it. */
export type ArtifactTotals = { branch?: number; pulls?: number; review?: number; comment?: number }
/** The subset awaiting a human. Only `pulls` (draft) and `review` (pending
 *  and ready) can ever appear here — the other kinds are terminal on write. */
export type PendingTotals = { pulls?: number; review?: number }

// pendingCount is what the card's frame brightens on. One derivation, so the
// frame and the strip's warm marks can never state different numbers.
export function pendingCount(pending?: PendingTotals): number {
  return (pending?.pulls ?? 0) + (pending?.review ?? 0)
}

/** Where the run is, as the status mark encodes it. */
export type Lifecycle = 'queued' | 'working' | 'idle' | 'done' | 'failed' | 'canceled'

export interface CardModel {
  lifecycle: Lifecycle
  /** What the work is: the run's own account when it wrote one, the
   *  reporter's description when it did not, nothing on a failure. */
  summary?: string
  /** The summary is still being generated — the row arrived whole and only
   *  its description is pending. */
  summaryPending: boolean
  /** The agent's live line while working: its current action, or the setup
   *  phase or the queue wait that stands in for one. */
  command?: string
  artifacts?: ArtifactTotals
  pending?: PendingTotals
  /** How long the run has been going, or ran for. Runs only. */
  elapsed?: string
  /** How long the task has waited, or when a snoozed one wakes. Tasks only. */
  age?: string
  ageTitle?: string
  /** The turns the conversation has taken — shape, never progress. */
  chain?: { done: number; total: number }
  /** The working mark's state: a queued run holds the pile built. */
  liveState: 'running' | 'idle'
  /** True while `elapsed` moves, so the card's host ticks the clock. */
  ticking: boolean
}

export function deriveCard(
  task: Task,
  conversation: Conversation | undefined,
  chainSteps: Conversation[] | undefined,
  now: number = Date.now(),
): CardModel {
  const queuedTask = task.status === 'queued' || task.status === 'snoozed'
  const hasRun = !!conversation && !queuedTask
  const lifecycle = lifecycleOf(task, hasRun ? conversation : undefined)
  const working = lifecycle === 'working'

  // The run's own account supersedes the reporter's once there is one; a
  // failure reports nothing here, because why it failed is a run-page fact.
  const summary =
    lifecycle === 'failed'
      ? undefined
      : (hasRun && conversation?.ResultSummary) || task.ai_summary || undefined

  // The row is never empty on a live card. A queued run is waiting on a
  // slot, and the board says so in the same moving line it keeps using once
  // the agent starts talking; from the claim on, the line is the one every
  // surface narrates a live conversation with — the setup phase, then the
  // agent's own action, with the bare state word standing in until it has
  // one — so a card and an Overview row never tell two stories about one run.
  let command: string | undefined
  if (working && conversation) {
    if (conversation.Status === 'queued') {
      const ahead = (conversation.queue_position ?? 1) - 1
      command = ahead > 0 ? `Waiting for a run slot · ${ahead} ahead` : 'Waiting for a run slot'
    } else command = activeProse(conversation)
  }

  let elapsed: string | undefined
  let ticking = false
  if (hasRun && conversation) {
    if (working) {
      // Queue dwell is its own clock: a queued run's wait ticks from when it
      // entered the line, a started run's elapsed from the claim stamp, so
      // queue time never inflates working time.
      const from =
        conversation.Status === 'queued'
          ? (conversation.QueuedAt ?? conversation.StartedAt)
          : workStartedAt(conversation)
      elapsed = formatElapsed(from, now)
      ticking = true
    } else if (conversation.DurationMs != null) {
      elapsed = formatDurationMs(conversation.DurationMs)
    }
  }

  let age: string | undefined
  let ageTitle: string | undefined
  if (!hasRun) {
    const wakes = parseFutureSnooze(task.snooze_until, now)
    if (wakes) {
      age = `wakes ${formatSnoozeUntil(wakes, now)}`
      ageTitle = `Snoozed until ${wakes.toLocaleString()}. Wakes automatically on the next matching event, or when it is claimed.`
    } else {
      age = formatAge(task.created_at, now)
    }
  }

  return {
    lifecycle,
    summary,
    summaryPending: !summary && task.scoring_status !== 'scored',
    command,
    artifacts: artifactTotals(conversation),
    pending: pendingTotals(conversation),
    elapsed,
    age,
    ageTitle,
    chain: chainShape(chainSteps),
    liveState: conversation?.Status === 'queued' ? 'idle' : 'running',
    ticking,
  }
}

function lifecycleOf(task: Task, run: Conversation | undefined): Lifecycle {
  // No run: a task, waiting — or, for a task ended by hand with nothing ever
  // run on it, simply done.
  if (!run) return task.status === 'done' ? 'done' : 'queued'
  const s = run.Status
  if (s === 'queued' || isActiveStatus(s)) return 'working'
  if (isFailedStatus(s)) return 'failed'
  if (s === 'open') return 'idle'
  // The one stored success terminal means three things (lib/conversationStatus
  // completionKind): the work is over, a step handed off, or the agent stopped
  // without finishing. Only the last is not a finish.
  if (s === 'completed') return completionKind(run) === 'stopped' ? 'canceled' : 'done'
  // A status this build does not know: nothing is moving, and the card says
  // so rather than guessing a verdict.
  return 'idle'
}

// The strip's four kinds. Issues and messages have no mark on the strip; a
// run that wrote one of those still has its artifact on the run page.
function artifactTotals(conversation: Conversation | undefined): ArtifactTotals | undefined {
  const counts = conversation?.artifact_counts
  if (!counts) return undefined
  const out: ArtifactTotals = {}
  if (counts.branch) out.branch = counts.branch
  if (counts.pull_request) out.pulls = counts.pull_request
  if (counts.review) out.review = counts.review
  if (counts.comment) out.comment = counts.comment
  return Object.keys(out).length > 0 ? out : undefined
}

function pendingTotals(conversation: Conversation | undefined): PendingTotals | undefined {
  if (!conversation) return undefined
  const pulls = conversation.unresolved_pr_count ?? 0
  const review = conversation.unresolved_review_count ?? 0
  if (pulls === 0 && review === 0) return undefined
  const out: PendingTotals = {}
  if (pulls > 0) out.pulls = pulls
  if (review > 0) out.review = review
  return out
}

// The chain reports SHAPE, not progress: an agent conversation has no known
// length, so the segments say "four turns, two of them finished" rather than
// a fraction of anything. A plan of one step is the plain single-prompt case
// and reads as no chain at all.
function chainShape(
  steps: Conversation[] | undefined,
): { done: number; total: number } | undefined {
  if (!steps || steps.length <= 1) return undefined
  return { total: steps.length, done: steps.filter((s) => s.Status === 'completed').length }
}

// formatAge — coarse "just now / 4h ago / 3d ago" for the card's age readout.
export function formatAge(dateStr: string, now: number = Date.now()): string {
  const diff = now - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

// parseFutureSnooze returns the snooze target as a Date when the task is
// currently snoozed (parseable and in the future); null otherwise.
function parseFutureSnooze(snoozeUntil: string | undefined, now: number): Date | null {
  if (!snoozeUntil) return null
  const d = new Date(snoozeUntil)
  if (Number.isNaN(d.getTime())) return null
  if (d.getTime() <= now) return null
  return d
}

// formatSnoozeUntil prints "in 2h" / "in 3d" / "Mar 5" so the readout stays
// compact. The full timestamp lives in the title attribute.
function formatSnoozeUntil(until: Date, now: number): string {
  const diff = until.getTime() - now
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'soon'
  if (hours < 24) return `in ${hours}h`
  const days = Math.floor(hours / 24)
  if (days < 7) return `in ${days}d`
  return until.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}
