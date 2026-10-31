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

// Shared deliverable-kind presentation, factored out of DeliverableList.tsx so
// every surface that renders a deliverable row renders it the same way. This is
// a data/helpers module (no components), so importing it into a component file
// keeps React Fast Refresh happy (the board's laneFilter.ts split for the same reason).

export interface KindMeta {
  icon: LucideIcon
  label: string
  text: string
}

// KIND_META is exhaustive over DeliverableKind. A server value outside the modeled
// union falls through to FALLBACK_META at the render layer (forward-compat).
export const KIND_META: Record<DeliverableKind, KindMeta> = {
  pull_request: { icon: GitPullRequest, label: 'pull request', text: 'text-delegate' },
  review: { icon: Eye, label: 'review', text: 'text-snooze' },
}

export const FALLBACK_META: KindMeta = {
  icon: CircleDot,
  label: 'deliverable',
  text: 'text-ink-3',
}

// metaForKind resolves a (possibly unmodeled) kind string to its presentation,
// falling back for a server value outside the union.
export function metaForKind(kind: string): KindMeta {
  return KIND_META[kind as DeliverableKind] ?? FALLBACK_META
}

// OBJECT_META is the presentation of every object kind an artifact can name.
// A kind a deliverable can also be reads the same as that deliverable; the
// rest that have a glyph of their own carry it, and the others share the
// generic glyph and differ only by label.
const OBJECT_META: Record<ObjectKind, KindMeta> = {
  ...KIND_META,
  branch: { icon: GitBranch, label: 'branch', text: 'text-ink-3' },
  issue: { icon: CircleDot, label: 'issue', text: 'text-warm' },
  comment: { icon: MessageSquare, label: 'comment', text: 'text-ink-3' },
  message: { icon: MessageCircle, label: 'message', text: 'text-ink-3' },
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

// OBJECT_KINDS is the Objects lens's kind filter order, derived from
// OBJECT_META so a newly modeled kind shows up in the filter the moment it's
// added.
export const OBJECT_KINDS = Object.keys(OBJECT_META) as ObjectKind[]

// OBJECT_PROVIDERS backs the Objects lens's provider filter: the systems a
// run's change can land in. 'network' (an egress denial) never names an
// object.
export const OBJECT_PROVIDERS = ['github', 'jira', 'linear', 'slack'] as const

// PROVIDER_LABEL names the external systems for the activity feed's provider
// filters (internal/domain Provider*), across both lenses' vocabularies.
export const PROVIDER_LABEL: Record<string, string> = {
  github: 'GitHub',
  jira: 'Jira',
  slack: 'Slack',
  linear: 'Linear',
  network: 'Network',
}

// rowTone is a row's border and fill: the warm highlight for work a person acts
// on, else the quiet chrome every other row shares.
export function rowTone(highlighted?: boolean): string {
  return highlighted
    ? 'border-warm/40 bg-warm/[0.06] hover:bg-warm/[0.1]'
    : 'border-line-1 bg-tint-1 hover:bg-tint-3'
}
