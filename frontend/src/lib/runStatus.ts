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

export function statusLabel(status: string): string {
  const map: Record<string, string> = {
    initializing: 'Initializing',
    cloning: 'Pulling repo',
    fetching: 'Fetching PR details',
    worktree_created: 'Creating worktree',
    agent_starting: 'Starting Claude Code',
    running: 'Running',
    open: 'Open',
    completed: 'Completed',
    pending_approval: 'Pending Approval',
    cancelled: 'Cancelled',
    failed: 'Failed',
    task_unsolvable: 'Unsolvable',
    taken_over: 'Taken over',
  }
  return map[status] || status
}

export interface StatusDisplay {
  color: string
  icon: string
  label: string
}

export function statusDisplay(run: AgentRun): StatusDisplay {
  const isFailed = run.Status === 'failed'
  const isCancelled = run.Status === 'cancelled'
  const isUnsolvable = run.Status === 'task_unsolvable'
  const isPendingApproval = run.Status === 'pending_approval'
  const isOpen = run.Status === 'open'
  const active = isActiveRun(run)

  const color =
    isFailed || isCancelled
      ? 'text-dismiss'
      : isUnsolvable || isPendingApproval || isOpen
        ? 'text-snooze'
        : active
          ? 'text-delegate'
          : 'text-claim'

  const icon = isFailed
    ? '✗'
    : isCancelled
      ? '◼'
      : isUnsolvable
        ? '⊘'
        : isPendingApproval
          ? '◉'
          : isOpen
            ? '◌'
            : active
              ? '●'
              : '✓'

  return { color, icon, label: statusLabel(run.Status) }
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
