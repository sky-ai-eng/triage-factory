import { useState, useEffect, useMemo, useCallback, useRef } from 'react'
import { RotateCw } from 'lucide-react'

// One live GitHub team the admin can assign to this Triage Factory team —
// the candidate shape from GET .../github-groups. member_count drives the
// "broad team — noisy queue" hint (best-effort; absent/0 = unknown); `mine`
// is true when the acting admin personally belongs to the team (so the
// wizard pre-checks it).
interface GitHubTeamCandidate {
  org_login: string
  team_slug: string
  name: string
  member_count?: number
  mine?: boolean
}

export type { GitHubTeamCandidate }

// The github-groups GET response we consume. `groups` is any already-saved
// mapping (re-entry / re-onboarding); `candidates` is the org-wide team list.
interface GroupsResponse {
  groups: { org_login: string; team_slug: string }[]
  candidates: GitHubTeamCandidate[]
  role: string
}

// Teams larger than this get a "broad team — noisy queue" hint. Fixed, not
// configurable: it's a nudge, not a policy (SKY-411 open-question call).
const NOISY_TEAM_THRESHOLD = 20

const keyOf = (org: string, slug: string) => `${org.toLowerCase()}/${slug.toLowerCase()}`

interface Props {
  /** Team path segment for the github-groups GET/PUT — a UUID or the
   *  literal "default" (matches the sibling team-settings routes).
   *  Onboarding configures the org's default team. */
  teamId: string
  /** Called with the checked teams when the user clicks Continue. */
  onContinue: (teams: GitHubTeamCandidate[]) => void
  /** Advance without writing any mappings. */
  onSkip: () => void
  /** Go back to the previous step. */
  onBack: () => void
  /** True while the parent's onContinue (the github-groups PUT) is in
   *  flight — disables the footer and swaps in a spinner. */
  saving?: boolean
}

// GitHubTeamSelector is the onboarding step that creates the GitHub-team →
// TF-team review-request mapping before the first poll, so a user who is
// only review-requested via a team isn't left with an empty queue (SKY-411).
// Mirrors RepoPickerModal's shape: search box, scrollable list, footer with
// Back / Skip / Continue, and a `saving` spinner.
//
// Candidate-source invariant (SKY-413). The rows come from the SAME org-wide
// source as the Settings editor — GET /api/settings/team/{id}/github-groups,
// which lists every team in the configured repos' GitHub orgs, NOT the
// caller's personal memberships. The only onboarding-specific behavior is
// pre-checking: with ?include_membership=true the server flags the teams the
// acting admin personally belongs to (`mine`), and those start checked. The
// candidate SET is therefore perspective-independent — two surfaces (this
// wizard and the Settings editor) share one list and one write target (the
// github-groups replace-set PUT), differing only in what starts checked.
// This is deliberately unlike the original SKY-411 shape, where the wizard
// sourced candidates from one user's `/user/teams`: mounting this elsewhere
// no longer risks scoping a team's candidates from a single admin's view —
// only the default check state is personal, and an over/under-checked
// default is a mild, user-correctable nudge, not a scope leak.
export default function GitHubTeamSelector({
  teamId,
  onContinue,
  onSkip,
  onBack,
  saving = false,
}: Props) {
  const [candidates, setCandidates] = useState<GitHubTeamCandidate[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [search, setSearch] = useState('')
  const [checked, setChecked] = useState<Set<string>>(new Set())
  // Once the user toggles anything, their selection is sacrosanct — the
  // seed below (which runs when the fetch lands) must never overwrite it.
  const userTouched = useRef(false)

  const fetchTeams = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const url = `/api/settings/team/${encodeURIComponent(teamId)}/github-groups?include_membership=true`
      const res = await fetch(url)
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        console.error('Failed to fetch teams:', data.error || `HTTP ${res.status}`)
        setError('Failed to fetch GitHub teams')
        return
      }
      const data: GroupsResponse = await res.json()
      const list = data.candidates ?? []
      setCandidates(list)
      // Seed the checkboxes once, unless the user has already started
      // clicking. A prior saved set (re-entry / re-onboarding) is their
      // explicit choice and wins; otherwise pre-check the admin's own
      // teams (`mine`) — the deliberate default in both deployment modes.
      if (!userTouched.current) {
        const saved = (data.groups ?? []).map((g) => keyOf(g.org_login, g.team_slug))
        const seed =
          saved.length > 0
            ? saved
            : list.filter((t) => t.mine).map((t) => keyOf(t.org_login, t.team_slug))
        setChecked(new Set(seed))
      }
    } catch (err) {
      console.error('Failed to fetch teams:', err)
      setError('Failed to fetch GitHub teams')
    } finally {
      setLoading(false)
    }
  }, [teamId])

  useEffect(() => {
    fetchTeams()
  }, [fetchTeams])

  const filtered = useMemo(() => {
    if (!search.trim()) return candidates
    const q = search.toLowerCase()
    return candidates.filter(
      (t) =>
        keyOf(t.org_login, t.team_slug).includes(q) || (t.name || '').toLowerCase().includes(q),
    )
  }, [candidates, search])

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
    const selected = candidates.filter((t) => checked.has(keyOf(t.org_login, t.team_slug)))
    onContinue(selected)
  }

  // Once the list has loaded and is empty, there are no teams to map —
  // Skip is the only forward affordance, Continue is meaningless.
  const noTeams = !loading && !error && candidates.length === 0

  return (
    <div className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.04] overflow-hidden">
      <div className="flex flex-col h-full max-h-[80vh]">
        {/* Header */}
        <div className="px-6 pt-6 pb-4">
          <h2 className="text-[18px] font-semibold text-text-primary tracking-tight">
            GitHub teams for this Triage Factory team
          </h2>
          <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
            Triage Factory routes PRs that request these GitHub teams to your Triage Factory team.
            PRs that request you individually are routed separately via your GitHub identity.
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
              No GitHub teams found for your configured repositories' organizations. Skip this step
              — you can add team mappings later in Settings.
            </p>
          )}

          {!loading &&
            !error &&
            filtered.map((team) => {
              const k = keyOf(team.org_login, team.team_slug)
              const isChecked = checked.has(k)
              const noisy = (team.member_count ?? 0) > NOISY_TEAM_THRESHOLD
              return (
                <button
                  key={k}
                  type="button"
                  onClick={() => toggle(team.org_login, team.team_slug)}
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
        {!loading && !error && candidates.length > 0 && (
          <div className="px-6 pt-3">
            <p className="text-[11px] text-text-tertiary leading-relaxed">
              Broad teams produce noisy queues. Skip those here; you can add them later in Settings
              if you want them after all.
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
