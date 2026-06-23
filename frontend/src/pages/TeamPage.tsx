import { useCallback, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Users } from 'lucide-react'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import { useTeams } from '../hooks/useTeams'
import TeamMembersPanel from '../components/TeamMembersPanel'
import TeamSettings from './settings/stack/TeamSettings'
import PromptsWorkspace from '../components/PromptsWorkspace'
import TeamSwitch from '../components/TeamSwitch'
import ZeroTeamState from '../components/ZeroTeamState'
import type { TeamSummary } from '../types'

// TeamPage is the multi-mode team surface (TFAC-445): the
// [Members · Settings · Prompts] shell with one shared team-switcher in the
// header. Members hosts the shared roster (TFAC-444); Settings hosts the
// team-scoped config relocated off the global Settings page; Prompts hosts the
// binding-graph canvas (also reachable one-click via the top-level Prompts nav,
// which deep-links to ?tab=prompts). Multi-mode only — mounted under
// /orgs/:org_id; local N=1 keeps team config on the global Settings page.
//
// The first-class zero-team state (a user in the org but on no team — produced
// today by team-less invites) renders a friendly empty landing here instead of
// a crash or an empty dropdown.

type TeamTab = 'members' | 'settings' | 'prompts'

const TABS: { id: TeamTab; label: string }[] = [
  { id: 'members', label: 'Members' },
  { id: 'settings', label: 'Settings' },
  { id: 'prompts', label: 'Prompts' },
]

// Device-local sticky team for the /team page (its own pageKey, independent of
// the per-page read/write scopes). Persisted so the switcher's selection
// survives reloads.
const ACTIVE_TEAM_KEY = 'tf.activeTeam.team'

function readStoredTeam(): string {
  try {
    return localStorage.getItem(ACTIVE_TEAM_KEY) ?? ''
  } catch {
    return ''
  }
}

export default function TeamPage() {
  const orgId = useActiveOrgId()
  const { isAdmin: orgIsAdmin } = useOrgRole()
  const { teams, lastActingTeamId, loaded, loading, error } = useTeams()

  if (!orgId || loading || !loaded) {
    return <p className="mx-auto max-w-3xl text-[13px] text-text-tertiary">Loading teams…</p>
  }
  if (error) {
    return <p className="mx-auto max-w-3xl text-[13px] text-dismiss">{error}</p>
  }
  // Zero-team safe landing — first-class, not an error. Reachable today via
  // team-less org invites; the archive slice produces it too.
  if (teams.length === 0) {
    return <ZeroTeamState canCreate={orgIsAdmin} />
  }

  return (
    <TeamPageBody
      teams={teams}
      lastActingTeamId={lastActingTeamId}
      orgId={orgId}
      orgIsAdmin={orgIsAdmin}
    />
  )
}

function TeamPageBody({
  teams,
  lastActingTeamId,
  orgId,
  orgIsAdmin,
}: {
  teams: TeamSummary[]
  lastActingTeamId: string
  orgId: string
  orgIsAdmin: boolean
}) {
  const [picked, setPicked] = useState(() => readStoredTeam())
  // Tracks whether the Settings tab has unsaved edits, so a team switch can
  // confirm-before-discard (the switch fires from the page header, outside the
  // tab body). Reset by TeamSettings on unmount.
  const [settingsDirty, setSettingsDirty] = useState(false)

  // Resolve the active team: a valid sticky pick → the server-side sticky
  // default (last-acting team) → the org's default (teams[0], oldest-first).
  // teams is non-empty here, so this always lands on a concrete team.
  const teamId = useMemo(() => {
    if (picked && teams.some((t) => t.id === picked)) return picked
    if (lastActingTeamId && teams.some((t) => t.id === lastActingTeamId)) return lastActingTeamId
    return teams[0].id
  }, [picked, teams, lastActingTeamId])

  const team = teams.find((t) => t.id === teamId)!
  // canManage: a team admin OR an org admin — the union the backend gate
  // enforces. Gates the Settings tab + the roster's management controls.
  const canManage = team.role === 'admin' || orgIsAdmin

  const switchTeam = useCallback(
    (id: string) => {
      if (id === teamId) return
      if (settingsDirty && !window.confirm('Discard unsaved changes and switch teams?')) return
      setPicked(id)
      try {
        localStorage.setItem(ACTIVE_TEAM_KEY, id)
      } catch {
        // localStorage unavailable — the selection still works this session.
      }
    },
    [teamId, settingsDirty],
  )

  const [searchParams, setSearchParams] = useSearchParams()
  const tab = resolveTab(searchParams.get('tab'), canManage)
  // Members owns the bare URL (drop the param); the others carry an explicit
  // ?tab=. replace so flipping tabs doesn't pile up history entries.
  const setTab = (t: TeamTab) =>
    setSearchParams(t === 'members' ? {} : { tab: t }, { replace: true })

  // Settings is gated to managers (team-admin or org-admin); members + prompts
  // are visible to every team member (writes are gated server-side).
  const tabs = canManage ? TABS : TABS.filter((t) => t.id !== 'settings')

  const promptsTab = tab === 'prompts'

  return (
    // Prompts is a full binding-graph canvas: give it a viewport-tall flex
    // column (nav + page padding ≈ 8rem) so the embedded editor fills the space
    // below the header + tab strip. Members + Settings stay a narrow column.
    <div
      className={`mx-auto ${
        promptsTab ? 'flex h-[calc(100vh-8rem)] max-w-6xl flex-col' : 'max-w-3xl'
      }`}
    >
      <div className="mb-5 flex shrink-0 items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent-soft text-accent">
            <Users size={15} />
          </span>
          <div>
            <h1 className="text-[17px] font-semibold leading-tight text-text-primary">
              {team.name}
            </h1>
            <p className="text-[11px] leading-tight text-text-tertiary">
              Members, settings, and prompts.
            </p>
          </div>
        </div>
        {/* Shared page-level switcher — renders for ≥2 teams. */}
        <TeamSwitch teams={teams} value={teamId} onChange={switchTeam} />
      </div>

      <div className="mb-5 flex shrink-0 gap-1 border-b border-border-subtle">
        {tabs.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => setTab(t.id)}
            className={`-mb-px border-b-2 px-3 py-2 text-[13px] font-medium transition-colors ${
              tab === t.id
                ? 'border-accent text-accent'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {/* key remounts each tab body on a team switch — fresh roster/config/graph. */}
      {tab === 'members' && (
        <TeamMembersPanel key={teamId} orgId={orgId} teamId={teamId} canManage={canManage} />
      )}
      {tab === 'settings' && (
        <TeamSettings
          key={teamId}
          isLocal={false}
          teamId={teamId}
          onDirtyChange={setSettingsDirty}
        />
      )}
      {promptsTab && (
        <div className="min-h-0 flex-1">
          <PromptsWorkspace key={teamId} teamId={teamId} ready={teamId !== ''} />
        </div>
      )}
    </div>
  )
}

// resolveTab maps the ?tab= param to a concrete tab, flooring Settings to
// Members for non-managers so a stale/hand-typed ?tab=settings can't surface a
// tab the viewer can't use. Members is the default.
function resolveTab(raw: string | null, canManage: boolean): TeamTab {
  if (raw === 'settings' && canManage) return 'settings'
  if (raw === 'prompts') return 'prompts'
  return 'members'
}
