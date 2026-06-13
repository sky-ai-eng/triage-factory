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
