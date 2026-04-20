import { useState, useEffect } from 'react'
import JiraStatusRule, { type JiraStatusRuleValue } from '../components/JiraStatusRule'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'

interface JiraStatus {
  id: string
  name: string
}

interface SettingsData {
  github: {
    enabled: boolean
    base_url: string
    has_token: boolean
    poll_interval: string
  }
  jira: {
    enabled: boolean
    base_url: string
    has_token: boolean
    poll_interval: string
    projects: string[]
    pickup: JiraStatusRuleValue
    in_progress: JiraStatusRuleValue
    done: JiraStatusRuleValue
  }
  server: { port: number }
  ai: {
    model: string
    reprioritize_threshold: number
    preference_update_interval: number
    auto_delegate_enabled: boolean
  }
}

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
    jira_poll_interval: string
    jira_projects: string
    jira_pickup: JiraStatusRuleValue
    jira_in_progress: JiraStatusRuleValue
    jira_done: JiraStatusRuleValue
    ai_model: string
    ai_auto_delegate_enabled: boolean
    server_port: number
  }>({
    github_enabled: true,
    github_url: '',
    github_pat: '',
    jira_enabled: false,
    jira_url: '',
    jira_pat: '',
    github_poll_interval: '60s',
    jira_poll_interval: '60s',
    jira_projects: '',
    jira_pickup: { members: [] },
    jira_in_progress: { members: [] },
    jira_done: { members: [] },
    ai_model: 'sonnet',
    ai_auto_delegate_enabled: true,
    server_port: 3000,
  })
  const [saving, setSaving] = useState(false)
  const [jiraStatuses, setJiraStatuses] = useState<JiraStatus[]>([])
  const [statusesLoading, setStatusesLoading] = useState(false)
  const [jiraConnected, setJiraConnected] = useState(false)
  const [jiraConnecting, setJiraConnecting] = useState(false)
  const [jiraConnectError, setJiraConnectError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/settings')
      .then((r) => r.json())
      .then((d: SettingsData) => {
        setData(d)
        setForm({
          github_enabled: true,
          github_url: d.github.base_url || '',
          github_pat: '',
          jira_enabled: d.jira.enabled,
          jira_url: d.jira.base_url || '',
          jira_pat: '',
          github_poll_interval: d.github.poll_interval,
          jira_poll_interval: d.jira.poll_interval,
          jira_projects: (d.jira.projects || []).join(', '),
          jira_pickup: d.jira.pickup || { members: [] },
          jira_in_progress: d.jira.in_progress || { members: [] },
          jira_done: d.jira.done || { members: [] },
          ai_model: d.ai.model,
          ai_auto_delegate_enabled: d.ai.auto_delegate_enabled,
          server_port: d.server.port,
        })
        if (d.jira.has_token && d.jira.base_url) {
          setJiraConnected(true)
          if (d.jira.projects?.length > 0) {
            fetchJiraStatuses(d.jira.projects)
          }
        }
      })
    // fetchJiraStatuses intentionally omitted — this effect is a one-shot
    // mount loader, and the call always passes projects explicitly so no
    // closure staleness is possible.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const fetchJiraStatuses = async (projects?: string[]) => {
    setStatusesLoading(true)
    try {
      const projectList =
        projects ||
        form.jira_projects
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean)
      if (projectList.length === 0) return
      const params = projectList.map((p) => `project=${encodeURIComponent(p)}`).join('&')
      const res = await fetch(`/api/jira/statuses?${params}`)
      if (res.ok) {
        const statuses: JiraStatus[] = await res.json()
        setJiraStatuses(statuses)
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
          jira_projects: '',
          jira_pickup: { members: [] },
          jira_in_progress: { members: [] },
          jira_done: { members: [] },
        }))
        setJiraStatuses([])
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
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          github_enabled: form.github_enabled,
          github_url: form.github_url,
          github_poll_interval: form.github_poll_interval,
          jira_enabled: false,
          ai_model: form.ai_model,
          ai_auto_delegate_enabled: form.ai_auto_delegate_enabled,
          server_port: form.server_port,
        }),
      })
      if (!res.ok) return
    } catch {
      return
    }
    setJiraConnected(false)
    setJiraStatuses([])
    setForm((f) => ({
      ...f,
      jira_enabled: false,
      jira_url: '',
      jira_pat: '',
      jira_projects: '',
      jira_pickup: { members: [] },
      jira_in_progress: { members: [] },
      jira_done: { members: [] },
    }))
  }

  const update = (field: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [field]: e.target.value }))

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)

    const projects = form.jira_projects
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)

    try {
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          github_enabled: form.github_enabled,
          github_url: form.github_url,
          github_pat: form.github_pat || undefined,
          jira_enabled: jiraConnected,
          jira_url: form.jira_url,
          jira_pat: form.jira_pat || undefined,
          github_poll_interval: form.github_poll_interval,
          jira_poll_interval: form.jira_poll_interval,
          jira_projects: projects,
          jira_pickup: form.jira_pickup,
          jira_in_progress: form.jira_in_progress,
          jira_done: form.jira_done,
          ai_model: form.ai_model,
          ai_auto_delegate_enabled: form.ai_auto_delegate_enabled,
          server_port: form.server_port,
        }),
      })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to save settings'))
      } else {
        toast.success('Settings saved')
        setForm((f) => ({ ...f, github_pat: '', jira_pat: '' }))
      }
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
            /* Stage 2: Configure projects & statuses */
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
              <Field label="Projects (comma-separated)">
                <div className="flex gap-2">
                  <input
                    type="text"
                    placeholder="PROJ, INFRA"
                    value={form.jira_projects}
                    onChange={update('jira_projects')}
                    className={inputClass + ' flex-1'}
                  />
                  <button
                    type="button"
                    onClick={() => fetchJiraStatuses()}
                    disabled={statusesLoading || !form.jira_projects.trim()}
                    className="shrink-0 text-[11px] text-accent hover:text-accent/80 disabled:opacity-40 border border-accent/20 rounded-xl px-3 py-2 transition-colors"
                  >
                    {statusesLoading ? 'Loading...' : 'Fetch Statuses'}
                  </button>
                </div>
              </Field>
              {jiraStatuses.length > 0 && (
                <div className="space-y-4 pt-2">
                  <JiraStatusRule
                    label="Pickup"
                    description="Poll for unassigned tickets in these states."
                    allStatuses={jiraStatuses}
                    value={form.jira_pickup}
                    onChange={(v) => setForm((f) => ({ ...f, jira_pickup: v }))}
                    requireCanonical={false}
                  />
                  <JiraStatusRule
                    label="In progress"
                    description="Count as actively being worked on."
                    allStatuses={jiraStatuses}
                    value={form.jira_in_progress}
                    onChange={(v) => setForm((f) => ({ ...f, jira_in_progress: v }))}
                    requireCanonical={true}
                    canonicalPrompt="Claim →"
                  />
                  <JiraStatusRule
                    label="Done"
                    description="Count as complete (add every variant — e.g. Resolved + Verified)."
                    allStatuses={jiraStatuses}
                    value={form.jira_done}
                    onChange={(v) => setForm((f) => ({ ...f, jira_done: v }))}
                    requireCanonical={true}
                    canonicalPrompt="Complete →"
                  />
                </div>
              )}
            </div>
          )}
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
          disabled={
            saving ||
            (jiraConnected &&
              (!form.jira_projects.trim() ||
                form.jira_pickup.members.length === 0 ||
                form.jira_in_progress.members.length === 0 ||
                !form.jira_in_progress.canonical ||
                form.jira_done.members.length === 0 ||
                !form.jira_done.canonical))
          }
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
              await fetch('/api/auth', { method: 'DELETE' })
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
