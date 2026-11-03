import { apiJSON } from './apiClient'

// Shared types, key helpers and the list read for the GitHub-team → TF-team
// mapping surfaces (the onboarding wizard and the Settings editor, which
// render the same GitHubTeamChecklist body). These live here rather than in
// the component file so the component exports only a component —
// react-refresh's only-export-components rule flags a value export sitting
// beside one.

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
// team. `member_count` drives the "broad team — noisy queue" hint
// (best-effort; absent/0 = unknown); `mine` is true when the acting admin
// personally belongs to the team and is only populated when the list is asked
// for membership (the onboarding wizard, to pre-check the admin's own teams).
// The Settings editor does not ask, so the "your team" badge never shows there.
export interface GitHubTeamCandidate extends GitHubTeamRef {
  name: string
  member_count?: number
  mine?: boolean
}

// One row of POST /api/teams/{id}/github-groups/list: a GitHub team the team
// can map. `listed` is false for a team it maps that the org's GitHub
// credentials did not list — deleted since the last poll, or in an
// organization whose teams could not be read — which carries no name.
export interface GitHubTeamOption extends GitHubTeamCandidate {
  listed: boolean
}

// One page of the mappable GitHub teams. `credentials_missing` is true when
// the team tracks repositories and no credential the org has connected reaches
// any of their owners, so an empty list means "reconnect GitHub" rather than
// "these organizations have no teams". It describes the whole set, not the
// page.
export interface GitHubTeamPage {
  items: GitHubTeamOption[]
  next_page_token: string
  total_count: number
  credentials_missing: boolean
}

export interface GitHubTeamListRequest {
  /** Matched server-side against `org_login/team_slug` and the name. */
  search?: string
  /** Mark the teams the caller belongs to (`mine`). */
  includeMembership?: boolean
  /** Only the teams the caller belongs to, marked `mine`. */
  onlyMine?: boolean
  pageSize?: number
  /** A token the server minted under the same `search` and `onlyMine`. */
  pageToken?: string
}

// listGitHubTeams reads one page of the GitHub teams a team can map, searched
// and ordered by the server (folded org_login/team_slug). It goes through
// apiJSON rather than apiList because `credentials_missing` rides beside the
// list envelope, which apiList normalizes away.
export async function listGitHubTeams(
  teamId: string,
  req: GitHubTeamListRequest = {},
): Promise<GitHubTeamPage> {
  const path = `/api/teams/${encodeURIComponent(teamId)}/github-groups/list`
  const page = await apiJSON<Partial<GitHubTeamPage>>(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      ...(req.search ? { search: req.search } : {}),
      ...(req.includeMembership ? { include_membership: true } : {}),
      ...(req.onlyMine ? { only_mine: true } : {}),
      ...(req.pageSize !== undefined ? { page_size: req.pageSize } : {}),
      ...(req.pageToken ? { page_token: req.pageToken } : {}),
    }),
  })
  return {
    items: page.items ?? [],
    next_page_token: page.next_page_token ?? '',
    total_count: page.total_count ?? 0,
    credentials_missing: page.credentials_missing === true,
  }
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
