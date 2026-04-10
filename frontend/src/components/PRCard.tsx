import { useState, useEffect } from 'react'
import type { PRSummary } from '../pages/PRDashboard'

interface PRStatusData {
  mergeable: boolean | null
  auto_merge: boolean
  mergeable_state: string
  reviews: { author: string; state: string; submitted_at: string }[]
  checks_status: { total: number; passing: number; failing: number; pending: number }
  conflicts: boolean
  review_decision: string
}

export default function PRCard({ pr }: { pr: PRSummary }) {
  const [status, setStatus] = useState<PRStatusData | null>(null)
  const [loading, setLoading] = useState(false)
  const [expanded, setExpanded] = useState(false)

  useEffect(() => {
    if (!expanded) return
    setLoading(true)
    fetch(`/api/dashboard/prs/${pr.number}/status?repo=${pr.repo}`)
      .then((r) => r.json())
      .then((d) => setStatus(d))
      .catch(() => {})
      .finally(() => setLoading(false))
  }, [expanded, pr.number, pr.repo])

  const age = formatAge(pr.updated_at)
  // mergeable_state "clean" means GitHub says it's good to merge
  // (accounts for branch protection, required reviews, CI, conflicts — everything)
  const canMerge = status
    ? status.mergeable_state === 'clean' && status.mergeable === true
    : null

  return (
    <div className="bg-surface-raised backdrop-blur-xl border border-border-glass rounded-2xl overflow-hidden shadow-sm shadow-black/[0.03]">
      {/* Main row — always visible */}
      <button
        onClick={() => setExpanded(!expanded)}
        className="w-full px-5 py-4 flex items-center gap-4 text-left hover:bg-black/[0.01] transition-colors"
      >
        {/* Merge indicator */}
        <div className="shrink-0">
          {pr.state === 'merged' ? (
            <div className="w-2.5 h-2.5 rounded-full bg-claim" />
          ) : pr.state === 'closed' ? (
            <div className="w-2.5 h-2.5 rounded-full bg-dismiss" />
          ) : !expanded ? (
            <div className={`w-2.5 h-2.5 rounded-full ${pr.draft ? 'bg-text-tertiary/30' : 'bg-accent/40'}`} />
          ) : canMerge === true ? (
            <div className="w-2.5 h-2.5 rounded-full bg-claim" title="Ready to merge" />
          ) : canMerge === false ? (
            <div className="w-2.5 h-2.5 rounded-full bg-snooze" title="Not ready" />
          ) : (
            <div className="w-2.5 h-2.5 rounded-full bg-text-tertiary/30" />
          )}
        </div>

        {/* PR info */}
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 mb-0.5">
            <span className="text-[11px] text-text-tertiary">{pr.repo}</span>
            <span className="text-[11px] text-text-tertiary">#{pr.number}</span>
            {pr.draft && (
              <span className="text-[10px] font-medium text-text-tertiary bg-black/[0.05] rounded px-1.5 py-0.5">
                DRAFT
              </span>
            )}
            {pr.state === 'merged' && (
              <span className="text-[10px] font-medium text-claim bg-claim/10 rounded px-1.5 py-0.5">
                MERGED
              </span>
            )}
            {pr.state === 'closed' && (
              <span className="text-[10px] font-medium text-dismiss bg-dismiss/10 rounded px-1.5 py-0.5">
                CLOSED
              </span>
            )}
          </div>
          <h3 className="text-[14px] font-medium text-text-primary truncate">
            {pr.title}
          </h3>
        </div>

        {/* Labels */}
        <div className="hidden sm:flex gap-1.5 shrink-0">
          {(pr.labels || []).slice(0, 2).map((l) => (
            <span key={l} className="text-[10px] text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5">
              {l}
            </span>
          ))}
        </div>

        {/* Age */}
        <span className="text-[11px] text-text-tertiary shrink-0">{age}</span>

        {/* Expand arrow */}
        <svg
          className={`w-4 h-4 text-text-tertiary transition-transform ${expanded ? 'rotate-180' : ''}`}
          viewBox="0 0 16 16"
          fill="none"
        >
          <path d="M4 6l4 4 4-4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>

      {/* Expanded status panel */}
      {expanded && (
        <div className="px-5 pb-4 border-t border-border-subtle">
          {loading ? (
            <div className="py-4 text-[12px] text-text-tertiary">Loading status...</div>
          ) : status ? (
            <div className="pt-3 space-y-3">
              {/* Merge readiness */}
              <div className="flex items-center gap-3">
                <MergeIndicator canMerge={canMerge} conflicts={status.conflicts} state={status.mergeable_state} />
                {status.auto_merge && (
                  <span className="text-[11px] text-delegate font-medium bg-delegate/10 rounded-full px-2.5 py-0.5">
                    Auto-merge on
                  </span>
                )}
              </div>

              {/* Reviews */}
              <div>
                <h4 className="text-[11px] font-medium text-text-tertiary mb-1.5">Reviews</h4>
                {!status.reviews?.length ? (
                  <p className="text-[12px] text-text-tertiary">No reviews yet</p>
                ) : (
                  <div className="flex flex-wrap gap-2">
                    {status.reviews.map((r) => (
                      <ReviewBadge key={r.author} review={r} />
                    ))}
                  </div>
                )}
              </div>

              {/* Checks */}
              <div>
                <h4 className="text-[11px] font-medium text-text-tertiary mb-1.5">Checks</h4>
                <ChecksBar checks={status.checks_status || { total: 0, passing: 0, failing: 0, pending: 0 }} />
              </div>

              {/* Actions */}
              <div className="flex items-center gap-3 pt-1">
                <a
                  href={pr.html_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
                >
                  Open on GitHub
                </a>
              </div>
            </div>
          ) : (
            <div className="py-4 text-[12px] text-dismiss">Failed to load status</div>
          )}
        </div>
      )}
    </div>
  )
}

function MergeIndicator({ canMerge, conflicts, state }: { canMerge: boolean | null; conflicts: boolean; state: string }) {
  if (canMerge) {
    return <span className="text-[12px] font-medium text-claim">Ready to merge</span>
  }

  const reasons: string[] = []
  if (conflicts) reasons.push('has conflicts')
  if (state === 'blocked') reasons.push('blocked by branch protection')
  if (state === 'behind') reasons.push('behind base branch')

  return (
    <span className="text-[12px] text-text-secondary">
      {reasons.length > 0 ? reasons.join(' · ') : 'Not ready'}
    </span>
  )
}

function ReviewBadge({ review }: { review: { author: string; state: string } }) {
  const colorMap: Record<string, string> = {
    APPROVED: 'bg-claim/10 text-claim border-claim/20',
    CHANGES_REQUESTED: 'bg-dismiss/10 text-dismiss border-dismiss/20',
    DISMISSED: 'bg-black/[0.04] text-text-tertiary border-border-subtle',
    PENDING: 'bg-snooze/10 text-snooze border-snooze/20',
  }
  const colors = colorMap[review.state] || 'bg-black/[0.04] text-text-tertiary border-border-subtle'
  const icon = review.state === 'APPROVED' ? '✓' : review.state === 'CHANGES_REQUESTED' ? '✗' : '○'

  return (
    <span className={`text-[11px] font-medium rounded-full px-2.5 py-0.5 border ${colors}`}>
      {icon} {review.author}
    </span>
  )
}

function ChecksBar({ checks }: { checks: { total: number; passing: number; failing: number; pending: number } }) {
  if (checks.total === 0) {
    return <p className="text-[12px] text-text-tertiary">No checks</p>
  }

  return (
    <div className="flex items-center gap-3">
      <div className="flex-1 h-1.5 rounded-full bg-black/[0.04] overflow-hidden flex">
        {checks.passing > 0 && (
          <div className="h-full bg-claim" style={{ width: `${(checks.passing / checks.total) * 100}%` }} />
        )}
        {checks.pending > 0 && (
          <div className="h-full bg-snooze" style={{ width: `${(checks.pending / checks.total) * 100}%` }} />
        )}
        {checks.failing > 0 && (
          <div className="h-full bg-dismiss" style={{ width: `${(checks.failing / checks.total) * 100}%` }} />
        )}
      </div>
      <span className="text-[11px] text-text-tertiary shrink-0">
        {checks.passing}/{checks.total} passing
      </span>
    </div>
  )
}

function formatAge(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}
