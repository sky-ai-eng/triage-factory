import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { CheckCircle2, ChevronRight } from 'lucide-react'
import RepoPickerModal, { type GitHubRepo } from '../components/RepoPickerModal'

interface JiraStatus {
  id: string
  name: string
}

// Setup flow steps:
//   github → repos → integrations → jira-creds → jira-config → (back to integrations)
type Step = 'github' | 'repos' | 'integrations' | 'jira-creds' | 'jira-config'

export default function Setup() {
  const navigate = useNavigate()
  const [step, setStep] = useState<Step>('github')

  // GitHub (mandatory)
  const [githubForm, setGithubForm] = useState({ url: '', pat: '' })

  // Repo selection — cached so navigating back doesn't re-fetch or lose selection
  const [cachedRepos, setCachedRepos] = useState<GitHubRepo[] | undefined>(undefined)
  const [selectedRepos, setSelectedRepos] = useState<string[]>([])

  // Jira state
  const [jiraConnected, setJiraConnected] = useState(false)
  const [jiraForm, setJiraForm] = useState({
    url: '',
    pat: '',
    projects: '',
    pickup_statuses: [] as string[],
    in_progress_status: '',
  })
  const [jiraStatuses, setJiraStatuses] = useState<JiraStatus[]>([])
  const [statusesLoading, setStatusesLoading] = useState(false)
  const [jiraConfigured, setJiraConfigured] = useState(false)

  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  // --- Step 1: GitHub credentials ---
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

  // --- Step 2: Repo selection ---
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
      setSelectedRepos(repos)
      setStep('integrations')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  // --- Step 3: Integrations list ---
  const canContinueFromIntegrations = !jiraConnected || jiraConfigured

  const finishSetup = () => {
    navigate('/')
  }

  // --- Step 4: Jira credentials ---
  const canConnectJira = jiraForm.url.trim() !== '' && jiraForm.pat.trim() !== ''

  const connectJira = async () => {
    setError('')
    setLoading(true)
    try {
      const res = await fetch('/api/jira/connect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          url: jiraForm.url.trim(),
          pat: jiraForm.pat.trim(),
        }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to connect to Jira')
        return
      }
      setJiraForm((f) => ({ ...f, pat: '' }))
      setJiraConnected(true)
      setStep('jira-config')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const backFromJiraCreds = () => {
    // Wipe entered-but-unsubmitted credentials (5.5 requirement)
    if (!jiraConnected) {
      setJiraForm((f) => ({ ...f, url: '', pat: '' }))
    }
    setError('')
    setStep('integrations')
  }

  // --- Step 5: Jira config (projects + statuses) ---
  const fetchStatuses = async () => {
    const projects = jiraForm.projects
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
    if (projects.length === 0) return
    setStatusesLoading(true)
    setError('')
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

  const canSaveJiraConfig =
    jiraForm.projects.trim() !== '' &&
    jiraForm.pickup_statuses.length > 0 &&
    jiraForm.in_progress_status !== ''

  const saveJiraConfig = async () => {
    setError('')
    setLoading(true)
    try {
      const projects = jiraForm.projects
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          // Explicitly preserve GitHub so the backend doesn't clear it.
          github_enabled: true,
          jira_enabled: true,
          jira_projects: projects,
          jira_pickup_statuses: jiraForm.pickup_statuses,
          jira_in_progress_status: jiraForm.in_progress_status,
        }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to save Jira config')
        return
      }
      setJiraConfigured(true)
      setStep('integrations')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const backFromJiraConfig = () => {
    setError('')
    setStep('jira-creds')
  }

  const updateJira = (field: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setJiraForm((f) => ({ ...f, [field]: e.target.value }))

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      {/* Step 1: GitHub credentials */}
      {step === 'github' && (
        <form onSubmit={submitGitHub} className={cardClass}>
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Connect GitHub
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Triage Factory needs access to your GitHub to watch repositories and manage PRs.
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
                href="https://github.com/settings/tokens/new?scopes=repo,read:org&description=Triage+Factory"
                target="_blank"
                rel="noopener noreferrer"
                className="text-accent hover:underline"
              >
                classic PAT
              </a>{' '}
              with <code className="text-text-secondary">repo</code> and{' '}
              <code className="text-text-secondary">read:org</code> scopes.
            </p>
          </div>

          <ErrorBanner error={error} />

          <button type="submit" disabled={loading || !canSubmitGitHub} className={primaryBtnClass}>
            {loading ? 'Validating...' : 'Connect'}
          </button>
        </form>
      )}

      {/* Step 2: Repo selection */}
      {step === 'repos' && (
        <RepoPickerModal
          selected={selectedRepos}
          onSave={saveRepos}
          onClose={() => {
            /* cannot skip */
          }}
          onBack={() => setStep('github')}
          cachedRepos={cachedRepos}
          onReposFetched={setCachedRepos}
          inline
        />
      )}

      {/* Step 3: Integrations list */}
      {step === 'integrations' && (
        <div className={cardClass}>
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Integrations
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Optionally connect other services. You can always configure these later in Settings.
            </p>
          </div>

          {/* Integration rows */}
          <div className="space-y-2">
            <button
              type="button"
              onClick={() => {
                setError('')
                setStep('jira-creds')
              }}
              className="w-full flex items-center justify-between px-4 py-3 rounded-xl border border-border-subtle bg-white/50 hover:border-accent/30 transition-colors text-left"
            >
              <div className="flex items-center gap-3">
                <span className="text-[13px] font-medium text-text-primary">Jira</span>
                {jiraConfigured ? (
                  <span className="flex items-center gap-1 text-[11px] text-claim font-medium">
                    <CheckCircle2 size={12} />
                    Connected
                  </span>
                ) : jiraConnected ? (
                  <span className="text-[11px] text-snooze font-medium">
                    Credentials saved — needs config
                  </span>
                ) : null}
              </div>
              <ChevronRight size={14} className="text-text-tertiary" />
            </button>
          </div>

          <ErrorBanner error={error} />

          <div className="flex gap-3">
            <button type="button" onClick={() => setStep('repos')} className={secondaryBtnClass}>
              Back
            </button>
            <button
              type="button"
              onClick={finishSetup}
              disabled={!canContinueFromIntegrations}
              className={primaryBtnClass}
            >
              Continue
            </button>
          </div>
        </div>
      )}

      {/* Step 4: Jira credentials */}
      {step === 'jira-creds' && (
        <div className={cardClass}>
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Connect Jira
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Enter your Jira instance URL and a Personal Access Token.
            </p>
          </div>

          <div className="space-y-3">
            <input
              type="url"
              placeholder="https://jira.yourcompany.com"
              value={jiraForm.url}
              onChange={updateJira('url')}
              disabled={jiraConnected}
              className={jiraConnected ? inputDisabledClass : inputClass}
            />
            <input
              type="password"
              placeholder="Jira Personal Access Token"
              value={jiraForm.pat}
              onChange={updateJira('pat')}
              disabled={jiraConnected}
              className={jiraConnected ? inputDisabledClass : inputClass}
            />
            {jiraConnected && (
              <p className="text-[11px] text-claim font-medium">
                Connected. Continue to configure projects and statuses.
              </p>
            )}
          </div>

          <ErrorBanner error={error} />

          <div className="flex gap-3">
            <button type="button" onClick={backFromJiraCreds} className={secondaryBtnClass}>
              Back
            </button>
            {jiraConnected ? (
              <button
                type="button"
                onClick={() => {
                  setError('')
                  setStep('jira-config')
                }}
                className={primaryBtnClass}
              >
                Continue
              </button>
            ) : (
              <button
                type="button"
                onClick={connectJira}
                disabled={loading || !canConnectJira}
                className={primaryBtnClass}
              >
                {loading ? 'Connecting...' : 'Connect'}
              </button>
            )}
          </div>
        </div>
      )}

      {/* Step 5: Jira config (projects + statuses) */}
      {step === 'jira-config' && (
        <div className={cardClass}>
          <div>
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Configure Jira
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Choose which projects to poll and how statuses map to your triage workflow.
            </p>
          </div>

          {/* Grayed-out credential fields */}
          <div className="space-y-3">
            <div>
              <span className="text-[11px] text-text-tertiary mb-1.5 block">Instance</span>
              <input type="url" value={jiraForm.url} disabled className={inputDisabledClass} />
            </div>

            <div>
              <span className="text-[11px] text-text-tertiary mb-1.5 block">
                Projects (comma-separated)
              </span>
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
          </div>

          <ErrorBanner error={error} />

          <div className="flex gap-3">
            <button type="button" onClick={backFromJiraConfig} className={secondaryBtnClass}>
              Back
            </button>
            <button
              type="button"
              onClick={saveJiraConfig}
              disabled={loading || !canSaveJiraConfig}
              className={primaryBtnClass}
            >
              {loading ? 'Saving...' : 'Save & Return'}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// --- Shared styles ---

const cardClass =
  'w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]'

const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors'

const inputDisabledClass =
  'w-full bg-black/[0.03] border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-tertiary cursor-not-allowed'

const primaryBtnClass =
  'flex-1 bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors'

const secondaryBtnClass =
  'flex-1 bg-white/50 hover:bg-white/80 border border-border-subtle text-text-secondary font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors'

function ErrorBanner({ error }: { error: string }) {
  if (!error) return null
  return (
    <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
      {error}
    </div>
  )
}

function StatusChip({
  label,
  selected,
  onClick,
}: {
  label: string
  selected: boolean
  onClick: () => void
}) {
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
