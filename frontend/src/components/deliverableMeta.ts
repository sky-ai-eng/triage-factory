import {
  CircleDot,
  GitBranch,
  GitPullRequest,
  MessageCircle,
  MessageSquare,
  Eye,
  type LucideIcon,
} from 'lucide-react'
import type { DeliverableKind } from '../types'

// Shared deliverable-kind presentation, factored out of DeliverableList.tsx so both
// the conversation-scoped list and the bot-activity audit feed render rows the
// same way — and the feed's kind filter reads the same labels. This is a
// data/helpers module (no components), so importing it into a component file
// keeps React Fast Refresh happy (the board's laneFilter.ts split for the same reason).

export interface KindMeta {
  icon: LucideIcon
  label: string
  text: string
}

// KIND_META is exhaustive over DeliverableKind. A server value outside the modeled
// union falls through to FALLBACK_META at the render layer (forward-compat).
export const KIND_META: Record<DeliverableKind, KindMeta> = {
  branch: { icon: GitBranch, label: 'branch', text: 'text-ink-3' },
  pull_request: { icon: GitPullRequest, label: 'pull request', text: 'text-delegate' },
  review: { icon: Eye, label: 'review', text: 'text-snooze' },
  issue: { icon: CircleDot, label: 'issue', text: 'text-warm' },
  comment: { icon: MessageSquare, label: 'comment', text: 'text-ink-3' },
  message: { icon: MessageCircle, label: 'message', text: 'text-ink-3' },
}

export const FALLBACK_META: KindMeta = {
  icon: CircleDot,
  label: 'deliverable',
  text: 'text-ink-3',
}

// DELIVERABLE_KINDS is the canonical kind order for the feed's kind filter — the
// modeled union, newest-product-surface-first. Derived from KIND_META so a new
// kind shows up in the filter the moment it's modeled.
export const DELIVERABLE_KINDS = Object.keys(KIND_META) as DeliverableKind[]

// metaForKind resolves a (possibly unmodeled) kind string to its presentation,
// falling back for a server value outside the union.
export function metaForKind(kind: string): KindMeta {
  return KIND_META[kind as DeliverableKind] ?? FALLBACK_META
}

// DELIVERABLE_PROVIDERS / PROVIDER_LABEL back the feed's provider filter — the
// external systems the bot acts in (internal/domain Provider*). 'git' is
// the push-layer provider branches carry; the rest are the API providers.
export const DELIVERABLE_PROVIDERS = ['github', 'jira', 'slack', 'git', 'linear'] as const

// PROVIDER_LABEL spans both feeds' provider vocabularies, so it carries
// 'network' — the Actions lens's sandbox-egress provider — even though no
// deliverable is ever filed under it.
export const PROVIDER_LABEL: Record<string, string> = {
  github: 'GitHub',
  jira: 'Jira',
  slack: 'Slack',
  git: 'Git',
  linear: 'Linear',
  network: 'Network',
}

// DELIVERABLE_STATES is the distinct lifecycle-state set across every kind
// (internal/domain DeliverableState*), backing the feed's state filter. Some values
// alias across kinds ('pending', 'deleted'), so the set is deduped — the filter
// matches the raw state regardless of kind.
export const DELIVERABLE_STATES = [
  'pushed',
  'pending',
  'draft',
  'open',
  'merged',
  'closed',
  'submitted',
  'dismissed',
  'created',
  'updated',
  'posted',
  'deleted',
] as const
