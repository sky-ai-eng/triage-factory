// The Team group of the Settings stack — rendered only when the viewer admins
// >=1 team. A write-scope team selector (admin'd teams only, reusing TeamSwitch)
// sits under the divider; switching teams reloads that team's config. Each
// team-scope group is its own flush collapsible composing the wizard's
// `bare`/glass primitives (the inline RepoPickerModal, bare GitHubTeamGroup /
// JiraProjectRulesGroup, ModelTierSelector) — no carded field groups.
//
// Two couplings to honour:
//   • Jira-projects + Team-defaults both ride POST /api/settings/team/{id}, so
//     each saves {...baseline, ...ownSlice} (the org-group pattern).
//   • The GitHub-teams candidate list is built live from the team's tracked
//     repo owners with no server cache, so saving the Repos section bumps a
//     `reposVersion` nonce that remounts GitHubTeamGroup — forcing it to
//     refetch candidates (a newly-tracked owner's teams appear; an untracked
//     owner's mapping shows as an orphan).

import { useCallback, useEffect, useState } from 'react'
import { toast } from '../../../components/Toast/toastStore'
import RepoPickerModal from '../../../components/RepoPickerModal'
import GitHubTeamGroup from '../GitHubTeamGroup'
import JiraProjectRulesGroup from '../JiraProjectRulesGroup'
import ModelTierSelector from '../ModelTierSelector'
import { MODEL_TIER_OPTIONS } from '../modelTiers'
import {
  emptyTeamConfig,
  fetchTeamRepos,
  fetchTeamSettings,
  saveTeamGitHubGroups,
  saveTeamRepos,
  saveTeamSettings,
  teamConfigFromSettings,
  teamProjectsBlocked,
  type GitHubGroup,
  type JiraProjectConfig,
  type TeamConfigForm,
} from '../teamConfig'
import type { TeamSummary } from '../../../types'
import TeamSwitch from '../../../components/TeamSwitch'
import SettingsSection from './SettingsSection'

const TIER_LABELS: Record<string, string> = { haiku: 'Haiku', sonnet: 'Sonnet', opus: 'Opus' }

const normProjects = (p: JiraProjectConfig[]): JiraProjectConfig[] =>
  p.map((x) => ({ ...x, key: x.key.trim() })).filter((x) => x.key !== '')

const sameRepos = (a: string[], b: string[]): boolean => {
  if (a.length !== b.length) return false
  const sb = new Set(b)
  return a.every((x) => sb.has(x))
}

const groupKeys = (g: GitHubGroup[]): string[] =>
  g.map((x) => `${x.org_login.toLowerCase()}/${x.team_slug.toLowerCase()}`).sort()
const sameGroups = (a: GitHubGroup[], b: GitHubGroup[]): boolean => {
  const ka = groupKeys(a)
  const kb = groupKeys(b)
  return ka.length === kb.length && ka.every((x, i) => x === kb[i])
}

// pickInitialTeam seeds the selector: the sticky default if it's an admin'd
// team, else the first admin'd team.
function pickInitialTeam(adminTeams: TeamSummary[], lastActingTeamId: string): string {
  if (adminTeams.length === 0) return ''
  if (lastActingTeamId && adminTeams.some((t) => t.id === lastActingTeamId)) return lastActingTeamId
  return adminTeams[0].id
}

export default function TeamSettings({
  isLocal,
  adminTeams,
  lastActingTeamId,
}: {
  isLocal: boolean
  adminTeams: TeamSummary[]
  lastActingTeamId: string
}) {
  const [selectedTeamId, setSelectedTeamId] = useState(() =>
    pickInitialTeam(adminTeams, lastActingTeamId),
  )
  // The endpoint alias: local addresses the sole team as "default"; multi uses
  // the selected team's real id.
  const teamId = isLocal ? 'default' : selectedTeamId

  const [baseline, setBaseline] = useState<TeamConfigForm>(emptyTeamConfig)
  const [reposLoaded, setReposLoaded] = useState(false)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [jiraConnected, setJiraConnected] = useState(false)

  // Per-section drafts.
  const [repos, setRepos] = useState<string[]>([])
  const [groups, setGroups] = useState<GitHubGroup[]>([])
  const [groupsBaseline, setGroupsBaseline] = useState<GitHubGroup[]>([])
  const [projects, setProjects] = useState<JiraProjectConfig[]>([])
  const [defaultModel, setDefaultModel] = useState('sonnet')
  const [autoDelegate, setAutoDelegate] = useState(true)

  const [savingRepos, setSavingRepos] = useState(false)
  const [savingProjects, setSavingProjects] = useState(false)
  const [savingDefaults, setSavingDefaults] = useState(false)
  const [savingGroups, setSavingGroups] = useState(false)

  // Bumped on a successful repos save to remount GitHubTeamGroup so it refetches
  // its candidate list (which is derived live from the tracked-repo owners).
  const [reposVersion, setReposVersion] = useState(0)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    setLoadError(null)
    Promise.all([
      fetchTeamSettings(teamId),
      fetchTeamRepos(teamId),
      fetch('/api/integrations/status')
        .then((r) => (r.ok ? r.json() : null))
        .catch(() => null),
    ])
      .then(([settings, teamRepos, integ]) => {
        if (cancelled) return
        if (!settings) {
          setLoadError('Could not load team settings. Check your connection and try again.')
          setLoading(false)
          return
        }
        const form = teamConfigFromSettings(settings)
        setBaseline(form)
        setProjects(form.jira_projects)
        setDefaultModel(form.default_model)
        setAutoDelegate(form.auto_delegate_enabled)
        setRepos(teamRepos ?? [])
        setReposLoaded(teamRepos !== null)
        setJiraConnected(!!integ?.jira && !!integ?.jira_url)
        // github_groups seed up through GitHubTeamGroup's onLoaded.
        setGroups([])
        setGroupsBaseline([])
        setLoading(false)
      })
      .catch(() => {
        if (cancelled) return
        setLoadError('Could not load team settings. Check your connection and try again.')
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [teamId])

  useEffect(() => {
    const cancel = load()
    return cancel
  }, [load])

  // Re-seed the selection if the admin'd team set changes (e.g. a team created
  // in the org section) and the held id is no longer admin'd.
  useEffect(() => {
    if (isLocal) return
    if (selectedTeamId && adminTeams.some((t) => t.id === selectedTeamId)) return
    const next = pickInitialTeam(adminTeams, lastActingTeamId)
    if (next !== selectedTeamId) setSelectedTeamId(next)
  }, [isLocal, adminTeams, lastActingTeamId, selectedTeamId])

  // ── Repos ──
  const reposDirty = reposLoaded && !sameRepos(repos, baseline.repos ?? [])
  const saveRepos = async (): Promise<boolean> => {
    setSavingRepos(true)
    try {
      const res = await saveTeamRepos(teamId, repos)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setBaseline((b) => ({ ...b, repos }))
      // Force the GitHub-teams candidates to refetch against the new owner set.
      setReposVersion((v) => v + 1)
      toast.success('Repositories saved')
      return true
    } finally {
      setSavingRepos(false)
    }
  }

  // ── GitHub teams ──
  const groupsDirty = !sameGroups(groups, groupsBaseline)
  const saveGroups = async (): Promise<boolean> => {
    setSavingGroups(true)
    try {
      const res = await saveTeamGitHubGroups(teamId, groups)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setGroupsBaseline(groups)
      toast.success('GitHub teams saved')
      return true
    } finally {
      setSavingGroups(false)
    }
  }

  // ── Jira projects (rides team-settings POST) ──
  const projectsDirty =
    JSON.stringify(normProjects(projects)) !== JSON.stringify(baseline.jira_projects ?? [])
  const projectsBlocked = teamProjectsBlocked(projects, jiraConnected)
  const saveProjects = async (): Promise<boolean> => {
    setSavingProjects(true)
    try {
      const res = await saveTeamSettings(teamId, { ...baseline, jira_projects: projects })
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      const normalized = normProjects(projects)
      setBaseline((b) => ({ ...b, jira_projects: normalized }))
      setProjects(normalized)
      if (res.warning) toast.info(res.warning)
      toast.success('Jira projects saved')
      return true
    } finally {
      setSavingProjects(false)
    }
  }

  // ── Team defaults (rides team-settings POST) ──
  const defaultsDirty =
    defaultModel !== baseline.default_model || autoDelegate !== baseline.auto_delegate_enabled
  const saveDefaults = async (): Promise<boolean> => {
    setSavingDefaults(true)
    try {
      const res = await saveTeamSettings(teamId, {
        ...baseline,
        default_model: defaultModel,
        auto_delegate_enabled: autoDelegate,
      })
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setBaseline((b) => ({
        ...b,
        default_model: defaultModel,
        auto_delegate_enabled: autoDelegate,
      }))
      if (res.warning) toast.info(res.warning)
      toast.success('Team defaults saved')
      return true
    } finally {
      setSavingDefaults(false)
    }
  }

  if (loadError) {
    return (
      <div className="px-1 py-3 text-[13px] text-text-secondary">
        {loadError}{' '}
        <button type="button" onClick={() => load()} className="text-accent underline">
          Retry
        </button>
      </div>
    )
  }

  const trackedProjects = projects.filter((p) => p.key.trim() !== '').length

  return (
    <div>
      {/* Selector — only renders for >=2 admin'd teams (solo/local hidden). */}
      {!isLocal && adminTeams.length >= 2 && (
        <div className="flex items-center justify-between pb-1 pl-1">
          <span className="text-[11px] uppercase tracking-wide text-text-tertiary">
            Configuring team
          </span>
          <TeamSwitch teams={adminTeams} value={selectedTeamId} onChange={setSelectedTeamId} />
        </div>
      )}

      {loading ? (
        <div className="px-1 py-3 text-[13px] text-text-tertiary">Loading team settings…</div>
      ) : (
        <div className="divide-y divide-border-subtle">
          <SettingsSection
            title="Repositories"
            summary={`${(baseline.repos ?? []).length} tracked`}
            dirty={reposDirty}
            saving={savingRepos}
            onSave={saveRepos}
            onCancel={() => setRepos(baseline.repos ?? [])}
          >
            <p className="text-[13px] leading-relaxed text-text-tertiary">
              Watched repos surface in this team&rsquo;s triage queue and anchor Jira-to-code
              matching for delegation.
            </p>
            {reposLoaded ? (
              <RepoPickerModal
                inline
                hideFooter
                selected={repos}
                onSelectionChange={setRepos}
                onSave={() => {}}
                onClose={() => {}}
              />
            ) : (
              <p className="text-[12px] italic text-amber-600">
                Couldn&rsquo;t load this team&rsquo;s repositories — they&rsquo;ll be left
                unchanged. Reload to edit them.
              </p>
            )}
          </SettingsSection>

          <SettingsSection
            title="GitHub teams"
            summary={`${groupsBaseline.length} mapped`}
            dirty={groupsDirty}
            saving={savingGroups}
            onSave={saveGroups}
            onCancel={() => setGroups(groupsBaseline)}
          >
            <GitHubTeamGroup
              // Remount on a repos save so candidates refetch against the new
              // tracked-owner set; also remount on a team switch.
              key={`${teamId}-${reposVersion}`}
              value={groups}
              onChange={setGroups}
              teamId={teamId}
              canEdit
              onLoaded={(seed) => {
                setGroups(seed)
                setGroupsBaseline(seed)
              }}
              bare
            />
          </SettingsSection>

          <SettingsSection
            title="Jira projects"
            summary={`${trackedProjects} tracked`}
            dirty={projectsDirty}
            saving={savingProjects}
            saveDisabled={projectsBlocked}
            onSave={saveProjects}
            onCancel={() => setProjects(baseline.jira_projects ?? [])}
          >
            <JiraProjectRulesGroup
              value={projects}
              onChange={setProjects}
              connected={jiraConnected}
              bare
            />
          </SettingsSection>

          <SettingsSection
            title="Team defaults"
            summary={`Model: ${TIER_LABELS[baseline.default_model] ?? baseline.default_model}${
              baseline.auto_delegate_enabled ? ' · auto-delegate on' : ''
            }`}
            dirty={defaultsDirty}
            saving={savingDefaults}
            onSave={saveDefaults}
            onCancel={() => {
              setDefaultModel(baseline.default_model)
              setAutoDelegate(baseline.auto_delegate_enabled)
            }}
          >
            <div className="space-y-2">
              <span className="text-[11px] uppercase tracking-wide text-text-tertiary">
                Default delegation model
              </span>
              <ModelTierSelector
                value={defaultModel}
                onChange={setDefaultModel}
                options={MODEL_TIER_OPTIONS}
                ariaLabel="Team default model"
              />
            </div>
            <div className="flex items-center justify-between">
              <div>
                <p className="text-[13px] text-text-primary">Auto-delegation</p>
                <p className="mt-0.5 text-[11px] text-text-tertiary">
                  Automatically delegate tasks when matching triggers fire
                </p>
              </div>
              <button
                type="button"
                role="switch"
                aria-checked={autoDelegate}
                onClick={() => setAutoDelegate((v) => !v)}
                className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
                  autoDelegate ? 'bg-accent' : 'bg-black/[0.08]'
                }`}
              >
                <span
                  className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow-sm transition-transform ${
                    autoDelegate ? 'translate-x-4' : 'translate-x-0'
                  }`}
                />
              </button>
            </div>
          </SettingsSection>
        </div>
      )}
    </div>
  )
}
