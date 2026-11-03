import { apiJSON } from './apiClient'
import type { ApiFetchOptions } from './apiClient'
import type { TeamBot, TeamMember } from '../types'

/** The team roster as one payload: a page of members plus the team's bot.
 *
 *  The bot rides beside the page rather than inside it — it is a workload
 *  identity occupying the agent claim slot, not a member, and a list whose last
 *  element is not a member would make `total_count` a lie. */
export interface TeamRoster {
  members: TeamMember[]
  bot: TeamBot | null
  /** The team's member count, which is `members.length` at any size this
   *  surface actually renders (see ROSTER_PAGE_SIZE) — and the honest number
   *  to say "of" against if a bigger team ever pages. */
  totalCount: number
}

/** One page of the roster. The roster is small by construction — a team past
 *  200 people is a different product problem than a longer page — so every
 *  consumer asks for the whole thing at the route's maximum, the way the teams
 *  cache does. */
export const ROSTER_PAGE_SIZE = 200

/** What a roster read asks for beyond the team. */
export interface TeamRosterQuery {
  /** The route's `search`: a substring of a member's display name or of an
   *  identity handle their row carries (GitHub login, Jira account id, Linear
   *  name). Blank is no search. */
  search?: string
  /** Rows to read; ROSTER_PAGE_SIZE when absent. */
  pageSize?: number
}

/** fetchTeamRoster reads a team's members and its bot in one call.
 *
 *  `teamId` is a concrete team, never an implied one: passing it is what keeps
 *  the assignee picker and the page around it naming the SAME team, which is
 *  what a session-scoped roster read could not promise for a user in more than
 *  one team.
 *
 *  The rows come in the server's order, narrowed by `query.search` on the
 *  server: a consumer that offers a search box sends what is typed here rather
 *  than filtering the roster it holds.
 *
 *  It goes through `apiJSON` rather than `apiList` for the same reason
 *  `listReachableRepos` does: the response carries a field beside the standard
 *  envelope, and `apiList` normalizes to the envelope alone. The paging keys are
 *  the standard ones and are normalized here identically. */
export async function fetchTeamRoster(
  teamId: string,
  query: TeamRosterQuery = {},
  options: ApiFetchOptions = {},
): Promise<TeamRoster> {
  const search = query.search?.trim() ?? ''
  const page = await apiJSON<{
    items?: TeamMember[]
    total_count?: number
    bot?: TeamBot | null
  }>(`/api/teams/${encodeURIComponent(teamId)}/members/list`, {
    ...options,
    method: 'POST',
    headers: { ...options.headers, 'Content-Type': 'application/json' },
    body: JSON.stringify({
      page_size: query.pageSize ?? ROSTER_PAGE_SIZE,
      ...(search ? { search } : {}),
    }),
  })
  const members = page.items ?? []
  return {
    members,
    bot: page.bot ?? null,
    totalCount: page.total_count ?? members.length,
  }
}

/** RosterSearch reads the team's members matching `needle` through the
 *  roster's `search`, at most `pageSize` of them, in the server's order. A
 *  picker takes one rather than a team id, so the picker decides how many rows
 *  it shows and its host decides which team and route answer. */
export type RosterSearch = (
  needle: string,
  pageSize: number,
  signal: AbortSignal,
) => Promise<TeamRoster>

/** teamRosterSearch is the RosterSearch for one team's roster route. */
export function teamRosterSearch(teamId: string): RosterSearch {
  return (needle, pageSize, signal) =>
    fetchTeamRoster(teamId, { search: needle, pageSize }, { signal })
}

/** usableBot is the bot a surface may OFFER: the roster's bot when the team has
 *  it enabled, and null otherwise. The delegate handlers re-check the same flag
 *  and refuse a disabled bot, so a picker that skipped this would offer an
 *  option the server would reject. A surface that renders the bot's state
 *  rather than offering it (the management roster) reads `roster.bot` directly
 *  — a disabled bot is something to show, not something to hide. */
export function usableBot(bot: TeamBot | null): TeamBot | null {
  return bot && bot.enabled ? bot : null
}
