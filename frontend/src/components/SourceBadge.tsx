import type { Task } from '../types'

/**
 * Displays a source badge ("PR", "GH", "Jira", "Slack") with entity_kind-aware
 * text and consistent styling. Use size="lg" for the Cards swipe card, default
 * "sm" for TaskCard / Board sidebar / AgentCard.
 */
export default function SourceBadge({ task, size = 'sm' }: { task: Task; size?: 'sm' | 'lg' }) {
  const isGitHub = task.source === 'github'
  const isSlack = task.source === 'slack'
  const label = isGitHub ? (task.entity_kind === 'pr' ? 'PR' : 'GH') : isSlack ? 'Slack' : 'Jira'
  const labelLg = isGitHub
    ? task.entity_kind === 'pr'
      ? 'Pull Request'
      : 'GitHub'
    : isSlack
      ? 'Slack'
      : 'Jira'

  // Slack aubergine (#4A154B) at the same tint treatment as the Jira pill.
  const colorCls = isGitHub
    ? 'bg-black/[0.04] text-text-secondary'
    : isSlack
      ? 'bg-[#4A154B]/10 text-[#4A154B]'
      : 'bg-blue-500/10 text-blue-600'

  if (size === 'lg') {
    return (
      <span
        className={`text-[11px] font-semibold uppercase tracking-wider px-2.5 py-1 rounded-full ${colorCls}`}
      >
        {labelLg}
      </span>
    )
  }

  return (
    <span
      className={`text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${colorCls}`}
    >
      {label}
    </span>
  )
}
