import {
  CircleDot,
  Eye,
  FolderGit2,
  GitBranch,
  GitPullRequest,
  MessageCircle,
  MessageSquare,
  Package,
  Play,
  Tag,
  Workflow,
  type LucideIcon,
} from 'lucide-react'
import type { ObjectKind } from '../types'

// Presentation for the object kinds an artifact can be (internal/domain
// ObjectKinds) — the activity feed's Objects lens glyphs and its kind filter.
// A data/helpers module (no components) so importing it into ObjectRow keeps
// React Fast Refresh happy. The kinds a deliverable shares (pull request,
// review, issue, branch, comment, message) take deliverableMeta's icon and
// tint, so one object reads the same on both surfaces.

export interface ObjectKindMeta {
  icon: LucideIcon
  label: string
  text: string
}

// OBJECT_KIND_META is exhaustive over ObjectKind. A server value outside the
// modeled union falls through to FALLBACK_OBJECT_META at the render layer.
export const OBJECT_KIND_META: Record<ObjectKind, ObjectKindMeta> = {
  pull_request: { icon: GitPullRequest, label: 'pull request', text: 'text-delegate' },
  issue: { icon: CircleDot, label: 'issue', text: 'text-warm' },
  review: { icon: Eye, label: 'review', text: 'text-snooze' },
  branch: { icon: GitBranch, label: 'branch', text: 'text-ink-3' },
  comment: { icon: MessageSquare, label: 'comment', text: 'text-ink-3' },
  message: { icon: MessageCircle, label: 'message', text: 'text-ink-3' },
  repository: { icon: FolderGit2, label: 'repository', text: 'text-ink-3' },
  label: { icon: Tag, label: 'label', text: 'text-ink-3' },
  release: { icon: Package, label: 'release', text: 'text-ink-3' },
  workflow: { icon: Workflow, label: 'workflow', text: 'text-ink-3' },
  workflow_run: { icon: Play, label: 'workflow run', text: 'text-ink-3' },
}

export const FALLBACK_OBJECT_META: ObjectKindMeta = {
  icon: CircleDot,
  label: 'object',
  text: 'text-ink-3',
}

// OBJECT_KINDS is the kind filter's order, derived from OBJECT_KIND_META so a
// newly modeled kind shows up in the filter the moment it's added.
export const OBJECT_KINDS = Object.keys(OBJECT_KIND_META) as ObjectKind[]

// metaForObjectKind resolves a (possibly unmodeled) kind string to its
// presentation.
export function metaForObjectKind(kind: string): ObjectKindMeta {
  return OBJECT_KIND_META[kind as ObjectKind] ?? FALLBACK_OBJECT_META
}

// OBJECT_PROVIDERS backs the Objects lens's provider filter: the systems a
// run's change can land in. 'network' (an egress denial) and 'git' never
// name an object.
export const OBJECT_PROVIDERS = ['github', 'jira', 'linear', 'slack'] as const
