// The team-scoped config sections — repos, GitHub teams, Jira projects, team
// defaults/model/auto-delegate, and unattended prompts. Relocated from the
// global Settings page into the /team page's Settings tab (TFAC-445): this
// component now renders for ONE explicit team (the page-level switcher owns
// team selection), so it no longer carries its own selector. The global
// Settings page still mounts it in LOCAL mode (N=1, no /team route), passing
// teamId 'default'.
//
// Each team-scope group is its own flush collapsible composing the wizard's
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
import { Archive } from 'lucide-react'
import { toast } from '../../../components/Toast/toastStore'
import ArchiveTeamModal from '../../../components/ArchiveTeamModal'
import { useTeams } from '../../../hooks/useTeams'
import RepoPickerModal from '../../../components/RepoPickerModal'
import GitHubTeamGroup from '../GitHubTeamGroup'
import JiraProjectRulesGroup from '../JiraProjectRulesGroup'
import { TeamModelStep } from '../../setup/ModelStep'
import { initialWizardState } from '../../setup/steps'
import type { StepContext } from '../../setup/types'
import {
  emptyTeamConfig,
  fetchTeamRepos,
  fetchTeamGitHubGroups,
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

export default function TeamSettings({
  isLocal,
  teamId,
  onDirtyChange,
  orgIsAdmin = false,
  teamName = '',
}: {
  isLocal: boolean
  // The team these sections configure. The page-level switcher resolves it and
  // remounts this component (key={teamId}) on a switch, so it's stable for the
  // component's lifetime. Local mode ignores it (the endpoint alias is fixed).
  teamId: string
  // Reports the aggregate dirty state up so the page-level team switcher can
  // confirm-before-discard on a switch (the switch happens outside this
  // component now). Optional — the local Settings page doesn't switch teams.
  onDirtyChange?: (dirty: boolean) => void
  // orgIsAdmin gates the org-admin-only "Archive team" danger zone (TFAC-448).
  // Defaults to false so the local Settings page (and non-admins) never render
  // it. teamName labels the confirm modal. Multi-mode only (isLocal hides it).
  orgIsAdmin?: boolean
  teamName?: string
}) {
  const { refresh: refreshTeams } = useTeams()
  const [archiveOpen, setArchiveOpen] = useState(false)
  // The endpoint alias: local addresses the sole team as "default"; multi uses
  // the selected team's real id.
  const endpointTeamId = isLocal ? 'default' : teamId

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
  // Advisory branch-name template suggested to delegated agents (TFAC-498).
  const [branchTemplate, setBranchTemplate] = useState('tfac/<ticket-id>')
  // Presence-gated absent auto-deny (TFAC-392).
  const [absentAutodeny, setAbsentAutodeny] = useState(true)
  const [absentGraceSeconds, setAbsentGraceSeconds] = useState(15)

  const [savingRepos, setSavingRepos] = useState(false)
  const [savingProjects, setSavingProjects] = useState(false)
  const [savingDefaults, setSavingDefaults] = useState(false)
  const [savingGroups, setSavingGroups] = useState(false)
  const [savingPrompts, setSavingPrompts] = useState(false)

  // Bumped on a successful repos save to remount GitHubTeamGroup so it refetches
  // its candidate list (which is derived live from the tracked-repo owners).
  const [reposVersion, setReposVersion] = useState(0)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    setLoadError(null)
    Promise.all([
      fetchTeamSettings(endpointTeamId),
      fetchTeamRepos(endpointTeamId),
      fetchTeamGitHubGroups(endpointTeamId),
      fetch('/api/integrations/status')
        .then((r) => (r.ok ? r.json() : null))
        .catch(() => null),
    ])
      .then(([settings, teamRepos, teamGroups, integ]) => {
        if (cancelled) return
        if (!settings) {
          setLoadError('Could not load team settings. Check your connection and try again.')
          setLoading(false)
          return
        }
        const form = teamConfigFromSettings(settings)
        // Seed baseline.repos with the separately-loaded set (teamConfigFromSettings
        // leaves it undefined) so the collapsed summary shows the real count and the
        // section isn't spuriously dirty — otherwise collapsing fires the discard
        // guard, whose revert would wipe the selection to [].
        setBaseline({ ...form, repos: teamRepos ?? undefined })
        setProjects(form.jira_projects)
        setDefaultModel(form.default_model)
        setAutoDelegate(form.auto_delegate_enabled)
        setBranchTemplate(form.branch_template)
        setAbsentAutodeny(form.permission_absent_autodeny_enabled)
        setAbsentGraceSeconds(form.permission_absent_grace_seconds)
        setRepos(teamRepos ?? [])
        setReposLoaded(teamRepos !== null)
        setJiraConnected(!!integ?.jira && !!integ?.jira_url)
        // Seed the GitHub-team mappings up front (the GitHubTeamGroup checklist
        // mounts lazily on expand and would otherwise leave the collapsed count
        // at 0); the group's onLoaded re-seeds on expand / after a repos save.
        setGroups(teamGroups ?? [])
        setGroupsBaseline(teamGroups ?? [])
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
  }, [endpointTeamId])

  useEffect(() => {
    const cancel = load()
    return cancel
  }, [load])

  // ── Repos ──
  const reposDirty = reposLoaded && !sameRepos(repos, baseline.repos ?? [])
  const saveRepos = async (): Promise<boolean> => {
    setSavingRepos(true)
    try {
      const res = await saveTeamRepos(endpointTeamId, repos)
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
      const res = await saveTeamGitHubGroups(endpointTeamId, groups)
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
      const res = await saveTeamSettings(endpointTeamId, { ...baseline, jira_projects: projects })
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
    defaultModel !== baseline.default_model ||
    autoDelegate !== baseline.auto_delegate_enabled ||
    branchTemplate !== baseline.branch_template
  const saveDefaults = async (): Promise<boolean> => {
    setSavingDefaults(true)
    try {
      const res = await saveTeamSettings(endpointTeamId, {
        ...baseline,
        default_model: defaultModel,
        auto_delegate_enabled: autoDelegate,
        branch_template: branchTemplate,
      })
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setBaseline((b) => ({
        ...b,
        default_model: defaultModel,
        auto_delegate_enabled: autoDelegate,
        branch_template: branchTemplate,
      }))
      if (res.warning) toast.info(res.warning)
      toast.success('Team defaults saved')
      return true
    } finally {
      setSavingDefaults(false)
    }
  }

  // ── Unattended prompts: absent auto-deny (rides team-settings POST) ──
  const promptsDirty =
    absentAutodeny !== baseline.permission_absent_autodeny_enabled ||
    absentGraceSeconds !== baseline.permission_absent_grace_seconds
  // A 0/blank grace is invalid (the backend floors it at 1s, but block the save
  // so the persisted value matches what the user sees).
  const promptsBlocked =
    absentAutodeny && (!Number.isFinite(absentGraceSeconds) || absentGraceSeconds < 1)
  const savePrompts = async (): Promise<boolean> => {
    setSavingPrompts(true)
    try {
      // Coerce a cleared/NaN input to a finite value before it touches the
      // wire or baseline. When fast-deny is off the grace input is hidden and
      // promptsBlocked can't guard it, so a stale NaN would otherwise persist
      // into baseline (and serialize as null). Fall back to the last-saved
      // grace so baseline never goes invalid; also normalize the visible input.
      const grace =
        Number.isFinite(absentGraceSeconds) && absentGraceSeconds >= 1
          ? absentGraceSeconds
          : baseline.permission_absent_grace_seconds
      const res = await saveTeamSettings(endpointTeamId, {
        ...baseline,
        permission_absent_autodeny_enabled: absentAutodeny,
        permission_absent_grace_seconds: grace,
      })
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setAbsentGraceSeconds(grace)
      setBaseline((b) => ({
        ...b,
        permission_absent_autodeny_enabled: absentAutodeny,
        permission_absent_grace_seconds: grace,
      }))
      toast.success('Unattended-prompt settings saved')
      return true
    } finally {
      setSavingPrompts(false)
    }
  }

  // Surface the aggregate dirty state to the page-level switcher so it can
  // confirm-before-discard on a team switch (the switch fires outside this
  // component now). Reset to false on unmount so a stale "dirty" can't block a
  // switch after the user navigates off the Settings tab.
  const anyDirty = reposDirty || groupsDirty || projectsDirty || defaultsDirty || promptsDirty
  useEffect(() => {
    onDirtyChange?.(anyDirty)
  }, [anyDirty, onDirtyChange])
  useEffect(() => () => onDirtyChange?.(false), [onDirtyChange])

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

  if (loading) {
    return <div className="px-1 py-3 text-[13px] text-text-tertiary">Loading team settings…</div>
  }

  return (
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
          Watched repos surface in this team&rsquo;s triage queue and anchor Jira-to-code matching
          for delegation.
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
            Couldn&rsquo;t load this team&rsquo;s repositories — they&rsquo;ll be left unchanged.
            Reload to edit them.
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
          // Key on teamId only — a team switch reseeds (also goes through
          // the loading gate). A repos save bumps refreshSignal instead of
          // the key, so candidates refetch against the new tracked-owner set
          // WITHOUT remounting and clobbering unsaved mapping edits here.
          key={endpointTeamId}
          value={groups}
          onChange={setGroups}
          teamId={endpointTeamId}
          canEdit
          onLoaded={(seed) => {
            setGroups(seed)
            setGroupsBaseline(seed)
          }}
          refreshSignal={reposVersion}
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
          setBranchTemplate(baseline.branch_template)
        }}
      >
        {/* The actual /setup team-model body — same heading + tier ladder. */}
        <TeamModelStep
          orgId={null}
          teamId={endpointTeamId}
          isLocal={isLocal}
          state={{
            ...initialWizardState(),
            team: { ...emptyTeamConfig(), default_model: defaultModel },
          }}
          patch={(p: Partial<StepContext['state']>) => {
            if (p.team?.default_model !== undefined) setDefaultModel(p.team.default_model)
          }}
          advance={() => {}}
        />
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
        <div>
          <p className="text-[13px] text-text-primary">Branch name template</p>
          <p className="mt-0.5 text-[11px] text-text-tertiary">
            Suggested to delegated agents when they create a branch. &lt;ticket-id&gt; is replaced
            with the ticket id. Guidance only — not enforced.
          </p>
          <input
            type="text"
            value={branchTemplate}
            onChange={(e) => setBranchTemplate(e.target.value)}
            placeholder="tfac/<ticket-id>"
            className="mt-1.5 w-full rounded-md border border-border-subtle bg-transparent px-2 py-1 text-[13px] text-text-primary focus:border-accent focus:outline-none"
          />
        </div>
      </SettingsSection>

      <SettingsSection
        title="Unattended prompts"
        summary={
          absentAutodeny
            ? `Fast-deny after ${baseline.permission_absent_grace_seconds}s when nobody's watching`
            : 'Always wait the full timeout'
        }
        dirty={promptsDirty}
        saving={savingPrompts}
        saveDisabled={promptsBlocked}
        onSave={savePrompts}
        onCancel={() => {
          setAbsentAutodeny(baseline.permission_absent_autodeny_enabled)
          setAbsentGraceSeconds(baseline.permission_absent_grace_seconds)
        }}
      >
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          When a delegated run needs permission for an off-allowlist tool and no one has the board
          or that run open and focused, deny after a short grace instead of parking the run for the
          full timeout. If someone opens or focuses the board (or the run) during the grace, the
          prompt waits the full timeout so they can answer.
        </p>
        <div className="flex items-center justify-between">
          <div>
            <p className="text-[13px] text-text-primary">Fast-deny when unattended</p>
            <p className="mt-0.5 text-[11px] text-text-tertiary">
              Off keeps the full-timeout behavior for every prompt
            </p>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={absentAutodeny}
            onClick={() => setAbsentAutodeny((v) => !v)}
            className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
              absentAutodeny ? 'bg-accent' : 'bg-black/[0.08]'
            }`}
          >
            <span
              className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow-sm transition-transform ${
                absentAutodeny ? 'translate-x-4' : 'translate-x-0'
              }`}
            />
          </button>
        </div>
        {absentAutodeny && (
          <div className="flex items-center justify-between">
            <div>
              <p className="text-[13px] text-text-primary">Grace window</p>
              <p className="mt-0.5 text-[11px] text-text-tertiary">
                How long to wait for someone to appear before denying
              </p>
            </div>
            <div className="flex items-center gap-1.5">
              <input
                type="number"
                min={1}
                value={Number.isFinite(absentGraceSeconds) ? absentGraceSeconds : ''}
                onChange={(e) => setAbsentGraceSeconds(parseInt(e.target.value, 10))}
                className="w-16 rounded-md border border-border-subtle bg-transparent px-2 py-1 text-right text-[13px] text-text-primary focus:border-accent focus:outline-none"
              />
              <span className="text-[12px] text-text-tertiary">seconds</span>
            </div>
          </div>
        )}
      </SettingsSection>

      {/* Danger zone: archive (org-admin, multi-mode only). Soft-deletes the
          team, force-stops its in-flight delegations + curator sessions, and
          blocks all writes — no "let it finish" branch (TFAC-448). */}
      {!isLocal && orgIsAdmin && (
        <div className="flex items-center justify-between gap-4 py-4">
          <div>
            <p className="flex items-center gap-1.5 text-[13px] font-medium text-text-primary">
              <Archive size={13} className="text-dismiss" />
              Archive team
            </p>
            <p className="mt-0.5 text-[11px] leading-relaxed text-text-tertiary">
              Stops all in-flight work for this team and hides it. Restorable by an org admin;
              stopped runs do not resume.
            </p>
          </div>
          <button
            type="button"
            onClick={() => setArchiveOpen(true)}
            className="shrink-0 rounded-lg border border-dismiss/30 px-3 py-1.5 text-[13px] font-medium text-dismiss transition-colors hover:bg-dismiss/[0.06]"
          >
            Archive…
          </button>
        </div>
      )}

      {archiveOpen && (
        <ArchiveTeamModal
          teamId={teamId}
          teamName={teamName || 'this team'}
          onClose={() => setArchiveOpen(false)}
          onDone={(runs, sessions) => {
            setArchiveOpen(false)
            toast.success(
              `Team archived — stopped ${runs} ${runs === 1 ? 'delegation' : 'delegations'} and ${sessions} curator ${
                sessions === 1 ? 'session' : 'sessions'
              }.`,
            )
            // The team vanishes from /api/teams; refreshing drives the page to
            // the next team (or the zero-team landing).
            void refreshTeams()
          }}
        />
      )}
    </div>
  )
}
