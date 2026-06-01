import { useState, useEffect, useMemo, useCallback, useRef } from 'react'
import { RotateCw } from 'lucide-react'

// One of the authenticated user's GitHub teams (GET /api/github/user-teams).
// member_count is best-effort — absent/0 means "unknown", no noisy hint.
interface GitHubUserTeam {
  org_slug: string
  team_slug: string
  name: string
  member_count?: number
}

export type { GitHubUserTeam }

// Teams larger than this get a "broad team — noisy queue" hint. Fixed, not
// configurable: it's a nudge, not a policy (SKY-411 open-question call).
const NOISY_TEAM_THRESHOLD = 20

const keyOf = (org: string, slug: string) => `${org.toLowerCase()}/${slug.toLowerCase()}`

interface Props {
  /**
   * How checkboxes start out. Local mode (acting as oneself) pre-checks
   * every team — the user almost certainly wants them all. Multi-mode
   * onboarding (configuring on behalf of an org) starts unchecked so each
   * team is an explicit opt-in.
   */
  defaultChecked: boolean
  /** Called with the checked teams when the user clicks Continue. */
  onContinue: (teams: GitHubUserTeam[]) => void
  /** Advance without writing any mappings. */
  onSkip: () => void
  /** Go back to the previous step. */
  onBack: () => void
  /** True while the parent's onContinue (the github-groups PUT) is in
   *  flight — disables the footer and swaps in a spinner. */
  saving?: boolean
}

// GitHubTeamSelector is the onboarding step that makes the GitHub-team →
// TF-team mapping exist *before* the first poll, so a user who is only
// review-requested via a team isn't left with an empty queue (SKY-411).
// Mirrors RepoPickerModal's shape: search box, scrollable list, footer
// with Back / Skip / Continue, and a `saving` spinner.
export default function GitHubTeamSelector({
  defaultChecked,
  onContinue,
  onSkip,
  onBack,
  saving = false,
}: Props) {
  const [teams, setTeams] = useState<GitHubUserTeam[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')
  const [checked, setChecked] = useState<Set<string>>(new Set())
  // Once the user toggles anything, their selection is sacrosanct — the
  // mode-driven seeding effect below must never overwrite it. This matters
  // because defaultChecked flips when /api/config resolves, which can land
  // *after* the teams fetch and after the user has started clicking.
  const userTouched = useRef(false)

  // Fetch the team list exactly once. Deliberately independent of
  // defaultChecked: seeding the checkboxes is a separate concern (the
  // effect below), so a late /api/config resolution neither refetches nor
  // clobbers in-progress selections.
  const fetchTeams = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const res = await fetch('/api/github/user-teams')
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        console.error('Failed to fetch teams:', data.error || `HTTP ${res.status}`)
        setError('Failed to fetch your GitHub teams')
        return
      }
      const data: GitHubUserTeam[] = await res.json()
      setTeams(data)
    } catch (err) {
      console.error('Failed to fetch teams:', err)
      setError('Failed to fetch your GitHub teams')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    fetchTeams()
  }, [fetchTeams])

  // Seed the checkbox state from the mode-driven default whenever the
  // teams arrive or the (possibly late-resolving) default changes — but
  // only until the user has touched the selection, after which it's
  // hands-off.
  useEffect(() => {
    if (userTouched.current) return
    setChecked(
      defaultChecked ? new Set(teams.map((t) => keyOf(t.org_slug, t.team_slug))) : new Set(),
    )
  }, [teams, defaultChecked])

  const filtered = useMemo(() => {
    if (!search.trim()) return teams
    const q = search.toLowerCase()
    return teams.filter(
      (t) => keyOf(t.org_slug, t.team_slug).includes(q) || (t.name || '').toLowerCase().includes(q),
    )
  }, [teams, search])

  const toggle = (org: string, slug: string) => {
    userTouched.current = true
    const k = keyOf(org, slug)
    setChecked((prev) => {
      const next = new Set(prev)
      if (next.has(k)) next.delete(k)
      else next.add(k)
      return next
    })
  }

  const handleContinue = () => {
    const selected = teams.filter((t) => checked.has(keyOf(t.org_slug, t.team_slug)))
    onContinue(selected)
  }

  // Once the list has loaded and is empty, the user has no GitHub teams —
  // Skip is the only forward affordance, Continue is meaningless.
  const noTeams = !loading && !error && teams.length === 0

  return (
    <div className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.04] overflow-hidden">
      <div className="flex flex-col h-full max-h-[80vh]">
        {/* Header */}
        <div className="px-6 pt-6 pb-4">
          <h2 className="text-[18px] font-semibold text-text-primary tracking-tight">
            Your GitHub teams
          </h2>
          <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
            Triage Factory routes PRs that request these teams to you, in addition to ones that
            request you individually.
          </p>
        </div>

        {/* Search — hidden in the empty/error states where there's nothing to filter */}
        {!noTeams && !error && (
          <div className="px-6 pb-3">
            <input
              type="text"
              placeholder="Search teams..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors"
            />
          </div>
        )}

        {/* List */}
        <div className="flex-1 overflow-y-auto px-6 min-h-0">
          {loading && (
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
          )}

          {error && (
            <div className="flex flex-col items-center justify-center py-12 gap-3">
              <div className="text-[13px] text-text-secondary text-center">{error}</div>
              <button
                type="button"
                onClick={fetchTeams}
                className="flex items-center gap-1.5 text-[12px] font-medium text-accent hover:text-accent/80 transition-colors"
              >
                <RotateCw size={13} />
                Retry
              </button>
            </div>
          )}

          {noTeams && (
            <p className="text-[13px] text-text-tertiary text-center py-8 leading-relaxed">
              You're not on any GitHub teams yet. Skip this step — you can add team mappings later
              in Settings.
            </p>
          )}

          {!loading &&
            !error &&
            filtered.map((team) => {
              const k = keyOf(team.org_slug, team.team_slug)
              const isChecked = checked.has(k)
              const noisy = (team.member_count ?? 0) > NOISY_TEAM_THRESHOLD
              return (
                <button
                  key={k}
                  type="button"
                  onClick={() => toggle(team.org_slug, team.team_slug)}
                  className={`w-full flex items-start gap-3 px-3 py-2.5 text-left rounded-xl transition-colors hover:bg-black/[0.02] ${
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
                        {team.org_slug}/{team.team_slug}
                      </span>
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
                    </div>
                    {team.name && team.name.toLowerCase() !== team.team_slug.toLowerCase() && (
                      <p className="text-[11px] text-text-tertiary truncate mt-0.5">{team.name}</p>
                    )}
                  </div>
                </button>
              )
            })}
        </div>

        {/* Hint */}
        {!loading && !error && teams.length > 0 && (
          <div className="px-6 pt-3">
            <p className="text-[11px] text-text-tertiary leading-relaxed">
              Skip teams with many members — broad teams produce noisy queues. You can change this
              later in Settings.
            </p>
          </div>
        )}

        {/* Footer */}
        <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between gap-3">
          <button
            type="button"
            onClick={onBack}
            disabled={saving}
            className="text-[13px] text-text-secondary hover:text-text-primary bg-white/50 hover:bg-white/80 disabled:opacity-40 border border-border-subtle rounded-xl px-4 py-2 transition-colors"
          >
            Back
          </button>
          <div className="flex gap-3">
            <button
              type="button"
              onClick={onSkip}
              disabled={saving}
              className="text-[13px] text-text-secondary hover:text-text-primary bg-white/50 hover:bg-white/80 disabled:opacity-40 border border-border-subtle rounded-xl px-4 py-2 transition-colors"
            >
              Skip
            </button>
            <button
              type="button"
              onClick={handleContinue}
              disabled={noTeams || loading || !!error || saving}
              className="flex items-center gap-1.5 bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-5 py-2 text-[13px] transition-colors"
            >
              {saving && <RotateCw size={13} className="animate-spin" />}
              {saving ? 'Saving…' : 'Continue'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
