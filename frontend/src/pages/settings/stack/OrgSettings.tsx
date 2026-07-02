// The Organization group of the Settings stack — rendered only for org admins.
// Most sections' BODY is the actual /setup step component (GitHubUrlStep,
// GitHubCloneStep, OrgModelStep, …) fed a WizardState draft, so Settings and
// /setup are literally the same components — same visuals, same copy.
//
// GitHub *access* is the exception (TFAC-328/329): it's either/or per org, so
// the old independently-editable selector / account-type / App-register /
// App-install / free-form-PAT sections are replaced by ONE "GitHub access"
// section — a mode header plus an explicit guided switch (GitHubAccessControl),
// which composes the same step components internally (account type, App panel,
// install view) but gates credential changes behind the switch flow + an
// inform-only reachability diff. Setting a PAT through the plain settings save
// while an App is registered 409s on the backend, so the switch flow is the
// only credential-mutation path here.
//
// What Settings adds over /setup is the per-section Save model (the approved
// design): expand a section, edit its draft, Save (or Cancel/discard). The
// org-form sections all persist through the single POST /api/settings/org, so
// each saves {...baseline.org, ...ownFields} against the LIVE baseline — saving
// one never flushes another's unsaved edits. Selector/panel sections (the
// access-method picker, the App register panel) carry no Save footer. Jira
// disconnect is footer-less too — it commits inline on its own button — though
// note it DOES mutate org settings (JiraAccessGroup.disconnect POSTs
// /api/settings/org to clear jira_base_url, on top of DELETE
// /api/integrations/jira); footer-less is about how it commits, not whether it
// persists. The Jira *connect* is the exception that proves the rule it used to
// break: its Save footer ("Connect") drives POST /api/jira/connect rather than
// the org POST, but it's a footer all the same — the same one-button shape as
// the GitHub PAT section.

import { useCallback, useEffect, useState } from 'react'
import { toast } from '../../../components/Toast/toastStore'
import { readError } from '../../../lib/api'
import {
  hostOf,
  normalizeBaseUrl,
  checkGitHubReachability,
  reachabilityMessage,
} from '../../../lib/reachability'
import { GitHubUrlStep, GitHubCloneStep } from '../../setup/GitHubStep'
import { OrgModelStep } from '../../setup/ModelStep'
import { initialWizardState, loadOrg, loadGitHubAppInstall } from '../../setup/steps'
import type { StepContext, WizardState } from '../../setup/types'
import PollerTimingGroup from '../PollerTimingGroup'
import { inputClass } from '../primitives'
import JiraAccessGroup from '../JiraAccessGroup'
import AtlassianOAuthAppCard from '../AtlassianOAuthAppCard'
import SlackWorkspacesCard from '../SlackWorkspacesCard'
import TeamManagementSection from '../../../components/TeamManagementSection'
import { dailyCapError, saveOrgConfig, type OrgConfigForm } from '../orgConfig'
import { connectJira, JIRA_DEPLOYMENT_OPTIONS } from '../jiraConnect'
import { connectAnthropic, CLAUDE_SOURCE_OPTIONS } from '../anthropicConnect'
import { ClaudeProviderCards, AnthropicKeyField } from '../../setup/ClaudeStep'
import { ChoiceCards } from '../../setup/parts'
import GitHubAccessControl from './GitHubAccessControl'
import SSOSettings from './SSOSettings'
import SettingsSection from './SettingsSection'
import { useEntitlements, FeatureSSO, FeatureSlack } from '../../../hooks/useEntitlements'

const TIER_LABELS: Record<string, string> = { haiku: 'Haiku', sonnet: 'Sonnet', opus: 'Opus' }
const intervalLabel = (d: string): string => d.replace(/m0s$/, 'm')
const noop = () => {}

export default function OrgSettings({
  orgId,
  isLocal,
}: {
  orgId: string | null
  isLocal: boolean
}) {
  const [draft, setDraft] = useState<WizardState>(initialWizardState)
  const [baseline, setBaseline] = useState<WizardState>(initialWizardState)
  const [loaded, setLoaded] = useState(false)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [urlError, setUrlError] = useState<string | null>(null)

  // EE SSO / Slack entitlements — dark until the probe resolves, matching the
  // backend's 404-and-hide at every /api/sso/* and /api/slack/* seam.
  const { has, loaded: entLoaded } = useEntitlements()
  const sso = entLoaded && has(FeatureSSO)
  const slackEnt = entLoaded && has(FeatureSlack)

  // Per-section in-flight keys. A Set (not a single string) so two sections
  // saving in quick succession each keep their own saving state — otherwise the
  // second save would clear the first section's lock mid-flight, re-enabling its
  // collapse/discard while the request still commits the pre-cancel values.
  const [savingKeys, setSavingKeys] = useState<Set<string>>(() => new Set())
  const isSaving = (k: string) => savingKeys.has(k)
  const setSavingKey = (k: string, on: boolean) =>
    setSavingKeys((s) => {
      const n = new Set(s)
      if (on) n.add(k)
      else n.delete(k)
      return n
    })

  const patch = useCallback((p: Partial<WizardState>) => {
    setDraft((d) => ({ ...d, ...p }))
  }, [])

  const load = useCallback(() => {
    let cancelled = false
    setLoadError(null)
    // loadGitHubAppInstall is the single App-status loader: it surfaces the
    // staged/registered/installed App state (the same staged-aware derivation the
    // wizard uses) so the mode header, the staged banner, and the clone-protocol
    // gate read the LIVE mode, AND it seeds the App account-type from the App's
    // persisted owner_type (so an org-owned App doesn't render as "Personal" on
    // reload) — one GET /github-app, not two. Both loaders catch internally and
    // resolve to {} on any failure, so neither rejects the Promise.all — only a
    // loadOrg failure surfaces the retry. The app slice merges LAST so its
    // tab/staged override wins over loadOrg's naive value.
    const loadCtx = { orgId, teamId: 'default', isLocal }
    Promise.all([loadOrg(loadCtx), loadGitHubAppInstall(loadCtx)])
      .then(([slice, appSlice]) => {
        if (cancelled) return
        const seeded: WizardState = {
          ...initialWizardState(),
          ...slice,
          ...appSlice,
          isLocal,
        }
        setBaseline(seeded)
        setDraft(seeded)
        setLoaded(true)
      })
      .catch(() => {
        if (cancelled) return
        setLoadError('Could not load organization settings. Check your connection and try again.')
      })
    // Suppress a stale completion if orgId/isLocal change mid-flight (mirrors
    // TeamSettings.load — near-zero risk here since both are stable once
    // OrgSettings mounts, but keeps the two load paths consistent).
    return () => {
      cancelled = true
    }
  }, [orgId, isLocal])

  useEffect(() => {
    const cancel = load()
    return cancel
  }, [load])

  // commitOrgSlice persists one section's fields merged onto the LIVE baseline
  // org form (the single POST /api/settings/org), folding the slice back in on
  // success. github_pat is always sent blank in the baseline (the stored token
  // never round-trips); a section that owns it passes the typed value in `slice`.
  const commitOrgSlice = useCallback(
    async (key: string, slice: Partial<OrgConfigForm>, label: string): Promise<boolean> => {
      setSavingKey(key, true)
      try {
        const next: OrgConfigForm = {
          ...baseline.org,
          ...slice,
          github_pat: slice.github_pat ?? '',
        }
        const res = await saveOrgConfig(next)
        if (!res.ok) {
          toast.error(res.error)
          return false
        }
        setBaseline((b) => ({ ...b, org: { ...b.org, ...slice, github_pat: '' } }))
        if (res.warning) toast.info(res.warning)
        toast.success(`${label} saved`)
        return true
      } finally {
        setSavingKey(key, false)
      }
    },
    [baseline.org],
  )

  // revertOrg resets the named org fields in the draft back to the baseline —
  // a section's Cancel, scoped so it never touches a neighbour's edits.
  const revertOrg = (fields: (keyof OrgConfigForm)[]) =>
    setDraft((d) => ({
      ...d,
      org: fields.reduce((o, f) => ({ ...o, [f]: baseline.org[f] }), { ...d.org }),
    }))

  if (loadError) {
    return (
      <div className="px-1 py-3 text-[13px] text-text-secondary">
        {loadError}{' '}
        <button type="button" onClick={load} className="text-accent underline">
          Retry
        </button>
      </div>
    )
  }
  if (!loaded) {
    return (
      <div className="px-1 py-3 text-[13px] text-text-tertiary">Loading organization settings…</div>
    )
  }

  // The StepContext every /setup body consumes — the live draft + patch, with
  // advance a no-op (Settings has no linear flow; selfAdvancing pickers just
  // record the choice). orgId/teamId/isLocal ride along for the App panel etc.
  const ctx: StepContext = { orgId, teamId: 'default', isLocal, state: draft, patch, advance: noop }

  // Live GitHub mode — derived from the App registration's active bit, NOT the
  // access tab (a staged App + live PAT is PAT mode with a pending switch). A
  // live App is registered AND active; everything else is PAT (a staged App
  // shows the staged banner inside the access section). This gates the
  // clone-protocol section, which is a PAT-mode + local-only concern.
  const liveApp = draft.githubAppRegistered && !draft.githubAppStaged
  const isPat = !liveApp
  const ghAccessSummary = liveApp
    ? `GitHub App — installed on ${draft.githubAppInstallCount} account${
        draft.githubAppInstallCount === 1 ? '' : 's'
      }`
    : draft.hasGitHubPat
      ? 'Personal access token — connected'
      : 'Not configured'
  const capSummary = baseline.org.max_llm_model_tier
    ? `Capped at ${TIER_LABELS[baseline.org.max_llm_model_tier] ?? baseline.org.max_llm_model_tier}`
    : 'No cap'
  const dailyCapValue = Number(baseline.org.max_daily_cost_usd)
  const dailyCapSummary =
    baseline.org.max_daily_cost_usd.trim() !== '' && dailyCapValue > 0
      ? `Cap at $${dailyCapValue.toLocaleString()} / day`
      : 'No cap'
  // Frontend input-layer validation: a typed cap must be a positive number
  // (blank = no cap). Gates the section's Save and surfaces an inline message.
  const dailyCapErr = dailyCapError(draft.org.max_daily_cost_usd)

  // ── Claude credentials ── Captured via the validated connectAnthropic
  // endpoint (never the bulk org POST). Local shows the system-vs-BYOK source
  // radio; multi shows provider+key only (no system-creds option). "Configured"
  // reflects has_anthropic_api_key (seeded into anthropicConnected by loadOrg).
  const claudeConfigured = baseline.anthropicConnected
  const claudeWantsByok = !isLocal || draft.anthropicKeySource === 'byok'
  const claudeKeyTyped = draft.org.anthropic_api_key.trim() !== ''
  const claudeSummary = claudeConfigured
    ? 'Configured'
    : isLocal
      ? 'System Claude credentials'
      : 'Not configured'
  // Dirty on a source switch (local) or a typed key.
  const claudeDirty =
    (isLocal && draft.anthropicKeySource !== baseline.anthropicKeySource) || claudeKeyTyped
  // BYOK needs a key unless one is already stored ("leave blank to keep current").
  const claudeSaveDisabled = claudeWantsByok && !claudeKeyTyped && !claudeConfigured

  return (
    <div className="divide-y divide-border-subtle">
      {/* ── GitHub URL ── */}
      <SettingsSection
        title="GitHub URL"
        summary={hostOf(baseline.org.github_url || 'github.com')}
        dirty={draft.org.github_url !== baseline.org.github_url}
        saving={isSaving('gh-url')}
        onSave={async () => {
          // The outer mark covers the reachability-probe window (before
          // commitOrgSlice, which marks/unmarks 'gh-url' itself — a Set makes
          // the overlap idempotent).
          setSavingKey('gh-url', true)
          setUrlError(null)
          try {
            const url = normalizeBaseUrl(draft.org.github_url)
            const probe = await checkGitHubReachability(url)
            if (!probe.reachable) {
              setUrlError(reachabilityMessage(probe))
              return false
            }
            patch({ org: { ...draft.org, github_url: url } })
            return await commitOrgSlice('gh-url', { github_url: url }, 'GitHub URL')
          } finally {
            setSavingKey('gh-url', false)
          }
        }}
        onCancel={() => {
          revertOrg(['github_url'])
          setUrlError(null)
        }}
      >
        <GitHubUrlStep {...ctx} error={urlError} />
      </SettingsSection>

      {/* ── GitHub access (one mode header + explicit guided switch) ──
          Either/or per org (TFAC-328): the section states the live mode and
          offers exactly one affordance — switch to the other mode, with a
          reachability diff + confirm — plus the staged-switch banner. It
          replaces the old independently-editable selector / account-type /
          register / install / free-form-PAT sections (the backend 409s a plain
          PAT save while an App is registered, so the switch flow is the only
          path). Auto-opens on a staged switch so the banner is visible. */}
      <SettingsSection
        title="GitHub access"
        summary={ghAccessSummary}
        defaultExpanded={draft.githubAppStaged}
      >
        <GitHubAccessControl ctx={ctx} reload={load} />
      </SettingsSection>

      {/* ── Clone protocol (PAT + local only) ── */}
      {isPat && isLocal && (
        <SettingsSection
          title="Clone protocol"
          summary={`Clone via ${baseline.org.github_clone_protocol.toUpperCase()}`}
          dirty={draft.org.github_clone_protocol !== baseline.org.github_clone_protocol}
          saving={isSaving('gh-clone')}
          onSave={() =>
            commitOrgSlice(
              'gh-clone',
              { github_clone_protocol: draft.org.github_clone_protocol },
              'Clone protocol',
            )
          }
          onCancel={() => revertOrg(['github_clone_protocol'])}
        >
          <GitHubCloneStep {...ctx} />
        </SettingsSection>
      )}

      {/* ── GitHub polling ── */}
      <SettingsSection
        title="GitHub polling"
        summary={`Every ${intervalLabel(baseline.org.github_poll_interval)}`}
        dirty={draft.org.github_poll_interval !== baseline.org.github_poll_interval}
        saving={isSaving('gh-poll')}
        onSave={() =>
          commitOrgSlice(
            'gh-poll',
            { github_poll_interval: draft.org.github_poll_interval },
            'GitHub polling',
          )
        }
        onCancel={() => revertOrg(['github_poll_interval'])}
      >
        <div className="space-y-5">
          <div className="space-y-1.5">
            <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
              How often should we poll GitHub?
            </h2>
            <p className="text-[13px] leading-relaxed text-text-tertiary">
              More frequent polling surfaces new PRs and reviews sooner; less frequent is lighter on
              rate limits.
            </p>
          </div>
          <PollerTimingGroup
            value={{
              github_poll_interval: draft.org.github_poll_interval,
              jira_poll_interval: draft.org.jira_poll_interval,
            }}
            onChange={(p) => patch({ org: { ...draft.org, ...p } })}
            showJira={false}
            bare
          />
        </div>
      </SettingsSection>

      {/* ── Jira connection ── When disconnected, this is a form section whose
          Save ("Connect") performs the connect on the section's button — the
          same shape as the GitHub PAT section, no separate Connect button.
          Once connected there's nothing to save, so it reverts to an action
          section carrying just the inline Disconnect. */}
      {draft.jiraConnected ? (
        <SettingsSection
          title="Jira connection"
          summary={`Connected · ${hostOf(baseline.org.jira_url)}`}
        >
          <JiraAccessGroup
            value={{
              jira_url: draft.org.jira_url,
              jira_pat: draft.org.jira_pat,
              jira_email: draft.org.jira_email,
              jira_api_token: draft.org.jira_api_token,
            }}
            onChange={(p) => patch({ org: { ...draft.org, ...p } })}
            connected
            // The connected view shows only a status line, so the deployment
            // doesn't pick fields here; loadOrg seeded it from the stored host.
            deployment={draft.jiraDeployment ?? 'data_center'}
            onDisconnected={() => {
              setDraft((d) => ({
                ...d,
                jiraConnected: false,
                jiraDeployment: null,
                org: { ...d.org, jira_url: '', jira_pat: '', jira_email: '', jira_api_token: '' },
              }))
              setBaseline((b) => ({ ...b, jiraConnected: false, org: { ...b.org, jira_url: '' } }))
            }}
            bare
          />
        </SettingsSection>
      ) : (
        <SettingsSection
          title="Jira connection"
          summary="Not connected"
          saveLabel="Connect"
          // dirty reflects any draft change against the baseline — it arms the
          // discard guard + unsaved dot, so a partially-typed URL/credential or a
          // deployment pick isn't silently dropped on collapse. The "needs the
          // right fields filled" rule that gates the connect lives in
          // saveDisabled instead (mirrors the old Connect button's condition).
          dirty={
            draft.org.jira_url !== baseline.org.jira_url ||
            draft.jiraDeployment !== null ||
            draft.org.jira_pat.trim() !== '' ||
            draft.org.jira_email.trim() !== '' ||
            draft.org.jira_api_token.trim() !== ''
          }
          saveDisabled={
            draft.org.jira_url.trim() === '' ||
            draft.jiraDeployment === null ||
            (draft.jiraDeployment === 'cloud'
              ? draft.org.jira_email.trim() === '' || draft.org.jira_api_token.trim() === ''
              : draft.org.jira_pat.trim() === '')
          }
          saving={isSaving('jira-connect')}
          onSave={async () => {
            setSavingKey('jira-connect', true)
            try {
              const url = normalizeBaseUrl(draft.org.jira_url)
              const deployment = draft.jiraDeployment ?? 'data_center'
              const result = await connectJira(url, deployment, draft.org)
              if (!result.ok) {
                toast.error(result.error)
                return false
              }
              setDraft((d) => ({
                ...d,
                jiraConnected: true,
                jiraDeployment: deployment,
                org: { ...d.org, jira_url: url, jira_pat: '', jira_email: '', jira_api_token: '' },
              }))
              setBaseline((b) => ({
                ...b,
                jiraConnected: true,
                jiraDeployment: deployment,
                org: { ...b.org, jira_url: url },
              }))
              toast.success('Jira connected')
              return true
            } finally {
              setSavingKey('jira-connect', false)
            }
          }}
          onCancel={() => {
            revertOrg(['jira_url', 'jira_pat', 'jira_email', 'jira_api_token'])
            patch({ jiraDeployment: baseline.jiraDeployment })
          }}
        >
          {/* Deployment picker first (the explicit Cloud-vs-DC choice), then the
              matching credential fields once a deployment is chosen — the
              Settings analog of the wizard's mode → access step split. */}
          <div className="space-y-4">
            <ChoiceCards
              ariaLabel="Jira deployment"
              options={JIRA_DEPLOYMENT_OPTIONS}
              selected={draft.jiraDeployment}
              onChoose={(d) => patch({ jiraDeployment: d })}
            />
            {draft.jiraDeployment !== null && (
              <JiraAccessGroup
                value={{
                  jira_url: draft.org.jira_url,
                  jira_pat: draft.org.jira_pat,
                  jira_email: draft.org.jira_email,
                  jira_api_token: draft.org.jira_api_token,
                }}
                onChange={(p) => patch({ org: { ...draft.org, ...p } })}
                connected={false}
                deployment={draft.jiraDeployment}
                bare
              />
            )}
          </div>
        </SettingsSection>
      )}

      {/* ── Atlassian OAuth app (Cloud only) ── The credential layer the
          per-user one-click "Connect Jira" flow runs against. Cloud-only: OAuth
          3LO is a Cloud concept (Data Center uses a per-user PAT), so it shows
          only for a connected Cloud org. An action section — the card commits
          its own store/remove inline, so there's no Save footer. */}
      {orgId && draft.jiraConnected && draft.jiraDeployment === 'cloud' && (
        <SettingsSection title="Atlassian OAuth app" summary="One-click Connect for Jira">
          <AtlassianOAuthAppCard orgId={orgId} />
        </SettingsSection>
      )}

      {/* ── Slack (TFAC-529) ── EE, multi-mode only: every /api/slack/* seam
          404s an unlicensed org, so the FE hides the surface rather than
          presenting a dead flow — same gating shape as SSO below. An action
          section (no Save footer): the card owns its own connect/disconnect
          controls and list fetch. */}
      {orgId && !isLocal && slackEnt && (
        <SettingsSection title="Slack" summary="Connect a workspace">
          <SlackWorkspacesCard orgId={orgId} />
        </SettingsSection>
      )}

      {/* ── Jira polling (only once connected) ── */}
      {draft.jiraConnected && (
        <SettingsSection
          title="Jira polling"
          summary={`Every ${intervalLabel(baseline.org.jira_poll_interval)}`}
          dirty={draft.org.jira_poll_interval !== baseline.org.jira_poll_interval}
          saving={isSaving('jira-poll')}
          onSave={() =>
            commitOrgSlice(
              'jira-poll',
              { jira_poll_interval: draft.org.jira_poll_interval },
              'Jira polling',
            )
          }
          onCancel={() => revertOrg(['jira_poll_interval'])}
        >
          <div className="space-y-5">
            <div className="space-y-1.5">
              <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
                How often should we poll Jira?
              </h2>
              <p className="text-[13px] leading-relaxed text-text-tertiary">
                The cadence for the Jira tracker — independent of the GitHub poll interval.
              </p>
            </div>
            <PollerTimingGroup
              value={{
                github_poll_interval: draft.org.github_poll_interval,
                jira_poll_interval: draft.org.jira_poll_interval,
              }}
              onChange={(p) => patch({ org: { ...draft.org, ...p } })}
              showGitHub={false}
              bare
            />
          </div>
        </SettingsSection>
      )}

      {/* ── Model cap ── */}
      <SettingsSection
        title="Model cap"
        summary={capSummary}
        dirty={draft.org.max_llm_model_tier !== baseline.org.max_llm_model_tier}
        saving={isSaving('model')}
        onSave={() =>
          commitOrgSlice('model', { max_llm_model_tier: draft.org.max_llm_model_tier }, 'Model cap')
        }
        onCancel={() => revertOrg(['max_llm_model_tier'])}
      >
        <OrgModelStep {...ctx} />
      </SettingsSection>

      {/* ── Daily spend cap (TFAC-477) ── A runaway-spend fuse: when the org's
          LLM spend for the current UTC day reaches this ceiling, every new agent
          run (manual + autonomous) is refused at the delegation choke point.
          In-flight runs are unaffected. Empty / 0 = no cap. */}
      <SettingsSection
        title="Daily spend cap"
        summary={dailyCapSummary}
        dirty={draft.org.max_daily_cost_usd !== baseline.org.max_daily_cost_usd}
        saving={isSaving('daily-cap')}
        saveDisabled={dailyCapErr !== null}
        onSave={() =>
          commitOrgSlice(
            'daily-cap',
            { max_daily_cost_usd: draft.org.max_daily_cost_usd },
            'Daily spend cap',
          )
        }
        onCancel={() => revertOrg(['max_daily_cost_usd'])}
      >
        <div className="space-y-5">
          <div className="space-y-1.5">
            <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
              Cap the org&rsquo;s daily LLM spend
            </h2>
            <p className="text-[13px] leading-relaxed text-text-tertiary">
              A safety fuse against runaway spend (most often a misconfigured autonomous trigger).
              When today&rsquo;s total LLM spend — measured on the UTC calendar day, across every
              category — reaches this amount, new agent runs, manual and autonomous alike, are
              blocked until tomorrow or until you raise the cap. In-flight runs keep going. Leave
              blank for no cap.
            </p>
          </div>
          <label className="block max-w-[220px]">
            <span className="mb-1.5 block text-[11px] text-text-tertiary">Daily limit (USD)</span>
            <div className="relative">
              <span className="pointer-events-none absolute left-4 top-1/2 -translate-y-1/2 text-[13px] text-text-tertiary">
                $
              </span>
              <input
                type="number"
                // Smallest valid positive cap (step is 0.01); aligns the native
                // spinner/validity with dailyCapError so the arrows can't land on
                // 0 — a value Save then rejects. Blank ("no cap") is still allowed.
                min="0.01"
                step="0.01"
                inputMode="decimal"
                placeholder="No cap"
                aria-invalid={dailyCapErr !== null}
                aria-describedby={dailyCapErr !== null ? 'daily-cap-error' : undefined}
                value={draft.org.max_daily_cost_usd}
                onChange={(e) =>
                  patch({ org: { ...draft.org, max_daily_cost_usd: e.target.value } })
                }
                className={`${inputClass} pl-7 ${
                  dailyCapErr !== null ? 'border-dismiss/60 focus:border-dismiss/60' : ''
                }`}
              />
            </div>
            {dailyCapErr !== null && (
              <span id="daily-cap-error" className="mt-1.5 block text-[11px] text-dismiss">
                {dailyCapErr}
              </span>
            )}
          </label>
        </div>
      </SettingsSection>

      {/* ── Claude credentials ── Save drives the validated connectAnthropic
          endpoint (not the bulk org POST). Local: source radio, then provider +
          key when BYOK. Multi: provider + key only. */}
      <SettingsSection
        title="Claude credentials"
        summary={claudeSummary}
        dirty={claudeDirty}
        saving={isSaving('claude')}
        saveDisabled={claudeSaveDisabled}
        onSave={async () => {
          setSavingKey('claude', true)
          try {
            const useSystem = isLocal && draft.anthropicKeySource === 'system'
            const key = draft.org.anthropic_api_key.trim()
            // BYOK + blank + already configured = "leave blank to keep current":
            // a no-op that must NOT POST an empty key (which would clear it).
            if (!useSystem && key === '') {
              if (claudeConfigured) {
                patch({ org: { ...draft.org, anthropic_api_key: '' } })
                return true
              }
              toast.error('Paste an Anthropic API key.')
              return false
            }
            const r = await connectAnthropic(useSystem ? '' : key)
            if (!r.ok) {
              toast.error(r.error)
              return false
            }
            const nowConfigured = !useSystem
            const nextSource = isLocal ? (useSystem ? 'system' : 'byok') : draft.anthropicKeySource
            setBaseline((b) => ({
              ...b,
              anthropicConnected: nowConfigured,
              anthropicKeySource: nextSource,
              org: { ...b.org, anthropic_api_key: '' },
            }))
            setDraft((d) => ({
              ...d,
              anthropicConnected: nowConfigured,
              anthropicKeySource: nextSource,
              org: { ...d.org, anthropic_api_key: '' },
            }))
            toast.success(useSystem ? 'Using system Claude credentials' : 'Claude API key saved')
            return true
          } finally {
            setSavingKey('claude', false)
          }
        }}
        onCancel={() =>
          setDraft((d) => ({
            ...d,
            anthropicKeySource: baseline.anthropicKeySource,
            org: { ...d.org, anthropic_api_key: '' },
          }))
        }
      >
        <div className="space-y-4">
          {isLocal && (
            <ChoiceCards
              ariaLabel="Claude credential source"
              options={CLAUDE_SOURCE_OPTIONS}
              selected={draft.anthropicKeySource}
              onChoose={(k) => patch({ anthropicKeySource: k })}
            />
          )}
          {claudeWantsByok && (
            <>
              <ClaudeProviderCards />
              <AnthropicKeyField
                value={draft.org.anthropic_api_key}
                onChange={(v) => patch({ org: { ...draft.org, anthropic_api_key: v } })}
                hasKey={claudeConfigured}
              />
            </>
          )}
        </div>
      </SettingsSection>

      {/* Add-team is hosted-only (POST /api/teams 404s in local). */}
      {!isLocal && (
        <SettingsSection title="Teams" summary="Add or review teams">
          <TeamManagementSection />
        </SettingsSection>
      )}

      {/* Single sign-on (TFAC-429) — multi-mode only: SAML/SSO lives in the
          GoTrue stack, so every /api/sso/* route 404s in local. Also gated on
          the `sso` entitlement — every /api/sso/* seam 404s an unlicensed org,
          so the FE hides the surface rather than presenting a dead flow. An
          action section (no Save footer) — SSOSettings owns its own
          register / enable / domain-claim / verify controls. The surface is
          admin-gated (the /org Settings tab only renders for org admins). */}
      {!isLocal && sso && (
        <SettingsSection title="Single sign-on" summary="SAML via your identity provider">
          <SSOSettings />
        </SettingsSection>
      )}

      <SettingsSection title="Integrations" summary="Import Claude Code skills">
        <SkillsImport />
      </SettingsSection>

      <SettingsSection title="Danger zone" summary="Clear stored tokens">
        <DangerZone />
      </SettingsSection>
    </div>
  )
}

// SkillsImport — the action body for the Integrations section.
function SkillsImport() {
  const [importing, setImporting] = useState(false)
  const run = async () => {
    if (importing) return
    setImporting(true)
    try {
      const res = await fetch('/api/skills/import', { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to import skills'))
        return
      }
      const result = await res.json()
      if (result.imported > 0) {
        toast.success(
          `Imported ${result.imported} skill${result.imported !== 1 ? 's' : ''} (${result.skipped} already imported)`,
        )
      } else {
        toast.info(
          `No new skills found (${result.scanned} scanned, ${result.skipped} already imported)`,
        )
      }
    } catch (err) {
      toast.error(`Failed to import skills: ${(err as Error).message}`)
    } finally {
      setImporting(false)
    }
  }
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <p className="text-[13px] text-text-primary">Import Claude Code Skills</p>
        <p className="mt-0.5 text-[11px] text-text-tertiary">
          Import SKILL.md files from ~/.claude/skills/ as delegation prompts
        </p>
      </div>
      <button
        type="button"
        onClick={() => void run()}
        disabled={importing}
        className="shrink-0 rounded-xl border border-accent/20 px-4 py-2 text-[13px] text-accent transition-colors hover:border-accent/30 hover:text-accent/80 disabled:opacity-40"
      >
        {importing ? 'Importing…' : 'Import Skills'}
      </button>
    </div>
  )
}

// DangerZone — clear all stored integration tokens. Credentials are re-entered
// through the access sections above; the tenant itself is untouched.
function DangerZone() {
  return (
    <button
      type="button"
      onClick={async () => {
        if (!confirm('Clear all stored tokens? You will need to re-authenticate.')) return
        await fetch('/api/integrations', { method: 'DELETE' })
        window.location.reload()
      }}
      className="rounded-xl border border-dismiss/20 px-4 py-2 text-[13px] text-dismiss transition-colors hover:border-dismiss/30 hover:text-dismiss/80"
    >
      Clear All Tokens
    </button>
  )
}
