import { ExternalLink, X } from 'lucide-react'
import type { Artifact } from '../types'
import { TONE_TEXT, TONE_VAR, type Tone } from './board/cardStyle'
import { metaForKind } from './artifactMeta'

// ArtifactRow + StateBadge, factored out of ArtifactList.tsx (TFAC-483) so the
// bot-activity audit feed can render a provided Artifact[] with the SAME row
// rendering — kind icon + target + state badge + link-out — without the
// run-scoped fetch or the approval overlay. ArtifactList still owns the fetch;
// the feed renders link-out-only rows (no onOpenApproval handler).

// A single artifact row. A still-ACTIONABLE pull_request / review row (a draft
// PR / pending review awaiting an approve-or-dismiss decision) WITH a wired
// onOpenApproval handler renders as a button that opens the approval overlay
// (the external link stays reachable as a trailing affordance). Every other row
// — resolved PRs/reviews, other kinds, and every row in the audit feed, which
// wires no handler — is the link itself: a submitted review links to the posted
// review's GitHub anchor, not back into the stale TF-side draft editor.
export function ArtifactRow({
  artifact,
  onOpenApproval,
  onDismiss,
  dismissing,
  note,
}: {
  artifact: Artifact
  onOpenApproval?: (kind: 'review' | 'pr', artifactId: string) => void
  // In-place dismiss for a row in the run's unresolved set (close the draft PR /
  // discard the pending review without opening its editor). The owner decides
  // which rows get it — ArtifactList gates on the run projection's authoritative
  // pending ids — so the audit feed and resolved rows never carry an [x].
  onDismiss?: () => void
  dismissing?: boolean
  // Optional trailing context rendered between the target and the state badge —
  // the bot-activity org feed (TFAC-483) passes the owning team chip here so a
  // cross-team row shows which team's bot acted. Undefined elsewhere (the
  // run-scoped list), so those rows are unchanged.
  note?: React.ReactNode
}) {
  const meta = metaForKind(artifact.kind)
  const Icon = meta.icon
  const overlayKind: 'review' | 'pr' | null =
    artifact.kind === 'pull_request' ? 'pr' : artifact.kind === 'review' ? 'review' : null
  // Only the awaiting-a-decision state routes to the overlay; any resolved state
  // falls through to the link-out rendering below.
  const actionable =
    artifact.kind === 'pull_request' ? artifact.state === 'draft' : artifact.state === 'pending'

  const body = (
    <>
      <Icon size={13} className={`shrink-0 ${meta.text}`} aria-hidden />
      <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-text-secondary">
        {artifact.target || artifact.external_id || meta.label}
      </span>
      {note}
      <StateBadge state={artifact.state} />
    </>
  )

  const rowClass =
    'flex w-full items-center gap-2 rounded-[4px] border border-border-subtle bg-black/[0.015] px-2 py-1.5 text-left transition-colors hover:bg-black/[0.04]'

  // The [x] rides the row's ACTIONABLE state, not the overlay handler — a
  // still-pending row stays dismissable even where no editor is wired, and a
  // freshly-resolved row drops it before the owner's pending set catches up.
  const dismissButton =
    onDismiss && actionable ? (
      <button
        type="button"
        onClick={onDismiss}
        disabled={dismissing}
        aria-label={`Dismiss ${meta.label}`}
        title={
          artifact.kind === 'pull_request'
            ? 'Dismiss — closes the draft PR (branch kept)'
            : 'Dismiss — discards the pending review'
        }
        className="inline-flex shrink-0 items-center justify-center rounded-[4px] p-1.5 text-text-tertiary/70 transition-colors hover:bg-dismiss/[0.1] hover:text-dismiss disabled:cursor-wait disabled:opacity-50"
      >
        <X size={12} aria-hidden />
      </button>
    ) : null

  // Inline (not a precomputed boolean) so TypeScript narrows both to non-null
  // inside the branch — no `!` assertions on the click handler.
  if (overlayKind != null && onOpenApproval != null && actionable) {
    return (
      <li className="flex items-center gap-1">
        <button
          type="button"
          onClick={() => onOpenApproval(overlayKind, artifact.id)}
          className={rowClass}
          aria-label={`Open ${meta.label}: ${artifact.target}`}
          title={`Open ${meta.label}`}
        >
          {body}
        </button>
        {artifact.url && <ExternalLinkIcon url={artifact.url} label={meta.label} />}
        {dismissButton}
      </li>
    )
  }

  // Link-out rows (branch / issue / comment, or any row with no overlay handler —
  // every audit-feed row). The whole row is the anchor; no external object means
  // a plain, non-interactive row.
  if (artifact.url) {
    return (
      <li className={dismissButton ? 'flex items-center gap-1' : undefined}>
        <a
          href={artifact.url}
          target="_blank"
          rel="noopener noreferrer"
          className={rowClass}
          aria-label={`Open ${meta.label} on ${artifact.provider}`}
          title={`Open ${meta.label} (new tab)`}
        >
          {body}
          <ExternalLink size={11} className="shrink-0 text-text-tertiary/70" aria-hidden />
        </a>
        {dismissButton}
      </li>
    )
  }
  return (
    <li className={rowClass} aria-label={meta.label}>
      {body}
      {dismissButton}
    </li>
  )
}

// ExternalLinkIcon — the trailing "open on GitHub/Jira" affordance for overlay
// rows, a sibling of (not nested in) the overlay-opening button, so a click on
// it never reaches the button — no stopPropagation needed. It carries its own
// hover target so the link reads as distinct from the row.
function ExternalLinkIcon({ url, label }: { url: string; label: string }) {
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      className="inline-flex shrink-0 items-center rounded-[4px] p-1.5 text-text-tertiary/70 transition-colors hover:bg-black/[0.04] hover:text-text-secondary"
      aria-label={`Open ${label} on its source (new tab)`}
      title="Open source (new tab)"
    >
      <ExternalLink size={11} aria-hidden />
    </a>
  )
}

// StateBadge — a quiet pill carrying the artifact's lifecycle state in a tone.
export function StateBadge({ state }: { state: string }) {
  if (!state) return null
  const tone = stateTone(state)
  return (
    <span
      className={`shrink-0 rounded-[3px] px-1.5 py-0.5 font-mono text-[9px] font-semibold uppercase tracking-[0.08em] ${TONE_TEXT[tone]}`}
      style={{ background: `color-mix(in srgb, ${TONE_VAR[tone]} 12%, transparent)` }}
    >
      {state}
    </span>
  )
}

// stateTone maps an artifact state to the card tone vocabulary. Landed-good
// outcomes read green — merged / submitted / posted / created, plus 'open' (an
// agent-filed PR/issue now live on GitHub/Jira, a done-and-waiting success).
// In-flight / awaiting-you states (draft / pending) read amber; retired states
// (closed / deleted / dismissed) read as the dismiss tone; the rest stay
// neutral. The distinct values are unambiguous across kinds, so the tone keys
// off state alone — the kind icon already carries the type.
function stateTone(state: string): Tone {
  switch (state) {
    case 'merged':
    case 'submitted':
    case 'posted':
    case 'created':
    case 'open':
      return 'good'
    case 'draft':
    case 'pending':
      return 'attention'
    case 'closed':
    case 'deleted':
    case 'dismissed':
      return 'problem'
    default:
      // pushed / updated and any unknown state.
      return 'neutral'
  }
}
