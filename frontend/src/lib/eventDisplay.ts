// eventDisplay — the canonical event-type → display lookup, extracted from
// EventBadge so non-badge surfaces (the board cards' detuned event tags) can
// read the same labels/descriptions without importing a component. EventBadge
// keeps rendering its colorful pills for the filter chips (where the saturated
// colors are load-bearing: selected = full color, unselected = grayscale);
// the board cards call eventTone() to collapse those colors down to our four
// warm semantic tones so the event reads as a quiet uppercase label, not a
// 2018 pastel pill.
//
// This is a user-facing subset/override of AllEventTypes() in
// internal/domain/event.go — labels may differ and backend-only types fall
// back to the default badge.

export interface EventInfo {
  label: string
  description: string
  // Tailwind color classes used by EventBadge's pills. The board cards don't
  // render this directly; eventTone() reads the family out of it.
  color: string
}

const EVENT_DISPLAY: Record<string, EventInfo> = {
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

  // --- GitHub PR: review request ---
  'github:pr:review_requested': {
    label: 'Review Requested',
    description: 'Someone requested your review on a PR',
    color: 'bg-amber-500/10 text-amber-700',
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
  'jira:issue:became_atomic': {
    label: 'Now Actionable',
    description: 'All subtasks closed — parent ticket is now an atomic work unit',
    color: 'bg-blue-500/10 text-blue-600',
  },
}

export const FALLBACK_EVENT: EventInfo = {
  label: 'Event',
  description: 'A triage event occurred',
  color: 'bg-black/5 text-ink-3',
}

export function eventDisplay(eventType?: string): EventInfo {
  if (!eventType) return FALLBACK_EVENT
  return EVENT_DISPLAY[eventType] || FALLBACK_EVENT
}

// EventTone collapses a badge's saturated tailwind family into our four warm
// semantic tones, so the board's event tags read as quiet colored labels in
// the rust/amber/green/red world instead of pastel blues and violets. We
// derive the tone from the color family already on the badge (rather than
// re-enumerating every event) so new event types inherit a sane tone for free.
export type EventTone = 'problem' | 'good' | 'attention' | 'neutral'

export function eventTone(eventType?: string): EventTone {
  const c = eventDisplay(eventType).color
  if (/red|orange/.test(c)) return 'problem'
  if (/emerald|green/.test(c)) return 'good'
  if (/amber/.test(c)) return 'attention'
  // blue / sky / indigo / violet / purple / slate — informational. In the warm
  // field these all read as neutral; we don't have a cool accent token to spend
  // on them, and tinting them amber/green would overstate their urgency.
  return 'neutral'
}
