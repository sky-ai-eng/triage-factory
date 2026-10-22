// Shared types + key helpers for the GitHub-team → TF-team mapping surfaces
// (the onboarding wizard and the Settings editor, which render the same
// GitHubTeamChecklist body). These live here rather than in the component
// file so the component exports only a component — react-refresh's
// only-export-components rule flags a value export sitting beside one.

// GitHubTeamRef names one GitHub team the way a mapping stores it: GitHub's
// ids for the team and its organization, which the PUT names a mapping by and
// routing matches on, and the org login and team slug GitHub gave them, for
// display. Both ids are '' on a stored mapping the server has not identified
// yet (a local one saved before ids were recorded); the PUT cannot name such
// a mapping, so it is kept only by matching it to a candidate (identifyGroups).
export interface GitHubTeamRef {
  github_team_id: string
  github_org_id: string
  org_login: string
  team_slug: string
}

// One live GitHub team candidate the admin can assign to a Triage Factory
// team — the candidate shape from GET .../github-groups. `member_count`
// drives the "broad team — noisy queue" hint (best-effort; absent/0 =
// unknown); `mine` is true when the acting admin personally belongs to the
// team and is only populated when the caller passes ?include_membership=true
// (the onboarding wizard, to pre-check the admin's own teams). The Settings
// editor omits the flag, so the "your team" badge simply never shows there.
export interface GitHubTeamCandidate extends GitHubTeamRef {
  name: string
  member_count?: number
  mine?: boolean
}

// handleOf is a team's handle as GitHub spells it in a mention, org/slug.
export const handleOf = (t: GitHubTeamRef) => `${t.org_login}/${t.team_slug}`

// groupKey is the identity of one GitHub team on both mapping surfaces:
// GitHub's team id. A mapping without one is keyed by its case-folded handle.
export const groupKey = (t: GitHubTeamRef) =>
  t.github_team_id ? `id:${t.github_team_id}` : `handle:${handleOf(t).toLowerCase()}`

// teamRef strips a candidate down to the fields a mapping stores.
export const teamRef = (t: GitHubTeamRef): GitHubTeamRef => ({
  github_team_id: t.github_team_id,
  github_org_id: t.github_org_id,
  org_login: t.org_login,
  team_slug: t.team_slug,
})

// identifyGroups gives each stored mapping that has no id the candidate with
// its handle, so the next save names it by GitHub's id. A mapping no
// candidate matches is returned unchanged, and a save drops it: it names a
// team the org's credentials no longer list.
export function identifyGroups(
  groups: GitHubTeamRef[],
  candidates: GitHubTeamRef[],
): GitHubTeamRef[] {
  return groups.map((g) => {
    if (g.github_team_id) return g
    const handle = handleOf(g).toLowerCase()
    const match = candidates.find((c) => handleOf(c).toLowerCase() === handle)
    return match ? teamRef(match) : g
  })
}
