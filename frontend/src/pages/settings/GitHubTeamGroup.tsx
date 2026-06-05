import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Section } from './primitives'
import GitHubTeamChecklist from '../../components/GitHubTeamChecklist'
import { apiJSON } from '../../lib/apiClient'
import { keyOf, type GitHubTeamCandidate } from '../../lib/githubTeams'
import type { GitHubGroup, TeamGitHubGroupsData } from './teamConfig'

// keyToGroup rebuilds a stored mapping from its case-folded Set key,
// preferring a live candidate's canonical casing, then an existing selection
// entry, then a raw split — so a toggle never corrupts org/slug casing.
function keyToGroup(
  k: string,
  candidates: GitHubTeamCandidate[],
  current: GitHubGroup[],
): GitHubGroup {
  const cand = candidates.find((c) => keyOf(c.org_login, c.team_slug) === k)
  if (cand) return { org_login: cand.org_login, team_slug: cand.team_slug }
  const cur = current.find((g) => keyOf(g.org_login, g.team_slug) === k)
  if (cur) return cur
  const [org_login, team_slug] = k.split('/')
  return { org_login, team_slug }
}

/**
 * GitHubTeamGroup is the team-scope GitHub-team → TF-team mapping field
 * group: which GitHub teams route their CODEOWNERS review requests to this
 * Triage Factory team. A controlled component — the container owns the
 * selected mappings (`value`) and the PUT /api/settings/team/{id}/github-
 * groups — so the same group serves the team Settings tab, the create-time
 * TeamConfigure step, and the local Setup wizard.
 *
 * It self-fetches the org-wide candidate list (the GET also re-triggers the
 * server's deletion reconcile) and renders the shared GitHubTeamChecklist
 * body — the same search / filter / bounded list / badges both prior mapping
 * surfaces used. On first load it seeds the selection up via onLoaded: a
 * prior saved set wins; otherwise, when includeMembership is set (the
 * onboarding default), the acting admin's own teams (`mine`) start checked.
 */
export default function GitHubTeamGroup({
  value,
  onChange,
  teamId,
  canEdit = true,
  includeMembership = false,
  onLoaded,
}: {
  value: GitHubGroup[]
  onChange: (groups: GitHubGroup[]) => void
  teamId: string
  canEdit?: boolean
  // When true, fetch with ?include_membership=true and (absent a saved set)
  // pre-check the admin's own teams. The Settings editor leaves it false —
  // it shows the saved set, no personal pre-check.
  includeMembership?: boolean
  // Fired once after the first successful load with the seed selection, so
  // the container can seed its form state (and a change-detection baseline).
  onLoaded?: (groups: GitHubGroup[]) => void
}) {
  const [candidates, setCandidates] = useState<GitHubTeamCandidate[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const seededRef = useRef(false)
  // onLoaded is held in a ref so `load` doesn't depend on it: callers pass an
  // inline closure (a new identity each render), and putting it in load's
  // deps would re-fire the fetch effect every render — an infinite loop.
  const onLoadedRef = useRef(onLoaded)
  onLoadedRef.current = onLoaded

  const base = `/api/settings/team/${encodeURIComponent(teamId)}/github-groups`

  const load = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const url = includeMembership ? `${base}?include_membership=true` : base
      const data = await apiJSON<TeamGitHubGroupsData>(url)
      const cands = data.candidates ?? []
      setCandidates(cands)
      // Seed once: the saved set is an explicit prior choice and wins;
      // otherwise pre-check the admin's own teams when membership was asked
      // for (onboarding), else nothing.
      if (!seededRef.current) {
        seededRef.current = true
        const saved = data.groups ?? []
        const seed =
          saved.length > 0
            ? saved
            : includeMembership
              ? cands
                  .filter((c) => c.mine)
                  .map((c) => ({ org_login: c.org_login, team_slug: c.team_slug }))
              : []
        onLoadedRef.current?.(seed)
      }
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : 'Failed to load GitHub teams')
    } finally {
      setLoading(false)
    }
  }, [base, includeMembership])

  useEffect(() => {
    void load()
  }, [load])

  const selected = useMemo(
    () => new Set(value.map((g) => keyOf(g.org_login, g.team_slug))),
    [value],
  )

  const toggle = (org: string, slug: string) => {
    const k = keyOf(org, slug)
    const next = new Set(selected)
    if (next.has(k)) next.delete(k)
    else next.add(k)
    onChange(Array.from(next).map((key) => keyToGroup(key, candidates, value)))
  }

  return (
    <Section>
      <div className="mb-3">
        <h2 className="text-[13px] font-medium text-text-secondary">GitHub teams</h2>
        <p className="text-[12px] text-text-tertiary mt-1 leading-relaxed">
          Assign GitHub teams to this Triage Factory team. When a pull request requests review from
          one of these GitHub teams (via CODEOWNERS), the work routes to this team&rsquo;s board.
          Names only — no membership or nesting is read.
        </p>
      </div>

      <GitHubTeamChecklist
        candidates={candidates}
        selected={selected}
        onToggle={toggle}
        loading={loading}
        error={loadError}
        onRetry={() => void load()}
        disabled={!canEdit}
        emptyLabel="No GitHub teams found. Connect GitHub and configure repositories whose org exposes teams, then reopen this panel."
      />

      {/* Mirror ReposGroup's reassurance: a failed load leaves the selection
          unseeded (undefined), so a save skips it rather than clearing the
          stored mappings. Say so, since the checklist's error alone doesn't. */}
      {loadError && (
        <p className="text-[12px] text-amber-600 italic mt-2">
          Couldn&rsquo;t load GitHub teams — existing mappings will be left unchanged. Retry above
          to edit them.
        </p>
      )}
    </Section>
  )
}
