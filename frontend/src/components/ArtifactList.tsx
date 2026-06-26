import { useEffect, useState } from 'react'
import {
  CircleDot,
  ExternalLink,
  GitBranch,
  GitPullRequest,
  MessageSquare,
  type LucideIcon,
  Eye,
} from 'lucide-react'
import type { Artifact, ArtifactKind } from '../types'
import { readError } from '../lib/api'
import { TONE_TEXT, TONE_VAR, type Tone } from './board/cardStyle'

// ArtifactList — the shared surface for everything a run produced (TFAC-470).
// One row per artifact: a kind icon + the target + a state badge + an external
// link. Fetched run-scoped from GET /api/agent/runs/{id}/artifacts (TFAC-465);
// the component owns its own fetch so both consumers stay one-liners — the
// run-detail rail (mounts with the page → "always visible") and the board
// card's popover (mounts on open → lazy-fetch).
//
// pull_request / review rows open their approval overlays (PendingPROverlay /
// ReviewOverlay, TFAC-462/463) by artifact id via onOpenApproval — those
// overlays are owned by the parent (Board / RunDetail), so this list never
// duplicates them. Every other kind (branch / issue / comment), and any row
// when no handler is wired, just links out to `url`.
interface Props {
  runId: string
  onOpenApproval?: (kind: 'review' | 'pr', artifactId: string) => void
}

export default function ArtifactList({ runId, onOpenApproval }: Props) {
  const [artifacts, setArtifacts] = useState<Artifact[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!runId) return
    let cancelled = false
    setArtifacts(null)
    setError(null)
    ;(async () => {
      try {
        const res = await fetch(`/api/agent/runs/${runId}/artifacts`)
        if (!res.ok) {
          // readError keeps the context ("Couldn't load artifacts: …") and
          // degrades through the server's JSON `error`, a non-JSON text body,
          // then the bare status code — never a context-less raw message.
          throw new Error(await readError(res, "Couldn't load artifacts"))
        }
        const data = (await res.json()) as Artifact[]
        if (!cancelled) setArtifacts(data ?? [])
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [runId])

  if (error) {
    return <p className="text-[11.5px] leading-relaxed text-dismiss">{error}</p>
  }
  if (artifacts === null) {
    return <p className="text-[11.5px] text-text-tertiary/70">Loading artifacts…</p>
  }
  if (artifacts.length === 0) {
    return <p className="text-[11.5px] text-text-tertiary/70">No artifacts yet.</p>
  }

  return (
    <ul className="space-y-1.5">
      {artifacts.map((a) => (
        <ArtifactRow key={a.id} artifact={a} onOpenApproval={onOpenApproval} />
      ))}
    </ul>
  )
}

// A single artifact row. PR/review rows with a wired handler render as a button
// that opens the overlay (the external link stays reachable as a trailing
// affordance); every other row is the link itself.
function ArtifactRow({
  artifact,
  onOpenApproval,
}: {
  artifact: Artifact
  onOpenApproval?: (kind: 'review' | 'pr', artifactId: string) => void
}) {
  // KIND_META is exhaustive over ArtifactKind; the fallback only catches a
  // server value outside the modeled union (forward-compat), so the cast lets
  // an unmodeled string fall through to it rather than typecheck as covered.
  const meta = KIND_META[artifact.kind as ArtifactKind] ?? FALLBACK_META
  const Icon = meta.icon
  const overlayKind: 'review' | 'pr' | null =
    artifact.kind === 'pull_request' ? 'pr' : artifact.kind === 'review' ? 'review' : null

  const body = (
    <>
      <Icon size={13} className={`shrink-0 ${meta.text}`} aria-hidden />
      <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-text-secondary">
        {artifact.target || artifact.external_id || meta.label}
      </span>
      <StateBadge state={artifact.state} />
    </>
  )

  const rowClass =
    'flex w-full items-center gap-2 rounded-[4px] border border-border-subtle bg-black/[0.015] px-2 py-1.5 text-left transition-colors hover:bg-black/[0.04]'

  // Inline (not a precomputed boolean) so TypeScript narrows both to non-null
  // inside the branch — no `!` assertions on the click handler.
  if (overlayKind != null && onOpenApproval != null) {
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
      </li>
    )
  }

  // Link-out rows (branch / issue / comment, or any row with no overlay
  // handler). The whole row is the anchor; no external object means a plain,
  // non-interactive row.
  if (artifact.url) {
    return (
      <li>
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
      </li>
    )
  }
  return (
    <li className={rowClass} aria-label={meta.label}>
      {body}
    </li>
  )
}

// ExternalLinkIcon — the trailing "open on GitHub/Jira" affordance for overlay
// rows, kept separate from the overlay-opening click. stopPropagation isn't
// needed (it's a sibling, not nested in the button), but it carries its own
// hover target so the link reads as distinct from the row.
function ExternalLinkIcon({ url, label }: { url: string; label: string }) {
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      onClick={(e) => e.stopPropagation()}
      className="inline-flex shrink-0 items-center rounded-[4px] p-1.5 text-text-tertiary/70 transition-colors hover:bg-black/[0.04] hover:text-text-secondary"
      aria-label={`Open ${label} on its source (new tab)`}
      title="Open source (new tab)"
    >
      <ExternalLink size={11} aria-hidden />
    </a>
  )
}

// StateBadge — a quiet pill carrying the artifact's lifecycle state in a tone.
function StateBadge({ state }: { state: string }) {
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

interface KindMeta {
  icon: LucideIcon
  label: string
  text: string
}

const KIND_META: Record<ArtifactKind, KindMeta> = {
  branch: { icon: GitBranch, label: 'branch', text: 'text-text-tertiary' },
  pull_request: { icon: GitPullRequest, label: 'pull request', text: 'text-delegate' },
  review: { icon: Eye, label: 'review', text: 'text-snooze' },
  issue: { icon: CircleDot, label: 'issue', text: 'text-accent' },
  comment: { icon: MessageSquare, label: 'comment', text: 'text-text-tertiary' },
}

const FALLBACK_META: KindMeta = {
  icon: CircleDot,
  label: 'artifact',
  text: 'text-text-tertiary',
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
