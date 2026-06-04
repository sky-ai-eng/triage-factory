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
 * "Start your Factory". By the time the founder lands here the org already
 * exists with default settings (POST /api/orgs) and is the session's active
 * org, so the same org-scoped endpoints the Settings page uses resolve to
 * the new org. The founder wires GitHub + Jira access, poller cadence, and
 * the model cap through the *same* shared field groups as Settings — no
 * parallel implementation — then lands in the factory.
 *
 * This route sits OUTSIDE RequireGitHubIdentity (like ConnectGitHub): it's
 * exactly where GitHub access gets configured, so gating it on GitHub
 * identity would be a loop. Multi-mode only (its sole mounter is
 * MultiRoutes). It's intentionally skippable — everything here is also
 * reachable later from Settings.
 */
export default function OrgConfigure() {
  const navigate = useNavigate()
  const { org_id: orgId } = useParams<{ org_id: string }>()

  // Multi mode hardwires HTTPS clone, so the GitHub group hides the toggle
  // and we seed https rather than the local-default ssh.
  const [form, setForm] = useState<OrgConfigForm>({
    ...emptyOrgConfig(),
    github_clone_protocol: 'https',
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

  const goToApp = () => navigate(orgId ? '/orgs/' + orgId : '/', { replace: true })

  const finish = async () => {
    setSaving(true)
    try {
      const result = await saveOrgConfig(form)
      if (!result.ok) {
        toast.error(result.error)
        return
      }
      if (result.warning) toast.info(result.warning)
      goToApp()
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
          isLocal={false}
          orgId={orgId ?? null}
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
