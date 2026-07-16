import type { AgentRun } from '../types'

export const ACTIVE_STATUSES = [
  'initializing',
  'cloning',
  'fetching',
  'worktree_created',
  'agent_starting',
  'running',
] as const

export const FAILED_STATUSES = ['failed', 'cancelled', 'task_unsolvable'] as const

// Statuses where a run is no longer executing a turn, so any tool-permission
// prompt parked on it is stale and must be dropped — a finished run's Allow/Deny
// would 404 or resolve a now-meaningless prompt. pending_approval counts: the
// turn ended and staged a review/PR, so it isn't waiting on a tool decision.
// Shared by the board and useRunDetail so the two surfaces drop in lockstep.
export const PERMISSION_TERMINAL_STATUSES = [
  'completed',
  'failed',
  'cancelled',
  'task_unsolvable',
  'pending_approval',
] as const

export function isPermissionTerminalStatus(status: string): boolean {
  return (PERMISSION_TERMINAL_STATUSES as readonly string[]).includes(status)
}

export function isActiveRun(run: AgentRun): boolean {
  return (ACTIVE_STATUSES as readonly string[]).includes(run.Status)
}

export function isActiveStatus(status: string): boolean {
  return (ACTIVE_STATUSES as readonly string[]).includes(status)
}

export function isFailedStatus(status: string): boolean {
  return (FAILED_STATUSES as readonly string[]).includes(status)
}

// isResumableRun mirrors the backend resumableState gate: a run with no live
// turn can still take a follow-up message (waking a durable resume) when it
// parked `open`, parked `pending_approval` (a queued review/PR — conversation is
// independent of approval), or aborted (completed + outcome='abort'). A finish
// run (completed + outcome='finish') is excluded. The composer keys off this so
// the same 409-refresh path covers every resumable state.
export function isResumableRun(run: AgentRun): boolean {
  return (
    run.Status === 'open' ||
    run.Status === 'pending_approval' ||
    (run.Status === 'completed' && run.Outcome === 'abort')
  )
}

// workStartedAt — when the run actually began executing: the dispatcher's
// claim stamp, falling back to the mint stamp for legacy rows that predate the
// queue columns. Live elapsed readouts tick from here so queue dwell never
// inflates working time.
export function workStartedAt(run: AgentRun): string {
  return run.ClaimedAt ?? run.StartedAt
}

// queueDwellMs — how long the run waited in the queue: live (now − QueuedAt)
// while it is still queued, else the latest episode's settled dwell
// (ClaimedAt − QueuedAt). null when the row predates the queue columns and the
// dwell is unknowable.
export function queueDwellMs(run: AgentRun, now: number = Date.now()): number | null {
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
