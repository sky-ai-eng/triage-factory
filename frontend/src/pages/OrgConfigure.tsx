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
  const [jiraConnected, setJiraConnected] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [saving, setSaving] = useState(false)

  // Seed from the new org's defaults so re-typing isn't required for fields
  // the bootstrap already set (base URLs, intervals). Tokens stay blank.
  useEffect(() => {
    let cancelled = false
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

  // Local mode's app is the flat route table (root '/'); multi is org-scoped.
  const goToApp = () => navigate(isLocal ? '/' : orgId ? '/orgs/' + orgId : '/', { replace: true })

  // After org config, hand off to the team configure step (the next link in
  // the create→configure chain). "default" is the alias the team endpoints
  // resolve to the org's freshly-bootstrapped Default team, so we don't need
  // its UUID here.
  const goToTeamConfigure = () =>
    navigate(orgId ? `/orgs/${orgId}/teams/default/configure` : '/', { replace: true })

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

        <div className="flex gap-3">
          <button
            type="button"
            onClick={goToApp}
            className="flex-1 bg-white/50 hover:bg-white/80 border border-border-subtle text-text-secondary font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            I&rsquo;ll configure later
          </button>
          <button
            type="button"
            onClick={finish}
            disabled={saving}
            className="flex-1 bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {saving ? 'Saving…' : 'Finish setup'}
          </button>
        </div>
      </div>
    </div>
  )
}
