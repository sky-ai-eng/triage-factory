import { useCallback, useMemo, useState } from 'react'
import { Bot } from 'lucide-react'
import { apiFetch } from '../lib/apiClient'
import { fetchTeamRoster } from '../lib/teamRoster'
import type { TeamBot } from '../types'
import { TEAM_ROLE_LABELS } from '../lib/teamRoles'
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

const TEAM_ROLES = ['admin', 'member', 'viewer']

export default function TeamMembersPanel({ orgId, teamId, canManage }: TeamMembersPanelProps) {
  // reloadKey remounts <MemberRoster> (re-running useMemberRoster's mount
  // fetch) after the add-picker enrolls a member — the roster owns its own
  // member-list lifecycle, so a remount is the lightest way to pull the new
  // row in without lifting the hook out of the shared component.
  const [reloadKey, setReloadKey] = useState(0)
  const [bot, setBot] = useState<TeamBot | null>(null)

  // Base adapter — referentially stable except when the team changes (the only
  // thing the I/O closures depend on). The composed adapter is memoized below
  // so the prop stays referentially stable per the MemberRoster contract.
  const base = useMemo<MemberRosterAdapter>(
    () => ({
      roles: TEAM_ROLES,
      // Teams guard their last admin (no ownership-transfer concept), so the
      // protected role is 'admin' and transferOwnership is left undefined.
      protectedRole: 'admin',
      roleLabels: TEAM_ROLE_LABELS,
      async fetchMembers(): Promise<RosterMember[]> {
        const data = await fetchTeamRoster(teamId)
        // The bot is a sibling field of the same roster payload, not a member —
        // capture it here so the panel can render one row for it WITHOUT a
        // second, identical read. useMemberRoster owns the fetch lifecycle
        // (mount + post-mutation reload), so this also keeps the bot row fresh.
        // setBot is a stable setter, so it isn't a dependency of this memo.
        // The RAW bot, not usableBot's: this surface reports the team's bot
        // state, and a disabled bot is a row that says "Disabled", not an
        // absent one.
        setBot(data.bot)
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

  // MemberRoster/useMemberRoster require a referentially stable adapter, so
  // memoize the composed pieces (not just `base`): a fresh object each render
  // would violate that contract and break a future refactor that keys on
  // adapter identity. addAffordance only changes when the picker's inputs do;
  // extraRows only when the bot loads; the spread adapter changes only when one
  // of those (or base) does.
  const addAffordance = useMemo(
    () => (canManage ? <TeamMemberPicker orgId={orgId} teamId={teamId} onAdded={reload} /> : null),
    [canManage, orgId, teamId, reload],
  )

  const extraRows = useMemo(() => (bot ? <TeamBotRow bot={bot} /> : null), [bot])

  const adapter = useMemo<MemberRosterAdapter>(
    () => ({ ...base, addAffordance, extraRows }),
    [base, addAffordance, extraRows],
  )

  // key remounts on an add so the new member is fetched in (role-change/remove
  // refetch through the hook's own choreography and need no remount).
  return <MemberRoster key={reloadKey} adapter={adapter} canManage={canManage} />
}

// TeamBotRow renders the team's agent (team_agents) as an extra roster row — a
// workload identity, not a member, so it slots into the <ul> alongside the
// member rows. Read-only here: the enable/model/autonomy controls are the page
// shell's concern; this surfaces the bot's current state.
function TeamBotRow({ bot }: { bot: TeamBot }) {
  return (
    <li className="flex items-center gap-4 bg-warm-2/20 px-4 py-3">
      <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-warm-2 text-warm">
        <Bot size={16} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-body font-medium text-ink-1">
            {bot.display_name || 'Team bot'}
          </span>
          <span className="rounded-full bg-warm-2 px-2 py-0.5 text-label font-medium text-warm">
            Bot
          </span>
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-label text-ink-3">
          <span>{bot.enabled ? 'Enabled' : 'Disabled'}</span>
          {bot.model && <span>· {bot.model}</span>}
          {bot.autonomy != null && <span>· autonomy {bot.autonomy}</span>}
        </div>
      </div>
    </li>
  )
}
