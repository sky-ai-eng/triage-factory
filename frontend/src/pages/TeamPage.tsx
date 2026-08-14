import { useCallback, useEffect, useMemo, useState, type KeyboardEvent } from 'react'
import { useSearchParams } from 'react-router'
import { Users } from 'lucide-react'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useOrgRole } from '../hooks/useOrgRole'
import { useTeams } from '../hooks/useTeams'
import { useEntitlements, FeatureSlack } from '../hooks/useEntitlements'
import TeamMembersPanel from '../components/TeamMembersPanel'
import TeamSettings from './settings/stack/TeamSettings'
import TeamSlackChannelsPanel from '../components/TeamSlackChannelsPanel'
import PromptsWorkspace from '../components/PromptsWorkspace'
import TeamSwitch from '../components/TeamSwitch'
import ZeroTeamState from '../components/ZeroTeamState'
import type { TeamSummary } from '../types'

// TeamPage is the multi-mode team surface (TFAC-445): the
// [Members · Settings · Slack · Prompts] shell with one shared team-switcher
// in the header. Members hosts the shared roster (TFAC-444); Settings hosts
// the team-scoped config relocated off the global Settings page; Slack hosts
// the channel-tracking claim/status UX (TFAC-544) — its own tab rather than a
// Settings subheading, since it's substantial enough (its own fetch, its own
// full-set-replace PUT, a primary-reassignment side action) to earn one, same
// as Members/Prompts; Prompts hosts the binding-graph canvas (also reachable
// one-click via the top-level Prompts nav, which deep-links to
// ?tab=prompts). Multi-mode only — mounted under /orgs/:org_id; local N=1
// keeps team config on the global Settings page (and never licenses the
// `slack` entitlement, so the Slack tab never applies there).
//
// The first-class zero-team state (a user in the org but on no team — produced
// today by team-less invites) renders a friendly empty landing here instead of
// a crash or an empty dropdown.

type TeamTab = 'members' | 'settings' | 'slack' | 'prompts'

const TABS: { id: TeamTab; label: string }[] = [
  { id: 'members', label: 'Members' },
  { id: 'settings', label: 'Settings' },
  { id: 'slack', label: 'Slack' },
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
    return <p className="mx-auto max-w-3xl text-body text-ink-3">Loading teams…</p>
  }
  if (error) {
    return <p className="mx-auto max-w-3xl text-body text-alarm">{error}</p>
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
  const [searchParams, setSearchParams] = useSearchParams()

  // ?team=<id> is a one-shot override — e.g. the marketplace install
  // success link, so "Open prompts workspace" reliably lands on the team
  // that was just installed into rather than whatever this device's sticky
  // pick (below) still points at. Wins over the sticky pick by seeding it
  // directly and persisting to the same localStorage slot, so a reload
  // keeps landing here too. The effect further down strips the param once
  // consumed so it can't re-win over a later manual switch on refresh.
  const [picked, setPicked] = useState(() => {
    const fromParam = searchParams.get('team')
    if (fromParam && teams.some((t) => t.id === fromParam)) {
      try {
        localStorage.setItem(ACTIVE_TEAM_KEY, fromParam)
      } catch {
        // localStorage unavailable — the override still applies this session.
      }
      return fromParam
    }
    return readStoredTeam()
  })
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

  // Slack tab (TFAC-544): gated on the `slack` entitlement, same idiom as
  // OrgSettings' Slack workspaces / SSO sections — dark until the probe
  // resolves so the tab doesn't flash in then out. Unlike Settings, it's
  // NOT gated on canManage: any team member can view tracked-channel status
  // (the GET only requires membership); TeamSlackChannelsPanel disables
  // editing for a non-admin viewer.
  const { has: hasFeature, loaded: entLoaded } = useEntitlements()
  const slackEnt = entLoaded && hasFeature(FeatureSlack)

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

  // Consumes the one-shot ?team= override: strips it from the URL once
  // applied above so it can't linger and re-win over a subsequent manual
  // switch if the user refreshes later in the same session. Runs once per
  // mount — searchParams is deliberately excluded from the deps so this
  // doesn't re-fire as other params (?tab=) change.
  useEffect(() => {
    if (!searchParams.has('team')) return
    const next = new URLSearchParams(searchParams)
    next.delete('team')
    setSearchParams(next, { replace: true })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const tab = resolveTab(searchParams.get('tab'), canManage, slackEnt)
  // Members owns the bare URL (drop the param); the others carry an explicit
  // ?tab=. replace so flipping tabs doesn't pile up history entries.
  const setTab = (t: TeamTab) => {
    if (t === tab) return
    // Leaving the Settings tab unmounts <TeamSettings>, dropping any unsaved
    // edits — confirm first when it's dirty (mirrors the team-switch guard).
    if (tab === 'settings' && settingsDirty && !window.confirm('Discard unsaved changes?')) return
    setSearchParams(t === 'members' ? {} : { tab: t }, { replace: true })
  }

  // Settings is gated to managers (team-admin or org-admin); Slack is gated
  // on the entitlement; members + prompts are visible to every team member
  // (writes are gated server-side).
  const tabs = TABS.filter(
    (t) => (t.id !== 'settings' || canManage) && (t.id !== 'slack' || slackEnt),
  )

  // Arrow-key navigation within the tablist (WAI-ARIA tabs pattern, automatic
  // activation): Left/Right move the active tab and follow focus to it.
  const onTabKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return
    e.preventDefault()
    const idx = tabs.findIndex((t) => t.id === tab)
    const nextIdx = (idx + (e.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length
    setTab(tabs[nextIdx].id)
    const btns = e.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]')
    btns[nextIdx]?.focus()
  }

  const promptsTab = tab === 'prompts'

  return (
    // Prompts is a full binding-graph canvas: give it a full-height flex column
    // so the embedded editor fills the space below the header + tab strip. The
    // height comes from the shell body rather than from the viewport minus a
    // guess at the chrome above it. Members + Settings stay a narrow column.
    <div className={`mx-auto ${promptsTab ? 'flex h-full max-w-6xl flex-col' : 'max-w-3xl'}`}>
      <div className="mb-5 flex shrink-0 items-center justify-between gap-3">
        <div className="flex items-center gap-2.5">
          <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-warm-2 text-warm">
            <Users size={15} />
          </span>
          <div>
            <h1 className="text-section font-semibold leading-tight text-ink-1">{team.name}</h1>
            <p className="text-reported leading-tight text-ink-3">
              Members, settings, Slack, and prompts.
            </p>
          </div>
        </div>
        {/* Shared page-level switcher — renders for ≥2 teams. */}
        <TeamSwitch teams={teams} value={teamId} onChange={switchTeam} />
      </div>

      <div
        role="tablist"
        aria-label="Team sections"
        onKeyDown={onTabKeyDown}
        className="mb-5 flex shrink-0 gap-1 border-b border-line-1"
      >
        {tabs.map((t) => (
          <button
            key={t.id}
            id={`team-tab-${t.id}`}
            type="button"
            role="tab"
            aria-selected={tab === t.id}
            aria-controls="team-tabpanel"
            // Roving tabindex: only the active tab is in the Tab order; the
            // tablist's arrow-key handler moves focus among the rest.
            tabIndex={tab === t.id ? 0 : -1}
            onClick={() => setTab(t.id)}
            className={`-mb-px border-b-2 px-3 py-2 text-body font-medium transition-colors ${
              tab === t.id
                ? 'border-warm text-warm'
                : 'border-transparent text-ink-3 hover:text-ink-2'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {/* key remounts each tab body on a team switch — fresh roster/config/graph.
          The min-h-0/flex-1 lets the Prompts canvas fill the column; the other
          tabs are auto-height, so it's inert for them. */}
      <div
        id="team-tabpanel"
        role="tabpanel"
        aria-labelledby={`team-tab-${tab}`}
        className={promptsTab ? 'flex min-h-0 flex-1 flex-col' : undefined}
      >
        {tab === 'members' && (
          <TeamMembersPanel key={teamId} orgId={orgId} teamId={teamId} canManage={canManage} />
        )}
        {tab === 'settings' && (
          <TeamSettings
            key={teamId}
            isLocal={false}
            teamId={teamId}
            teamName={team.name}
            orgIsAdmin={orgIsAdmin}
            onDirtyChange={setSettingsDirty}
          />
        )}
        {tab === 'slack' && (
          <TeamSlackChannelsPanel key={teamId} teamId={teamId} orgIsAdmin={orgIsAdmin} />
        )}
        {promptsTab && <PromptsWorkspace key={teamId} teamId={teamId} ready={teamId !== ''} />}
      </div>
    </div>
  )
}

// resolveTab maps the ?tab= param to a concrete tab, flooring Settings to
// Members for non-managers and Slack to Members when the org isn't entitled,
// so a stale/hand-typed ?tab= can't surface a tab the viewer can't use.
// Members is the default.
function resolveTab(raw: string | null, canManage: boolean, slackEnt: boolean): TeamTab {
  if (raw === 'settings' && canManage) return 'settings'
  if (raw === 'slack' && slackEnt) return 'slack'
  if (raw === 'prompts') return 'prompts'
  return 'members'
}
