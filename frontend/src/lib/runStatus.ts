import { CLAIM_PHASES, TERMINAL_RUN_STATUSES } from '../types'
import type { ClaimPhase, Conversation, RunStatusValue, TerminalRunStatus } from '../types'

// Every set here is derived from the vocabulary in types.ts, which is checked
// against internal/domain/run_status.go by a Go test. Adding a claim phase in
// one place therefore lands in every predicate below at once.

// ACTIVE_STATUSES — the run is claimed and occupying an executor slot: setting
// up, or executing a turn. Mirrors domain.IsActiveRunStatus, which is likewise
// `running` plus every claim phase. `queued` is excluded (waiting, not
// working) and so is `open` (parked between turns).
export const ACTIVE_STATUSES = ['running', ...CLAIM_PHASES] as const

// FAILED_STATUSES — the terminals that are not a success, styled rose rather
// than neutral. Derived by excluding the one success rather than re-listing
// the other three, so a renamed terminal can't quietly fall out of it; a
// future terminal lands here by default, which is the safe direction.
export const FAILED_STATUSES = TERMINAL_RUN_STATUSES.filter((s) => s !== 'completed')

export function isTerminalStatus(status: RunStatusValue): status is TerminalRunStatus {
  return (TERMINAL_RUN_STATUSES as readonly string[]).includes(status)
}

// A run that reached a terminal is no longer executing a turn, so any
// tool-permission prompt parked on it is stale and must be dropped — its
// Allow/Deny would 404 or resolve a now-meaningless prompt. Its own name
// because the question is about permission staleness, not lifecycle; the two
// answers coincide today. Shared by the board and useRunDetail so the two
// surfaces drop in lockstep.
export function isPermissionTerminalStatus(status: RunStatusValue): boolean {
  return isTerminalStatus(status)
}

export function isClaimPhase(status: RunStatusValue): status is ClaimPhase {
  return (CLAIM_PHASES as readonly string[]).includes(status)
}

export function isActiveRun(run: Conversation): boolean {
  return isActiveStatus(run.Status)
}

export function isActiveStatus(status: RunStatusValue): boolean {
  return (ACTIVE_STATUSES as readonly string[]).includes(status)
}

export function isFailedStatus(status: RunStatusValue): boolean {
  return (FAILED_STATUSES as readonly string[]).includes(status)
}

// ChainPosition — where a conversation sits in its blueprint's frozen step
// plan. Every delegated run belongs to a blueprint (the schema requires it), so
// a plan of one step IS the plain single-prompt run: that reads as no chain at
// all, and returns null here.
//
// null also covers "position unknown": blueprint_step_count is 0 when the
// server could not resolve the plan (a manual blueprint run belongs to its
// creator under RLS, so a teammate reads 0), and a run predating the field
// carries neither. Callers fall back to the unqualified reading rather than
// guessing a position.
export interface ChainPosition {
  /** 1-based, for display — "step 2 of 4". */
  step: number
  total: number
  /** The last step of the plan: nothing runs after this one. */
  isFinal: boolean
}

export function chainPosition(run: Conversation): ChainPosition | null {
  const total = run.blueprint_step_count ?? 0
  const index = run.blueprint_step_index
  if (total <= 1 || index == null || index < 0) return null
  return { step: index + 1, total, isFinal: index >= total - 1 }
}

// CompletionKind splits the one stored success terminal into what it actually
// meant. `completed` is where three different endings land, and collapsing them
// to one word is how a mid-chain step came to read as the whole task finishing:
//
//   - 'handoff' — a step of a chain that isn't the last one, whose part is done.
//     Another step picks the work up; the task is NOT over.
//   - 'stopped' — the work stopped without finishing and the task stays open for
//     a human: the agent aborted, or a non-final step ended with no usable
//     hand-off.
//   - 'done'    — the work is over. The chain's final step, a single-prompt run,
//     or a step that deliberately ended the whole workflow early ('finish').
//
// Position, not outcome, is what separates the first from the third, because
// `continue` is what an ORDINARY completion records — the native loop stamps it
// on every step including the last (internal/agentloop), while under the SDK a
// terminal step reports `finish`.
//
// The arms below mirror decideBlueprintStep (internal/delegate/blueprint.go)
// one for one, because that function is what actually decides whether anything
// runs after this conversation — so it is written as the same switch over the
// same two inputs rather than as a position test with an outcome test inside
// it. Only two of its arms consult the position, and the difference is
// load-bearing:
//
//   - `continue` / `''` branch on it. Handing off needs a step to hand off TO,
//     so on the final step both resolve to a plain finish, and only before it
//     does `continue` advance and a missing outcome abort ("no-outcome").
//   - `abort`, and anything the vocabulary does not recognize, abort the
//     blueprint wherever they land. An unrecognized value is a name this build
//     predates or a bug, and the orchestrator refuses to close a task on one
//     at any position — reading it as 'done' on the last step would put a green
//     verdict on a blueprint that actually aborted.
//
// A position we cannot read at all (chainPosition null — an unresolved plan
// length) is the one genuine unknown. It reads as final, which is the
// conservative direction for the two arms that care and irrelevant to the two
// that don't.
export type CompletionKind = 'done' | 'handoff' | 'stopped'

export function completionKind(run: Conversation): CompletionKind | null {
  if (run.Status !== 'completed') return null
  const pos = chainPosition(run)
  const isFinal = pos ? pos.isFinal : true
  switch (run.Outcome) {
    case 'continue':
      return isFinal ? 'done' : 'handoff'
    case 'finish':
      return 'done'
    case 'abort':
      return 'stopped'
    case '':
    case undefined:
      return isFinal ? 'done' : 'stopped'
    default:
      return 'stopped'
  }
}

// completionGloss — the plain-language line for a settled conversation, in one
// place so the run dock and the telemetry rail can't tell the viewer two
// different stories about the same row. Empty for a run that hasn't completed.
export function completionGloss(run: Conversation): string {
  const kind = completionKind(run)
  if (!kind) return ''
  const pos = chainPosition(run)
  if (kind === 'stopped') {
    // Two ways to stop, and they are different news: the agent decided to,
    // or it ended on nothing the workflow could act on (no outcome recorded,
    // or one this build doesn't know). The rail prints the raw token beside
    // this, so the second line doesn't repeat it.
    return run.Outcome === 'abort'
      ? 'stopped without finishing — the task stays open for a human'
      : 'ended without a usable outcome — the workflow stopped here for a human'
  }
  if (kind === 'handoff') {
    return `step ${pos!.step} of ${pos!.total} done — handed off to the next step`
  }
  // Only an explicit `finish` reaches this from mid-chain, so the sentence is
  // safe to say: the agent chose to end the workflow. Every other non-final
  // ending is 'stopped' above and must never be described as a deliberate one.
  if (pos && !pos.isFinal) return 'ended the workflow early — the later steps were skipped'
  if (pos) return `work complete — the last of ${pos.total} steps`
  return 'work complete'
}

// stopReasonLabel glosses the stored stop_reason for display. The stored
// values are claim-layer machine vocabulary — `user_cancelled` is what the
// claim releases as, and it stays that way — but printing them raw tells a
// viewer their run was cancelled, when a stop cancels nothing: the
// conversation parks, keeps its workspace, and can be picked back up. An
// unrecognized code prints as stored rather than being hidden.
export function stopReasonLabel(reason: string): string {
  switch (reason) {
    case 'user_cancelled':
      return 'stopped by user'
    case 'system_cancelled':
      return 'stopped by system'
    default:
      return reason
  }
}

// isResumableRun mirrors the backend resumableState gate: a run with no live
// turn can still take a follow-up message (waking a durable resume) when it
// parked `open`, or aborted (completed + outcome='abort'). A finish run
// (completed + outcome='finish') is excluded. The composer keys off this so
// the same 409-refresh path covers every resumable state.
export function isResumableRun(run: Conversation): boolean {
  return run.Status === 'open' || (run.Status === 'completed' && run.Outcome === 'abort')
}

// workStartedAt — when the run actually began executing: the dispatcher's
// claim stamp, falling back to the mint stamp for legacy rows that predate the
// queue columns. Live elapsed readouts tick from here so queue dwell never
// inflates working time.
export function workStartedAt(run: Conversation): string {
  return run.ClaimedAt ?? run.StartedAt
}

// Settled queue dwell below this stays off the run surfaces (card footer,
// telemetry rail): a couple of seconds is normal dispatch latency (the claim
// scan tick), not a wait worth a readout. One constant so the two surfaces
// can't drift; a live QUEUED run always shows its wait regardless.
export const QUEUE_DWELL_VISIBLE_MS = 5000

// queueDwellMs — how long the run waited in the queue: live (now − QueuedAt)
// while it is still queued, else the latest episode's settled dwell
// (ClaimedAt − QueuedAt). null when the row predates the queue columns and the
// dwell is unknowable.
export function queueDwellMs(run: Conversation, now: number = Date.now()): number | null {
  const queuedAt = run.QueuedAt ?? (run.Status === 'queued' ? run.StartedAt : null)
  if (!queuedAt) return null
  if (run.Status === 'queued') return Math.max(0, now - new Date(queuedAt).getTime())
  if (!run.ClaimedAt) return null
  return Math.max(0, new Date(run.ClaimedAt).getTime() - new Date(queuedAt).getTime())
}

export function formatDurationMs(ms: number): string {
  const seconds = Math.floor(ms / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  if (minutes < 60) return `${minutes}m ${secs}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

export function formatElapsed(dateStr: string, now: number = Date.now()): string {
  const diff = now - new Date(dateStr).getTime()
  const seconds = Math.floor(diff / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const secs = seconds % 60
  if (minutes < 60) return `${minutes}m ${secs}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}
