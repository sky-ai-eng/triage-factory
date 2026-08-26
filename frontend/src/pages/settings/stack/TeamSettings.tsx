// TODO(TFAC-892): the new /team surface supersedes this stack; delete it once
// every group here is covered there, including the local-mode mount below.
//
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
// JiraProjectRulesGroup, ModelPicker) — no carded field groups.
//
// Two couplings to honour:
//   • Team-defaults ride PATCH /api/teams/{id}/settings and Jira-projects their
//     own PUT /api/teams/{id}/jira-projects, so
//     each saves {...baseline, ...ownSlice} (the org-group pattern).
//   • The GitHub-teams candidate list is built live from the team's tracked
//     repo owners with no server cache, so saving the Repos section bumps a
//     `reposVersion` nonce that remounts GitHubTeamGroup — forcing it to
//     refetch candidates (a newly-tracked owner's teams appear; an untracked
//     owner's mapping shows as an orphan).

import { useCallback, useEffect, useState } from 'react'
import { Archive } from 'lucide-react'
import { toast } from '../../../components/Toast/toastStore'
import Slider from '../../../components/Slider'
import ArchiveTeamModal from '../../../components/ArchiveTeamModal'
import { fetchArchivePreview, type ArchivePreview } from '../../../lib/teamLifecycle'
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
  saveTeamJiraProjects,
  saveTeamSettings,
  teamConfigFromSettings,
  teamProjectsBlocked,
  type GitHubGroup,
  type JiraProjectConfig,
  type TeamConfigForm,
} from '../teamConfig'
import { apiJSON } from '../../../lib/apiClient'
import { modelCatalogEntry, modelDisplayName } from '../../../hooks/useModelCatalog'
import { useApiOrgId } from '../../../hooks/useApiOrgId'
import { gateModelSave } from '../../../lib/modelGate'
import SettingsSection from './SettingsSection'

// Review-posting postures, in the order they're offered: the
// identity-derived default first, then the three fixed choices from most to
// least human oversight. `help` is what distinguishes them — the setting is
// about who the review posts as and what a wrong comment costs, which the value
// names alone don't convey. Keep the values in sync with
// domain.ValidReviewPostures (internal/domain/settings.go).
const REVIEW_POSTURES: { value: string; label: string; help: string }[] = [
  {
    value: 'identity',
    label: 'Match the credential',
    help: 'Posts as the app, drafts when acting as you.',
  },
  {
    value: 'draft',
    label: 'Always draft for approval',
    help: 'Every review waits for a human to approve it before it reaches GitHub.',
  },
  {
    value: 'auto',
    label: 'Always post',
    help: 'Reviews go straight to GitHub when the agent finishes them.',
  },
  {
    value: 'auto_unless_blocking',
    label: 'Post unless blocking',
    help: 'Posts directly, except a request-changes review or one with a blocker-severity comment, which waits for approval.',
  },
]

const REVIEW_POSTURE_LABELS: Record<string, string> = Object.fromEntries(
  REVIEW_POSTURES.map((p) => [p.value, p.label]),
)

// Base-branch push policies, strictest first. The setting is a safety guard
// against an agent pushing to main by mistake — not a security control, since
// a local-mode agent runs as the operator — so `help` says what it does rather
// than promising enforcement. Keep the values in sync with
// domain.ValidBaseBranchPushPolicies (internal/domain/settings.go).
const BASE_BRANCH_PUSH_POLICIES: { value: string; label: string; help: string }[] = [
  {
    value: 'never',
    label: 'Never',
    help: 'Agents push to their own branch and open a pull request. Refused on every run.',
  },
  {
    value: 'manual_only',
    label: 'Only runs a human started',
    help: 'Allowed when someone dispatched the run themselves; refused on runs a trigger fired.',
  },
  {
    value: 'always',
    label: 'Always',
    help: 'Allowed on every run — for trunk-based repos, docs and config repos, generated files.',
  },
]

const BASE_BRANCH_PUSH_LABELS: Record<string, string> = Object.fromEntries(
  BASE_BRANCH_PUSH_POLICIES.map((p) => [p.value, p.label]),
)

// Fallback bounds for the grace-window slider when the backend doesn't advertise
// them (an older server). The backend is authoritative when present — the
// permission_absent_grace_{min,max}_seconds fields are preferred and these only
// apply against a server too old to send them. Keep in sync with the source of
// truth: delegate.AbsentGrace{Min,Max}Seconds in internal/delegate/permissions.go
// (max = DefaultIdleHibernateTimeout/2 − 1s). If that constant changes, this stale
// fallback only affects users on an older server that doesn't advertise bounds.
const GRACE_MIN_FALLBACK = 1
const GRACE_MAX_FALLBACK = 149

const clampToRange = (v: number, min: number, max: number): number =>
  Math.min(max, Math.max(min, v))

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
  // The archive confirm opens already holding its consequences preview — the
  // fetch happens on the button, and a preview that cannot be read withdraws
  // the question instead of opening an empty dialog.
  const [archivePreview, setArchivePreview] = useState<ArchivePreview | null>(null)
  const [archiveReading, setArchiveReading] = useState(false)
  // The endpoint alias: local addresses the sole team as "default"; multi uses
  // the selected team's real id.
  const endpointTeamId = isLocal ? 'default' : teamId
  // The org whose catalog this team's models are drawn from — the model routes
  // are org-scoped, and the save gate reads the same catalog the picker does.
  const apiOrgId = useApiOrgId()

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
  const [defaultModel, setDefaultModel] = useState('')
  const [autoDelegate, setAutoDelegate] = useState(true)
  const [autoMode, setAutoMode] = useState(true)
  // Advisory branch-name template suggested to delegated agents (TFAC-498).
  const [branchTemplate, setBranchTemplate] = useState('tfac/<ticket-id>')
  // How finished agent reviews reach GitHub.
  const [reviewPosture, setReviewPosture] = useState('identity')
  // Whether agents may push to a repo's base/default branch.
  const [basePushPolicy, setBasePushPolicy] = useState('never')
  // Presence-gated absent auto-deny (TFAC-392).
  const [absentAutodeny, setAbsentAutodeny] = useState(true)
  const [absentGraceSeconds, setAbsentGraceSeconds] = useState(15)
  // Honored grace bounds advertised by the backend (whole seconds), driving the
  // slider's range so a user can never pick a value the backend would re-clamp.
  const [graceMin, setGraceMin] = useState(GRACE_MIN_FALLBACK)
  const [graceMax, setGraceMax] = useState(GRACE_MAX_FALLBACK)

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
      apiJSON<{ jira?: boolean; jira_url?: string }>('/api/integrations/status').catch(() => null),
    ])
      .then(([settings, teamRepos, teamGroups, integ]) => {
        if (cancelled) return
        if (!settings) {
          setLoadError('Could not load team settings. Check your connection and try again.')
          setLoading(false)
          return
        }
        const form = teamConfigFromSettings(settings)
        // Resolve the honored grace band (backend-authoritative, with a fallback)
        // and snap the seeded grace into it, so a previously-stored out-of-band
        // value lands on a valid slider position instead of leaving baseline and
        // the control disagreeing (which would read as spuriously dirty).
        const gMin = settings.permission_absent_grace_min_seconds ?? GRACE_MIN_FALLBACK
        const gMax = settings.permission_absent_grace_max_seconds ?? GRACE_MAX_FALLBACK
        const seededGrace = clampToRange(form.permission_absent_grace_seconds, gMin, gMax)
        setGraceMin(gMin)
        setGraceMax(gMax)
        // Seed baseline.repos with the separately-loaded set (teamConfigFromSettings
        // leaves it undefined) so the collapsed summary shows the real count and the
        // section isn't spuriously dirty — otherwise collapsing fires the discard
        // guard, whose revert would wipe the selection to [].
        setBaseline({
          ...form,
          permission_absent_grace_seconds: seededGrace,
          repos: teamRepos ?? undefined,
        })
        setProjects(form.jira_projects)
        setDefaultModel(form.default_model)
        setAutoDelegate(form.auto_delegate_enabled)
        setAutoMode(form.auto_mode_enabled)
        setBranchTemplate(form.branch_template)
        setReviewPosture(form.review_posture)
        setBasePushPolicy(form.base_branch_push_policy)
        setAbsentAutodeny(form.permission_absent_autodeny_enabled)
        setAbsentGraceSeconds(seededGrace)
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

  // ── Jira projects (their own replace-set PUT) ──
  const projectsDirty =
    JSON.stringify(normProjects(projects)) !== JSON.stringify(baseline.jira_projects ?? [])
  const projectsBlocked = teamProjectsBlocked(projects, jiraConnected)
  const saveProjects = async (): Promise<boolean> => {
    setSavingProjects(true)
    try {
      const res = await saveTeamJiraProjects(endpointTeamId, projects)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      // Render what was STORED, not what was sent: the server resolved every
      // status display name from Jira on the way in.
      const stored = normProjects(res.projects)
      setBaseline((b) => ({ ...b, jira_projects: stored }))
      setProjects(stored)
      toast.success('Jira projects saved')
      return true
    } finally {
      setSavingProjects(false)
    }
  }

  // ── Team defaults (ride the team-settings PATCH) ──
  const defaultsDirty =
    defaultModel !== baseline.default_model ||
    autoDelegate !== baseline.auto_delegate_enabled ||
    (isLocal && autoMode !== baseline.auto_mode_enabled) ||
    branchTemplate !== baseline.branch_template ||
    reviewPosture !== baseline.review_posture ||
    basePushPolicy !== baseline.base_branch_push_policy
  const saveDefaults = async (): Promise<boolean> => {
    // The save gate first: a team default nothing has established these
    // credentials can run is tested before it is stored, because the alternative
    // is a team whose every unpinned step fails at its next dispatch.
    const picked = modelCatalogEntry(apiOrgId, defaultModel)
    if (picked && !(await gateModelSave(apiOrgId!, picked))) return false
    setSavingDefaults(true)
    try {
      const res = await saveTeamSettings(
        endpointTeamId,
        {
          ...baseline,
          default_model: defaultModel,
          auto_delegate_enabled: autoDelegate,
          auto_mode_enabled: autoMode,
          branch_template: branchTemplate,
          review_posture: reviewPosture,
          base_branch_push_policy: basePushPolicy,
        },
        isLocal,
      )
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setBaseline((b) => ({
        ...b,
        default_model: defaultModel,
        auto_delegate_enabled: autoDelegate,
        auto_mode_enabled: autoMode,
        branch_template: branchTemplate,
        review_posture: reviewPosture,
        base_branch_push_policy: basePushPolicy,
      }))
      if (res.warning) toast.info(res.warning)
      toast.success('Team defaults saved')
      return true
    } finally {
      setSavingDefaults(false)
    }
  }

  // ── Unattended prompts: absent auto-deny (rides the team-settings PATCH) ──
  const promptsDirty =
    absentAutodeny !== baseline.permission_absent_autodeny_enabled ||
    absentGraceSeconds !== baseline.permission_absent_grace_seconds
  // The slider keeps the grace inside [graceMin, graceMax]; this is a belt-and-
  // braces guard so a value outside the honored band can never reach the save.
  const promptsBlocked =
    absentAutodeny &&
    (!Number.isFinite(absentGraceSeconds) ||
      absentGraceSeconds < graceMin ||
      absentGraceSeconds > graceMax)
  const savePrompts = async (): Promise<boolean> => {
    setSavingPrompts(true)
    try {
      // Snap the grace into the honored band before it touches the wire or
      // baseline. The slider already constrains it, but this also covers a
      // non-finite value (defensive) and keeps the persisted value in lockstep
      // with what the backend would clamp to — never serializing an out-of-band
      // grace (or a NaN, which would land as null).
      const grace = Number.isFinite(absentGraceSeconds)
        ? clampToRange(absentGraceSeconds, graceMin, graceMax)
        : baseline.permission_absent_grace_seconds
      const res = await saveTeamSettings(
        endpointTeamId,
        {
          ...baseline,
          permission_absent_autodeny_enabled: absentAutodeny,
          permission_absent_grace_seconds: grace,
        },
        isLocal,
      )
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
      <div className="px-1 py-3 text-body text-ink-2">
        {loadError}{' '}
        <button type="button" onClick={() => load()} className="text-warm underline">
          Retry
        </button>
      </div>
    )
  }

  const trackedProjects = projects.filter((p) => p.key.trim() !== '').length

  if (loading) {
    return <div className="px-1 py-3 text-body text-ink-3">Loading team settings…</div>
  }

  return (
    <div className="divide-y divide-line-1">
      <SettingsSection
        title="Repositories"
        summary={`${(baseline.repos ?? []).length} tracked`}
        dirty={reposDirty}
        saving={savingRepos}
        onSave={saveRepos}
        onCancel={() => setRepos(baseline.repos ?? [])}
      >
        <p className="text-body leading-relaxed text-ink-3">
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
          <p className="text-ui italic text-warm">
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
        summary={`Model: ${baseline.default_model ? modelDisplayName(baseline.default_model) : 'Not set'}${
          baseline.auto_delegate_enabled ? ' · auto-delegate on' : ''
        }${isLocal ? ` · auto mode ${baseline.auto_mode_enabled ? 'on' : 'off'}` : ''} · Reviews: ${
          REVIEW_POSTURE_LABELS[baseline.review_posture] ?? baseline.review_posture
        } · Base-branch pushes: ${
          BASE_BRANCH_PUSH_LABELS[baseline.base_branch_push_policy] ??
          baseline.base_branch_push_policy
        }`}
        dirty={defaultsDirty}
        saving={savingDefaults}
        onSave={saveDefaults}
        onCancel={() => {
          setDefaultModel(baseline.default_model)
          setAutoDelegate(baseline.auto_delegate_enabled)
          setAutoMode(baseline.auto_mode_enabled)
          setBranchTemplate(baseline.branch_template)
          setReviewPosture(baseline.review_posture)
          setBasePushPolicy(baseline.base_branch_push_policy)
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
            <p className="text-body text-ink-1">Auto-delegation</p>
            <p className="mt-0.5 text-reported text-ink-3">
              Automatically delegate tasks when matching triggers fire
            </p>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={autoDelegate}
            onClick={() => setAutoDelegate((v) => !v)}
            className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
              autoDelegate ? 'bg-warm' : 'bg-tint-3'
            }`}
          >
            <span
              className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-raised shadow-float transition-transform ${
                autoDelegate ? 'translate-x-4' : 'translate-x-0'
              }`}
            />
          </button>
        </div>
        {isLocal && (
          <div className="flex items-center justify-between gap-4">
            <div>
              <p className="text-[13px] text-ink-1">Auto mode</p>
              <p className="mt-0.5 text-[11px] text-ink-3">
                Let Claude automatically approve tool calls it determines are safe
              </p>
            </div>
            <button
              type="button"
              role="switch"
              aria-label="Auto mode"
              aria-checked={autoMode}
              onClick={() => setAutoMode((v) => !v)}
              className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
                autoMode ? 'bg-warm' : 'bg-line-2'
              }`}
            >
              <span
                className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-raised shadow-sm transition-transform ${
                  autoMode ? 'translate-x-4' : 'translate-x-0'
                }`}
              />
            </button>
          </div>
        )}
        <div>
          <p className="text-body text-ink-1">Branch name template</p>
          <p className="mt-0.5 text-reported text-ink-3">
            Suggested to delegated agents when they create a branch. &lt;ticket-id&gt; is replaced
            with the ticket id. Guidance only — not enforced.
          </p>
          <input
            type="text"
            value={branchTemplate}
            onChange={(e) => setBranchTemplate(e.target.value)}
            placeholder="tfac/<ticket-id>"
            className="mt-1.5 w-full rounded-md border border-line-1 bg-transparent px-2 py-1 text-body text-ink-1 focus:border-warm focus:outline-none"
          />
        </div>
        <div>
          <p className="text-body text-ink-1">Review posting</p>
          <p className="mt-0.5 text-reported text-ink-3">
            What happens when an agent finishes a code review. A drafted review waits in the
            approval queue; a posted one appears on the pull request immediately.
          </p>
          <select
            value={reviewPosture}
            onChange={(e) => setReviewPosture(e.target.value)}
            className="mt-1.5 w-full rounded-md border border-line-1 bg-transparent px-2 py-1 text-body text-ink-1 focus:border-warm focus:outline-none"
          >
            {REVIEW_POSTURES.map((p) => (
              <option key={p.value} value={p.value}>
                {p.label}
              </option>
            ))}
          </select>
          <p className="mt-1 text-reported text-ink-3">
            {REVIEW_POSTURES.find((p) => p.value === reviewPosture)?.help}
          </p>
        </div>
        <div>
          <p className="text-body text-ink-1">Pushes to the base branch</p>
          <p className="mt-0.5 text-reported text-ink-3">
            Whether a delegated agent may push straight to a repo&rsquo;s base or default branch
            (main, master, or whatever the repository records). A safety guard against an agent
            pushing there by mistake &mdash; not a substitute for branch protection on the host.
          </p>
          <select
            value={basePushPolicy}
            onChange={(e) => setBasePushPolicy(e.target.value)}
            className="mt-1.5 w-full rounded-md border border-line-1 bg-transparent px-2 py-1 text-body text-ink-1 focus:border-warm focus:outline-none"
          >
            {BASE_BRANCH_PUSH_POLICIES.map((p) => (
              <option key={p.value} value={p.value}>
                {p.label}
              </option>
            ))}
          </select>
          <p className="mt-1 text-reported text-ink-3">
            {BASE_BRANCH_PUSH_POLICIES.find((p) => p.value === basePushPolicy)?.help}
          </p>
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
        <p className="text-body leading-relaxed text-ink-3">
          When a delegated run needs permission for an off-allowlist tool and no one has the board
          or that run open and focused, deny after a short grace instead of parking the run for the
          full timeout. If someone opens or focuses the board (or the run) during the grace, the
          prompt waits the full timeout so they can answer.
        </p>
        <div className="flex items-center justify-between">
          <div>
            <p className="text-body text-ink-1">Fast-deny when unattended</p>
            <p className="mt-0.5 text-reported text-ink-3">
              Off keeps the full-timeout behavior for every prompt
            </p>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={absentAutodeny}
            onClick={() => setAbsentAutodeny((v) => !v)}
            className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
              absentAutodeny ? 'bg-warm' : 'bg-tint-3'
            }`}
          >
            <span
              className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-raised shadow-float transition-transform ${
                absentAutodeny ? 'translate-x-4' : 'translate-x-0'
              }`}
            />
          </button>
        </div>
        {absentAutodeny && (
          <div>
            <div className="flex items-baseline justify-between gap-2">
              <p className="text-body text-ink-1">Grace window</p>
              <span className="text-ui text-ink-3 tabular-nums">{absentGraceSeconds}s</span>
            </div>
            <p className="mt-0.5 text-reported text-ink-3">
              How long to wait for someone to appear before denying ({graceMin}s–{graceMax}s)
            </p>
            <div className="mt-3 flex items-center gap-3">
              <Slider
                value={absentGraceSeconds}
                onChange={setAbsentGraceSeconds}
                min={graceMin}
                max={graceMax}
                step={1}
                label="Grace window in seconds"
              />
            </div>
          </div>
        )}
      </SettingsSection>

      {/* Danger zone: archive (org-admin, multi-mode only). Soft-deletes the
          team, force-stops its in-flight delegations, and blocks all writes —
          no "let it finish" branch (TFAC-448). */}
      {!isLocal && orgIsAdmin && (
        <div className="flex items-center justify-between gap-4 py-4">
          <div>
            <p className="flex items-center gap-1.5 text-body font-medium text-ink-1">
              <Archive size={13} className="text-alarm" />
              Archive team
            </p>
            <p className="mt-0.5 text-reported leading-relaxed text-ink-3">
              Stops all in-flight work for this team and hides it. Restorable by an org admin;
              stopped runs do not resume.
            </p>
          </div>
          <button
            type="button"
            onClick={() => {
              if (archiveReading) return
              setArchiveReading(true)
              fetchArchivePreview(teamId)
                .then(setArchivePreview)
                .catch(() => toast.error('Could not check the team for in-flight work. Try again.'))
                .finally(() => setArchiveReading(false))
            }}
            disabled={archiveReading}
            className="shrink-0 rounded-lg border border-alarm/30 px-3 py-1.5 text-body font-medium text-alarm transition-colors hover:bg-alarm/[0.06] disabled:opacity-50"
          >
            {archiveReading ? 'Checking…' : 'Archive…'}
          </button>
        </div>
      )}

      {archivePreview && (
        <ArchiveTeamModal
          teamId={teamId}
          teamName={teamName || 'this team'}
          preview={archivePreview}
          onClose={() => setArchivePreview(null)}
          onDone={(runs) => {
            setArchivePreview(null)
            toast.success(
              `Team archived — stopped ${runs} ${runs === 1 ? 'delegation' : 'delegations'}.`,
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
