import * as Tooltip from '@radix-ui/react-tooltip'

// Maps canonical per-action event types from the events_catalog to display info.
// Matches the AllEventTypes() seed in internal/domain/event.go.
const EVENT_DISPLAY: Record<string, { label: string; description: string; color: string }> = {
  // --- GitHub PR: per-action review events (split on review type) ---
  'github:pr:review_changes_requested': {
    label: 'Changes Requested',
    description: 'A reviewer requested changes on a PR',
    color: 'bg-orange-500/10 text-orange-700',
  },
  'github:pr:review_approved': {
    label: 'Approved',
    description: 'A reviewer approved a PR',
    color: 'bg-emerald-500/10 text-emerald-700',
  },
  'github:pr:review_commented': {
    label: 'Review Comment',
    description: 'A reviewer left non-blocking comments on a PR',
    color: 'bg-blue-500/10 text-blue-600',
  },
  'github:pr:review_dismissed': {
    label: 'Review Dismissed',
    description: 'A reviewer dismissed their previous review on a PR',
    color: 'bg-slate-500/10 text-slate-600',
  },

  // --- GitHub PR: review request / submission ---
  'github:pr:review_requested': {
    label: 'Review Requested',
    description: 'Someone requested your review on a PR',
    color: 'bg-amber-500/10 text-amber-700',
  },
  'github:pr:review_submitted': {
    label: 'Review Submitted',
    description: "You reviewed someone else's PR",
    color: 'bg-blue-500/10 text-blue-600',
  },

  // --- GitHub PR: per-check CI events (split on conclusion) ---
  'github:pr:ci_check_failed': {
    label: 'CI Failed',
    description: 'A CI check failed on a PR',
    color: 'bg-red-500/10 text-red-600',
  },
  'github:pr:ci_check_passed': {
    label: 'CI Passed',
    description: 'A CI check passed on a PR',
    color: 'bg-emerald-500/10 text-emerald-700',
  },

  // --- GitHub PR: labels ---
  'github:pr:label_added': {
    label: 'Label Added',
    description: 'A label was added to a PR',
    color: 'bg-violet-500/10 text-violet-600',
  },
  'github:pr:label_removed': {
    label: 'Label Removed',
    description: 'A label was removed from a PR',
    color: 'bg-slate-500/10 text-slate-600',
  },

  // --- GitHub PR: state events ---
  'github:pr:new_commits': {
    label: 'New Commits',
    description: 'A tracked PR has new commits since the last poll',
    color: 'bg-sky-500/10 text-sky-600',
  },
  'github:pr:conflicts': {
    label: 'Conflicts',
    description: 'A PR has merge conflicts',
    color: 'bg-red-500/10 text-red-600',
  },
  'github:pr:ready_for_review': {
    label: 'Ready for Review',
    description: 'A draft PR was marked ready for review',
    color: 'bg-blue-500/10 text-blue-600',
  },
  'github:pr:mentioned': {
    label: 'Mentioned',
    description: 'You were @mentioned in a PR',
    color: 'bg-indigo-500/10 text-indigo-600',
  },
  'github:pr:opened': {
    label: 'PR Opened',
    description: 'A pull request was opened',
    color: 'bg-sky-500/10 text-sky-600',
  },
  'github:pr:merged': {
    label: 'Merged',
    description: 'A pull request was merged',
    color: 'bg-purple-500/10 text-purple-600',
  },
  'github:pr:closed': {
    label: 'Closed',
    description: 'A pull request was closed without merging',
    color: 'bg-slate-500/10 text-slate-600',
  },

  // --- Jira ---
  'jira:issue:assigned': {
    label: 'Assigned',
    description: 'Issue was assigned to you',
    color: 'bg-blue-500/10 text-blue-600',
  },
  'jira:issue:available': {
    label: 'Available',
    description: 'New unassigned issue in pickup queue',
    color: 'bg-slate-500/10 text-slate-600',
  },
  'jira:issue:status_changed': {
    label: 'Status Changed',
    description: 'Issue status changed',
    color: 'bg-violet-500/10 text-violet-600',
  },
  'jira:issue:priority_changed': {
    label: 'Priority Changed',
    description: 'Issue priority was changed',
    color: 'bg-amber-500/10 text-amber-700',
  },
  'jira:issue:commented': {
    label: 'New Comment',
    description: 'A new comment was added to an issue',
    color: 'bg-blue-500/10 text-blue-600',
  },
  'jira:issue:completed': {
    label: 'Completed',
    description: 'Issue was marked as done',
    color: 'bg-emerald-500/10 text-emerald-700',
  },
}

const FALLBACK = {
  label: 'Event',
  description: 'A triage event occurred',
  color: 'bg-black/5 text-text-tertiary',
}

export default function EventBadge({
  eventType,
  compact,
}: {
  eventType?: string
  compact?: boolean
}) {
  if (!eventType) return null
  const info = EVENT_DISPLAY[eventType] || FALLBACK

  const badge = compact ? (
    <span
      className={`text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded cursor-default ${info.color}`}
    >
      {info.label}
    </span>
  ) : (
    <span
      className={`text-[11px] font-semibold px-2.5 py-1 rounded-full cursor-default ${info.color}`}
    >
      {info.label}
    </span>
  )

  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>{badge}</Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            side="top"
            sideOffset={6}
            className="z-[100] max-w-[240px] rounded-lg bg-surface-raised border border-border-glass px-3 py-2 shadow-lg shadow-black/[0.06] text-[12px] text-text-secondary leading-relaxed animate-in fade-in-0 zoom-in-95"
          >
            {info.description}
            <Tooltip.Arrow className="fill-surface-raised" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  )
}
