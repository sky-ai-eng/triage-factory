import { useState, useEffect } from 'react'
import type { PRSummary } from '../pages/PRDashboard'
import { apiJSON } from '../lib/apiClient'

interface PRStatusData {
  mergeable: boolean | null
  auto_merge: boolean
  mergeable_state: string
  reviews: { author: string; state: string; submitted_at: string }[]
  checks_status: {
    total: number
    passing: number
    failing: number
    pending: number
    skipped: number
  }
  conflicts: boolean
  review_decision: string
}

export default function PRCard({ pr }: { pr: PRSummary }) {
  const [status, setStatus] = useState<PRStatusData | null>(null)
  const [fetchFailed, setFetchFailed] = useState(false)
  const [expanded, setExpanded] = useState(false)
  // Derived: "loading" means expanded, no status received yet, AND the last
  // fetch attempt hasn't failed. Without the fetchFailed flag, a failed
  // fetch would leave `status` null forever and the "Failed to load status"
  // branch below would be unreachable.
  const loading = expanded && status === null && !fetchFailed

  useEffect(() => {
    if (!expanded) return
    let cancelled = false
    apiJSON<PRStatusData>(`/api/dashboard/prs/${pr.repo}/${pr.number}/status`)
      .then((d) => {
        if (!cancelled) {
          setStatus(d)
          setFetchFailed(false)
        }
      })
      .catch(() => {
        if (!cancelled) setFetchFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [expanded, pr.number, pr.repo])

  const age = formatAge(pr.updated_at)
  // mergeable_state "clean" means GitHub says it's good to merge
  // (accounts for branch protection, required reviews, CI, conflicts — everything)
  const canMerge = status ? status.mergeable_state === 'clean' && status.mergeable === true : null

  return (
    <div className="bg-raised backdrop-blur-xl border border-line-1 rounded-2xl overflow-hidden shadow-float shadow-black/[0.03]">
      {/* Main row — always visible */}
      <button
        onClick={() => setExpanded(!expanded)}
        className="w-full px-5 py-4 flex items-center gap-4 text-left hover:bg-tint-1 transition-colors"
      >
        {/* Merge indicator */}
        <div className="shrink-0">
          {pr.state === 'merged' ? (
            <div className="w-2.5 h-2.5 rounded-full bg-ink-1" />
          ) : pr.state === 'closed' ? (
            <div className="w-2.5 h-2.5 rounded-full bg-alarm" />
          ) : !expanded ? (
            <div
              className={`w-2.5 h-2.5 rounded-full ${pr.draft ? 'bg-ink-3/30' : 'bg-warm/40'}`}
            />
          ) : canMerge === true ? (
            <div className="w-2.5 h-2.5 rounded-full bg-warm" title="Ready to merge" />
          ) : canMerge === false ? (
            <div className="w-2.5 h-2.5 rounded-full bg-ink-3" title="Not ready" />
          ) : (
            <div className="w-2.5 h-2.5 rounded-full bg-ink-3/30" />
          )}
        </div>

        {/* PR info */}
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 mb-0.5">
            <span className="text-reported text-ink-3">{pr.repo}</span>
            <span className="text-reported text-ink-3">#{pr.number}</span>
            {pr.draft && (
              <span className="text-label font-medium text-ink-3 bg-tint-3 rounded px-1.5 py-0.5">
                DRAFT
              </span>
            )}
            {pr.state === 'merged' && (
              <span className="text-label font-medium text-ink-2 bg-tint-2 rounded px-1.5 py-0.5">
                MERGED
              </span>
            )}
            {pr.state === 'closed' && (
              <span className="text-label font-medium text-alarm bg-alarm/10 rounded px-1.5 py-0.5">
                CLOSED
              </span>
            )}
          </div>
          <h3 className="text-body font-medium text-ink-1 truncate">{pr.title}</h3>
        </div>

        {/* Labels */}
        <div className="hidden sm:flex gap-1.5 shrink-0">
          {(pr.labels || []).slice(0, 2).map((l) => (
            <span key={l} className="text-label text-ink-3 bg-tint-3 rounded-full px-2 py-0.5">
              {l}
            </span>
          ))}
        </div>

        {/* Age */}
        <span className="text-reported text-ink-3 shrink-0">{age}</span>

        {/* Expand arrow */}
        <svg
          className={`w-4 h-4 text-ink-3 transition-transform ${expanded ? 'rotate-180' : ''}`}
          viewBox="0 0 16 16"
          fill="none"
        >
          <path
            d="M4 6l4 4 4-4"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </button>

      {/* Expanded status panel */}
      {expanded && (
        <div className="px-5 pb-4 border-t border-line-1">
          {loading ? (
            <div className="py-4 text-ui text-ink-3">Loading status...</div>
          ) : status ? (
            <div className="pt-3 space-y-3">
              {/* Merge readiness */}
              <div className="flex items-center gap-3">
                <MergeIndicator
                  canMerge={canMerge}
                  conflicts={status.conflicts}
                  state={status.mergeable_state}
                />
                {status.auto_merge && (
                  <span className="text-reported text-ink-2 font-medium bg-tint-2 rounded-full px-2.5 py-0.5">
                    Auto-merge on
                  </span>
                )}
              </div>

              {/* Reviews */}
              <div>
                <h4 className="text-reported font-medium text-ink-3 mb-1.5">Reviews</h4>
                {!status.reviews?.length ? (
                  <p className="text-ui text-ink-3">No reviews yet</p>
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
                <h4 className="text-reported font-medium text-ink-3 mb-1.5">Checks</h4>
                <ChecksBar
                  checks={
                    status.checks_status || {
                      total: 0,
                      passing: 0,
                      failing: 0,
                      pending: 0,
                      skipped: 0,
                    }
                  }
                />
              </div>

              {/* Actions */}
              <div className="flex items-center gap-3 pt-1">
                <a
                  href={pr.html_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-ui text-warm hover:text-warm/70 font-medium transition-colors"
                >
                  Open on GitHub
                </a>
              </div>
            </div>
          ) : (
            <div className="py-4 text-ui text-alarm">Failed to load status</div>
          )}
        </div>
      )}
    </div>
  )
}

function MergeIndicator({
  canMerge,
  conflicts,
  state,
}: {
  canMerge: boolean | null
  conflicts: boolean
  state: string
}) {
  if (canMerge) {
    return <span className="text-ui font-medium text-ink-2">Ready to merge</span>
  }

  const reasons: string[] = []
  if (conflicts) reasons.push('has conflicts')
  if (state === 'blocked') reasons.push('blocked by branch protection')
  if (state === 'behind') reasons.push('behind base branch')

  return (
    <span className="text-ui text-ink-2">
      {reasons.length > 0 ? reasons.join(' · ') : 'Not ready'}
    </span>
  )
}

function ReviewBadge({ review }: { review: { author: string; state: string } }) {
  const colorMap: Record<string, string> = {
    APPROVED: 'bg-tint-2 text-ink-2 border-line-1',
    CHANGES_REQUESTED: 'bg-alarm/10 text-alarm border-alarm/20',
    DISMISSED: 'bg-tint-3 text-ink-3 border-line-1',
    PENDING: 'bg-tint-2 text-ink-2 border-line-1',
  }
  const colors = colorMap[review.state] || 'bg-tint-3 text-ink-3 border-line-1'
  const icon = review.state === 'APPROVED' ? '✓' : review.state === 'CHANGES_REQUESTED' ? '✗' : '○'

  return (
    <span className={`text-reported font-medium rounded-full px-2.5 py-0.5 border ${colors}`}>
      {icon} {review.author}
    </span>
  )
}

function ChecksBar({
  checks,
}: {
  checks: { total: number; passing: number; failing: number; pending: number; skipped: number }
}) {
  if (checks.total === 0) {
    return <p className="text-ui text-ink-3">No checks</p>
  }

  return (
    <div className="flex items-center gap-3">
      <div className="flex-1 h-1.5 rounded-full bg-tint-3 overflow-hidden flex">
        {checks.passing > 0 && (
          <div
            className="h-full bg-tint-2"
            style={{ width: `${(checks.passing / checks.total) * 100}%` }}
          />
        )}
        {checks.skipped > 0 && (
          <div
            className="h-full bg-ink-3/20"
            style={{ width: `${(checks.skipped / checks.total) * 100}%` }}
          />
        )}
        {checks.pending > 0 && (
          <div
            className="h-full bg-tint-2"
            style={{ width: `${(checks.pending / checks.total) * 100}%` }}
          />
        )}
        {checks.failing > 0 && (
          <div
            className="h-full bg-alarm"
            style={{ width: `${(checks.failing / checks.total) * 100}%` }}
          />
        )}
      </div>
      <span className="text-reported text-ink-3 shrink-0">
        {checks.passing}/{checks.total} passing
        {checks.skipped > 0 && ` · ${checks.skipped} skipped`}
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
