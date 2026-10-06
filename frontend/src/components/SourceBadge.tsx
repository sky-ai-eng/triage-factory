import type { Task } from '../types'

/**
 * Displays a source badge ("PR", "GH", "Jira", "Linear", "Slack") with
 * entity_kind-aware text and consistent styling. Use size="lg" for the Cards
 * swipe card, default "sm" elsewhere.
 */
export default function SourceBadge({ task, size = 'sm' }: { task: Task; size?: 'sm' | 'lg' }) {
  const isGitHub = task.source === 'github'
  const isSlack = task.source === 'slack'
  const isLinear = task.source === 'linear'
  const label = isGitHub
    ? task.entity_kind === 'pr'
      ? 'PR'
      : 'GH'
    : isSlack
      ? 'Slack'
      : isLinear
        ? 'Linear'
        : 'Jira'
  const labelLg = isGitHub
    ? task.entity_kind === 'pr'
      ? 'Pull Request'
      : 'GitHub'
    : isSlack
      ? 'Slack'
      : isLinear
        ? 'Linear'
        : 'Jira'

  // Slack aubergine (#4A154B) and Linear indigo (#5E6AD2) at the same tint
  // treatment as the Jira pill.
  const colorCls = isGitHub
    ? 'bg-tint-3 text-ink-2'
    : isSlack
      ? 'bg-[#4A154B]/10 text-[#4A154B]'
      : isLinear
        ? 'bg-[#5E6AD2]/10 text-[#5E6AD2]'
        : 'bg-ink-3/10 text-ink-3'

  if (size === 'lg') {
    return (
      <span
        className={`text-reported font-semibold uppercase tracking-wider px-2.5 py-1 rounded-full ${colorCls}`}
      >
        {labelLg}
      </span>
    )
  }

  return (
    <span
      className={`text-label font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${colorCls}`}
    >
      {label}
    </span>
  )
}
