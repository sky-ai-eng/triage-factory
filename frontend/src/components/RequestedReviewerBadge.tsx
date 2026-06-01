import { Users } from 'lucide-react'
import type { Task } from '../types'
import { parseRequestedReviewer, reviewerProfileURL } from '../lib/requestedReviewer'

// SKY-412: with per-reviewer dedup (SKY-370) a single PR mints one
// `github:pr:review_requested` task per requested reviewer, distinguished
// only by dedup_key. The cards are otherwise identical, so this badge
// surfaces *who* the review was requested of — the queue's most relevant
// routing signal (me personally vs. a team I'm on) — without a click
// through to GitHub. Parsing/URL helpers live in lib/requestedReviewer.

// RequestedReviewerBadge is a subtle, inline link in a card's badge row.
// It renders nothing unless the task is a review_requested task with a
// parseable reviewer and a derivable URL, so callers can drop it into a
// badge row unconditionally — the row layout is unchanged for every other
// task. The badge IS the link; click/pointerdown stopPropagation so it
// doesn't trip the card's drag handle (mirrors the "Open" link).
export default function RequestedReviewerBadge({ task }: { task: Task }) {
  const reviewer = parseRequestedReviewer(task)
  if (!reviewer) return null
  const url = reviewerProfileURL(task.source_url, reviewer)
  if (!url) return null

  // The user glyph below already renders a leading "@", so the label omits
  // it to avoid "@ @login"; the team label has no such prefix glyph.
  const label = reviewer.kind === 'user' ? reviewer.login : `${reviewer.org}/${reviewer.slug}`
  const title =
    reviewer.kind === 'user'
      ? `Review requested of @${reviewer.login}`
      : `Review requested of team ${reviewer.org}/${reviewer.slug}`

  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      title={title}
      aria-label={title}
      onClick={(e) => e.stopPropagation()}
      onPointerDown={(e) => e.stopPropagation()}
      className="inline-flex shrink-0 items-center gap-1 text-[10px] text-text-tertiary hover:text-text-secondary truncate max-w-[120px] transition-colors"
    >
      {reviewer.kind === 'user' ? (
        <span aria-hidden>@</span>
      ) : (
        <Users aria-hidden size={10} className="shrink-0" />
      )}
      <span className="truncate">{label}</span>
    </a>
  )
}
