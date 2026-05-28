import { useState, useEffect } from 'react'
import { ChevronDown, ChevronRight, Trash2 } from 'lucide-react'
import JiraStatusRule, { type JiraStatusRuleValue } from '../components/JiraStatusRule'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'
import { getStoredTheme, setTheme, type ThemeMode } from '../lib/theme'

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

interface OrgSettingsData {
  github_base_url: string
  github_poll_interval: string
  github_clone_protocol: 'ssh' | 'https'
  has_github_pat: boolean
  jira_base_url: string
  jira_poll_interval: string
  has_jira_pat: boolean
  max_llm_model_tier?: string
  has_anthropic_api_key: boolean
  has_bedrock_credentials: boolean
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
  const [data, setData] = useState<SettingsData | null>(null)
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
  const [jiraConnecting, setJiraConnecting] = useState(false)
  const [jiraConnectError, setJiraConnectError] = useState<string | null>(null)
  const [theme, setThemeState] = useState<ThemeMode>(() => getStoredTheme())
  const [sshTestState, setSshTestState] = useState<
    { kind: 'idle' } | { kind: 'running' } | { kind: 'ok' } | { kind: 'fail'; stderr: string }
  >({ kind: 'idle' })

  // Test SSH preflight on demand. Doesn't save settings — purely
  // diagnostic. Useful for users on the Settings page after they've
  // toggled SSH (or fixed their key) and want a clean confirmation
  // before saving and watching the bootstrap re-run.
  const testSSH = async () => {
    setSshTestState({ kind: 'running' })
    try {
      const res = await fetch('/api/github/preflight-ssh', { method: 'POST' })
      if (!res.ok) {
        setSshTestState({ kind: 'fail', stderr: `Server returned ${res.status}` })
        return
      }
      const data = (await res.json()) as { ok: boolean; stderr?: string }
      if (data.ok) {
        setSshTestState({ kind: 'ok' })
      } else {
        setSshTestState({ kind: 'fail', stderr: data.stderr || 'Preflight failed.' })
      }
    } catch (err) {
      setSshTestState({ kind: 'fail', stderr: (err as Error).message })
    }
  }

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
      }
      setData(merged)
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

  const connectJira = async () => {
    setJiraConnecting(true)
    setJiraConnectError(null)
    try {
      const res = await fetch('/api/jira/connect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: form.jira_url, pat: form.jira_pat }),
      })
      const body = await res.json()
      if (!res.ok) {
        setJiraConnectError(body.error || 'Connection failed')
        return
      }
      // If URL changed from what was previously stored, wipe project/status config
      if (data && data.jira.base_url && data.jira.base_url !== form.jira_url) {
        setForm((f) => ({
          ...f,
          jira_pat: '',
          jira_projects: [],
        }))
        setJiraStatusesByProject({})
      } else {
        setForm((f) => ({ ...f, jira_pat: '' }))
      }
      setJiraConnected(true)
    } catch {
      setJiraConnectError('Could not connect to server')
    } finally {
      setJiraConnecting(false)
    }
  }

  const disconnectJira = async () => {
    try {
      // DELETE /api/integrations/jira clears the SecretStore entries
      // (URL + PAT) but leaves org_settings.jira_base_url populated.
      // Follow with an explicit org POST so the URL column also clears,
      // otherwise reloading the page would show the stale URL prefilled
      // with has_jira_pat:false.
      const credRes = await fetch('/api/integrations/jira', { method: 'DELETE' })
      if (!credRes.ok) return
      const orgRes = await fetch('/api/settings/org', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ jira_base_url: '' }),
      })
      if (!orgRes.ok) return
    } catch {
      return
    }
    setJiraConnected(false)
    setJiraStatusesByProject({})
    setForm((f) => ({
      ...f,
      jira_enabled: false,
      jira_url: '',
      jira_pat: '',
      jira_projects: [],
    }))
  }

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

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)

    // Trim project keys before sending. Empty-key entries are
    // dropped — the user added a section but never typed a key.
    const projects = form.jira_projects
      .map((p) => ({ ...p, key: p.key.trim() }))
      .filter((p) => p.key !== '')

    // Org and team are independent permission domains (org-admin vs
    // team-admin) on separate endpoints, so there's no single
    // transaction spanning both. We POST only the scope(s) whose fields
    // actually changed — that way an org-admin-only user saving org
    // fields never trips the team endpoint's requireTeamAdmin (and vice
    // versa), and a single-domain edit can't half-commit. For a genuine
    // dual-domain edit we run team first: its validation is pure
    // server-side (project rules, dup keys) with no external calls, so
    // it fails before the org POST's PAT/SSH work commits anything. On
    // success we fold the saved values into `data` so the next save's
    // change-detection compares against current state, not the stale
    // mount snapshot.
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
      form.jira_poll_interval !== data?.jira.poll_interval

    try {
      const jsonHeaders = { 'Content-Type': 'application/json' }

      if (teamChanged) {
        const teamRes = await fetch('/api/settings/team/default', {
          method: 'POST',
          headers: jsonHeaders,
          body: JSON.stringify({
            ai_model: form.ai_model,
            ai_auto_delegate_enabled: form.ai_auto_delegate_enabled,
            jira_projects: projects,
          }),
        })
        if (!teamRes.ok) {
          toast.error(await readError(teamRes, 'Failed to save team settings'))
          return
        }
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
      }

      if (orgChanged) {
        const orgRes = await fetch('/api/settings/org', {
          method: 'POST',
          headers: jsonHeaders,
          body: JSON.stringify({
            github_base_url: form.github_url,
            github_pat: form.github_pat || undefined,
            github_poll_interval: form.github_poll_interval,
            github_clone_protocol: form.github_clone_protocol,
            jira_base_url: form.jira_url,
            jira_pat: form.jira_pat || undefined,
            jira_poll_interval: form.jira_poll_interval,
          }),
        })
        if (!orgRes.ok) {
          toast.error(await readError(orgRes, 'Failed to save settings'))
          return
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
              }
            : d,
        )
      }

      toast.success('Settings saved')
      setForm((f) => ({ ...f, github_pat: '', jira_pat: '' }))
    } catch (err) {
      toast.error(`Could not save settings: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
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
  // form, just applied per project.
  const incompleteProjects = jiraConnected
    ? form.jira_projects.filter((p) => p.key.trim() !== '' && !projectIsComplete(p)).length
    : 0
  const hasAnyValidProject =
    !jiraConnected || form.jira_projects.some((p) => p.key.trim() !== '' && projectIsComplete(p))

  return (
    <div className="max-w-2xl mx-auto">
      <h1 className="text-[22px] font-semibold text-text-primary tracking-tight mb-6">Settings</h1>
      <form onSubmit={save} className="space-y-5">
        {/* GitHub (always on) */}
        <Section>
          <h2 className="text-[13px] font-medium text-text-secondary mb-4">GitHub</h2>
          <div className="space-y-3">
            <Field label="Base URL">
              <input
                type="url"
                placeholder="https://github.com"
                value={form.github_url}
                onChange={update('github_url')}
                className={inputClass}
              />
            </Field>
            <Field label={`Token${data.github.has_token ? ' (leave blank to keep current)' : ''}`}>
              <input
                type="password"
                placeholder={data.github.has_token ? '••••••••' : 'GitHub Personal Access Token'}
                value={form.github_pat}
                onChange={update('github_pat')}
                className={inputClass}
              />
              <p className="text-[11px] text-text-tertiary mt-1">
                Requires a{' '}
                <a
                  href="https://github.com/settings/tokens/new?scopes=repo,read:org&description=Triage+Factory"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-accent hover:underline"
                >
                  classic PAT
                </a>{' '}
                with <code className="text-text-secondary">repo</code> and{' '}
                <code className="text-text-secondary">read:org</code> scopes.{' '}
                <code className="text-text-secondary">read:org</code> is needed to resolve your team
                memberships so review requests sent to your teams (e.g. CODEOWNERS) surface as tasks
                — without it, only PRs that request you individually will show up.
              </p>
            </Field>
            <Field label="Poll interval">
              <select
                value={form.github_poll_interval}
                onChange={update('github_poll_interval')}
                className={inputClass}
              >
                <option value="30s">30 seconds</option>
                <option value="1m0s">1 minute</option>
                <option value="2m0s">2 minutes</option>
                <option value="5m0s">5 minutes</option>
              </select>
            </Field>
            <Field label="Clone protocol">
              <div className="flex items-center gap-3">
                <div className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5">
                  {(['ssh', 'https'] as const).map((p) => (
                    <button
                      key={p}
                      type="button"
                      onClick={() => {
                        setForm((f) => ({ ...f, github_clone_protocol: p }))
                        setSshTestState({ kind: 'idle' })
                      }}
                      className={`px-3 py-1 text-[12px] font-medium rounded-md transition-colors ${
                        form.github_clone_protocol === p
                          ? 'bg-white text-text-primary shadow-sm'
                          : 'text-text-tertiary hover:text-text-secondary'
                      }`}
                    >
                      {p.toUpperCase()}
                    </button>
                  ))}
                </div>
                <button
                  type="button"
                  onClick={testSSH}
                  disabled={sshTestState.kind === 'running'}
                  className="text-[11px] text-accent hover:underline disabled:opacity-50"
                >
                  {sshTestState.kind === 'running' ? 'Testing...' : 'Test SSH connection'}
                </button>
              </div>
              <p className="text-[11px] text-text-tertiary mt-1.5 leading-relaxed">
                Your token is still required for the GitHub API. The protocol only affects how
                Triage Factory clones repos to your machine. Saving the toggle re-clones bare repos
                with the new origin URL.
              </p>
              {sshTestState.kind === 'ok' && (
                <p className="text-[11px] text-[var(--color-claim)] mt-1.5">
                  ✓ SSH preflight succeeded — git@
                  {(() => {
                    try {
                      return new URL(form.github_url).hostname || 'github.com'
                    } catch {
                      return 'github.com'
                    }
                  })()}{' '}
                  is reachable with your key.
                </p>
              )}
              {sshTestState.kind === 'fail' && (
                <pre
                  className="
                  mt-1.5 max-h-[120px] overflow-auto rounded
                  bg-[var(--color-dismiss)]/10 p-2 text-[11px]
                  text-[var(--color-dismiss)] whitespace-pre-wrap break-words
                "
                >
                  {sshTestState.stderr}
                </pre>
              )}
            </Field>
          </div>
        </Section>

        {/* Jira */}
        <Section>
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-[13px] font-medium text-text-secondary">Jira</h2>
            {jiraConnected && (
              <button
                type="button"
                onClick={disconnectJira}
                className="text-[11px] text-dismiss hover:text-dismiss/80 transition-colors"
              >
                Disconnect
              </button>
            )}
          </div>

          {!jiraConnected ? (
            /* Stage 1: Connect credentials */
            <div className="space-y-3">
              <Field label="Base URL">
                <input
                  type="url"
                  placeholder="https://jira.yourcompany.com"
                  value={form.jira_url}
                  onChange={update('jira_url')}
                  className={inputClass}
                />
              </Field>
              <Field label="Personal Access Token">
                <input
                  type="password"
                  placeholder="Jira Personal Access Token"
                  value={form.jira_pat}
                  onChange={update('jira_pat')}
                  className={inputClass}
                />
              </Field>
              {jiraConnectError && (
                <div className="rounded-xl px-4 py-2.5 text-[13px] bg-dismiss/[0.08] border border-dismiss/20 text-dismiss">
                  {jiraConnectError}
                </div>
              )}
              <button
                type="button"
                onClick={connectJira}
                disabled={jiraConnecting || !form.jira_url.trim() || !form.jira_pat.trim()}
                className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
              >
                {jiraConnecting ? 'Connecting...' : 'Connect'}
              </button>
            </div>
          ) : (
            /* Stage 2: Configure projects & statuses (per-project) */
            <div className="space-y-3">
              <div className="flex items-center gap-2 rounded-xl bg-claim/[0.06] border border-claim/15 px-4 py-2.5">
                <div className="w-1.5 h-1.5 rounded-full bg-claim shrink-0" />
                <span className="text-[12px] text-claim">
                  Connected to {form.jira_url.replace(/^https?:\/\//, '')}
                </span>
              </div>
              <Field label="Poll interval">
                <select
                  value={form.jira_poll_interval}
                  onChange={update('jira_poll_interval')}
                  className={inputClass}
                >
                  <option value="30s">30 seconds</option>
                  <option value="1m0s">1 minute</option>
                  <option value="2m0s">2 minutes</option>
                  <option value="5m0s">5 minutes</option>
                </select>
              </Field>

              {/* Per-project sections. Each carries its own rules. */}
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
            </div>
          )}
        </Section>

        {/* Appearance */}
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

        {/* AI */}
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

        <button
          type="submit"
          disabled={saving || (jiraConnected && (!hasAnyValidProject || incompleteProjects > 0))}
          className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          {saving ? 'Saving...' : 'Save Settings'}
        </button>

        {/* Integrations */}
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
                  const data = await res.json()
                  if (data.imported > 0) {
                    toast.success(
                      `Imported ${data.imported} skill${data.imported !== 1 ? 's' : ''} (${data.skipped} already imported)`,
                    )
                  } else {
                    toast.info(
                      `No new skills found (${data.scanned} scanned, ${data.skipped} already imported)`,
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

        {/* Danger zone */}
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
      </form>
    </div>
  )
}

const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors'

function Section({ children, danger }: { children: React.ReactNode; danger?: boolean }) {
  return (
    <section
      className={`backdrop-blur-xl bg-surface-raised border rounded-2xl p-6 shadow-sm shadow-black/[0.03] ${
        danger ? 'border-dismiss/15' : 'border-border-glass'
      }`}
    >
      {children}
    </section>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span className="text-[11px] text-text-tertiary mb-1.5 block">{label}</span>
      {children}
    </label>
  )
}
