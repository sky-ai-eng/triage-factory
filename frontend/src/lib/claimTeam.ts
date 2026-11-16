import type { Task, TeamSummary } from '../types'
import { pickerDefault } from '../hooks/useTeams'
import { apiJSON } from './apiClient'

/** What a claim or delegate gesture does about the team the task lands on,
 *  resolved before the write:
 *
 *  - `send`: go on. `teamId` is the team to send, absent when the gesture
 *    sends none and the server resolves the caller's one team.
 *  - `ask`: more than one of the caller's teams can take the task, so the
 *    gesture asks which, offering `candidates` with `preselected` chosen.
 *  - `none`: no team of the caller's can take the task, so nothing is written.
 */
export type ClaimTeamResolution =
  | { kind: 'send'; teamId?: string }
  | { kind: 'ask'; candidates: string[]; preselected: string }
  | { kind: 'none' }

/** What the caller's page knows about their teams. */
export interface ClaimTeamContext {
  /** Whether the caller is on two or more teams. False for a person on one
   *  team and in local mode, where the gesture asks nothing. */
  multi: boolean
  teams: TeamSummary[]
  lastActingTeamId: string
  /** The page's team filter: the team ids its list is narrowed to. */
  teamFilter: string[]
}

/** The toast a gesture shows when no team of the caller's can take the task. */
export const NO_CLAIM_TEAM = 'You have no team this task can land on.'

/** The team a gesture preselects among `candidates`: the page's team filter
 *  when it names exactly one team and that team is a candidate, and otherwise
 *  the write pickers' default over the candidates. */
export function preselectClaimTeam(candidates: string[], ctx: ClaimTeamContext): string {
  if (ctx.teamFilter.length === 1 && candidates.includes(ctx.teamFilter[0])) {
    return ctx.teamFilter[0]
  }
  const offered = ctx.teams.filter((t) => candidates.includes(t.id))
  return pickerDefault(offered, ctx.lastActingTeamId) || candidates[0]
}

/** Resolves the team a claim or a delegate of the task lands on, from the
 *  candidates the task's detail read lists for the caller (`claim_team_ids`).
 *  A read that fails, or answers without the list, resolves to sending no
 *  team: the server then answers the write, and the gesture shows that. */
export async function resolveClaimTeam(
  taskId: string,
  ctx: ClaimTeamContext,
): Promise<ClaimTeamResolution> {
  if (!ctx.multi) return { kind: 'send' }
  let candidates: string[] | undefined
  try {
    const task = await apiJSON<Task>(`/api/tasks/${encodeURIComponent(taskId)}`)
    candidates = task.claim_team_ids
  } catch {
    return { kind: 'send' }
  }
  if (!candidates) return { kind: 'send' }
  if (candidates.length === 0) return { kind: 'none' }
  if (candidates.length === 1) return { kind: 'send', teamId: candidates[0] }
  return { kind: 'ask', candidates, preselected: preselectClaimTeam(candidates, ctx) }
}
