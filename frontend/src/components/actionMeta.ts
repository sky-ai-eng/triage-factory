import {
  CircleDot,
  GitBranch,
  GitPullRequest,
  MessageSquare,
  Eye,
  type LucideIcon,
} from 'lucide-react'
import type { Tone } from './board/cardStyle'

// Presentation for the external-action audit log's Actions lens (TFAC-483) —
// the sibling of artifactMeta.ts for the action discriminator. A data/helpers
// module (no components) so importing it into ActionRow keeps React Fast Refresh
// happy. Mirrors the backend domain.Action* consts; an unmodeled server value
// falls through to FALLBACK_ACTION_META at the render layer (forward-compat).

export interface ActionMeta {
  icon: LucideIcon
  // label is the humanized verb shown on the row ("PR opened", "Issue
  // transitioned").
  label: string
  // text is the icon's tint class (matches artifactMeta's per-kind tint).
  text: string
  // tone colors the verb pill via the card tone vocabulary.
  tone: Tone
}

// ACTION_META is keyed by the backend action discriminator. The icon follows the
// object family (PR / review / comment / branch / issue); the tone reads the
// outcome — created/advanced = good, retired = problem, in-place change = neutral.
export const ACTION_META: Record<string, ActionMeta> = {
  pr_created: { icon: GitPullRequest, label: 'PR opened', text: 'text-delegate', tone: 'good' },
  pr_marked_ready: {
    icon: GitPullRequest,
    label: 'PR marked ready',
    text: 'text-delegate',
    tone: 'good',
  },
  pr_converted_to_draft: {
    icon: GitPullRequest,
    label: 'PR → draft',
    text: 'text-delegate',
    tone: 'attention',
  },
  pr_edited: { icon: GitPullRequest, label: 'PR edited', text: 'text-delegate', tone: 'neutral' },
  pr_closed: { icon: GitPullRequest, label: 'PR closed', text: 'text-delegate', tone: 'problem' },
  review_started: { icon: Eye, label: 'Review started', text: 'text-snooze', tone: 'neutral' },
  review_submitted: { icon: Eye, label: 'Review submitted', text: 'text-snooze', tone: 'good' },
  review_dismissed: { icon: Eye, label: 'Review dismissed', text: 'text-snooze', tone: 'problem' },
  review_comment_edited: {
    icon: Eye,
    label: 'Review comment edited',
    text: 'text-snooze',
    tone: 'neutral',
  },
  review_comment_deleted: {
    icon: Eye,
    label: 'Review comment deleted',
    text: 'text-snooze',
    tone: 'problem',
  },
  comment_posted: {
    icon: MessageSquare,
    label: 'Comment posted',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  comment_edited: {
    icon: MessageSquare,
    label: 'Comment edited',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  comment_deleted: {
    icon: MessageSquare,
    label: 'Comment deleted',
    text: 'text-text-tertiary',
    tone: 'problem',
  },
  branch_pushed: {
    icon: GitBranch,
    label: 'Branch pushed',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  issue_created: { icon: CircleDot, label: 'Issue created', text: 'text-accent', tone: 'good' },
  issue_transitioned: {
    icon: CircleDot,
    label: 'Issue transitioned',
    text: 'text-accent',
    tone: 'neutral',
  },
  issue_assigned: {
    icon: CircleDot,
    label: 'Issue assigned',
    text: 'text-accent',
    tone: 'neutral',
  },
  issue_updated: { icon: CircleDot, label: 'Issue updated', text: 'text-accent', tone: 'neutral' },
  issue_comment_posted: {
    icon: CircleDot,
    label: 'Issue comment posted',
    text: 'text-accent',
    tone: 'good',
  },
}

export const FALLBACK_ACTION_META: ActionMeta = {
  icon: CircleDot,
  label: 'action',
  text: 'text-text-tertiary',
  tone: 'neutral',
}

// metaForAction resolves a (possibly unmodeled) action string to its
// presentation, falling back for a server value outside the modeled set.
export function metaForAction(action: string): ActionMeta {
  return ACTION_META[action] ?? FALLBACK_ACTION_META
}

// ACTION_OPTIONS backs the Actions-lens action filter — the modeled action set,
// grouped by family for a readable dropdown. Derived from ACTION_META so a newly
// modeled action shows up in the filter the moment it's added.
export const ACTION_OPTIONS = Object.keys(ACTION_META).map((value) => ({
  value,
  label: ACTION_META[value].label,
}))

// ACTION_PROVIDERS backs the Actions-lens provider filter. Unlike the artifacts
// feed (which also carries 'git'/'linear'), every external ACTION is performed
// under an org github/jira credential, so only those two appear.
export const ACTION_PROVIDERS = ['github', 'jira'] as const
