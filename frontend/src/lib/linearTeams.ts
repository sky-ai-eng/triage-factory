import { HttpError, apiJSON, apiList, apiListAll } from './apiClient'

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

/** How many rows one page asks for: Linear's own page cap, which the server
 *  serves at most anyway. A workspace with more teams than this is reached
 *  through the search box, which filters server-side. */
export const LINEAR_TEAM_PAGE_SIZE = 50

export interface LinearTeamPage {
  items: LinearTeamCandidate[]
  /** True when Linear had teams this page didn't carry. There is no count to
   *  show beside it: the list is proxied live and Linear reports no total. */
  hasMore: boolean
}

/** listLinearTeams reads one page of the teams the org's Linear credential can
 *  see, `search` matched server-side against key and name. */
export async function listLinearTeams(
  orgId: string,
  search: string,
  options: { signal?: AbortSignal } = {},
): Promise<LinearTeamPage> {
  const page = await apiList<LinearTeamCandidate>(
    `/api/orgs/${encodeURIComponent(orgId)}/linear/teams/list`,
    { search, page_size: LINEAR_TEAM_PAGE_SIZE },
    options,
  )
  return { items: page.items, hasMore: page.next_page_token !== '' }
}

/** getLinearTeam reads one team the org's Linear credential can see, by id;
 *  null when the read fails, which includes a team Linear no longer answers
 *  for. */
export async function getLinearTeam(
  orgId: string,
  teamId: string,
): Promise<LinearTeamCandidate | null> {
  return apiJSON<LinearTeamCandidate>(
    `/api/orgs/${encodeURIComponent(orgId)}/linear/teams/${encodeURIComponent(teamId)}`,
  ).catch(() => null)
}

/** lookupLinearTeam reads one team by id and says whether the org's Linear
 *  credential can still see it: the team when it can, 'gone' when the server
 *  answers that Linear has no such team for this credential, and null when the
 *  read fails any other way, which establishes nothing either way. */
export async function lookupLinearTeam(
  orgId: string,
  teamId: string,
): Promise<LinearTeamCandidate | 'gone' | null> {
  try {
    return await apiJSON<LinearTeamCandidate>(
      `/api/orgs/${encodeURIComponent(orgId)}/linear/teams/${encodeURIComponent(teamId)}`,
    )
  } catch (e) {
    return e instanceof HttpError && e.status === 404 ? 'gone' : null
  }
}

/** listLinearStates reads one team's whole workflow, in the board order the
 *  server serves it in. It is walked to the end because each caller reasons
 *  over the whole set: the rules board finds the states no rule maps, and the
 *  predicate field finds the stored ids the workflow no longer offers. */
export async function listLinearStates(
  orgId: string,
  teamId: string,
  options: { signal?: AbortSignal } = {},
): Promise<LinearStateOption[]> {
  return apiListAll<LinearStateOption>(
    `/api/orgs/${encodeURIComponent(orgId)}/linear/teams/${encodeURIComponent(teamId)}/states/list`,
    {},
    options,
  )
}

/** One label an issue in a Linear team can carry, as the labels read serves
 *  it: its id, which a predicate names it by, its name, the group it sits in
 *  (null for none), and whether it is itself a group, which an issue cannot
 *  carry and a group filter names. */
export interface LinearLabelOption {
  id: string
  name: string
  group: { id: string; name: string } | null
  is_group: boolean
}

/** listLinearLabels reads the labels of one team — the team's own and the
 *  workspace's — ordered by name: the groups when `isGroup` is true, else the
 *  labels an issue can carry. The server applies the narrowing, since whether
 *  a label is a group is known only from the whole list. It is walked to the
 *  end because the predicate field finds the stored ids the team no longer
 *  offers. */
export async function listLinearLabels(
  orgId: string,
  teamId: string,
  isGroup: boolean,
  options: { signal?: AbortSignal } = {},
): Promise<LinearLabelOption[]> {
  return apiListAll<LinearLabelOption>(
    `/api/orgs/${encodeURIComponent(orgId)}/linear/teams/${encodeURIComponent(teamId)}/labels/list`,
    { is_group: isGroup },
    options,
  )
}
