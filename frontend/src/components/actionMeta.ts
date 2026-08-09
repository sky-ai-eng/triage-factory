import {
  CircleDot,
  GitBranch,
  GitPullRequest,
  Lock,
  LockOpen,
  MessageCircle,
  MessageSquare,
  Pin,
  PinOff,
  Play,
  SmilePlus,
  Tag,
  Terminal,
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
// object family (PR / review / comment / branch / issue / message); the tone
// reads the outcome — created/advanced = good, retired = problem, in-place
// change = neutral.
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
  pr_reopened: {
    icon: GitPullRequest,
    label: 'PR reopened',
    text: 'text-delegate',
    tone: 'neutral',
  },
  pr_merged: { icon: GitPullRequest, label: 'PR merged', text: 'text-delegate', tone: 'good' },
  // Auto-merge reads as attention rather than good: nothing merged yet, and a
  // merge armed to fire on a green check is the row a reader most wants to
  // notice while it is still only armed.
  pr_auto_merge_enabled: {
    icon: GitPullRequest,
    label: 'Auto-merge enabled',
    text: 'text-delegate',
    tone: 'attention',
  },
  pr_auto_merge_disabled: {
    icon: GitPullRequest,
    label: 'Auto-merge disabled',
    text: 'text-delegate',
    tone: 'neutral',
  },
  pr_reverted: {
    icon: GitPullRequest,
    label: 'PR reverted',
    text: 'text-delegate',
    tone: 'attention',
  },
  pr_branch_updated: {
    icon: GitPullRequest,
    label: 'PR branch updated',
    text: 'text-delegate',
    tone: 'neutral',
  },
  review_started: { icon: Eye, label: 'Review started', text: 'text-snooze', tone: 'neutral' },
  review_submitted: { icon: Eye, label: 'Review submitted', text: 'text-snooze', tone: 'good' },
  review_dismissed: { icon: Eye, label: 'Review dismissed', text: 'text-snooze', tone: 'problem' },
  review_requested: { icon: Eye, label: 'Review requested', text: 'text-snooze', tone: 'neutral' },
  review_request_removed: {
    icon: Eye,
    label: 'Review request removed',
    text: 'text-snooze',
    tone: 'neutral',
  },
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
  reaction_added: {
    icon: SmilePlus,
    label: 'Reaction added',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  reaction_removed: {
    icon: SmilePlus,
    label: 'Reaction removed',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  workflow_dispatched: {
    icon: Play,
    label: 'Workflow dispatched',
    text: 'text-delegate',
    tone: 'good',
  },
  workflow_run_cancelled: {
    icon: Play,
    label: 'Workflow run cancelled',
    text: 'text-delegate',
    tone: 'problem',
  },
  label_added: { icon: Tag, label: 'Label added', text: 'text-text-tertiary', tone: 'good' },
  label_removed: { icon: Tag, label: 'Label removed', text: 'text-text-tertiary', tone: 'neutral' },
  conversation_locked: {
    icon: Lock,
    label: 'Conversation locked',
    text: 'text-text-tertiary',
    tone: 'attention',
  },
  conversation_unlocked: {
    icon: LockOpen,
    label: 'Conversation unlocked',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  branch_pushed: {
    icon: GitBranch,
    label: 'Branch pushed',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  linked_branch_created: {
    icon: GitBranch,
    label: 'Linked branch created',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  // The unclassified gh-channel write. It has no verb precisely because the
  // shared classifier didn't recognize the shape, so the label says what is
  // actually known and the details carry the method and path.
  gh_channel_write: {
    icon: Terminal,
    label: 'Raw gh write',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  // Its GraphQL sibling. Separate because the two rows know different things —
  // this one's details carry the mutation names rather than a path — and
  // because a run whose unnamed writes are all GraphQL is a different signal
  // from one whose writes are raw REST calls.
  graphql_write: {
    icon: Terminal,
    label: 'Raw gh GraphQL write',
    text: 'text-text-tertiary',
    tone: 'neutral',
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
  issue_closed: { icon: CircleDot, label: 'Issue closed', text: 'text-accent', tone: 'problem' },
  issue_reopened: {
    icon: CircleDot,
    label: 'Issue reopened',
    text: 'text-accent',
    tone: 'neutral',
  },
  issue_deleted: { icon: CircleDot, label: 'Issue deleted', text: 'text-accent', tone: 'problem' },
  issue_pinned: { icon: Pin, label: 'Issue pinned', text: 'text-accent', tone: 'good' },
  issue_unpinned: { icon: PinOff, label: 'Issue unpinned', text: 'text-accent', tone: 'neutral' },
  issue_transferred: {
    icon: CircleDot,
    label: 'Issue transferred',
    text: 'text-accent',
    tone: 'attention',
  },
  issue_comment_posted: {
    icon: CircleDot,
    label: 'Issue comment posted',
    text: 'text-accent',
    tone: 'good',
  },
  slack_message_posted: {
    icon: MessageCircle,
    label: 'Message posted',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  slack_message_edited: {
    icon: MessageCircle,
    label: 'Message edited',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  slack_reaction_added: {
    icon: SmilePlus,
    label: 'Reaction added',
    text: 'text-text-tertiary',
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
// under an org github/jira credential or the workspace Slack bot token, so
// only those three appear.
export const ACTION_PROVIDERS = ['github', 'jira', 'slack'] as const
