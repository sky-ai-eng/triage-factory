import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import RepoPickerModal, { type GitHubRepo } from '../components/RepoPickerModal'
import GitHubTeamSelector, { type GitHubTeamCandidate } from '../components/GitHubTeamSelector'
import CarryOverList from '../components/CarryOverList'
import GitHubAccessGroup from './settings/GitHubAccessGroup'
import JiraAccessGroup from './settings/JiraAccessGroup'
import JiraProjectRulesGroup from './settings/JiraProjectRulesGroup'
import {
  saveTeamJiraProjects,
  teamProjectsBlocked,
  type JiraProjectConfig,
} from './settings/teamConfig'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { useAuthStatus } from '../hooks/useAuthStatus'

// Setup flow steps:
//   github → repos → github-teams → integrations → jira-config → jira-carry-over
//
// The org-level credential steps (github, integrations) render the SAME
// shared field groups as the Settings page and the org-create configure
// step — GitHubAccessGroup (local PAT path) and JiraAccessGroup — so there's
// no bespoke re-implementation to drift. The team-level steps render shared
// groups too: jira-config is JiraProjectRulesGroup (the same per-project rule
// editor as the Settings team tab + TeamConfigure), and repos / github-teams
// reuse RepoPickerModal / GitHubTeamSelector (which the team groups also
// wrap). jira-carry-over is the shared CarryOverList. The wizard owns only
// the step chrome + per-step persistence.
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

  // Jira state. Credentials (url/pat) drive the integrations step's shared
  // JiraAccessGroup; the per-project tracking rules are edited via the shared
  // JiraProjectRulesGroup and persisted on the jira-config step's Continue.
  const [jiraConnected, setJiraConnected] = useState(false)
  const [jiraForm, setJiraForm] = useState<{ url: string; pat: string }>({ url: '', pat: '' })
  const [jiraProjects, setJiraProjects] = useState<JiraProjectConfig[]>([])
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
          // Trim to match the canSubmitGitHub guard, so stray whitespace
          // can't be stored and then fail a later "invalid token" check.
          github_url: githubForm.url.trim(),
          github_pat: githubForm.pat.trim(),
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

  // Back out of the integrations step. Wipe entered-but-unsubmitted Jira
  // credentials so a stray token can't linger in component state (or be
  // accidentally submitted later). Connected creds live server-side and the
  // shared group already clears the PAT on connect, so only the unconnected
  // case holds anything worth clearing.
  const backFromIntegrations = () => {
    setError('')
    if (!jiraConnected) {
      setJiraForm((f) => ({ ...f, url: '', pat: '' }))
    }
    setStep('github-teams')
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

  // --- Step 5: Jira config (per-project tracking rules) ---
  // The shared JiraProjectRulesGroup owns the project list, the status fetch,
  // and the per-project rule pickers; the wizard only needs the validity gate
  // and the persist-on-Continue. Continue is blocked only by a partially-
  // configured project (a key with incomplete rules); zero projects is a valid
  // choice, so the step can be continued without adding one (connected is
  // always true here — it's only reached with Jira connected).
  const canSaveJiraConfig = !teamProjectsBlocked(jiraProjects, true)

  const saveJiraConfig = async () => {
    setError('')
    setLoading(true)
    try {
      const res = await saveTeamJiraProjects('default', jiraProjects)
      if (!res.ok) {
        setError(res.error)
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
            <button type="button" onClick={backFromIntegrations} className={secondaryBtnClass}>
              Back
            </button>
            <button type="button" onClick={continueFromIntegrations} className={primaryBtnClass}>
              {jiraConnected && !jiraConfigured ? 'Configure Jira' : 'Continue'}
            </button>
          </div>
        </div>
      )}

      {/* Step 5: Jira config (per-project tracking rules, shared group) */}
      {step === 'jira-config' && (
        <div className="w-full max-w-lg space-y-4">
          <div className="px-1">
            <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
              Configure Jira
            </h1>
            <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
              Choose which projects to poll and how statuses map to your triage workflow.
            </p>
          </div>

          <JiraProjectRulesGroup value={jiraProjects} onChange={setJiraProjects} connected />

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
