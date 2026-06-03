import { useMemo, useState, type ReactNode } from 'react'
import { RotateCw } from 'lucide-react'
import { keyOf, type GitHubTeamCandidate } from '../lib/githubTeams'

// Teams larger than this get a "broad team — noisy queue" hint. Fixed, not
// configurable: it's a nudge, not a policy (SKY-411 open-question call).
const NOISY_TEAM_THRESHOLD = 20

// The three filter modes behind the segmented control. 'all' is the default
// alphabetically-laid-out full list; 'selected' / 'unselected' subset it
// (rows stay alphabetical within each), which is how a user with a long org
// team list finds what they've already picked — or what's left.
type FilterMode = 'all' | 'unselected' | 'selected'

// One rendered row: a candidate, or a selected mapping GitHub no longer
// returns (orphan = stale / unreadable org).
interface Row extends GitHubTeamCandidate {
  orphan: boolean
}

interface Props {
  /** Live org-wide GitHub teams from the github-groups GET. */
  candidates: GitHubTeamCandidate[]
  /** Currently-checked mappings, keyed `keyOf(org, slug)`. */
  selected: Set<string>
  /** Toggle one team's membership in the selection. */
  onToggle: (org: string, slug: string) => void
  /** Fetch in flight — renders skeleton rows in place of the list. */
  loading?: boolean
  /** Fetch error — renders the message plus a Retry that calls onRetry. */
  error?: string | null
  onRetry?: () => void
  /** Disable every row (Settings read-only, or a save in flight). */
  disabled?: boolean
  /** Copy for the post-load "no candidates at all" state — each wrapper
   *  phrases this for its own surface. */
  emptyLabel?: ReactNode
  /** Extra classes for the root flex column. The onboarding card passes
   *  `flex-1 min-h-0` so the list fills the space between header and
   *  footer; Settings leaves it content-height. */
  className?: string
  /** Classes for the scrollable list region. Default is a fixed bounded
   *  cap (Settings — the SKY-388 fix); the onboarding card overrides to
   *  flex-fill within its own max-h-[80vh] envelope. */
  scrollClassName?: string
}

// GitHubTeamChecklist is the shared body for BOTH GitHub-team → TF-team
// mapping surfaces: the onboarding wizard (GitHubTeamSelector) and the
// Settings editor (GitHubGroupsEditor). It owns every behavior common to
// the two — search, a selected/unselected/all filter, a bounded scrollable
// list, the badges (your team / member count / noisy / stale), and the
// loading / error / empty states — so the wrappers differ only in chrome
// (header copy, footer vs. Save button) and how they seed and persist the
// selection.
//
// Rows are always alphabetical by `org/slug`. A selected mapping GitHub no
// longer returns (a team deleted since the last poll, or a repo whose org
// the token can't read) still renders, flagged "not on GitHub" — so an
// admin can always un-assign a stale mapping. Both surfaces get this for
// free; before unification only Settings did.
export default function GitHubTeamChecklist({
  candidates,
  selected,
  onToggle,
  loading = false,
  error = null,
  onRetry,
  disabled = false,
  emptyLabel,
  className = '',
  scrollClassName = 'max-h-80',
}: Props) {
  const [search, setSearch] = useState('')
  const [mode, setMode] = useState<FilterMode>('all')

  // Merge live candidates with any selected mapping GitHub didn't return,
  // then sort A–Z. Keyed dedup so a candidate that's also selected appears
  // once (as a non-orphan candidate, not a stale row).
  const rows = useMemo<Row[]>(() => {
    const byKey = new Map<string, Row>()
    for (const c of candidates) {
      byKey.set(keyOf(c.org_login, c.team_slug), { ...c, orphan: false })
    }
    for (const k of selected) {
      if (byKey.has(k)) continue
      const [org_login, team_slug] = k.split('/')
      byKey.set(k, { org_login, team_slug, name: '', orphan: true })
    }
    return Array.from(byKey.values()).sort((a, b) =>
      keyOf(a.org_login, a.team_slug).localeCompare(keyOf(b.org_login, b.team_slug)),
    )
  }, [candidates, selected])

  // Apply the active filter mode, then the search query, preserving the
  // alphabetical order from `rows`.
  const visible = useMemo(() => {
    const q = search.trim().toLowerCase()
    return rows.filter((r) => {
      const k = keyOf(r.org_login, r.team_slug)
      if (mode === 'selected' && !selected.has(k)) return false
      if (mode === 'unselected' && selected.has(k)) return false
      if (!q) return true
      return k.includes(q) || (r.name || '').toLowerCase().includes(q)
    })
  }, [rows, search, mode, selected])

  // Loading / error / empty pre-empt the controls — there's nothing to
  // search or filter yet. Rendered inside the same flex root so the
  // onboarding card's flex-1 still fills.
  if (loading) {
    return (
      <div className={`flex flex-col min-h-0 ${className}`}>
        <div className="space-y-1 py-2">
          {[1, 2, 3, 4, 5, 6].map((i) => (
            <div key={i} className="flex items-center gap-3 px-3 py-2.5 rounded-xl">
              <div className="w-4 h-4 rounded bg-black/[0.04] animate-pulse" />
              <div className="flex-1 space-y-1.5">
                <div
                  className="h-3 rounded bg-black/[0.04] animate-pulse"
                  style={{ width: `${50 + ((i * 17) % 35)}%` }}
                />
              </div>
            </div>
          ))}
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className={`flex flex-col min-h-0 ${className}`}>
        <div className="flex flex-col items-center justify-center py-12 gap-3">
          <div className="text-[13px] text-text-secondary text-center">{error}</div>
          {onRetry && (
            <button
              type="button"
              onClick={onRetry}
              className="flex items-center gap-1.5 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors"
            >
              <RotateCw size={13} />
              Retry
            </button>
          )}
        </div>
      </div>
    )
  }

  if (rows.length === 0) {
    return (
      <div className={`flex flex-col min-h-0 ${className}`}>
        <div className="text-[13px] text-text-tertiary py-8 leading-relaxed">{emptyLabel}</div>
      </div>
    )
  }

  const segments: { mode: FilterMode; label: string; count?: number }[] = [
    { mode: 'all', label: 'All', count: rows.length },
    { mode: 'selected', label: 'Selected', count: selected.size },
    { mode: 'unselected', label: 'Unselected', count: rows.length - selected.size },
  ]

  return (
    <div className={`flex flex-col min-h-0 ${className}`}>
      {/* Search + filter — shrink-0 so only the list below scrolls. */}
      <div className="shrink-0 space-y-2 pb-3">
        <input
          type="text"
          placeholder="Search teams..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors"
        />
        <div className="flex items-center gap-1 rounded-xl bg-black/[0.03] p-0.5 text-[12px]">
          {segments.map((s) => (
            <button
              key={s.mode}
              type="button"
              onClick={() => setMode(s.mode)}
              className={`flex-1 rounded-lg px-2.5 py-1.5 font-medium transition-colors ${
                mode === s.mode
                  ? 'bg-surface-raised text-text-primary shadow-sm'
                  : 'text-text-tertiary hover:text-text-secondary'
              }`}
            >
              {s.label}
              <span className="ml-1 text-text-tertiary tabular-nums">{s.count}</span>
            </button>
          ))}
        </div>
      </div>

      {/* List */}
      <div className={`${scrollClassName} overflow-y-auto`}>
        {visible.length === 0 ? (
          <p className="text-[13px] text-text-tertiary text-center py-8">
            {mode === 'selected'
              ? 'No selected teams match.'
              : mode === 'unselected'
                ? 'No unselected teams match.'
                : `No teams match "${search}"`}
          </p>
        ) : (
          visible.map((team) => {
            const k = keyOf(team.org_login, team.team_slug)
            const isChecked = selected.has(k)
            const noisy = (team.member_count ?? 0) > NOISY_TEAM_THRESHOLD
            return (
              <button
                key={k}
                type="button"
                role="checkbox"
                aria-checked={isChecked}
                disabled={disabled}
                onClick={() => onToggle(team.org_login, team.team_slug)}
                className={`w-full flex items-start gap-3 px-3 py-2.5 text-left rounded-xl transition-colors hover:bg-black/[0.02] disabled:cursor-not-allowed disabled:opacity-60 ${
                  isChecked ? 'bg-accent/[0.04]' : ''
                }`}
              >
                <span
                  className={`mt-0.5 shrink-0 w-4 h-4 rounded border flex items-center justify-center transition-colors ${
                    isChecked ? 'bg-accent border-accent text-white' : 'border-border-subtle'
                  }`}
                >
                  {isChecked && (
                    <svg
                      width="10"
                      height="10"
                      viewBox="0 0 10 10"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    >
                      <polyline points="2 5 4 7 8 3" />
                    </svg>
                  )}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="text-[12.5px] font-medium text-text-primary font-mono truncate">
                      {team.org_login}/{team.team_slug}
                    </span>
                    {team.mine && (
                      <span className="text-[10px] text-accent border border-accent/30 rounded px-1 py-0.5">
                        your team
                      </span>
                    )}
                    {team.member_count ? (
                      <span className="text-[10px] text-text-tertiary">
                        {team.member_count} member{team.member_count !== 1 ? 's' : ''}
                      </span>
                    ) : null}
                    {noisy && (
                      <span className="text-[10px] text-snooze border border-snooze/30 rounded px-1 py-0.5">
                        broad team — noisy queue
                      </span>
                    )}
                    {team.orphan && (
                      <span className="text-[10px] text-dismiss border border-dismiss/30 rounded px-1 py-0.5">
                        not on GitHub
                      </span>
                    )}
                  </div>
                  {team.name && team.name.toLowerCase() !== team.team_slug.toLowerCase() && (
                    <p className="text-[11px] text-text-tertiary truncate mt-0.5">{team.name}</p>
                  )}
                </div>
              </button>
            )
          })
        )}
      </div>
    </div>
  )
}
