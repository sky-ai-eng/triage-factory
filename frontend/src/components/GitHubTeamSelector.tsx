import { useState, useEffect, useCallback, useRef } from 'react'
import { RotateCw } from 'lucide-react'
import GitHubTeamChecklist from './GitHubTeamChecklist'
import { keyOf, type GitHubTeamCandidate } from '../lib/githubTeams'

// Re-exported for callers that still import the candidate shape from the
// wizard (e.g. Setup's onContinue handler). The canonical definition lives
// in GitHubTeamChecklist, the shared body both surfaces render.
export type { GitHubTeamCandidate }

// The github-groups GET response we consume. `groups` is any already-saved
// mapping (re-entry / re-onboarding); `candidates` is the org-wide team list.
interface GroupsResponse {
  groups: { org_login: string; team_slug: string }[]
  candidates: GitHubTeamCandidate[]
  role: string
}

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
// It is a thin wrapper around the shared GitHubTeamChecklist: this file owns
// only the wizard chrome (header, Back / Skip / Continue footer, the broad-
// team hint) and the onboarding-specific seed; the search box, the
// selected/unselected/all filter, the bounded scrollable list, and the
// badges all live in the shared body (SKY-388).
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
        <div className="px-6 pt-6 pb-4 shrink-0">
          <h2 className="text-[18px] font-semibold text-text-primary tracking-tight">
            GitHub teams for this Triage Factory team
          </h2>
          <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
            Triage Factory routes PRs that request these GitHub teams to your Triage Factory team.
            PRs that request you individually are routed separately via your GitHub identity.
          </p>
        </div>

        {/* Shared body: search + selected/unselected/all filter + bounded
            scrollable list + badges + loading/error/empty states. */}
        <GitHubTeamChecklist
          candidates={candidates}
          selected={checked}
          onToggle={toggle}
          loading={loading}
          error={error || null}
          onRetry={fetchTeams}
          disabled={saving}
          className="flex-1 min-h-0 px-6"
          scrollClassName="flex-1 min-h-0"
          emptyLabel="No GitHub teams found for your configured repositories' organizations. Skip this step — you can add team mappings later in Settings."
        />

        {/* Hint */}
        {!loading && !error && candidates.length > 0 && (
          <div className="px-6 pt-3 shrink-0">
            <p className="text-[11px] text-text-tertiary leading-relaxed">
              Broad teams produce noisy queues. Skip those here; you can add them later in Settings
              if you want them after all.
            </p>
          </div>
        )}

        {/* Footer */}
        <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between gap-3 shrink-0">
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
