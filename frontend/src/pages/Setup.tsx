import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { CheckCircle2, ChevronRight } from 'lucide-react'
import RepoPickerModal, { type GitHubRepo } from '../components/RepoPickerModal'
import CarryOverList from '../components/CarryOverList'
import JiraStatusRule, { type JiraStatusRuleValue } from '../components/JiraStatusRule'
import { useAuthStatus } from '../hooks/useAuthStatus'

interface JiraStatus {
  id: string
  name: string
}

// Setup flow steps:
//   github → repos → integrations → jira-creds → jira-config → jira-carry-over → integrations
type Step = 'github' | 'repos' | 'integrations' | 'jira-creds' | 'jira-config' | 'jira-carry-over'

export default function Setup() {
  const navigate = useNavigate()
  const authStatus = useAuthStatus()
  const [step, setStep] = useState<Step>('github')
  const [initDone, setInitDone] = useState(false)

  // GitHub (mandatory). clone_protocol defaults to 'ssh' — the user
  // can flip it to 'https' on this same step if their machine doesn't
  // have SSH access set up. The server runs a preflight when "ssh" is
  // selected and rejects setup if it fails; the rejection surfaces as
  // an inline error via the existing ErrorBanner.
  const [githubForm, setGithubForm] = useState<{
    url: string
    pat: string
    clone_protocol: 'ssh' | 'https'
  }>({ url: '', pat: '', clone_protocol: 'ssh' })

  // Repo selection — cached so navigating back doesn't re-fetch or lose selection
  const [cachedRepos, setCachedRepos] = useState<GitHubRepo[] | undefined>(undefined)
  const [selectedRepos, setSelectedRepos] = useState<string[]>([])

  // Jira state
  const [jiraConnected, setJiraConnected] = useState(false)
  const [jiraForm, setJiraForm] = useState<{
    url: string
    pat: string
    projects: string
    pickup: JiraStatusRuleValue
    in_progress: JiraStatusRuleValue
    done: JiraStatusRuleValue
  }>({
    url: '',
    pat: '',
    projects: '',
    pickup: { members: [] },
    in_progress: { members: [] },
    done: { members: [] },
  })
  const [jiraStatuses, setJiraStatuses] = useState<JiraStatus[]>([])
  const [statusesLoading, setStatusesLoading] = useState(false)
  const [jiraConfigured, setJiraConfigured] = useState(false)

  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const envProvided = authStatus.env_provided ?? []
  const githubFromEnv = envProvided.includes('github')
  const jiraFromEnv = envProvided.includes('jira')

  useEffect(() => {
    if (authStatus.loading || initDone) return
    setInitDone(true)

    if (authStatus.github) {
      setStep('repos')
    }
    if (authStatus.jira && authStatus.jira_url) {
      setJiraConnected(true)
      setJiraForm((f) => ({ ...f, url: authStatus.jira_url! }))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [authStatus.loading])

  // --- Step 1: GitHub credentials ---
  const canSubmitGitHub = githubForm.url.trim() !== '' && githubForm.pat.trim() !== ''

  const submitGitHub = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      // The server is authoritative on SSH preflight: it runs the
      // check itself when clone_protocol == "ssh" and rejects the
      // setup with a clear error if it fails. Don't double-check
      // here — duplicating the gate forces us to keep two error
      // surfaces in sync.
      const res = await fetch('/api/integrations/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          github_url: githubForm.url,
          github_pat: githubForm.pat,
          clone_protocol: githubForm.clone_protocol,
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
      // Repo tracking is per-team (SKY-375). Onboarding writes the org's
      // default team; SKY-294 will thread the actual team being set up.
      const res = await fetch('/api/settings/team/default/repos', {
        method: 'PUT',
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
    } catch (err) {
      // Don't swallow the failure silently — a bare `catch {}` here is how
      // the next person debugging a slow/failed Continue ends up flying blind
      // (SKY-409). Log the real error and surface an inline message.
      console.error('saveRepos failed:', err)
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
    jiraForm.projects
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean).length > 0 &&
    jiraForm.pickup.members.length > 0 &&
    jiraForm.in_progress.members.length > 0 &&
    !!jiraForm.in_progress.canonical &&
    jiraForm.done.members.length > 0 &&
    !!jiraForm.done.canonical

  const saveJiraConfig = async () => {
    setError('')
    setLoading(true)
    try {
      // Setup configures all listed projects with the same rule
      // triple — heterogeneous workflows are configured later in
      // Settings (SKY-272). For the common single-project setup
      // flow this still works fine: one project gets the rules the
      // user just picked.
      const projects = jiraForm.projects
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
        .map((key) => ({
          key,
          pickup: jiraForm.pickup,
          in_progress: jiraForm.in_progress,
          done: jiraForm.done,
        }))
      const res = await fetch('/api/settings/team/default', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          jira_projects: projects,
        }),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Failed to save Jira config')
        return
      }
      setStep('jira-carry-over')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  const backFromJiraConfig = async () => {
    setError('')
    if (jiraFromEnv) {
      setStep('integrations')
      return
    }
    // Disconnect Jira — clear stored credentials so the user must
    // re-enter at least the PAT to reconnect.
    try {
      await fetch('/api/integrations/jira', { method: 'DELETE' })
    } catch {
      // Best-effort — proceed with local state reset regardless.
    }
    setJiraConnected(false)
    setJiraConfigured(false)
    setJiraForm((f) => ({
      ...f,
      pat: '',
      projects: '',
      pickup: { members: [] },
      in_progress: { members: [] },
      done: { members: [] },
    }))
    setJiraStatuses([])
    setStep('jira-creds')
  }

  const updateJira = (field: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setJiraForm((f) => ({ ...f, [field]: e.target.value }))

  if (authStatus.loading) {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <p className="text-text-tertiary text-sm">Loading...</p>
      </div>
    )
  }

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
              <code className="text-text-secondary">read:org</code> scopes.{' '}
              <code className="text-text-secondary">read:org</code> is needed to resolve your team
              memberships so review requests sent to your teams (e.g. CODEOWNERS) surface as tasks —
              without it, only PRs that request you individually will show up.
            </p>

            {/* Clone protocol — controls how we materialize bare clones in
                ~/.triagefactory/repos/. The token is still required for
                the GitHub API regardless of which form we pick here. */}
            <div className="space-y-1.5">
              <label className="text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                Clone protocol
              </label>
              <div className="inline-flex rounded-lg border border-border-glass bg-black/[0.02] p-0.5">
                {(['ssh', 'https'] as const).map((p) => (
                  <button
                    key={p}
                    type="button"
                    onClick={() => setGithubForm((f) => ({ ...f, clone_protocol: p }))}
                    className={`px-3 py-1 text-[12px] font-medium rounded-md transition-colors ${
                      githubForm.clone_protocol === p
                        ? 'bg-white text-text-primary shadow-sm'
                        : 'text-text-tertiary hover:text-text-secondary'
                    }`}
                  >
                    {p.toUpperCase()}
                  </button>
                ))}
              </div>
              <p className="text-[11px] text-text-tertiary leading-relaxed">
                Your token is still required for the GitHub API. The protocol only affects how
                Triage Factory clones repos to your machine — SSH uses your existing key + agent,
                HTTPS uses your git credential helper.
              </p>
            </div>
          </div>

          <ErrorBanner error={error} />

          <button type="submit" disabled={loading || !canSubmitGitHub} className={primaryBtnClass}>
            {loading ? 'Validating...' : 'Connect'}
          </button>
        </form>
      )}

      {/* Step 2: Repo selection */}
      {step === 'repos' && (
        <div className="w-full max-w-lg space-y-3">
          <RepoPickerModal
            selected={selectedRepos}
            onSave={saveRepos}
            onClose={() => {
              /* cannot skip */
            }}
            onBack={githubFromEnv ? undefined : () => setStep('github')}
            cachedRepos={cachedRepos}
            onReposFetched={setCachedRepos}
            saving={loading}
            inline
          />
          {/* The save (team-repos PUT) can fail or be rejected (e.g. an
              unreachable repo); surface it inline instead of failing silently
              the way the bare picker did (SKY-409). */}
          <ErrorBanner error={error} />
        </div>
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
                setStep(jiraFromEnv || jiraConnected ? 'jira-config' : 'jira-creds')
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
              <div className="space-y-4">
                <JiraStatusRule
                  label="Pickup"
                  description="Poll for unassigned tickets in these states."
                  allStatuses={jiraStatuses}
                  value={jiraForm.pickup}
                  onChange={(v) => setJiraForm((f) => ({ ...f, pickup: v }))}
                  requireCanonical={false}
                />
                <JiraStatusRule
                  label="In progress"
                  description="Count as actively being worked on."
                  allStatuses={jiraStatuses}
                  value={jiraForm.in_progress}
                  onChange={(v) => setJiraForm((f) => ({ ...f, in_progress: v }))}
                  requireCanonical={true}
                  canonicalPrompt="Claim →"
                />
                <JiraStatusRule
                  label="Done"
                  description="Count as complete (add every variant — e.g. Resolved + Verified)."
                  allStatuses={jiraStatuses}
                  value={jiraForm.done}
                  onChange={(v) => setJiraForm((f) => ({ ...f, done: v }))}
                  requireCanonical={true}
                  canonicalPrompt="Complete →"
                />
              </div>
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
              {loading ? 'Saving...' : 'Continue'}
            </button>
          </div>
        </div>
      )}

      {/* Step 6: Jira carry-over — decide what to do with existing assigned work */}
      {step === 'jira-carry-over' && (
        <CarryOverList
          onSave={() => {
            setJiraConfigured(true)
            setStep('integrations')
          }}
          onSkip={() => {
            setJiraConfigured(true)
            setStep('integrations')
          }}
          onBack={() => setStep('jira-config')}
        />
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
    <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss whitespace-pre-line">
      {error}
    </div>
  )
}
