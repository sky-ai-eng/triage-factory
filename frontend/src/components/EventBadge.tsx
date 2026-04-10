import * as Tooltip from '@radix-ui/react-tooltip'

const EVENT_DISPLAY: Record<string, { label: string; description: string; color: string }> = {
  'github:pr:review_requested': { label: 'Review Requested', description: 'Someone requested your review on this pull request', color: 'bg-amber-500/10 text-amber-700' },
  'github:pr:review_received':  { label: 'Review Received',  description: 'Your pull request received a new review', color: 'bg-blue-500/10 text-blue-600' },
  'github:pr:approved':         { label: 'Approved',         description: 'Your pull request was approved by a reviewer', color: 'bg-emerald-500/10 text-emerald-700' },
  'github:pr:changes_requested':{ label: 'Changes Requested',description: 'A reviewer requested changes on your pull request', color: 'bg-orange-500/10 text-orange-700' },
  'github:pr:ci_passed':        { label: 'CI Passed',        description: 'All CI checks passed on this pull request', color: 'bg-emerald-500/10 text-emerald-700' },
  'github:pr:ci_failed':        { label: 'CI Failed',        description: 'One or more CI checks failed on this pull request', color: 'bg-red-500/10 text-red-600' },
  'github:pr:merged':           { label: 'Merged',           description: 'This pull request was merged', color: 'bg-purple-500/10 text-purple-600' },
  'github:pr:mentioned':        { label: 'Mentioned',        description: 'You were @mentioned in this pull request', color: 'bg-indigo-500/10 text-indigo-600' },
  'github:pr:conflicts':        { label: 'Conflicts',        description: 'This pull request has merge conflicts that need resolving', color: 'bg-red-500/10 text-red-600' },
  'github:pr:opened':           { label: 'Authored PR',       description: 'Your open pull request is being tracked', color: 'bg-sky-500/10 text-sky-600' },
  'github:pr:closed':           { label: 'Closed',           description: 'A pull request was closed without merging', color: 'bg-slate-500/10 text-slate-600' },
  'github:pr:ready_for_review': { label: 'Ready for Review', description: 'A draft pull request was marked ready for review', color: 'bg-blue-500/10 text-blue-600' },
  'jira:issue:available':       { label: 'Available',        description: 'Unassigned issue available for pickup in your project', color: 'bg-slate-500/10 text-slate-600' },
  'jira:issue:assigned':        { label: 'Assigned',         description: 'This issue was assigned to you', color: 'bg-blue-500/10 text-blue-600' },
  'jira:issue:status_changed':  { label: 'Status Changed',   description: 'The workflow status of this issue changed', color: 'bg-violet-500/10 text-violet-600' },
  'jira:issue:completed':       { label: 'Completed',        description: 'This issue was marked as done', color: 'bg-emerald-500/10 text-emerald-700' },
  'jira:issue:priority_changed':{ label: 'Priority Changed', description: 'The priority level of this issue was changed', color: 'bg-amber-500/10 text-amber-700' },
}

const FALLBACK = { label: 'Event', description: 'A triage event occurred', color: 'bg-black/5 text-text-tertiary' }

export default function EventBadge({ eventType, compact }: { eventType?: string; compact?: boolean }) {
  if (!eventType) return null
  const info = EVENT_DISPLAY[eventType] || FALLBACK

  const badge = compact ? (
    <span className={`text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded cursor-default ${info.color}`}>
      {info.label}
    </span>
  ) : (
    <span className={`text-[11px] font-semibold px-2.5 py-1 rounded-full cursor-default ${info.color}`}>
      {info.label}
    </span>
  )

  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          {badge}
        </Tooltip.Trigger>
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
