import { useCallback, useEffect, useMemo, useState } from 'react'
import { Bot } from 'lucide-react'
import { apiFetch, apiJSON } from '../lib/apiClient'
import MemberRoster from './MemberRoster'
import TeamMemberPicker from './TeamMemberPicker'
import type { MemberRosterAdapter, RosterMember } from '../hooks/useMemberRoster'

// TeamMembersPanel is the reusable team-roster surface (TFAC-444): the shared
// <MemberRoster> driven by a team adapter, plus the team_agents bot row and the
// org-member add-picker. Its permanent home is the /team page shell (TFAC-445);
// this component is what that shell will drop into its Members tab. The
// temporary TeamPage mount renders it for the caller's default team.
interface TeamMembersPanelProps {
  // orgId scopes the add-picker's org-member source (the team endpoints
  // themselves are not org-prefixed).
  orgId: string
  teamId: string
  // canManage gates the role <select> + Remove + the add-picker. A non-admin
  // member sees a read-only roster but can still Leave their own row.
  canManage: boolean
}

// Wire shapes of GET /api/teams/{team_id}/members.
interface TeamRosterApiRow {
  user_id: string
  display_name: string
  github_username: string | null
  jira_account_id: string | null
  role: string
  is_current_user: boolean
}

interface TeamRosterBot {
  agent_id: string
  display_name: string
  enabled: boolean
  model: string
  autonomy: number | null
}

interface TeamRosterApiResponse {
  members: TeamRosterApiRow[]
  bot: TeamRosterBot | null
}

const TEAM_ROLES = ['admin', 'member', 'viewer']
// viewer is assignable now; its read-only enforcement is a later slice
// (TFAC-447), so flag that in the role <select> — until then a viewer behaves
// like a member.
const TEAM_ROLE_LABELS: Record<string, string> = { viewer: 'viewer (read-only soon)' }

export default function TeamMembersPanel({ orgId, teamId, canManage }: TeamMembersPanelProps) {
  // reloadKey remounts <MemberRoster> (re-running useMemberRoster's mount
  // fetch) after the add-picker enrolls a member — the roster owns its own
  // member-list lifecycle, so a remount is the lightest way to pull the new
  // row in without lifting the hook out of the shared component.
  const [reloadKey, setReloadKey] = useState(0)
  const [bot, setBot] = useState<TeamRosterBot | null>(null)

  // The bot row is fetched alongside the roster but lives outside the member
  // list, so we read it directly. Member mutations don't change the bot, so
  // this only re-runs on a team switch.
  useEffect(() => {
    let alive = true
    apiJSON<TeamRosterApiResponse>(`/api/teams/${teamId}/members`)
      .then((d) => {
        if (alive) setBot(d.bot)
      })
      .catch(() => {
        if (alive) setBot(null)
      })
    return () => {
      alive = false
    }
  }, [teamId])

  // Base adapter — referentially stable except when the team changes (the only
  // thing the I/O closures depend on). addAffordance/extraRows are spread on
  // top (a fresh object each render) so their per-render JSX identity can't
  // trigger a refetch loop: useMemberRoster keys its load effect on
  // fetchMembers, which stays stable here.
  const base = useMemo<MemberRosterAdapter>(
    () => ({
      roles: TEAM_ROLES,
      // Teams guard their last admin (no ownership-transfer concept), so the
      // protected role is 'admin' and transferOwnership is left undefined.
      protectedRole: 'admin',
      roleLabels: TEAM_ROLE_LABELS,
      async fetchMembers(): Promise<RosterMember[]> {
        const data = await apiJSON<TeamRosterApiResponse>(`/api/teams/${teamId}/members`)
        return data.members.map((m) => ({
          userId: m.user_id,
          displayName: m.display_name,
          githubUsername: m.github_username,
          jiraAccountId: m.jira_account_id,
          role: m.role,
          isCurrentUser: m.is_current_user,
        }))
      },
      async changeRole(userId, role) {
        // 204 (no body) — apiFetch (not apiJSON) so a missing JSON body isn't
        // parsed; it still throws HttpError on the 409 last-admin guard.
        await apiFetch(`/api/teams/${teamId}/members/${userId}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ role }),
        })
      },
      async remove(userId) {
        await apiFetch(`/api/teams/${teamId}/members/${userId}`, { method: 'DELETE' })
      },
    }),
    [teamId],
  )

  const reload = useCallback(() => setReloadKey((k) => k + 1), [])

  const addAffordance = canManage ? (
    <TeamMemberPicker orgId={orgId} teamId={teamId} onAdded={reload} />
  ) : null

  const extraRows = bot ? <TeamBotRow bot={bot} /> : null

  const adapter: MemberRosterAdapter = { ...base, addAffordance, extraRows }

  // key remounts on an add so the new member is fetched in (role-change/remove
  // refetch through the hook's own choreography and need no remount).
  return <MemberRoster key={reloadKey} adapter={adapter} canManage={canManage} />
}

// TeamBotRow renders the team's agent (team_agents) as an extra roster row — a
// workload identity, not a member, so it slots into the <ul> alongside the
// member rows. Read-only here: the enable/model/autonomy controls are the page
// shell's concern; this surfaces the bot's current state.
function TeamBotRow({ bot }: { bot: TeamRosterBot }) {
  return (
    <li className="flex items-center gap-4 bg-accent-soft/20 px-4 py-3">
      <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-accent-soft text-accent">
        <Bot size={16} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-[13px] font-medium text-text-primary">
            {bot.display_name || 'Team bot'}
          </span>
          <span className="rounded-full bg-accent-soft px-2 py-0.5 text-[10px] font-medium text-accent">
            Bot
          </span>
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-[10px] text-text-tertiary">
          <span>{bot.enabled ? 'Enabled' : 'Disabled'}</span>
          {bot.model && <span>· {bot.model}</span>}
          {bot.autonomy != null && <span>· autonomy {bot.autonomy}</span>}
        </div>
      </div>
    </li>
  )
}
