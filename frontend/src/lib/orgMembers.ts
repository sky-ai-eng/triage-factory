import { apiList } from './apiClient'
import type { ApiFetchOptions, ListPage } from './apiClient'

/** One row of `POST /api/orgs/{org_id}/members/list`. */
export interface OrgMember {
  user_id: string
  display_name: string
  github_username: string | null
  jira_account_id: string | null
  linear_user_id: string | null
  role: string
  is_current_user: boolean
}

/** What an org roster read asks for. */
export interface OrgMemberQuery {
  /** The route's `search`: a substring of a member's display name or of one
   *  of their verified email addresses. Blank is no search. */
  search?: string
  /** The route's `excluding_team_id`: only the members not on this team, the
   *  people it could add. */
  excludingTeamId?: string
  /** Rows to read; the route's default when absent. */
  pageSize?: number
}

/** listOrgMembers reads one page of an org's members, narrowed on the server.
 *  The rows come in the route's order, display name then id. */
export function listOrgMembers(
  orgId: string,
  query: OrgMemberQuery = {},
  options: ApiFetchOptions = {},
): Promise<ListPage<OrgMember>> {
  const search = query.search?.trim() ?? ''
  return apiList<OrgMember>(
    `/api/orgs/${encodeURIComponent(orgId)}/members/list`,
    {
      ...(query.pageSize !== undefined ? { page_size: query.pageSize } : {}),
      ...(search ? { search } : {}),
      ...(query.excludingTeamId ? { excluding_team_id: query.excludingTeamId } : {}),
    },
    options,
  )
}
