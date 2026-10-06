import { apiJSON, apiList } from './apiClient'

/** One option in the Linear team picker. The id is what the rules are keyed
 *  on and what the save sends; key and name are what a person recognizes the
 *  team by. A private team is visible to its members only. */
export interface LinearTeamCandidate {
  id: string
  key: string
  name: string
  private: boolean
}

/** One workflow state as the rules store it: the id is the identity, and the
 *  name and type are what Linear gave for it at the last save. The type is
 *  Linear's own vocabulary — triage, backlog, unstarted, started, completed or
 *  canceled — and is what a team's rules are pre-filled from. */
export interface LinearStateRef {
  id: string
  name: string
  type: string
}

/** A workflow state as the states read serves it: a ref plus its position on
 *  the team's board. */
export interface LinearStateOption extends LinearStateRef {
  position: number
}

/** How many candidates one page asks for: Linear's own page cap, which the
 *  server serves at most anyway. A workspace with more teams than this is
 *  reached through the search box, which filters server-side. */
export const LINEAR_TEAM_PAGE_SIZE = 50

export interface LinearTeamPage {
  items: LinearTeamCandidate[]
  /** True when Linear had teams this page didn't carry. There is no count to
   *  show beside it: the list is proxied live and Linear reports no total. */
  hasMore: boolean
}

/** listLinearTeams reads one page of the teams this workspace's Linear
 *  credential can see, `q` matched server-side against key and name. */
export async function listLinearTeams(
  q: string,
  options: { signal?: AbortSignal } = {},
): Promise<LinearTeamPage> {
  const page = await apiList<LinearTeamCandidate>(
    '/api/linear/teams/list',
    { q, page_size: LINEAR_TEAM_PAGE_SIZE },
    options,
  )
  return { items: page.items, hasMore: page.next_page_token !== '' }
}

/** listLinearStates reads one team's workflow states, in board order. */
export async function listLinearStates(
  teamId: string,
  options: { signal?: AbortSignal } = {},
): Promise<LinearStateOption[]> {
  return apiJSON<LinearStateOption[]>(
    `/api/linear/states?team=${encodeURIComponent(teamId)}`,
    options,
  )
}
