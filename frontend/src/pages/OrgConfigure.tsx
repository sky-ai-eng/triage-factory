import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { toast } from '../components/Toast/toastStore'
import GitHubAccessGroup from './settings/GitHubAccessGroup'
import JiraAccessGroup from './settings/JiraAccessGroup'
import PollerTimingGroup from './settings/PollerTimingGroup'
import ModelGroup from './settings/ModelGroup'
import {
  emptyOrgConfig,
  fetchOrgSettings,
  orgConfigFromSettings,
  saveOrgConfig,
  type OrgConfigForm,
} from './settings/orgConfig'

/**
 * OrgConfigure is the create-time configure step — the second half of
 * "Start your Factory" (multi) / "Start your Triage Factory" (local). By the
 * time the founder lands here the tenant already exists with default settings
 * (POST /api/orgs in multi, POST /api/setup/start in local) and is the
 * session's active org, so the same org-scoped endpoints the Settings page
 * uses resolve to it. The founder wires GitHub + Jira access, poller cadence,
 * and the model cap through the *same* shared field groups as Settings — no
 * parallel implementation — then continues to the team configure step
 * (repos / GitHub-team mappings / Jira rules / team settings) before landing
 * in the factory. "I'll configure later" short-circuits straight to the app.
 *
 * This route sits OUTSIDE RequireGitHubIdentity (like ConnectGitHub): it's
 * exactly where GitHub access gets configured, so gating it on GitHub
 * identity would be a loop. It's intentionally skippable — everything here is
 * also reachable later from Settings.
 *
 * `isLocal` flips the two mode divergences the shared GitHub group exposes:
 * the SSH clone toggle (local can clone over SSH; multi hardwires HTTPS) and
 * the GitHub App registration panel (multi-only — local uses a PAT). Local
 * mounts it under <AuthGate mode="local"> (single-user machine = implicit
 * admin); multi under <AuthGate mode="multi"> (real session + admin RLS).
 * env-provided creds need no special handling here — /api/settings/org
 * reflects the env overlay, so the base URL prefills and the token field
 * shows "leave blank to keep current".
 */
export default function OrgConfigure({ isLocal = false }: { isLocal?: boolean }) {
  const navigate = useNavigate()
  const { org_id: orgId } = useParams<{ org_id: string }>()

  // Multi mode hardwires HTTPS clone (no SSH machinery in the container);
  // local defaults to SSH and lets the user flip via the GitHub group.
  const [form, setForm] = useState<OrgConfigForm>({
    ...emptyOrgConfig(),
    github_clone_protocol: isLocal ? 'ssh' : 'https',
  })
  const [hasGitHubPat, setHasGitHubPat] = useState(false)
  // githubReady folds the stored PAT, the env overlay, AND a registered
  // GitHub App together (the server's setup predicate), so an App-configured
  // org isn't false-blocked from finishing just because no PAT is stored.
  const [githubReady, setGithubReady] = useState(false)
  const [jiraConnected, setJiraConnected] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [saving, setSaving] = useState(false)

  // Seed from the new org's defaults so re-typing isn't required for fields
  // the bootstrap already set (base URLs, intervals). Tokens stay blank.
  useEffect(() => {
    let cancelled = false
    // Separate from the settings read: integrations/status reports
    // github_ready (PAT | env | registered App), the signal the Finish
    // guard needs to avoid false-blocking an App-configured org that has no
    // stored PAT. Best-effort — a failure leaves the PAT/typed signals.
    fetch('/api/integrations/status')
      .then((res) => (res.ok ? res.json() : null))
      .then((data) => {
        if (!cancelled && data) setGithubReady(!!data.github_ready)
      })
      .catch(() => {})
    fetchOrgSettings().then((org) => {
      if (cancelled) return
      if (org) {
        setForm(orgConfigFromSettings(org))
        setHasGitHubPat(org.has_github_pat)
        setJiraConnected(org.has_jira_pat && !!org.jira_base_url)
      } else {
        // Couldn't read the new org's defaults. The step still works off the
        // seeded form, but the prefilled base URLs / intervals may be wrong,
        // so flag it rather than silently presenting defaults as truth.
        toast.error(
          'Could not load your organization settings. Showing defaults — verify before saving.',
        )
      }
      setLoaded(true)
    })
    return () => {
      cancelled = true
    }
  }, [])

  const patchForm = (patch: Partial<OrgConfigForm>) => setForm((f) => ({ ...f, ...patch }))

  // GitHub access is mandatory — it's the spine of every downstream feature,
  // so the founder can't skip past it. Block Finish until GitHub is set up by
  // any means: a stored PAT / env overlay (hasGitHubPat), a freshly typed PAT
  // (saved on Finish), or a registered App (githubReady). Jira stays optional.
  const githubBlocked = !hasGitHubPat && !githubReady && form.github_pat.trim() === ''

  // After org config, hand off to the team configure step (the next link in
  // the create→configure chain). "default" is the alias the team endpoints
  // resolve to the org's freshly-bootstrapped Default team, so we don't need
  // its UUID here.
  const goToTeamConfigure = () => {
    if (!orgId) {
      // Unreachable in practice — both mounting routes supply :org_id — but a
      // missing id would otherwise silently drop the founder into the app,
      // skipping the team-configure step. Surface it rather than fail quietly.
      console.error('OrgConfigure: missing org_id, cannot continue to team configure')
      navigate('/', { replace: true })
      return
    }
    navigate(`/orgs/${orgId}/teams/default/configure`, { replace: true })
  }

  const finish = async () => {
    setSaving(true)
    try {
      const result = await saveOrgConfig(form)
      if (!result.ok) {
        toast.error(result.error)
        return
      }
      if (result.warning) toast.info(result.warning)
      goToTeamConfigure()
    } catch (err) {
      toast.error(`Could not save configuration: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  if (!loaded) {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <p className="text-text-tertiary text-sm">Loading…</p>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-surface py-10 px-4">
      <div className="max-w-2xl mx-auto space-y-5">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Configure your Factory
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            Connect GitHub and (optionally) Jira, set how often Triage Factory polls them, and cap
            the model tier. You can change any of this later in Settings.
          </p>
        </div>

        <GitHubAccessGroup
          value={{
            github_url: form.github_url,
            github_pat: form.github_pat,
            github_clone_protocol: form.github_clone_protocol,
          }}
          onChange={patchForm}
          hasToken={hasGitHubPat}
          isLocal={isLocal}
          orgId={orgId ?? null}
          showAppPanel={!isLocal}
        />

        <JiraAccessGroup
          value={{ jira_url: form.jira_url, jira_pat: form.jira_pat }}
          onChange={patchForm}
          connected={jiraConnected}
          onConnected={() => setJiraConnected(true)}
          onDisconnected={() => setJiraConnected(false)}
        />

        <PollerTimingGroup
          value={{
            github_poll_interval: form.github_poll_interval,
            jira_poll_interval: form.jira_poll_interval,
          }}
          onChange={patchForm}
          showJira={jiraConnected}
        />

        <ModelGroup value={{ max_llm_model_tier: form.max_llm_model_tier }} onChange={patchForm} />

        <div className="space-y-2">
          <button
            type="button"
            onClick={finish}
            disabled={saving || githubBlocked}
            className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {saving ? 'Saving…' : 'Finish setup'}
          </button>
          {githubBlocked && (
            <p className="text-[12px] text-text-tertiary text-center">
              Connect GitHub to continue — it&rsquo;s required before you can use Triage Factory.
            </p>
          )}
        </div>
      </div>
    </div>
  )
}
