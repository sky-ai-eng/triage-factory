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
