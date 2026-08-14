import { useMemo, useState, type ReactNode } from 'react'
import { RotateCw } from 'lucide-react'
import { keyOf, type GitHubTeamCandidate } from '../lib/githubTeams'
import SearchField from './SearchField'

// Teams larger than this get a "broad team — noisy queue" hint. Fixed, not
// configurable: it's a nudge, not a policy (an open-question call).
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
   *  cap (the Settings fix); the onboarding card overrides to
   *  flex-fill within its own max-h-[80vh] envelope. */
  scrollClassName?: string
  /** Flush variant for the setup wizard: glass search + filter, no carded
   *  chrome. Default false keeps the Settings look. */
  bare?: boolean
}

// GitHubTeamChecklist is the shared body for BOTH GitHub-team → TF-team
// mapping surfaces: the onboarding wizard (GitHubTeamSelector) and the
// controlled team-config group (GitHubTeamGroup, used in the Settings team
// tab + the setup wizard's team steps). It owns every behavior common to the
// two — search, a selected/unselected/all filter, a bounded scrollable list,
// the badges (your team / member count / noisy / stale), and the loading / error /
// empty states — so the wrappers differ only in chrome (header copy, footer
// vs. controlled value) and how they seed the selection.
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
  bare = false,
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
              <div className="w-4 h-4 rounded bg-tint-3 animate-pulse" />
              <div className="flex-1 space-y-1.5">
                <div
                  className="h-3 rounded bg-tint-3 animate-pulse"
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
          <div className="text-body text-ink-2 text-center">{error}</div>
          {onRetry && (
            <button
              type="button"
              onClick={onRetry}
              className="flex items-center gap-1.5 text-ui font-medium text-warm hover:text-warm/80 transition-colors"
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
        <div className="text-body text-ink-3 py-8 leading-relaxed">{emptyLabel}</div>
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
      <div className={`shrink-0 pb-3 ${bare ? 'space-y-3.5' : 'space-y-2'}`}>
        {bare ? (
          <SearchField
            value={search}
            onChange={setSearch}
            placeholder="Search teams…"
            ariaLabel="Search GitHub teams"
          />
        ) : (
          <input
            type="text"
            aria-label="Search GitHub teams"
            placeholder="Search teams..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="w-full bg-raised border border-line-1 rounded-xl px-4 py-2.5 text-body text-ink-1 placeholder-ink-3 focus:outline-none focus:ring-2 focus:ring-warm/30 focus:border-warm/40 transition-colors"
          />
        )}
        {bare ? (
          // Rail-underline filters — borderless, blended (no segmented box).
          <div
            role="tablist"
            aria-label="Team filter"
            className="flex items-center gap-5 text-ui"
          >
            {segments.map((s) => {
              const active = mode === s.mode
              return (
                <button
                  key={s.mode}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => setMode(s.mode)}
                  className={`relative pb-1.5 font-medium transition-colors ${
                    active ? 'text-warm' : 'text-ink-3 hover:text-ink-2'
                  }`}
                >
                  {s.label}
                  <span className="ml-1 tabular-nums opacity-70">{s.count}</span>
                  {active && (
                    <span
                      aria-hidden
                      className="absolute inset-x-0 bottom-0 h-[2px] rounded-full bg-warm"
                    />
                  )}
                </button>
              )
            })}
          </div>
        ) : (
          <div
            role="tablist"
            aria-label="Team filter"
            className="flex items-center gap-1 rounded-xl bg-tint-2 p-0.5 text-ui"
          >
            {segments.map((s) => (
              <button
                key={s.mode}
                type="button"
                role="tab"
                aria-selected={mode === s.mode}
                onClick={() => setMode(s.mode)}
                className={`flex-1 rounded-lg px-2.5 py-1.5 font-medium transition-colors ${
                  mode === s.mode
                    ? 'bg-raised text-ink-1 shadow-float'
                    : 'text-ink-3 hover:text-ink-2'
                }`}
              >
                {s.label}
                <span className="ml-1 text-ink-3 tabular-nums">{s.count}</span>
              </button>
            ))}
          </div>
        )}
      </div>

      {/* List */}
      <div className={`${scrollClassName} overflow-y-auto`}>
        {visible.length === 0 ? (
          <p className="text-body text-ink-3 text-center py-8">
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
                className={`w-full flex items-start gap-3 px-3 py-2.5 text-left rounded-xl transition-colors hover:bg-tint-2 disabled:cursor-not-allowed disabled:opacity-60 ${
                  isChecked ? 'bg-warm/[0.04]' : ''
                }`}
              >
                <span
                  className={`mt-0.5 shrink-0 w-4 h-4 rounded border flex items-center justify-center transition-colors ${
                    isChecked ? 'bg-warm border-warm text-warm-ink' : 'border-line-1'
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
                    <span className="text-card-title font-medium text-ink-1 font-mono truncate">
                      {team.org_login}/{team.team_slug}
                    </span>
                    {team.mine && (
                      <span className="text-label text-warm border border-warm/30 rounded px-1 py-0.5">
                        your team
                      </span>
                    )}
                    {team.member_count ? (
                      <span className="text-label text-ink-3">
                        {team.member_count} member{team.member_count !== 1 ? 's' : ''}
                      </span>
                    ) : null}
                    {noisy && (
                      <span className="text-label text-ink-2 border border-line-1 rounded px-1 py-0.5">
                        broad team — noisy queue
                      </span>
                    )}
                    {team.orphan && (
                      <span className="text-label text-alarm border border-alarm/30 rounded px-1 py-0.5">
                        not on GitHub
                      </span>
                    )}
                  </div>
                  {team.name && team.name.toLowerCase() !== team.team_slug.toLowerCase() && (
                    <p className="text-reported text-ink-3 truncate mt-0.5">{team.name}</p>
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
