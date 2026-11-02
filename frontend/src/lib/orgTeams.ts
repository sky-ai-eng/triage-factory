import { apiList } from './apiClient'
import type { TeamSummary } from '../types'

// Every active team in an org, for any of its members: the names an org-wide
// view needs for the teams the viewer's own list (useTeams) leaves out. Each
// row carries the viewer's role on the teams they are on, none on the rest.

/** Every active team in the org, walked across pages. */
export async function listOrgTeams(orgID: string): Promise<TeamSummary[]> {
  const path = `/api/orgs/${encodeURIComponent(orgID)}/teams/list`
  const out: TeamSummary[] = []
  let token = ''
  do {
    const page = await apiList<TeamSummary>(path, {
      page_size: 200,
      ...(token ? { page_token: token } : {}),
    })
    out.push(...page.items)
    token = page.next_page_token
  } while (token)
  return out
}
