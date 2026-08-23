import {
  CircleDot,
  GitBranch,
  GitPullRequest,
  MessageCircle,
  MessageSquare,
  Eye,
  type LucideIcon,
} from 'lucide-react'
import type { ArtifactKind } from '../types'

// Shared artifact-kind presentation, factored out of ArtifactList.tsx so both
// the conversation-scoped list and the bot-activity audit feed (TFAC-483) render rows the
// same way — and the feed's kind filter reads the same labels. This is a
// data/helpers module (no components), so importing it into a component file
// keeps React Fast Refresh happy (the columnFilter.ts split for the same reason).

export interface KindMeta {
  icon: LucideIcon
  label: string
  text: string
}

// KIND_META is exhaustive over ArtifactKind. A server value outside the modeled
// union falls through to FALLBACK_META at the render layer (forward-compat).
export const KIND_META: Record<ArtifactKind, KindMeta> = {
  branch: { icon: GitBranch, label: 'branch', text: 'text-ink-3' },
  pull_request: { icon: GitPullRequest, label: 'pull request', text: 'text-delegate' },
  review: { icon: Eye, label: 'review', text: 'text-snooze' },
  issue: { icon: CircleDot, label: 'issue', text: 'text-warm' },
  comment: { icon: MessageSquare, label: 'comment', text: 'text-ink-3' },
  message: { icon: MessageCircle, label: 'message', text: 'text-ink-3' },
}

export const FALLBACK_META: KindMeta = {
  icon: CircleDot,
  label: 'artifact',
  text: 'text-ink-3',
}

// ARTIFACT_KINDS is the canonical kind order for the feed's kind filter — the
// modeled union, newest-product-surface-first. Derived from KIND_META so a new
// kind shows up in the filter the moment it's modeled.
export const ARTIFACT_KINDS = Object.keys(KIND_META) as ArtifactKind[]

// metaForKind resolves a (possibly unmodeled) kind string to its presentation,
// falling back for a server value outside the union.
export function metaForKind(kind: string): KindMeta {
  return KIND_META[kind as ArtifactKind] ?? FALLBACK_META
}

// ARTIFACT_PROVIDERS / PROVIDER_LABEL back the feed's provider filter — the
// external systems the bot acts in (internal/domain ArtifactProvider*). 'git' is
// the push-layer provider branches carry; the rest are the API providers.
export const ARTIFACT_PROVIDERS = ['github', 'jira', 'slack', 'git', 'linear'] as const

// PROVIDER_LABEL spans both feeds' provider vocabularies, so it carries
// 'network' — the Actions lens's sandbox-egress provider — even though no
// artifact is ever filed under it.
export const PROVIDER_LABEL: Record<string, string> = {
  github: 'GitHub',
  jira: 'Jira',
  slack: 'Slack',
  git: 'Git',
  linear: 'Linear',
  network: 'Network',
}

// ARTIFACT_STATES is the distinct lifecycle-state set across every kind
// (internal/domain ArtifactState*), backing the feed's state filter. Some values
// alias across kinds ('pending', 'deleted'), so the set is deduped — the filter
// matches the raw state regardless of kind.
export const ARTIFACT_STATES = [
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
