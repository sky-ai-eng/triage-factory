import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import RepoPickerModal, { type GitHubRepo } from '../components/RepoPickerModal'
import GitHubTeamSelector, { type GitHubTeamCandidate } from '../components/GitHubTeamSelector'
import CarryOverList from '../components/CarryOverList'
import JiraStatusRule, { type JiraStatusRuleValue } from '../components/JiraStatusRule'
import GitHubAccessGroup from './settings/GitHubAccessGroup'
import JiraAccessGroup from './settings/JiraAccessGroup'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { useAuthStatus } from '../hooks/useAuthStatus'

interface JiraStatus {
  id: string
  name: string
}

// Setup flow steps:
//   github → repos → github-teams → integrations → jira-config → jira-carry-over
//
// The org-level credential steps (github, integrations) render the SAME
// shared field groups as the Settings page and the org-create configure
// step — GitHubAccessGroup (local PAT path) and JiraAccessGroup — so there's
// no bespoke re-implementation to drift. The team-level steps (repos,
// github-teams, jira-config, jira-carry-over) are unchanged.
//
// github-teams (SKY-411) lands between repo selection and integrations: it
// makes the GitHub-team → TF-team mapping exist *before* the first poll so a
// user who is only review-requested via a team isn't left with an empty
// queue on first run. Skippable; configurable later in Settings.
type Step = 'github' | 'repos' | 'github-teams' | 'integrations' | 'jira-config' | 'jira-carry-over'

export default function Setup() {
  const navigate = useNavigate()
  const authStatus = useAuthStatus()
  const [step, setStep] = useState<Step>('github')
  const [initDone, setInitDone] = useState(false)

  // GitHub (mandatory). clone_protocol defaults to 'ssh' — the user can flip
  // it to 'https' on this same step (via the shared group) if their machine
  // doesn't have SSH set up. The server runs a preflight when "ssh" is
  // selected and rejects setup if it fails; the rejection surfaces inline.
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

  // patchGitHub maps the shared group's org_settings-keyed patch onto the
  // wizard's local github form shape.
  const patchGitHub = (patch: {
    github_url?: string
    github_pat?: string
    github_clone_protocol?: 'ssh' | 'https'
  }) =>
    setGithubForm((f) => ({
      ...f,
      ...(patch.github_url !== undefined ? { url: patch.github_url } : {}),
      ...(patch.github_pat !== undefined ? { pat: patch.github_pat } : {}),
      ...(patch.github_clone_protocol !== undefined
        ? { clone_protocol: patch.github_clone_protocol }
        : {}),
    }))

  const submitGitHub = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      // The server is authoritative on SSH preflight: it runs the check
      // itself when clone_protocol == "ssh" and rejects the setup with a
      // clear error if it fails. Don't double-check here — duplicating the
      // gate forces us to keep two error surfaces in sync.
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
        return
      }
      setSelectedRepos(repos)
      setStep('github-teams')
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

  // --- Step 3: GitHub teams (SKY-411) ---
  // Continue writes the checked teams to the *existing* per-team
  // github-groups replace-set endpoint (the same write the Settings editor
  // uses) — one canonical bulk write, idempotent and re-run safe. Onboarding
  // targets the org's default team; SKY-294 will thread the acting team.
  const saveTeamMappings = async (teams: GitHubTeamCandidate[]) => {
    setLoading(true)
    setError('')
    try {
      const groups = teams.map((t) => ({ org_login: t.org_login, team_slug: t.team_slug }))
      const res = await fetch('/api/settings/team/default/github-groups', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ groups }),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        setError(data.error || 'Failed to save GitHub teams')
        return
      }
      setStep('integrations')
    } catch (err) {
      console.error('saveTeamMappings failed:', err)
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  // --- Step 4: Integrations (Jira credentials, via the shared group) ---
  const patchJira = (patch: { jira_url?: string; jira_pat?: string }) =>
    setJiraForm((f) => ({
      ...f,
      ...(patch.jira_url !== undefined ? { url: patch.jira_url } : {}),
      ...(patch.jira_pat !== undefined ? { pat: patch.jira_pat } : {}),
    }))

  const finishSetup = () => {
    navigate('/')
  }

  // Continue from the integrations step: connected-but-unconfigured Jira goes
  // to the team project/status step; otherwise (no Jira, or already
  // configured) we're done.
  const continueFromIntegrations = () => {
    setError('')
    if (jiraConnected && !jiraConfigured) {
      setStep('jira-config')
      return
    }
    finishSetup()
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
      // Setup configures all listed projects with the same rule triple —
      // heterogeneous workflows are configured later in Settings (SKY-272).
      // For the common single-project setup this still works fine: one
      // project gets the rules the user just picked.
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

  // Back from jira-config returns to the integrations step. Credentials stay
  // connected — the user can disconnect explicitly from the shared group
  // there if they want to re-enter them.
  const backFromJiraConfig = () => {
    setError('')
    setStep('integrations')
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
      {/* Step 1: GitHub credentials (shared GitHubAccessGroup, local PAT path) */}
      {step === 'github' && (
        <form onSubmit={submitGitHub} className="w-full max-w-lg space-y-4">
          <div className="px-1">
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Connect GitHub
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Triage Factory needs access to your GitHub to watch repositories and manage PRs.
              Tokens are stored in your OS keychain and never leave your machine.
            </p>
          </div>

          <GitHubAccessGroup
            value={{
              github_url: githubForm.url,
              github_pat: githubForm.pat,
              github_clone_protocol: githubForm.clone_protocol,
            }}
            onChange={patchGitHub}
            hasToken={false}
            isLocal
            orgId={LOCAL_DEFAULT_ORG_ID}
            showAppPanel={false}
          />

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

      {/* Step 3: GitHub teams (SKY-411) */}
      {step === 'github-teams' && (
        <div className="w-full max-w-lg space-y-3">
          <GitHubTeamSelector
            teamId="default"
            onContinue={saveTeamMappings}
            onSkip={() => {
              // Skip advances without writing any mappings — idempotent
              // re-run safe; the user can configure later in Settings.
              setError('')
              setStep('integrations')
            }}
            onBack={() => {
              setError('')
              setStep('repos')
            }}
            saving={loading}
          />
          <ErrorBanner error={error} />
        </div>
      )}

      {/* Step 4: Integrations — Jira credentials via the shared group */}
      {step === 'integrations' && (
        <div className="w-full max-w-lg space-y-4">
          <div className="px-1">
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Integrations
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Optionally connect Jira. You can always configure this later in Settings.
            </p>
          </div>

          <JiraAccessGroup
            value={{ jira_url: jiraForm.url, jira_pat: jiraForm.pat }}
            onChange={patchJira}
            connected={jiraConnected}
            onConnected={() => setJiraConnected(true)}
            onDisconnected={() => {
              setJiraConnected(false)
              setJiraConfigured(false)
            }}
          />

          <ErrorBanner error={error} />

          <div className="flex gap-3">
            <button
              type="button"
              onClick={() => setStep('github-teams')}
              className={secondaryBtnClass}
            >
              Back
            </button>
            <button type="button" onClick={continueFromIntegrations} className={primaryBtnClass}>
              {jiraConnected && !jiraConfigured ? 'Configure Jira' : 'Continue'}
            </button>
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

          {/* Grayed-out instance field */}
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
