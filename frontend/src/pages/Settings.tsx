import { useState, useEffect } from 'react'
import { ChevronDown, ChevronRight, Lock, Trash2 } from 'lucide-react'
import GitHubGroupsEditor from '../components/GitHubGroupsEditor'
import JiraStatusRule, { type JiraStatusRuleValue } from '../components/JiraStatusRule'
import SettingsTabs, { type SettingsTab } from '../components/SettingsTabs'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'
import { getStoredTheme, setTheme, type ThemeMode } from '../lib/theme'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { computeAccess } from './settings/access'
import TeamManagementSection from '../components/TeamManagementSection'
import { Section, Field, inputClass } from './settings/primitives'
import GitHubAccessGroup from './settings/GitHubAccessGroup'
import JiraAccessGroup from './settings/JiraAccessGroup'
import PollerTimingGroup from './settings/PollerTimingGroup'
import ModelGroup from './settings/ModelGroup'
import { saveOrgConfig, type OrgSettingsData } from './settings/orgConfig'

interface JiraStatus {
  id: string
  name: string
}

// JiraProjectConfig mirrors the backend wire shape for a single
// project's rules. SKY-272 collapsed three global rule fields into
// this per-project array so teams with heterogeneous workflows can
// configure each project independently.
interface JiraProjectConfig {
  key: string
  pickup: JiraStatusRuleValue
  in_progress: JiraStatusRuleValue
  done: JiraStatusRuleValue
}

interface TeamSettingsData {
  team_settings: {
    JiraProjects: string[]
    AIReprioritizeThreshold: number
    AIPreferenceUpdateInterval: number
    DefaultModel: string
    AutoDelegateEnabled: boolean
  }
  jira_projects: JiraProjectConfig[]
  member_count: number
  role: string
}

interface SettingsData {
  github: {
    enabled: boolean
    base_url: string
    has_token: boolean
    poll_interval: string
    clone_protocol: 'ssh' | 'https'
  }
  jira: {
    enabled: boolean
    base_url: string
    has_token: boolean
    poll_interval: string
    projects: JiraProjectConfig[]
  }
  ai: {
    model: string
    reprioritize_threshold: number
    preference_update_interval: number
    auto_delegate_enabled: boolean
  }
  max_llm_model_tier: string
}

const emptyProject = (key = ''): JiraProjectConfig => ({
  key,
  pickup: { members: [] },
  in_progress: { members: [] },
  done: { members: [] },
})

const projectIsComplete = (p: JiraProjectConfig): boolean =>
  p.key.trim() !== '' &&
  p.pickup.members.length > 0 &&
  p.in_progress.members.length > 0 &&
  !!p.in_progress.canonical &&
  p.done.members.length > 0 &&
  !!p.done.canonical

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
    jira_projects: JiraProjectConfig[]
    ai_model: string
    ai_auto_delegate_enabled: boolean
    max_llm_model_tier: string
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
    jira_projects: [],
    ai_model: 'sonnet',
    ai_auto_delegate_enabled: true,
    max_llm_model_tier: '',
  })
  const [saving, setSaving] = useState(false)
  // Statuses keyed by project key so each project's picker pulls from
  // the right per-project status list. The "Fetch Statuses" button
  // refreshes the union for all configured projects.
  const [jiraStatusesByProject, setJiraStatusesByProject] = useState<Record<string, JiraStatus[]>>(
    {},
  )
  const [statusesLoading, setStatusesLoading] = useState(false)
  // Per-project expand/collapse state. For the common N=1 case the
  // first project stays expanded so the UX matches the pre-SKY-272
  // flow; additional projects start collapsed.
  const [expandedKeys, setExpandedKeys] = useState<Record<string, boolean>>({})
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

  useEffect(() => {
    Promise.all([
      fetch('/api/settings/org').then((r) => (r.ok ? r.json() : null)),
      fetch('/api/settings/team/default').then((r) => (r.ok ? r.json() : null)),
    ]).then(([org, team]: [OrgSettingsData | null, TeamSettingsData | null]) => {
      if (!org || !team) return
      const projects = team.jira_projects && team.jira_projects.length > 0 ? team.jira_projects : []
      const merged: SettingsData = {
        github: {
          enabled: org.has_github_pat,
          base_url: org.github_base_url || '',
          has_token: org.has_github_pat,
          poll_interval: org.github_poll_interval,
          clone_protocol: org.github_clone_protocol === 'https' ? 'https' : 'ssh',
        },
        jira: {
          enabled: org.has_jira_pat,
          base_url: org.jira_base_url || '',
          has_token: org.has_jira_pat,
          poll_interval: org.jira_poll_interval,
          projects,
        },
        ai: {
          model: team.team_settings.DefaultModel,
          reprioritize_threshold: team.team_settings.AIReprioritizeThreshold,
          preference_update_interval: team.team_settings.AIPreferenceUpdateInterval,
          auto_delegate_enabled: team.team_settings.AutoDelegateEnabled,
        },
        max_llm_model_tier: org.max_llm_model_tier || '',
      }
      setData(merged)
      setOrgMemberCount(org.member_count ?? 1)
      setTeamMemberCount(team.member_count ?? 1)
      setTeamRole(team.role ?? '')
      setForm({
        github_enabled: true,
        github_url: merged.github.base_url,
        github_pat: '',
        jira_enabled: merged.jira.enabled,
        jira_url: merged.jira.base_url,
        jira_pat: '',
        github_poll_interval: merged.github.poll_interval,
        github_clone_protocol: merged.github.clone_protocol,
        jira_poll_interval: merged.jira.poll_interval,
        jira_projects: projects,
        ai_model: merged.ai.model,
        ai_auto_delegate_enabled: merged.ai.auto_delegate_enabled,
        max_llm_model_tier: merged.max_llm_model_tier,
      })
      const initialExpanded: Record<string, boolean> = {}
      if (projects.length === 1) {
        initialExpanded[projects[0].key] = true
      }
      setExpandedKeys(initialExpanded)
      if (merged.jira.has_token && merged.jira.base_url) {
        setJiraConnected(true)
        const keys = projects.map((p) => p.key).filter(Boolean)
        if (keys.length > 0) {
          fetchJiraStatuses(keys)
        }
      }
    })
    // fetchJiraStatuses intentionally omitted — this effect is a one-shot
    // mount loader.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // fetchJiraStatuses queries the backend for statuses across the
  // given project list. The backend intersects across projects, so
  // the returned list is the safe set to offer in EVERY project's
  // picker (a status not in every project would fail TransitionTo).
  // Mirrors today's behavior — per-project status autocomplete is v2.
  const fetchJiraStatuses = async (projectKeys?: string[]) => {
    setStatusesLoading(true)
    try {
      const keys = projectKeys || form.jira_projects.map((p) => p.key.trim()).filter(Boolean)
      if (keys.length === 0) return
      const params = keys.map((p) => `project=${encodeURIComponent(p)}`).join('&')
      const res = await fetch(`/api/jira/statuses?${params}`)
      if (res.ok) {
        const statuses: JiraStatus[] = await res.json()
        // Same status list applies to every queried project; mirror
        // it under each key so the per-project pickers can read by key.
        const next: Record<string, JiraStatus[]> = {}
        for (const k of keys) {
          next[k] = statuses
        }
        setJiraStatusesByProject((current) => ({ ...current, ...next }))
      }
    } catch {
      // Non-critical
    } finally {
      setStatusesLoading(false)
    }
  }

  // Jira connect/disconnect live in JiraAccessGroup now; these callbacks
  // own the team-scope follow-up the shared group can't see. On a connect
  // against a different instance URL, the prior team project/status config
  // no longer maps, so it's wiped; disconnect clears it outright.
  const onJiraConnected = (url: string) => {
    if (data && data.jira.base_url && data.jira.base_url !== url) {
      setForm((f) => ({ ...f, jira_projects: [] }))
      setJiraStatusesByProject({})
    }
    setJiraConnected(true)
  }

  const onJiraDisconnected = () => {
    setJiraConnected(false)
    setJiraStatusesByProject({})
    setForm((f) => ({ ...f, jira_enabled: false, jira_projects: [] }))
  }

  // Spread a shared field-group patch into the flat form state. The group
  // patches are keyed by the org_settings field names, so this is a plain
  // merge — no per-field mapping.
  const patchForm = (patch: Partial<typeof form>) => setForm((f) => ({ ...f, ...patch }))

  const update = (field: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [field]: e.target.value }))

  // updateProject swaps the project at index i with the produced patch.
  // Centralized so each picker doesn't re-derive the splice.
  const updateProject = (i: number, patch: Partial<JiraProjectConfig>) => {
    setForm((f) => {
      const next = f.jira_projects.slice()
      next[i] = { ...next[i], ...patch }
      return { ...f, jira_projects: next }
    })
  }

  const addProject = () => {
    // New section is appended at index === current length, so we stamp
    // idx_${length} into expandedKeys — that's the same key isExpanded
    // will read on the next render. Using a synthesized __new_... token
    // would never be reached by isExpanded and the new section would
    // render collapsed (the bug this comment guards against).
    const newIndex = form.jira_projects.length
    setForm((f) => ({ ...f, jira_projects: [...f.jira_projects, emptyProject('')] }))
    setExpandedKeys((m) => ({ ...m, [`idx_${newIndex}`]: true }))
  }

  const removeProject = (i: number) => {
    setForm((f) => {
      const next = f.jira_projects.slice()
      next.splice(i, 1)
      return { ...f, jira_projects: next }
    })
    // Shift expandedKeys down for every index above the removed slot —
    // otherwise idx_${i} still maps to the entry that was at i+1, which
    // is now at i. Drop the highest-index entry since it no longer
    // exists.
    setExpandedKeys((m) => {
      const next: Record<string, boolean> = {}
      for (const [k, v] of Object.entries(m)) {
        if (!k.startsWith('idx_')) {
          next[k] = v
          continue
        }
        const idx = Number(k.slice('idx_'.length))
        if (Number.isNaN(idx)) {
          next[k] = v
        } else if (idx < i) {
          next[k] = v
        } else if (idx > i) {
          next[`idx_${idx - 1}`] = v
        }
        // idx === i: dropped along with the removed project.
      }
      return next
    })
  }

  // Section expansion uses the project's index because the key field
  // is user-editable during the same render and would otherwise lose
  // its open/closed state every keystroke.
  const toggleExpanded = (i: number) => {
    const id = `idx_${i}`
    setExpandedKeys((m) => ({ ...m, [id]: !m[id] }))
  }

  const isExpanded = (i: number): boolean => {
    const id = `idx_${i}`
    if (id in expandedKeys) return expandedKeys[id]
    // Fallback for projects that came from initial load: keyed by
    // project.key.
    const key = form.jira_projects[i]?.key
    return key ? expandedKeys[key] === true : false
  }

  // saveTeam / saveOrg each POST a single scope and fold the saved values
  // back into `data` so the next change-detection compares against current
  // state, not the stale mount snapshot. They're independent permission
  // domains on separate endpoints — there's no transaction spanning both.
  const saveTeam = async (): Promise<boolean> => {
    const projects = form.jira_projects
      .map((p) => ({ ...p, key: p.key.trim() }))
      .filter((p) => p.key !== '')
    const res = await fetch('/api/settings/team/default', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ai_model: form.ai_model,
        ai_auto_delegate_enabled: form.ai_auto_delegate_enabled,
        jira_projects: projects,
      }),
    })
    if (!res.ok) {
      toast.error(await readError(res, 'Failed to save team settings'))
      return false
    }
    const body = (await res.json().catch(() => null)) as { warning?: string } | null
    setData((d) =>
      d
        ? {
            ...d,
            jira: { ...d.jira, projects },
            ai: {
              ...d.ai,
              model: form.ai_model,
              auto_delegate_enabled: form.ai_auto_delegate_enabled,
            },
          }
        : d,
    )
    // The org cap can clamp the team default; the save still succeeds, the
    // warning just tells the team the effective model differs from the pick.
    if (body?.warning) toast.info(body.warning)
    return true
  }

  // saveOrg persists the org-level field group through the shared
  // saveOrgConfig helper (the same POST /api/settings/org path the
  // create-configure step and Setup use), then folds the saved values back
  // into `data` so the next change-detection compares against current
  // state, not the stale mount snapshot.
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
    // Lowering the cap below the default team's preference still saves;
    // the warning surfaces that its effective model dropped.
    if (result.warning) toast.info(result.warning)
    return true
  }

  // runSave drives the requested scope(s). Team runs first: its validation
  // is pure server-side (project rules, dup keys) with no external calls,
  // so it fails before the org POST's PAT/SSH work commits anything.
  const runSave = async (scopes: SettingsTab[]) => {
    setSaving(true)
    try {
      if (scopes.includes('team') && !(await saveTeam())) return
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
    const projects = form.jira_projects
      .map((p) => ({ ...p, key: p.key.trim() }))
      .filter((p) => p.key !== '')
    const teamChanged =
      form.ai_model !== data?.ai.model ||
      form.ai_auto_delegate_enabled !== data?.ai.auto_delegate_enabled ||
      JSON.stringify(projects) !== JSON.stringify(data?.jira.projects ?? [])
    const orgChanged =
      !!form.github_pat ||
      !!form.jira_pat ||
      form.github_url !== (data?.github.base_url ?? '') ||
      form.github_poll_interval !== data?.github.poll_interval ||
      form.github_clone_protocol !== data?.github.clone_protocol ||
      form.jira_url !== (data?.jira.base_url ?? '') ||
      form.jira_poll_interval !== data?.jira.poll_interval ||
      form.max_llm_model_tier !== (data?.max_llm_model_tier ?? '')
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
      <div className="flex items-center justify-center min-h-[50vh]">
        <p className="text-text-tertiary text-[13px]">Loading settings...</p>
      </div>
    )
  }

  // Save button is disabled when Jira is connected but any tracked
  // project has incomplete rules — same gating as the pre-SKY-272
  // form, just applied per project. This gates the team scope (projects).
  const incompleteProjects = jiraConnected
    ? form.jira_projects.filter((p) => p.key.trim() !== '' && !projectIsComplete(p)).length
    : 0
  const hasAnyValidProject =
    !jiraConnected || form.jira_projects.some((p) => p.key.trim() !== '' && projectIsComplete(p))
  const teamSaveBlocked = jiraConnected && (!hasAnyValidProject || incompleteProjects > 0)

  // ---- Section renderers (closures over current state; no prop drilling).

  // GitHub access (org scope): base URL + PAT + clone protocol, plus the
  // App-registration alternative — all shared with the create-configure
  // step and (PAT path) the local Setup wizard.
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

  const renderJiraProjects = () => (
    <Section>
      <h2 className="text-[13px] font-medium text-text-secondary mb-4">Jira projects</h2>
      {!jiraConnected ? (
        <p className="text-[12px] text-text-tertiary italic">
          Connect Jira under Workspace settings before configuring tracked projects.
        </p>
      ) : (
        <div className="space-y-2">
          {form.jira_projects.length === 0 && (
            <p className="text-[12px] text-text-tertiary italic">
              No Jira projects configured. Click &ldquo;Add project&rdquo; to start.
            </p>
          )}
          {form.jira_projects.map((project, i) => {
            const statuses = jiraStatusesByProject[project.key] || []
            const complete = projectIsComplete(project)
            const expanded = isExpanded(i)
            return (
              <div key={i} className="rounded-xl border border-border-subtle bg-white/40">
                <div className="flex items-center gap-2 px-3 py-2">
                  <button
                    type="button"
                    onClick={() => toggleExpanded(i)}
                    className="text-text-tertiary hover:text-text-secondary"
                    aria-label={expanded ? 'Collapse project' : 'Expand project'}
                  >
                    {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                  </button>
                  <input
                    type="text"
                    placeholder="PROJ"
                    value={project.key}
                    onChange={(e) => updateProject(i, { key: e.target.value })}
                    className="flex-1 bg-transparent border-0 focus:outline-none text-[13px] font-medium text-text-primary placeholder-text-tertiary"
                  />
                  {project.key.trim() !== '' && (
                    <span
                      className={`text-[10px] uppercase tracking-wide ${
                        complete ? 'text-claim' : 'text-snooze'
                      }`}
                    >
                      {complete ? 'Ready' : 'Needs rules'}
                    </span>
                  )}
                  <button
                    type="button"
                    onClick={() => removeProject(i)}
                    className="text-text-tertiary hover:text-dismiss"
                    aria-label="Remove project"
                  >
                    <Trash2 size={14} />
                  </button>
                </div>

                {expanded && (
                  <div className="px-4 pb-4 pt-1 space-y-3">
                    <div className="flex items-center justify-between">
                      <p className="text-[11px] text-text-tertiary">
                        {statuses.length > 0
                          ? `${statuses.length} statuses available`
                          : 'Click Fetch Statuses to load options'}
                      </p>
                      <button
                        type="button"
                        onClick={() => fetchJiraStatuses([project.key].filter(Boolean))}
                        disabled={statusesLoading || !project.key.trim()}
                        className="shrink-0 text-[11px] text-accent hover:text-accent/80 disabled:opacity-40 border border-accent/20 rounded-xl px-3 py-1 transition-colors"
                      >
                        {statusesLoading ? 'Loading...' : 'Fetch Statuses'}
                      </button>
                    </div>

                    {statuses.length > 0 && (
                      <div className="space-y-4 pt-1">
                        <JiraStatusRule
                          label="Pickup"
                          description="Poll for unassigned tickets in these states."
                          allStatuses={statuses}
                          value={project.pickup}
                          onChange={(v) => updateProject(i, { pickup: v })}
                          requireCanonical={false}
                        />
                        <JiraStatusRule
                          label="In progress"
                          description="Count as actively being worked on."
                          allStatuses={statuses}
                          value={project.in_progress}
                          onChange={(v) => updateProject(i, { in_progress: v })}
                          requireCanonical={true}
                          canonicalPrompt="Claim →"
                        />
                        <JiraStatusRule
                          label="Done"
                          description="Count as complete (add every variant — e.g. Resolved + Verified)."
                          allStatuses={statuses}
                          value={project.done}
                          onChange={(v) => updateProject(i, { done: v })}
                          requireCanonical={true}
                          canonicalPrompt="Complete →"
                        />
                      </div>
                    )}
                  </div>
                )}
              </div>
            )
          })}
          <button
            type="button"
            onClick={addProject}
            className="w-full text-[12px] text-accent hover:text-accent/80 border border-dashed border-accent/30 rounded-xl px-3 py-2 transition-colors"
          >
            + Add project
          </button>
        </div>
      )}
    </Section>
  )

  const renderAI = () => (
    <Section>
      <h2 className="text-[13px] font-medium text-text-secondary mb-4">AI</h2>
      <div className="space-y-3">
        <Field label="Delegation model">
          <select value={form.ai_model} onChange={update('ai_model')} className={inputClass}>
            <option value="haiku">Haiku (fast, cheap)</option>
            <option value="sonnet">Sonnet (balanced)</option>
            <option value="opus">Opus (most capable)</option>
          </select>
        </Field>
        <div className="flex items-center justify-between">
          <div>
            <p className="text-[13px] text-text-primary">Auto-delegation</p>
            <p className="text-[11px] text-text-tertiary mt-0.5">
              Automatically delegate tasks when matching triggers fire
            </p>
          </div>
          <button
            type="button"
            role="switch"
            aria-checked={form.ai_auto_delegate_enabled}
            onClick={() =>
              setForm((f) => ({ ...f, ai_auto_delegate_enabled: !f.ai_auto_delegate_enabled }))
            }
            className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
              form.ai_auto_delegate_enabled ? 'bg-accent' : 'bg-black/[0.08]'
            }`}
          >
            <span
              className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-sm transform transition-transform ${
                form.ai_auto_delegate_enabled ? 'translate-x-4' : 'translate-x-0'
              }`}
            />
          </button>
        </div>
      </div>
    </Section>
  )

  // Workspace (org) scope: a hard ceiling over every team's model choice.
  // The team default lives in renderAI (team scope); this caps it.
  const renderModelCap = () => (
    <ModelGroup value={{ max_llm_model_tier: form.max_llm_model_tier }} onChange={patchForm} />
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
          window.location.href = '/setup'
        }}
        className="text-[13px] text-dismiss hover:text-dismiss/80 border border-dismiss/20 hover:border-dismiss/30 rounded-xl px-4 py-2 transition-colors"
      >
        Clear All Tokens
      </button>
    </Section>
  )

  // "Add team" (org admin, hosted-only). Org-scope settings; hidden in
  // local mode and for non-admins. The entry point a solo hosted user
  // uses to create a 2nd team, which then flips the count-gated selectors
  // on across the app.
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
      {renderAI()}
      {renderJiraProjects()}
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
          <GitHubGroupsEditor teamId="default" canEdit={isTeamAdmin} />
          {renderJiraAccess()}
          {renderPollerTiming()}
          {renderJiraProjects()}
          {renderAI()}
          {renderModelCap()}
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
            renders its own <form> (Add team), and nesting forms is
            invalid HTML — the inner submit would bubble as the outer
            Save Settings. renderIntegrations / renderDanger are
            self-contained sections with their own actions too, so they
            belong outside the save form regardless. */}
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
          <GitHubGroupsEditor teamId="default" canEdit={isTeamAdmin} />
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

// ReadOnlyNotice explains why a scope's fields are disabled. Per SKY-358 we
// disable (not hide) the fields and surface the policy, so a non-admin sees
// what exists and who can change it rather than a missing tab.
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
