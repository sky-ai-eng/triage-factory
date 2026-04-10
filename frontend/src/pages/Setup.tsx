import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import RepoPickerModal from '../components/RepoPickerModal'

interface JiraStatus {
  id: string
  name: string
}

type Step = 'github' | 'repos' | 'integrations'

export default function Setup() {
  const navigate = useNavigate()
  const [step, setStep] = useState<Step>('github')

  // GitHub (mandatory)
  const [githubForm, setGithubForm] = useState({ url: '', pat: '' })

  // Integrations (optional)
  const [jiraEnabled, setJiraEnabled] = useState(false)
  const [jiraForm, setJiraForm] = useState({
    url: '',
    pat: '',
    projects: '',
    pickup_statuses: [] as string[],
    in_progress_status: '',
  })
  const [jiraStatuses, setJiraStatuses] = useState<JiraStatus[]>([])
  const [statusesLoading, setStatusesLoading] = useState(false)

  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  // Step 1: GitHub credentials
  const canSubmitGitHub = githubForm.url.trim() !== '' && githubForm.pat.trim() !== ''

  const submitGitHub = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      const res = await fetch('/api/auth/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          github_url: githubForm.url,
          github_pat: githubForm.pat,
        }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Setup failed')
        return
      }
      setStep('repos')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  // Step 2: Repo selection
  const saveRepos = async (repos: string[]) => {
    setLoading(true)
    setError('')
    try {
      const res = await fetch('/api/repos', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ repos }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to save repos')
        setLoading(false)
        return
      }
      setStep('integrations')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  // Step 3: Optional integrations
  const fetchStatuses = async () => {
    const projects = jiraForm.projects.split(',').map((s) => s.trim()).filter(Boolean)
    if (projects.length === 0) return
    setStatusesLoading(true)
    try {
      const params = projects.map((p) => `project=${encodeURIComponent(p)}`).join('&')
      const res = await fetch(`/api/jira/statuses?${params}`)
      if (res.ok) {
        setJiraStatuses(await res.json())
      } else {
        const data = await res.json()
        setError(data.error || 'Failed to fetch statuses')
      }
    } catch {
      setError('Could not fetch Jira statuses')
    } finally {
      setStatusesLoading(false)
    }
  }

  const finishSetup = async () => {
    setLoading(true)
    setError('')
    try {
      if (jiraEnabled) {
        const projects = jiraForm.projects.split(',').map((s) => s.trim()).filter(Boolean)
        const res = await fetch('/api/settings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            jira_enabled: true,
            jira_url: jiraForm.url,
            jira_pat: jiraForm.pat,
            jira_projects: projects,
            jira_pickup_statuses: jiraForm.pickup_statuses,
            jira_in_progress_status: jiraForm.in_progress_status,
          }),
        })
        if (!res.ok) {
          const data = await res.json()
          setError(data.error || 'Failed to save')
          return
        }
      }
      navigate('/')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const updateJira = (field: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setJiraForm((f) => ({ ...f, [field]: e.target.value }))

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">

      {/* Step 1: GitHub credentials (mandatory) */}
      {step === 'github' && (
        <form
          onSubmit={submitGitHub}
          className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]"
        >
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">Connect GitHub</h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Todo Tinder needs access to your GitHub to watch repositories and manage PRs.
              Tokens are stored in your OS keychain and never leave your machine.
            </p>
          </div>

          <div className="space-y-3">
            <input
              type="url"
              placeholder="https://github.yourcompany.com"
              value={githubForm.url}
              onChange={(e) => setGithubForm((f) => ({ ...f, url: e.target.value }))}
              className={inputClass}
            />
            <input
              type="password"
              placeholder="GitHub Personal Access Token"
              value={githubForm.pat}
              onChange={(e) => setGithubForm((f) => ({ ...f, pat: e.target.value }))}
              className={inputClass}
            />
            <p className="text-[11px] text-text-tertiary">
              Requires a{' '}
              <a
                href="https://github.com/settings/tokens/new?scopes=repo,read:org&description=Todo+Tinder"
                target="_blank"
                rel="noopener noreferrer"
                className="text-accent hover:underline"
              >classic PAT</a>
              {' '}with <code className="text-text-secondary">repo</code> and{' '}
              <code className="text-text-secondary">read:org</code> scopes.
            </p>
          </div>

          {error && (
            <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
              {error}
            </div>
          )}

          <button
            type="submit"
            disabled={loading || !canSubmitGitHub}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {loading ? 'Validating...' : 'Connect'}
          </button>
        </form>
      )}

      {/* Step 2: Repo selection (required, min 1) */}
      {step === 'repos' && (
        <RepoPickerModal
          selected={[]}
          onSave={saveRepos}
          onClose={() => {/* cannot skip */}}
          inline
        />
      )}

      {/* Step 3: Optional integrations */}
      {step === 'integrations' && (
        <div className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">Integrations</h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Optionally connect other services. You can always configure these later in Settings.
            </p>
          </div>

          {/* Jira */}
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <span className="text-[13px] font-medium text-text-secondary">Jira</span>
              <Toggle enabled={jiraEnabled} onChange={setJiraEnabled} />
            </div>

            {jiraEnabled && (
              <>
                <input
                  type="url"
                  placeholder="https://jira.yourcompany.com"
                  value={jiraForm.url}
                  onChange={updateJira('url')}
                  className={inputClass}
                />
                <input
                  type="password"
                  placeholder="Jira Personal Access Token"
                  value={jiraForm.pat}
                  onChange={updateJira('pat')}
                  className={inputClass}
                />
                <div>
                  <span className="text-[11px] text-text-tertiary mb-1.5 block">Projects (comma-separated)</span>
                  <div className="flex gap-2">
                    <input
                      type="text"
                      placeholder="PROJ, INFRA"
                      value={jiraForm.projects}
                      onChange={updateJira('projects')}
                      className={inputClass + ' flex-1'}
                    />
                    <button
                      type="button"
                      onClick={fetchStatuses}
                      disabled={statusesLoading || !jiraForm.projects.trim()}
                      className="shrink-0 text-[11px] text-accent hover:text-accent/80 disabled:opacity-40 border border-accent/20 rounded-xl px-3 py-2 transition-colors"
                    >
                      {statusesLoading ? 'Loading...' : 'Fetch Statuses'}
                    </button>
                  </div>
                </div>

                {jiraStatuses.length > 0 && (
                  <>
                    <div>
                      <span className="text-[11px] text-text-tertiary mb-1.5 block">
                        Pickup statuses (poll for unassigned tickets in these states)
                      </span>
                      <div className="flex flex-wrap gap-2">
                        {jiraStatuses.map((s) => (
                          <StatusChip
                            key={s.id}
                            label={s.name}
                            selected={jiraForm.pickup_statuses.includes(s.name)}
                            onClick={() =>
                              setJiraForm((f) => ({
                                ...f,
                                pickup_statuses: f.pickup_statuses.includes(s.name)
                                  ? f.pickup_statuses.filter((n) => n !== s.name)
                                  : [...f.pickup_statuses, s.name],
                              }))
                            }
                          />
                        ))}
                      </div>
                    </div>
                    <div>
                      <span className="text-[11px] text-text-tertiary mb-1.5 block">
                        In-progress status (set when you claim a ticket)
                      </span>
                      <div className="flex flex-wrap gap-2">
                        {jiraStatuses.map((s) => (
                          <StatusChip
                            key={s.id}
                            label={s.name}
                            selected={jiraForm.in_progress_status === s.name}
                            onClick={() =>
                              setJiraForm((f) => ({
                                ...f,
                                in_progress_status: f.in_progress_status === s.name ? '' : s.name,
                              }))
                            }
                          />
                        ))}
                      </div>
                    </div>
                  </>
                )}
              </>
            )}
          </div>

          {error && (
            <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
              {error}
            </div>
          )}

          <button
            type="button"
            onClick={finishSetup}
            disabled={loading}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {loading ? 'Saving...' : jiraEnabled ? 'Save & Start' : 'Start'}
          </button>
        </div>
      )}
    </div>
  )
}

const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors'

function Toggle({ enabled, onChange }: { enabled: boolean; onChange: (v: boolean) => void }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={enabled}
      onClick={() => onChange(!enabled)}
      className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
        enabled ? 'bg-accent' : 'bg-black/[0.08]'
      }`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-sm transform transition-transform ${
          enabled ? 'translate-x-4' : 'translate-x-0'
        }`}
      />
    </button>
  )
}

function StatusChip({ label, selected, onClick }: { label: string; selected: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`text-[11px] px-3 py-1.5 rounded-full border transition-colors ${
        selected
          ? 'bg-accent/[0.1] border-accent/30 text-accent font-medium'
          : 'bg-white/50 border-border-subtle text-text-tertiary hover:text-text-secondary hover:border-border-subtle/80'
      }`}
    >
      {label}
    </button>
  )
}
