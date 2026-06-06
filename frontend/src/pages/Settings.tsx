import { useState, useEffect, useRef, useCallback } from 'react'
import { Lock } from 'lucide-react'
import SettingsTabs, { type SettingsTab } from '../components/SettingsTabs'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'
import { getStoredTheme, setTheme, type ThemeMode } from '../lib/theme'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { computeAccess } from './settings/access'
import TeamManagementSection from '../components/TeamManagementSection'
import { Section, Field } from './settings/primitives'
import GitHubAccessGroup from './settings/GitHubAccessGroup'
import JiraAccessGroup from './settings/JiraAccessGroup'
import PollerTimingGroup from './settings/PollerTimingGroup'
import ModelGroup from './settings/ModelGroup'
import ReposGroup from './settings/ReposGroup'
import GitHubTeamGroup from './settings/GitHubTeamGroup'
import JiraProjectRulesGroup from './settings/JiraProjectRulesGroup'
import TeamSettingsGroup from './settings/TeamSettingsGroup'
import { saveOrgConfig, type OrgSettingsData } from './settings/orgConfig'
import {
  fetchTeamRepos,
  fetchTeamSettings,
  saveTeamGitHubGroups,
  saveTeamRepos,
  saveTeamSettings,
  teamProjectsBlocked,
  type GitHubGroup,
  type JiraProjectConfig,
  type TeamConfigForm,
  type TeamSettingsData,
} from './settings/teamConfig'

// SettingsData holds the org-scope baseline (GitHub/Jira access + the model
// cap) plus the Jira connection state. Team-scope baselines live in
// teamBaseline so the two permission domains' change-detection stay
// independent.
interface SettingsData {
  github: {
    base_url: string
    has_token: boolean
    poll_interval: string
    clone_protocol: 'ssh' | 'https'
  }
  jira: {
    base_url: string
    has_token: boolean
    poll_interval: string
  }
  max_llm_model_tier: string
}

// TeamBaseline is the last-saved (or last-loaded) snapshot of the team
// slices, compared against the live form so a save touches only the slices
// that actually changed — a settings-only edit doesn't re-PUT repos (which
// would re-trigger profiling) or re-replace the GitHub-team mappings.
type TeamBaseline = Pick<
  TeamConfigForm,
  'default_model' | 'auto_delegate_enabled' | 'jira_projects' | 'repos' | 'github_groups'
>

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

export default function Settings() {
  // useOptionalAuth is null in local mode (no AuthProvider mounted), which
  // is the degenerate single-user world — always N=1. In multi mode it
  // carries the orgs list + active org used for N=1 detection and role
  // gating. Member counts + team role come from the scope GET responses,
  // not /api/me, so a future team switch refetches the right scope.
  const auth = useOptionalAuth()
  const isLocal = auth === null
  // Org-scoped GitHub App endpoints take the id in the path. Local mode has
  // no OrgContext, so it always uses the sentinel; multi mode uses the
  // resolved active org and is null until that resolves (don't fall back to
  // the sentinel there — it would fetch the wrong org's state).
  const activeOrgId = useActiveOrgId()
  const orgId = isLocal ? LOCAL_DEFAULT_ORG_ID : activeOrgId

  const [data, setData] = useState<SettingsData | null>(null)
  // Non-null when the initial load failed (rejected fetch, or org/team
  // resolved null). Rendered in place of the loading spinner so a failure
  // surfaces with a Retry instead of hanging on "Loading settings…".
  const [loadError, setLoadError] = useState<string | null>(null)
  const [teamBaseline, setTeamBaseline] = useState<TeamBaseline | null>(null)
  // False until the team-repos GET lands. A failed load leaves form.repos []
  // — indistinguishable from "tracks nothing" — so editing is suppressed and
  // change-detection treats repos as unchanged, never PUTting [] back over a
  // set we just failed to read (the Repos page guards the same way).
  const [reposLoaded, setReposLoaded] = useState(false)
  const [orgMemberCount, setOrgMemberCount] = useState(1)
  const [teamMemberCount, setTeamMemberCount] = useState(1)
  const [teamRole, setTeamRole] = useState('')
  // The manifest callback lands the browser on this page with #github-app
  // after a round-trip to GitHub. Start on the Workspace tab so the panel
  // showing the freshly-registered App is visible without a manual click.
  const [tab, setTab] = useState<SettingsTab>(() =>
    typeof window !== 'undefined' && window.location.hash === '#github-app' ? 'workspace' : 'team',
  )
  const [form, setForm] = useState<{
    github_enabled: boolean
    github_url: string
    github_pat: string
    jira_enabled: boolean
    jira_url: string
    jira_pat: string
    github_poll_interval: string
    github_clone_protocol: 'ssh' | 'https'
    jira_poll_interval: string
    max_llm_model_tier: string
    default_model: string
    auto_delegate_enabled: boolean
    ai_reprioritize_threshold: number
    ai_preference_update_interval: number
    jira_projects: JiraProjectConfig[]
    repos: string[]
    github_groups: GitHubGroup[]
  }>({
    github_enabled: true,
    github_url: '',
    github_pat: '',
    jira_enabled: false,
    jira_url: '',
    jira_pat: '',
    github_poll_interval: '5m0s',
    github_clone_protocol: 'ssh',
    jira_poll_interval: '5m0s',
    max_llm_model_tier: '',
    default_model: 'sonnet',
    auto_delegate_enabled: true,
    ai_reprioritize_threshold: 0,
    ai_preference_update_interval: 0,
    jira_projects: [],
    repos: [],
    github_groups: [],
  })
  const [saving, setSaving] = useState(false)
  const [jiraConnected, setJiraConnected] = useState(false)
  const [theme, setThemeState] = useState<ThemeMode>(() => getStoredTheme())

  const access = computeAccess({
    isLocal,
    orgs: auth?.orgs ?? [],
    activeOrgId: auth?.serverActiveOrgId ?? null,
    orgMemberCount,
    teamMemberCount,
    teamRole,
  })
  const { isN1, isOrgAdmin, isTeamAdmin } = access

  const loadSettings = useCallback(() => {
    setLoadError(null)
    Promise.all([
      fetch('/api/settings/org').then((r) => (r.ok ? r.json() : null)),
      fetchTeamSettings('default'),
      fetchTeamRepos('default'),
    ])
      .then(
        ([org, team, repos]: [
          OrgSettingsData | null,
          TeamSettingsData | null,
          string[] | null,
        ]) => {
          if (!org || !team) {
            setLoadError('Could not load settings. Check your connection and try again.')
            return
          }
          const projects = team.jira_projects ?? []
          const reposVal = repos ?? []
          const merged: SettingsData = {
            github: {
              base_url: org.github_base_url || '',
              has_token: org.has_github_pat,
              poll_interval: org.github_poll_interval,
              clone_protocol: org.github_clone_protocol === 'https' ? 'https' : 'ssh',
            },
            jira: {
              base_url: org.jira_base_url || '',
              has_token: org.has_jira_pat,
              poll_interval: org.jira_poll_interval,
            },
            max_llm_model_tier: org.max_llm_model_tier || '',
          }
          setData(merged)
          setReposLoaded(repos !== null)
          setOrgMemberCount(org.member_count ?? 1)
          setTeamMemberCount(team.member_count ?? 1)
          setTeamRole(team.role ?? '')
          setForm((f) => ({
            ...f,
            github_url: merged.github.base_url,
            github_pat: '',
            jira_enabled: merged.jira.has_token,
            jira_url: merged.jira.base_url,
            jira_pat: '',
            github_poll_interval: merged.github.poll_interval,
            github_clone_protocol: merged.github.clone_protocol,
            jira_poll_interval: merged.jira.poll_interval,
            max_llm_model_tier: merged.max_llm_model_tier,
            default_model: team.team_settings.DefaultModel || 'sonnet',
            auto_delegate_enabled: team.team_settings.AutoDelegateEnabled,
            ai_reprioritize_threshold: team.team_settings.AIReprioritizeThreshold,
            ai_preference_update_interval: team.team_settings.AIPreferenceUpdateInterval,
            jira_projects: projects,
            repos: reposVal,
            // github_groups seeds via GitHubTeamGroup's onLoaded once it fetches
            // the org's candidate teams.
          }))
          setTeamBaseline({
            default_model: team.team_settings.DefaultModel || 'sonnet',
            auto_delegate_enabled: team.team_settings.AutoDelegateEnabled,
            jira_projects: projects,
            repos: reposVal,
            github_groups: [],
          })
          if (merged.jira.has_token && merged.jira.base_url) {
            setJiraConnected(true)
          }
        },
      )
      .catch(() => {
        // A rejected fetch (network error) must not hang on "Loading settings…".
        setLoadError('Could not load settings. Check your connection and try again.')
      })
  }, [])

  useEffect(() => {
    loadSettings()
  }, [loadSettings])

  // Jira connect/disconnect live in JiraAccessGroup (org scope); these
  // callbacks own the team-scope follow-up the shared group can't see. On a
  // connect against a different instance URL, the prior team project config
  // no longer maps, so it's wiped; disconnect clears it outright.
  const onJiraConnected = (url: string) => {
    if (data && data.jira.base_url && data.jira.base_url !== url) {
      setForm((f) => ({ ...f, jira_projects: [] }))
    }
    setJiraConnected(true)
  }

  const onJiraDisconnected = () => {
    setJiraConnected(false)
    setForm((f) => ({ ...f, jira_enabled: false, jira_projects: [] }))
  }

  // Spread a shared field-group patch into the flat form state. The group
  // patches are keyed by the same field names the form uses, so this is a
  // plain merge — no per-field mapping.
  const patchForm = (patch: Partial<typeof form>) => setForm((f) => ({ ...f, ...patch }))

  // GitHubTeamGroup seeds the saved mappings up once it has loaded the org's
  // candidate teams; capture them as both the live value and the baseline so
  // an untouched GitHub-teams panel never counts as a change. Guarded to fire
  // once per page load: in the tabbed layout the team groups unmount on a tab
  // switch and remount on return, so an unguarded seed would re-fetch and
  // clobber unsaved toggles with the server's set.
  const groupsSeededRef = useRef(false)
  const seedGitHubGroups = (groups: GitHubGroup[]) => {
    if (groupsSeededRef.current) return
    groupsSeededRef.current = true
    setForm((f) => ({ ...f, github_groups: groups }))
    setTeamBaseline((b) => (b ? { ...b, github_groups: groups } : b))
  }

  // ---- Change detection (per slice, against the loaded/last-saved snapshot).
  const normalizedProjects = normProjects(form.jira_projects)
  const teamSettingsChanged =
    form.default_model !== teamBaseline?.default_model ||
    form.auto_delegate_enabled !== teamBaseline?.auto_delegate_enabled ||
    JSON.stringify(normalizedProjects) !== JSON.stringify(teamBaseline?.jira_projects ?? [])
  // Only a *loaded* repo set can be considered changed — see reposLoaded.
  const reposChanged = reposLoaded && !sameRepos(form.repos, teamBaseline?.repos ?? [])
  const groupsChanged = !sameGroups(form.github_groups, teamBaseline?.github_groups ?? [])
  const teamChanged = teamSettingsChanged || reposChanged || groupsChanged

  const orgChanged =
    !!form.github_pat ||
    !!form.jira_pat ||
    form.github_url !== (data?.github.base_url ?? '') ||
    form.github_poll_interval !== data?.github.poll_interval ||
    form.github_clone_protocol !== data?.github.clone_protocol ||
    form.jira_url !== (data?.jira.base_url ?? '') ||
    form.jira_poll_interval !== data?.jira.poll_interval ||
    form.max_llm_model_tier !== (data?.max_llm_model_tier ?? '')

  // saveTeamScope writes only the team slices that changed, in a deliberate
  // order (settings → repos → github-groups), folding each saved slice back
  // into the baseline so the next change-detection compares against current
  // state. Settings + repos + github-groups are separate endpoints with no
  // spanning transaction, so a later failure leaves the earlier writes
  // committed — it stops and surfaces the error rather than pressing on.
  const saveTeamScope = async (): Promise<boolean> => {
    if (teamSettingsChanged) {
      const res = await saveTeamSettings('default', form)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      if (res.warning) toast.info(res.warning)
      setTeamBaseline((b) =>
        b
          ? {
              ...b,
              default_model: form.default_model,
              auto_delegate_enabled: form.auto_delegate_enabled,
              jira_projects: normalizedProjects,
            }
          : b,
      )
    }
    if (reposChanged) {
      const res = await saveTeamRepos('default', form.repos)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setTeamBaseline((b) => (b ? { ...b, repos: form.repos } : b))
    }
    if (groupsChanged) {
      const res = await saveTeamGitHubGroups('default', form.github_groups)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setTeamBaseline((b) => (b ? { ...b, github_groups: form.github_groups } : b))
    }
    return true
  }

  // saveOrg persists the org-level field group through the shared
  // saveOrgConfig helper (the same POST /api/settings/org path the
  // create-configure step uses), then folds the saved values back into
  // `data` so the next change-detection compares against current state.
  const saveOrg = async (): Promise<boolean> => {
    const result = await saveOrgConfig({
      github_url: form.github_url,
      github_pat: form.github_pat,
      github_clone_protocol: form.github_clone_protocol,
      github_poll_interval: form.github_poll_interval,
      jira_url: form.jira_url,
      jira_pat: form.jira_pat,
      jira_poll_interval: form.jira_poll_interval,
      max_llm_model_tier: form.max_llm_model_tier,
    })
    if (!result.ok) {
      toast.error(result.error)
      return false
    }
    setData((d) =>
      d
        ? {
            ...d,
            github: {
              ...d.github,
              base_url: form.github_url,
              poll_interval: form.github_poll_interval,
              clone_protocol: form.github_clone_protocol,
              has_token: d.github.has_token || !!form.github_pat,
            },
            jira: {
              ...d.jira,
              base_url: form.jira_url,
              poll_interval: form.jira_poll_interval,
              has_token: d.jira.has_token || !!form.jira_pat,
            },
            max_llm_model_tier: form.max_llm_model_tier,
          }
        : d,
    )
    // Lowering the cap below the default team's preference still saves; the
    // warning surfaces that its effective model dropped.
    if (result.warning) toast.info(result.warning)
    return true
  }

  // runSave drives the requested scope(s). Team runs first: its validation is
  // pure server-side (project rules, dup keys) with no external calls, so it
  // fails before the org POST's PAT/SSH work commits anything.
  const runSave = async (scopes: SettingsTab[]) => {
    setSaving(true)
    try {
      if (scopes.includes('team') && !(await saveTeamScope())) return
      if (scopes.includes('workspace') && !(await saveOrg())) return
      toast.success('Settings saved')
      setForm((f) => ({ ...f, github_pat: '', jira_pat: '' }))
    } catch (err) {
      toast.error(`Could not save settings: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  // Flat (N=1) save: POST only the scope(s) whose fields actually changed
  // against the mount/last-save snapshot, so a single-domain edit can't
  // half-commit and we skip a no-op round trip.
  const saveAll = (e: React.FormEvent) => {
    e.preventDefault()
    const scopes: SettingsTab[] = []
    if (teamChanged) scopes.push('team')
    if (orgChanged) scopes.push('workspace')
    if (scopes.length === 0) {
      toast.info('No changes to save')
      return
    }
    void runSave(scopes)
  }

  if (!data) {
    return (
      <div className="flex flex-col items-center justify-center min-h-[50vh] gap-3">
        {loadError ? (
          <>
            <p className="text-text-secondary text-[13px]">{loadError}</p>
            <button
              type="button"
              onClick={loadSettings}
              className="text-accent text-[13px] underline"
            >
              Retry
            </button>
          </>
        ) : (
          <p className="text-text-tertiary text-[13px]">Loading settings...</p>
        )}
      </div>
    )
  }

  // Save is blocked (team scope) when Jira is connected but a tracked project
  // has incomplete rules — same gating as before, now via the shared helper.
  const teamSaveBlocked = teamProjectsBlocked(form.jira_projects, jiraConnected)

  // ---- Section renderers (closures over current state; no prop drilling).

  // GitHub access (org scope): base URL + PAT + clone protocol, plus the
  // App-registration alternative — all shared with the setup wizard's
  // GitHub step.
  const renderGitHubAccess = () => (
    <GitHubAccessGroup
      value={{
        github_url: form.github_url,
        github_pat: form.github_pat,
        github_clone_protocol: form.github_clone_protocol,
      }}
      onChange={patchForm}
      hasToken={data.github.has_token}
      isLocal={isLocal}
      orgId={orgId}
    />
  )

  // Jira access (org scope): credentials + connect/disconnect. Poll cadence
  // moved to the shared poller-timing group.
  const renderJiraAccess = () => (
    <JiraAccessGroup
      value={{ jira_url: form.jira_url, jira_pat: form.jira_pat }}
      onChange={patchForm}
      connected={jiraConnected}
      onConnected={onJiraConnected}
      onDisconnected={onJiraDisconnected}
    />
  )

  // Poller timing (org scope): GitHub + Jira cadence. The Jira interval is
  // only meaningful once Jira is connected.
  const renderPollerTiming = () => (
    <PollerTimingGroup
      value={{
        github_poll_interval: form.github_poll_interval,
        jira_poll_interval: form.jira_poll_interval,
      }}
      onChange={patchForm}
      showJira={jiraConnected}
    />
  )

  // Workspace (org) scope: a hard ceiling over every team's model choice.
  // The team default lives in TeamSettingsGroup (team scope); this caps it.
  const renderModelCap = () => (
    <ModelGroup value={{ max_llm_model_tier: form.max_llm_model_tier }} onChange={patchForm} />
  )

  // ---- Team-scope groups (shared with the setup wizard's team steps).
  // Controlled — the container owns the form + the per-slice saves.
  const renderRepos = () => (
    <ReposGroup
      value={form.repos}
      onChange={(repos) => patchForm({ repos })}
      canEdit={isTeamAdmin}
      loaded={reposLoaded}
    />
  )

  const renderGitHubTeams = () => (
    <GitHubTeamGroup
      value={form.github_groups}
      onChange={(github_groups) => patchForm({ github_groups })}
      teamId="default"
      canEdit={isTeamAdmin}
      onLoaded={seedGitHubGroups}
    />
  )

  const renderJiraProjects = () => (
    <JiraProjectRulesGroup
      value={form.jira_projects}
      onChange={(jira_projects) => patchForm({ jira_projects })}
      connected={jiraConnected}
    />
  )

  const renderTeamSettings = () => (
    <TeamSettingsGroup
      value={{
        default_model: form.default_model,
        auto_delegate_enabled: form.auto_delegate_enabled,
      }}
      onChange={patchForm}
    />
  )

  const renderAppearance = () => (
    <Section>
      <h2 className="text-[13px] font-medium text-text-secondary mb-4">Appearance</h2>
      <Field label="Theme">
        <div className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5">
          {(['light', 'dark', 'auto'] as const).map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => {
                setThemeState(m)
                setTheme(m)
              }}
              className={`px-3 py-1 text-[12px] font-medium rounded-md transition-colors capitalize ${
                theme === m
                  ? 'bg-white text-text-primary shadow-sm'
                  : 'text-text-tertiary hover:text-text-secondary'
              }`}
            >
              {m}
            </button>
          ))}
        </div>
        <p className="text-[11px] text-text-tertiary mt-1.5">
          Auto follows your system preference.
        </p>
      </Field>
    </Section>
  )

  const renderIntegrations = () => (
    <Section>
      <h2 className="text-[13px] font-medium text-text-primary mb-3">Integrations</h2>
      <div className="flex items-center justify-between">
        <div>
          <p className="text-[13px] text-text-primary">Import Claude Code Skills</p>
          <p className="text-[11px] text-text-tertiary mt-0.5">
            Import SKILL.md files from ~/.claude/skills/ as delegation prompts
          </p>
        </div>
        <button
          type="button"
          onClick={async () => {
            try {
              const res = await fetch('/api/skills/import', { method: 'POST' })
              if (!res.ok) {
                toast.error(await readError(res, 'Failed to import skills'))
                return
              }
              const result = await res.json()
              if (result.imported > 0) {
                toast.success(
                  `Imported ${result.imported} skill${result.imported !== 1 ? 's' : ''} (${result.skipped} already imported)`,
                )
              } else {
                toast.info(
                  `No new skills found (${result.scanned} scanned, ${result.skipped} already imported)`,
                )
              }
            } catch (err) {
              toast.error(`Failed to import skills: ${(err as Error).message}`)
            }
          }}
          className="text-[13px] text-accent hover:text-accent/80 border border-accent/20 hover:border-accent/30 rounded-xl px-4 py-2 transition-colors shrink-0"
        >
          Import Skills
        </button>
      </div>
    </Section>
  )

  const renderDanger = () => (
    <Section danger>
      <h2 className="text-[13px] font-medium text-dismiss mb-3">Danger Zone</h2>
      <button
        type="button"
        onClick={async () => {
          if (!confirm('Clear all stored tokens? You will need to re-authenticate.')) return
          await fetch('/api/integrations', { method: 'DELETE' })
          // Reload Settings so the cleared state is reflected — credentials
          // are re-entered here via the GitHub/Jira access groups (the legacy
          // /setup wizard is retired); the tenant itself still exists.
          window.location.reload()
        }}
        className="text-[13px] text-dismiss hover:text-dismiss/80 border border-dismiss/20 hover:border-dismiss/30 rounded-xl px-4 py-2 transition-colors"
      >
        Clear All Tokens
      </button>
    </Section>
  )

  // "Add team" (org admin, hosted-only). Org-scope settings; hidden in local
  // mode and for non-admins. The entry point a solo hosted user uses to
  // create a 2nd team, which then flips the count-gated selectors on.
  const renderTeams = () => {
    if (isLocal || !isOrgAdmin) return null
    return <TeamManagementSection />
  }

  const workspaceSections = (
    <>
      {renderGitHubAccess()}
      {renderJiraAccess()}
      {renderPollerTiming()}
      {renderModelCap()}
      {renderTeams()}
      {renderIntegrations()}
      {renderDanger()}
    </>
  )
  const teamSections = (
    <>
      {renderRepos()}
      {renderGitHubTeams()}
      {renderJiraProjects()}
      {renderTeamSettings()}
    </>
  )

  // ---- Flat (N=1) layout: everything on one page, single Save.
  if (isN1) {
    return (
      <div className="max-w-2xl mx-auto">
        <h1 className="text-[22px] font-semibold text-text-primary tracking-tight mb-6">
          Settings
        </h1>
        <form onSubmit={saveAll} className="space-y-5">
          {renderGitHubAccess()}
          {renderJiraAccess()}
          {renderPollerTiming()}
          {renderModelCap()}
          {renderRepos()}
          {renderGitHubTeams()}
          {renderJiraProjects()}
          {renderTeamSettings()}
          {renderAppearance()}
          <button
            type="submit"
            disabled={saving || teamSaveBlocked}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {saving ? 'Saving...' : 'Save Settings'}
          </button>
        </form>
        {/* These sit OUTSIDE the settings <form>: TeamManagementSection
            renders its own <form> (Add team), and nesting forms is invalid
            HTML — the inner submit would bubble as the outer Save Settings.
            renderIntegrations / renderDanger are self-contained sections with
            their own actions too. */}
        <div className="space-y-5 mt-5">
          {/* Hosted solo (N=1) admins can still grow past one team. */}
          {renderTeams()}
          {renderIntegrations()}
          {renderDanger()}
        </div>
      </div>
    )
  }

  // ---- Tabbed (N≥2) layout: Team / Workspace, role-gated.
  return (
    <div className="max-w-2xl mx-auto">
      <h1 className="text-[22px] font-semibold text-text-primary tracking-tight mb-1">Settings</h1>
      <SettingsTabs tab={tab} onChange={setTab} />

      {tab === 'my' && (
        <div className="space-y-5">
          {/* My Settings is personal/device scope — always editable, no role
              gating. Theme persists immediately to localStorage, so there's
              no Save button. Future user_settings fields land here. */}
          {renderAppearance()}
        </div>
      )}

      {tab === 'team' && (
        <div className="space-y-5">
          {!isTeamAdmin && <ReadOnlyNotice scope="team" />}
          <fieldset
            disabled={!isTeamAdmin}
            className={`space-y-5 min-w-0 border-0 p-0 m-0 ${isTeamAdmin ? '' : 'opacity-60'}`}
          >
            {teamSections}
          </fieldset>
          <button
            type="button"
            onClick={() => void runSave(['team'])}
            disabled={saving || !isTeamAdmin || teamSaveBlocked}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {saving ? 'Saving...' : 'Save team settings'}
          </button>
        </div>
      )}

      {tab === 'workspace' && (
        <div className="space-y-5">
          {!isOrgAdmin && <ReadOnlyNotice scope="workspace" />}
          <fieldset
            disabled={!isOrgAdmin}
            className={`space-y-5 min-w-0 border-0 p-0 m-0 ${isOrgAdmin ? '' : 'opacity-60'}`}
          >
            {workspaceSections}
          </fieldset>
          <button
            type="button"
            onClick={() => void runSave(['workspace'])}
            disabled={saving || !isOrgAdmin}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {saving ? 'Saving...' : 'Save workspace settings'}
          </button>
        </div>
      )}
    </div>
  )
}

// ReadOnlyNotice explains why a scope's fields are disabled. We disable (not
// hide) the fields and surface the policy, so a non-admin sees what exists
// and who can change it rather than a missing tab.
function ReadOnlyNotice({ scope }: { scope: 'team' | 'workspace' }) {
  const who = scope === 'team' ? 'team admins' : 'workspace (org) admins'
  return (
    <div className="flex items-center gap-2 rounded-xl bg-black/[0.03] border border-border-subtle px-4 py-2.5">
      <Lock size={13} className="text-text-tertiary shrink-0" />
      <span className="text-[12px] text-text-tertiary">
        These settings are read-only for you. Only {who} can change them.
      </span>
    </div>
  )
}
