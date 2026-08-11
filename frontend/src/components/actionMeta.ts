import {
  Archive,
  ArchiveRestore,
  CircleDot,
  FolderGit2,
  GitBranch,
  GitFork,
  GitPullRequest,
  Lock,
  Package,
  LockOpen,
  MessageCircle,
  MessageSquare,
  Pin,
  PinOff,
  Play,
  ShieldBan,
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
  review_submitted: { icon: Eye, label: 'Review submitted', text: 'text-snooze', tone: 'good' },
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
  // Changing which labels the repository offers, as opposed to putting one on
  // an issue. The labels say "definition" out loud because the pair above reads
  // almost identically at a glance, and a deleted definition takes the label off
  // every issue that had it — the more consequential act of the two, hence
  // 'problem' where label_removed is merely neutral.
  label_defined: { icon: Tag, label: 'Label defined', text: 'text-text-tertiary', tone: 'good' },
  label_definition_edited: {
    icon: Tag,
    label: 'Label definition edited',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  label_definition_deleted: {
    icon: Tag,
    label: 'Label definition deleted',
    text: 'text-text-tertiary',
    tone: 'problem',
  },
  // Repository configuration. One tint for the family, and the tone carries how
  // much a reader should care: archiving makes a repository read-only and
  // deleting it is unrecoverable, while a fork or a settings edit changes
  // nothing anyone was relying on.
  repo_created: {
    icon: FolderGit2,
    label: 'Repo created',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  repo_edited: {
    icon: FolderGit2,
    label: 'Repo edited',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  repo_deleted: {
    icon: FolderGit2,
    label: 'Repo deleted',
    text: 'text-text-tertiary',
    tone: 'problem',
  },
  repo_forked: { icon: GitFork, label: 'Repo forked', text: 'text-text-tertiary', tone: 'neutral' },
  repo_archived: {
    icon: Archive,
    label: 'Repo archived',
    text: 'text-text-tertiary',
    tone: 'attention',
  },
  repo_unarchived: {
    icon: ArchiveRestore,
    label: 'Repo unarchived',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  release_created: {
    icon: Package,
    label: 'Release created',
    text: 'text-text-tertiary',
    tone: 'good',
  },
  release_edited: {
    icon: Package,
    label: 'Release edited',
    text: 'text-text-tertiary',
    tone: 'neutral',
  },
  release_deleted: {
    icon: Package,
    label: 'Release deleted',
    text: 'text-text-tertiary',
    tone: 'problem',
  },
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
  // The push the upstream turned down — nothing landed, so no branch artifact
  // exists and this row is the only trace of the attempt. 'problem' rather than
  // the denials' 'attention' below: those are Triage Factory declining to do
  // something, this is work the agent believed it had done and had not.
  branch_push_failed: {
    icon: GitBranch,
    label: 'Branch push failed',
    text: 'text-text-tertiary',
    tone: 'problem',
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
  // A write Triage Factory refused before it reached GitHub. Deliberately not
  // in the neutral tint the two fallbacks above sit in: those say a write
  // happened and could not be named, this says one was stopped, and someone
  // scanning a run's actions should be able to see the difference without
  // reading labels.
  gh_write_denied: {
    icon: ShieldBan,
    label: 'gh write refused',
    text: 'text-text-tertiary',
    tone: 'attention',
  },
  // The other two per-run gates, sharing gh_write_denied's shield and tone
  // because a reader scanning for "what did we stop" wants all three to answer
  // to the same shape. They keep separate labels because the boundary that
  // refused each one is different, and so is what to do about it: a git denial
  // is a repo/ref outside the run's grant, an egress denial is a host outside
  // its allowlist.
  git_denied: {
    icon: ShieldBan,
    label: 'Git op refused',
    text: 'text-text-tertiary',
    tone: 'attention',
  },
  egress_denied: {
    icon: ShieldBan,
    label: 'Network refused',
    text: 'text-text-tertiary',
    tone: 'attention',
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

// DetailSource is the subset of an action row actionDetailSummary reads. Typed
// structurally rather than as ActivityAction so the helper stays in this
// data module and the test can call it with a literal.
export interface DetailSource {
  target?: string
  external_id?: string
  from_state?: string
  to_state?: string
  details?: unknown
}

// actionDetailSummary renders a row's detail_json as one compact line, or null
// when it carries nothing the row doesn't already say.
//
// It is deliberately generic. For the two fallback actions the detail IS the
// row's identifying content — a `gh_channel_write` says only "a write happened"
// until its method and path are on screen — and the rest of the epic keeps
// adding keys (`attempted`, `unreadable`, `refused`). A per-action list of
// keys to display would leave each new one invisible until someone remembered
// this file, which is the exact failure being fixed here. So every key renders,
// and a shape nobody anticipated still says something.
//
// Two rules shape the output. Method and path pair into `POST /repos/…`,
// because that is how an HTTP request is written and `method POST · path /…`
// is not. And a value the row already displays elsewhere is dropped: a Jira
// transition repeats its destination status in detail_json, and an egress
// denial repeats the host that is already the row's target.
export function actionDetailSummary(action: DetailSource): string | null {
  const details = action.details
  if (typeof details !== 'object' || details === null || Array.isArray(details)) return null
  const entries = Object.entries(details as Record<string, unknown>)
  if (entries.length === 0) return null

  const shown = new Set(
    [action.target, action.external_id, action.from_state, action.to_state].filter(
      (v): v is string => !!v,
    ),
  )
  const chunks: string[] = []
  const { method, path } = details as Record<string, unknown>
  if (typeof method === 'string' && method !== '' && typeof path === 'string' && path !== '') {
    chunks.push(`${method} ${path}`)
  }
  for (const [key, value] of entries) {
    if (chunks.length > 0 && (key === 'method' || key === 'path')) continue
    // `false` is the absence of a flag, not a fact worth a chunk; `true` needs
    // no value beyond its own name.
    if (value === false || value === null || value === undefined || value === '') continue
    if (value === true) {
      chunks.push(key)
      continue
    }
    const text = Array.isArray(value) ? value.join(', ') : String(value)
    if (text === '' || shown.has(text)) continue
    chunks.push(`${key} ${text}`)
  }
  return chunks.length > 0 ? chunks.join(' · ') : null
}

// ACTION_OPTIONS backs the Actions-lens action filter — the modeled action set,
// grouped by family for a readable dropdown. Derived from ACTION_META so a newly
// modeled action shows up in the filter the moment it's added.
export const ACTION_OPTIONS = Object.keys(ACTION_META).map((value) => ({
  value,
  label: ACTION_META[value].label,
}))

// ACTION_PROVIDERS backs the Actions-lens provider filter. Three of the four are
// the credentials an action can be performed under — an org github/jira
// credential or the workspace Slack bot token — and 'network' is the odd one:
// the sandbox egress denial spends no credential at all, because nothing left
// the box. It is in the filter for exactly that reason. A refused connection is
// the row an operator most wants to isolate, and it is the only one no other
// provider value can reach. ('git'/'linear', which the artifacts feed carries,
// stay out: a branch push is a github action even though its artifact is a git
// one, and nothing writes to Linear.)
export const ACTION_PROVIDERS = ['github', 'jira', 'slack', 'network'] as const
