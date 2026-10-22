import { useCallback, useEffect, useRef, useState } from 'react'
import { Section } from './primitives'
import GitHubTeamChecklist from '../../components/GitHubTeamChecklist'
import { apiJSON } from '../../lib/apiClient'
import {
  groupKey,
  identifyGroups,
  teamRef,
  type GitHubTeamCandidate,
  type GitHubTeamRef,
} from '../../lib/githubTeams'
import type { GitHubGroup, TeamGitHubGroupsData } from './teamConfig'

/**
 * GitHubTeamGroup is the team-scope GitHub-team → TF-team mapping field
 * group: which GitHub teams route their CODEOWNERS review requests to this
 * Triage Factory team. A controlled component — the container owns the
 * selected mappings (`value`) and the PUT /api/teams/{id}/github-
 * groups — so the same group serves the team Settings tab and the setup
 * wizard's team steps.
 *
 * It self-fetches the org-wide candidate list and renders the shared
 * GitHubTeamChecklist body. On first load it seeds the selection up via
 * onLoaded: a prior saved set wins, with any mapping the server has not
 * identified by GitHub's id yet matched to its candidate (identifyGroups) so
 * a save can name it; otherwise, when includeMembership is set (the
 * onboarding default), the acting admin's own teams (`mine`) start checked.
 */
export default function GitHubTeamGroup({
  value,
  onChange,
  teamId,
  canEdit = true,
  includeMembership = false,
  onLoaded,
  refreshSignal = 0,
  bare = false,
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
  // Bumped by the container to re-fetch the candidate list — e.g. after a
  // Repositories save changed the tracked-owner set — WITHOUT re-seeding the
  // selection. seededRef stays true across the refetch, so onLoaded never
  // fires again and the user's unsaved mapping edits are preserved (a repo
  // save must not flush in-progress GitHub-team edits in a sibling section).
  refreshSignal?: number
  // The setup wizard composes this flush (no Section card, glass search +
  // filter); Settings keeps the carded default.
  bare?: boolean
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

  const base = `/api/teams/${encodeURIComponent(teamId)}/github-groups`

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
            ? identifyGroups(saved, cands)
            : includeMembership
              ? cands.filter((c) => c.mine).map(teamRef)
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

  // Candidate-only refresh: when the container bumps refreshSignal, refetch
  // the candidate list but keep the current selection. load() re-runs with
  // seededRef already true, so its onLoaded seed is skipped — `value` (the
  // unsaved selection) is untouched while candidates refresh. The leading
  // guard skips the mount run, which the effect above already covers.
  const refreshedOnce = useRef(false)
  useEffect(() => {
    if (!refreshedOnce.current) {
      refreshedOnce.current = true
      return
    }
    void load()
  }, [refreshSignal, load])

  const toggle = (team: GitHubTeamRef) => {
    const k = groupKey(team)
    onChange(
      value.some((g) => groupKey(g) === k)
        ? value.filter((g) => groupKey(g) !== k)
        : [...value, teamRef(team)],
    )
  }

  const inner = (
    <>
      <div className={bare ? 'mb-4 space-y-1.5' : 'mb-3'}>
        <h2
          className={
            bare
              ? 'text-[19px] font-medium tracking-tight text-ink-1'
              : 'text-body font-medium text-ink-2'
          }
        >
          GitHub teams
        </h2>
        <p
          className={
            bare
              ? 'text-body leading-relaxed text-ink-3'
              : 'mt-1 text-ui leading-relaxed text-ink-3'
          }
        >
          Assign GitHub teams to this Triage Factory team. When a pull request requests review from
          one of these GitHub teams (via CODEOWNERS), the work routes to this team&rsquo;s board.
          Teams are matched by GitHub&rsquo;s id, so renaming one keeps its mapping. No membership
          or nesting is read.
        </p>
      </div>

      <GitHubTeamChecklist
        candidates={candidates}
        selected={value}
        onToggle={toggle}
        loading={loading}
        error={loadError}
        onRetry={() => void load()}
        disabled={!canEdit}
        bare={bare}
        emptyLabel="No GitHub teams found. Connect GitHub and configure repositories whose org exposes teams, then reopen this panel."
      />

      {/* Mirror ReposGroup's reassurance: a failed load leaves the selection
          unseeded (undefined), so a save skips it rather than clearing the
          stored mappings. Say so, since the checklist's error alone doesn't. */}
      {loadError && (
        <p className="mt-2 text-ui italic text-warm">
          Couldn&rsquo;t load GitHub teams — existing mappings will be left unchanged. Retry above
          to edit them.
        </p>
      )}
    </>
  )

  return bare ? inner : <Section>{inner}</Section>
}
