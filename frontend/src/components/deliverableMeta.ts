import {
  CircleDot,
  GitBranch,
  GitPullRequest,
  MessageCircle,
  MessageSquare,
  Eye,
  type LucideIcon,
} from 'lucide-react'
import type { DeliverableKind, ObjectKind } from '../types'

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

// OBJECT_META is the presentation of every object kind an artifact can name.
// A kind a deliverable can also be reads the same as that deliverable; the
// rest share the generic glyph and differ only by label.
const OBJECT_META: Record<ObjectKind, KindMeta> = {
  ...KIND_META,
  repository: { ...FALLBACK_META, label: 'repository' },
  label: { ...FALLBACK_META, label: 'label' },
  release: { ...FALLBACK_META, label: 'release' },
  workflow: { ...FALLBACK_META, label: 'workflow' },
  workflow_run: { ...FALLBACK_META, label: 'workflow run' },
}

const FALLBACK_OBJECT_META: KindMeta = { ...FALLBACK_META, label: 'object' }

// metaForObjectKind resolves an artifact's object kind to its presentation,
// falling back for a server value outside the union.
export function metaForObjectKind(kind: string): KindMeta {
  return OBJECT_META[kind as ObjectKind] ?? FALLBACK_OBJECT_META
}

// overlayKindFor names the approval overlay a deliverable kind opens in, or
// null for a kind that has none.
export function overlayKindFor(kind: string): 'pr' | 'review' | null {
  if (kind === 'pull_request') return 'pr'
  if (kind === 'review') return 'review'
  return null
}

// awaitsDecision reports whether a deliverable is in the one state a person
// acts on: a draft pull request, or a pending review. Any other state is
// resolved and only links out.
export function awaitsDecision(kind: string, state: string): boolean {
  if (kind === 'pull_request') return state === 'draft'
  if (kind === 'review') return state === 'pending'
  return false
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

// rowTone is a row's border and fill: the warm highlight for work a person acts
// on, else the quiet chrome every other row shares.
export function rowTone(highlighted?: boolean): string {
  return highlighted
    ? 'border-warm/40 bg-warm/[0.06] hover:bg-warm/[0.1]'
    : 'border-line-1 bg-tint-1 hover:bg-tint-3'
}
