// The Organization group of the Settings stack — rendered only for org admins.
// Most sections' BODY is the actual /setup step component (GitHubUrlStep,
// GitHubCloneStep, OrgModelStep, …) fed a WizardState draft, so Settings and
// /setup are literally the same components — same visuals, same copy.
//
// GitHub *access* is the exception (TFAC-328/329): it's either/or per org, so
// the old independently-editable selector / account-type / App-register /
// App-install / free-form-PAT sections are replaced by ONE "GitHub access"
// section — a mode header plus a guided switch and, on a PAT org, an in-place
// token replacement (GitHubAccessControl), which composes the same step
// components internally (account type, App panel, install view) but routes every
// credential change through a validated preflight + an inform-only reachability
// diff. There is no free-form PAT field: the settings PATCH carries no
// credential at all.
//
// What Settings adds over /setup is the per-section Save model (the approved
// design): expand a section, edit its draft, Save (or Cancel/discard). The
// org-form sections all persist through the single org-settings PATCH, each
// sending ONLY its own fields — so saving one section never flushes another's
// unsaved edits, and never carries a value its user wasn't looking at.
// Selector/panel sections (the GitHub access control, the App register panel)
// carry no Save footer, and Jira disconnect commits inline on its own button.
// The Jira *credential* is the exception that proves the rule it used to
// break: its Save footer ("Connect" / "Replace credential") drives
// PUT /api/orgs/{org}/jira/access/credential rather than the org POST, but
// it's a footer all the same.

import { useCallback, useEffect, useRef, useState } from 'react'
import TeamPicker from '../../../components/TeamPicker'
import { toast } from '../../../components/Toast/toastStore'
import { modelDisplayName } from '../../../hooks/useModelCatalog'
import { noteWrittenTeam, useWriteTeam } from '../../../hooks/useTeams'
import { apiJSON, httpErrorMessage } from '../../../lib/apiClient'
import {
  hostOf,
  normalizeBaseUrl,
  checkGitHubReachability,
  reachabilityMessage,
} from '../../../lib/reachability'
import { GitHubUrlStep, GitHubCloneStep } from '../../setup/GitHubStep'
import { OrgBackgroundJobsModelStep, OrgModelStep } from '../../setup/ModelStep'
import {
  initialWizardState,
  loadOrg,
  loadGitHubAppInstall,
  bedrockFormError,
} from '../../setup/steps'
import type { StepContext, WizardState } from '../../setup/types'
import PollerTimingGroup from '../PollerTimingGroup'
import { inputClass } from '../primitives'
import JiraAccessGroup from '../JiraAccessGroup'
import EventSourcesGroup from '../EventSourcesGroup'
import AtlassianOAuthAppCard from '../AtlassianOAuthAppCard'
import SlackWorkspacesCard from '../SlackWorkspacesCard'
import TeamManagementSection from '../../../components/TeamManagementSection'
import {
  dailyCapError,
  concurrentRunsError,
  MAX_CONCURRENT_RUNS_CEILING,
  fetchOrgSettings,
  patchOrgSettings,
  type OrgConfigForm,
  type OrgSettingsPatch,
} from '../orgConfig'
import { connectJira, JIRA_DEPLOYMENT_OPTIONS } from '../jiraConnect'
import { disconnectGitHubPAT, disconnectJira } from '../orgCredentials'
import { connectAnthropic, disconnectLLM, CLAUDE_SOURCE_OPTIONS } from '../anthropicConnect'
import { connectBedrock, bedrockPayloadFromForm } from '../bedrockConnect'
import { ClaudeProviderCards, AnthropicKeyField, BedrockFields } from '../../setup/ClaudeStep'
import { ChoiceCards } from '../../setup/parts'
import GitHubAccessControl from './GitHubAccessControl'
import SSOSettings from './SSOSettings'
import SettingsSection from './SettingsSection'
import FailedEventsPanel from '../FailedEventsPanel'
import { useFailedEvents, type UseFailedEvents } from '../../../hooks/useFailedEvents'
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
  // The connected Jira section, re-opened as its own connect form to bind a
  // replacement credential in place. Not derived from the draft: it's a UI
  // intent ("I want to retype this"), and it has to survive the fields being
  // blank, which is the state it starts in.
  const [jiraRebinding, setJiraRebinding] = useState(false)

  // Parked event_queue rows. Fetched at this level, not inside the panel,
  // because the collapsed section's badge has to show the count before the
  // body mounts — a diagnostics surface nobody opens is a diagnostics surface
  // nobody sees. Enabled unconditionally: OrgSettings only renders for an org
  // admin (multi) or N=1 (local), which is exactly the API's own gate.
  const failedEvents = useFailedEvents(true)

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
    // reload) — one GET /github/app, not two. Both loaders catch internally and
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

  // commitOrgSlice persists one section's fields through the org-settings
  // PATCH, folding the slice back into the baseline on success. Only the
  // slice's own fields ride the wire — the PATCH's absent-means-keep contract
  // covers the rest — so a section can never carry a neighbour's draft or
  // clobber a concurrent change to fields it doesn't own.
  //
  // The baseline carries the row's concurrency token, so the fold-back has to
  // include the version the save just produced — the next section's save
  // asserts it, and without the refresh it would conflict with this one's own
  // write. A save that LOSES the race (another admin committed in between)
  // reloads the whole group rather than reporting a retryable error: the draft
  // the user is looking at was assembled from a row that no longer exists.
  const commitOrgSlice = useCallback(
    async (key: string, slice: OrgSettingsPatch, label: string): Promise<boolean> => {
      if (!orgId) return false
      setSavingKey(key, true)
      try {
        const res = await patchOrgSettings(orgId, baseline.org.version, slice)
        if (!res.ok) {
          toast.error(res.error)
          if (res.conflict) load()
          return false
        }
        setBaseline((b) => ({
          ...b,
          org: { ...b.org, ...slice, version: res.settings.version },
        }))
        if (res.settings.warning) toast.info(res.settings.warning)
        toast.success(`${label} saved`)
        return true
      } finally {
        setSavingKey(key, false)
      }
    },
    [baseline.org.version, orgId, load],
  )

  // refreshOrgVersion folds the settings row's current concurrency token into
  // the live baseline + draft after a write that lands OUTSIDE the settings
  // PATCH: the credential binds and unbinds persist their host / key-ref
  // columns on the same row server-side, which bumps the token, so the one
  // this screen holds goes stale the moment a connect succeeds — and the next
  // section's save would 409 against a change this same screen just made.
  // Best-effort: on a failed re-read the held token stands, and the save
  // path's conflict → reload recovers. It also RETURNS the token, for the one
  // caller that has to write with it in the same turn — state updates are not
  // readable until the next render, so a follow-up PATCH cannot pick it up from
  // the draft.
  const refreshOrgVersion = useCallback(async (): Promise<number | undefined> => {
    if (!orgId) return undefined
    const fresh = await fetchOrgSettings(orgId)
    if (!fresh) return undefined
    setBaseline((b) => ({ ...b, org: { ...b.org, version: fresh.version } }))
    setDraft((d) => ({ ...d, org: { ...d.org, version: fresh.version } }))
    return fresh.version
  }, [orgId])

  // revertOrg resets the named org fields in the draft back to the baseline —
  // a section's Cancel, scoped so it never touches a neighbour's edits.
  const revertOrg = (fields: (keyof OrgConfigForm)[]) =>
    setDraft((d) => ({
      ...d,
      org: fields.reduce((o, f) => ({ ...o, [f]: baseline.org[f] }), { ...d.org }),
    }))

  if (loadError) {
    return (
      <div className="px-1 py-3 text-body text-ink-2">
        {loadError}{' '}
        <button type="button" onClick={load} className="text-warm underline">
          Retry
        </button>
      </div>
    )
  }
  if (!loaded) {
    return <div className="px-1 py-3 text-body text-ink-3">Loading organization settings…</div>
  }

  // The StepContext every /setup body consumes — the live draft + patch, with
  // advance a no-op (Settings has no linear flow; selfAdvancing pickers just
  // record the choice). orgId/isLocal ride along for the App panel etc.;
  // teamId only satisfies the context shape — no org step reads it, and the
  // "default" alias would not resolve on a multi-mode team route.
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
  // "Not set" is not a gap to paper over — it is the state in which the three
  // background jobs do not run, so the collapsed section says so.
  const backgroundJobsSummary = baseline.org.background_jobs_model
    ? modelDisplayName(baseline.org.background_jobs_model)
    : 'Not set — background jobs are off'
  const dailyCapValue = Number(baseline.org.max_daily_cost_usd)
  const dailyCapSummary =
    baseline.org.max_daily_cost_usd.trim() !== '' && dailyCapValue > 0
      ? `Cap at $${dailyCapValue.toLocaleString()} / day`
      : 'No cap'
  // Frontend input-layer validation: a typed cap must be a positive number
  // (blank = no cap). Gates the section's Save and surfaces an inline message.
  const dailyCapErr = dailyCapError(draft.org.max_daily_cost_usd)
  const concurrentRunsValue = Number(baseline.org.max_concurrent_runs)
  const concurrentRunsSummary =
    baseline.org.max_concurrent_runs.trim() !== '' && concurrentRunsValue > 0
      ? `${concurrentRunsValue} at once`
      : 'Unlimited'
  // Frontend input-layer validation: a typed limit must be a non-negative whole
  // number (blank or 0 = unlimited). Gates the section's Save + inline message.
  const concurrentRunsErr = concurrentRunsError(draft.org.max_concurrent_runs)

  // ── Claude credentials ── Captured via the validated connectAnthropic /
  // connectBedrock endpoints (never the bulk org POST). Local shows the
  // system-vs-BYOK source radio; multi shows provider + credentials only (no
  // system-creds option). The provider radio picks which credential this save
  // binds, not which one the org holds: both can be stored at once, and the
  // summary names every provider that is.
  const claudeWantsByok = !isLocal || draft.anthropicKeySource === 'byok'
  const claudeKeyTyped = draft.org.anthropic_api_key.trim() !== ''
  const connectedProviders = [
    baseline.anthropicConnected ? 'Anthropic' : '',
    baseline.bedrockConnected ? 'Amazon Bedrock' : '',
  ].filter(Boolean)
  // Nothing bound is two different states, and the stored source is what tells
  // them apart: an org running on the machine's credentials is configured, and
  // one that brings its own and has bound none is not.
  const claudeSummary = connectedProviders.length
    ? `Configured · ${connectedProviders.join(' + ')}`
    : baseline.anthropicKeySource === 'system'
      ? 'System Claude credentials'
      : 'Not configured'
  const bedrockSelected = claudeWantsByok && draft.claudeProvider === 'bedrock'
  const bedrockSecretTyped =
    draft.org.bedrock_bearer_token !== '' ||
    draft.org.aws_access_key_id !== '' ||
    draft.org.aws_secret_access_key !== '' ||
    draft.org.aws_session_token !== ''
  const bedrockConfigChanged =
    draft.org.bedrock_auth_method !== baseline.org.bedrock_auth_method ||
    draft.org.bedrock_role_arn !== baseline.org.bedrock_role_arn ||
    draft.org.bedrock_region !== baseline.org.bedrock_region ||
    draft.org.bedrock_model_id !== baseline.org.bedrock_model_id ||
    draft.org.bedrock_base_url !== baseline.org.bedrock_base_url
  // Input-layer gate for the Bedrock form — the same rules the wizard step
  // validates, so Save blocks before bouncing off the backend's 422.
  const bedrockErr = bedrockSelected ? bedrockFormError(draft) : null
  // Dirty on a source switch (local), a provider switch, a typed secret, or a
  // Bedrock config edit.
  const claudeDirty =
    (isLocal && draft.anthropicKeySource !== baseline.anthropicKeySource) ||
    draft.claudeProvider !== baseline.claudeProvider ||
    claudeKeyTyped ||
    (bedrockSelected && (bedrockSecretTyped || bedrockConfigChanged))
  // Anthropic BYOK needs a key unless one is already stored ("leave blank to
  // keep current"); Bedrock gates on its own form validation.
  const claudeSaveDisabled = bedrockSelected
    ? bedrockErr !== null
    : claudeWantsByok && !claudeKeyTyped && !baseline.anthropicConnected

  return (
    <div className="divide-y divide-line-1">
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

      {/* ── GitHub access (mode header + guided switch / token replacement) ──
          Either/or per org (TFAC-328): the section states the live mode and the
          identity it connects as, and offers a switch to the other mode — with a
          reachability diff + confirm — plus, on a PAT org, replacing the token in
          place. Both go through the same validate-then-confirm path; the staged-
          switch banner takes over when a PAT→App switch is pending. It replaces
          the old independently-editable selector / account-type / register /
          install / free-form-PAT sections. Auto-opens on a staged switch so the
          banner is visible. */}
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
            <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
              How often should we poll GitHub?
            </h2>
            <p className="text-body leading-relaxed text-ink-3">
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

      {/* ── Jira connection ── Two faces of one section. Without a credential
          to show, it's a form whose Save ("Connect") performs the bind on the
          section's own button — no separate Connect button anywhere. Connected,
          it collapses to a status line carrying the inline Disconnect and
          "Replace credential"; the latter just re-opens this same form
          (jiraRebinding) against the still-connected org, because the credential
          resource's PUT accepts a bind on one. Rotating therefore no longer
          starts with a disconnect, which is what used to open a window with no
          credential stored and the poller stopped. */}
      {draft.jiraConnected && !jiraRebinding ? (
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
            orgId={orgId}
            // The connected view shows only a status line, so the deployment
            // doesn't pick fields here; loadOrg seeded it from the stored host.
            deployment={draft.jiraDeployment ?? 'data_center'}
            onReplace={() => setJiraRebinding(true)}
            // Suppresses that rebind (and says why) when TRIAGE_FACTORY_JIRA_*
            // supplies the host or the token: the overlay wins on read, so a
            // credential typed here would be stored and then ignored. Reported,
            // not managed — the same rule the GitHub section applies to an
            // env-supplied PAT. Disconnect stays available: it's honest about
            // its outcome, warning that the env vars keep supplying the value.
            envProvided={draft.jiraCredentialEnvProvided}
            onDisconnected={() => {
              setDraft((d) => ({
                ...d,
                jiraConnected: false,
                jiraDeployment: null,
                org: { ...d.org, jira_url: '', jira_pat: '', jira_email: '', jira_api_token: '' },
              }))
              setBaseline((b) => ({
                ...b,
                jiraConnected: false,
                jiraDeployment: null,
                org: { ...b.org, jira_url: '' },
              }))
              // The disconnect also cleared the URL on the settings row.
              void refreshOrgVersion()
            }}
            bare
          />
        </SettingsSection>
      ) : (
        <SettingsSection
          title="Jira connection"
          summary={jiraRebinding ? `Connected · ${hostOf(baseline.org.jira_url)}` : 'Not connected'}
          saveLabel={jiraRebinding ? 'Replace credential' : 'Connect'}
          // dirty reflects any draft change against the baseline — it arms the
          // discard guard + unsaved dot, so a partially-typed URL/credential or a
          // deployment pick isn't silently dropped on collapse. The "needs the
          // right fields filled" rule that gates the connect lives in
          // saveDisabled instead (mirrors the old Connect button's condition).
          // The deployment term compares against the baseline rather than null so
          // it means the same thing in both modes: a fresh org's baseline is
          // null, so any pick is a change; a rebinding org is only dirty if it
          // switches backends.
          dirty={
            draft.org.jira_url !== baseline.org.jira_url ||
            draft.jiraDeployment !== baseline.jiraDeployment ||
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
              if (!orgId) {
                toast.error('No organization context — reload and try again.')
                return false
              }
              // Same call for a first bind and a replacement: the backend
              // validates against the host before it stores anything, so a bad
              // credential 422s with the org still on the one it had.
              const result = await connectJira(orgId, url, deployment, draft.org)
              if (!result.ok) {
                toast.error(result.error)
                return false
              }
              // The bind also persisted the URL onto the settings row.
              await refreshOrgVersion()
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
              toast.success(jiraRebinding ? 'Jira credential replaced' : 'Jira connected')
              setJiraRebinding(false)
              return true
            } finally {
              setSavingKey('jira-connect', false)
            }
          }}
          onCancel={() => {
            revertOrg(['jira_url', 'jira_pat', 'jira_email', 'jira_api_token'])
            patch({ jiraDeployment: baseline.jiraDeployment })
            setJiraRebinding(false)
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
                orgId={orgId}
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

      {/* ── Event sources ── Which sources may produce events for the org.
          An action section: each switch commits inline through
          PATCH /api/orgs/{org}/sources/{kind}, so there is no Save footer.
          canEdit is unconditional because this whole group renders only for an
          org admin (multi) or N=1 (local) — the same gate the route applies. */}
      <SettingsSection title="Event sources" summary="Which sources create tasks">
        <EventSourcesGroup orgId={orgId} canEdit />
      </SettingsSection>

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
              <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
                How often should we poll Jira?
              </h2>
              <p className="text-body leading-relaxed text-ink-3">
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

      {/* ── Background jobs model ── The one model the scorer, the project
          classifier and the repo profiler all run on. Empty is a real state:
          those jobs skip until somebody picks, because nothing falls back. */}
      <SettingsSection
        title="Background jobs model"
        summary={backgroundJobsSummary}
        dirty={draft.org.background_jobs_model !== baseline.org.background_jobs_model}
        saving={isSaving('background-jobs-model')}
        onSave={() =>
          commitOrgSlice(
            'background-jobs-model',
            { background_jobs_model: draft.org.background_jobs_model },
            'Background jobs model',
          )
        }
        onCancel={() => revertOrg(['background_jobs_model'])}
      >
        <OrgBackgroundJobsModelStep {...ctx} allowOff />
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
            <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
              Cap the org&rsquo;s daily LLM spend
            </h2>
            <p className="text-body leading-relaxed text-ink-3">
              A safety fuse against runaway spend (most often a misconfigured autonomous trigger).
              When today&rsquo;s total LLM spend — measured on the UTC calendar day, across every
              category — reaches this amount, new agent runs, manual and autonomous alike, are
              blocked until tomorrow or until you raise the cap. In-flight runs keep going. Leave
              blank for no cap.
            </p>
          </div>
          <label className="block max-w-[220px]">
            <span className="mb-1.5 block text-reported text-ink-3">Daily limit (USD)</span>
            <div className="relative">
              <span className="pointer-events-none absolute left-4 top-1/2 -translate-y-1/2 text-body text-ink-3">
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
                  dailyCapErr !== null ? 'border-alarm/60 focus:border-alarm/60' : ''
                }`}
              />
            </div>
            {dailyCapErr !== null && (
              <span id="daily-cap-error" className="mt-1.5 block text-reported text-alarm">
                {dailyCapErr}
              </span>
            )}
          </label>
        </div>
      </SettingsSection>

      {/* ── Concurrent run limit ── An admission ceiling on how many of the org's
          runs execute at once across the fleet — the instantaneous sibling of
          the daily spend cap. When the org is at its limit, further queued runs
          wait (invisible to the claim) until an active one finishes; nothing is
          dropped. Protects the org's own downstream (GitHub App rate limits, CI,
          a flood of PRs) from an event storm. Blank or 0 = unlimited. Read live
          by the claim, so a change takes effect on the next claim. */}
      <SettingsSection
        title="Concurrent run limit"
        summary={concurrentRunsSummary}
        dirty={draft.org.max_concurrent_runs !== baseline.org.max_concurrent_runs}
        saving={isSaving('concurrent-runs')}
        saveDisabled={concurrentRunsErr !== null}
        onSave={() =>
          commitOrgSlice(
            'concurrent-runs',
            { max_concurrent_runs: draft.org.max_concurrent_runs },
            'Concurrent run limit',
          )
        }
        onCancel={() => revertOrg(['max_concurrent_runs'])}
      >
        <div className="space-y-5">
          <div className="space-y-1.5">
            <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
              Limit concurrent agent runs
            </h2>
            <p className="text-body leading-relaxed text-ink-3">
              A ceiling on how many agent runs execute at once across the fleet. Use it to protect
              your own downstream — a burst of events can otherwise open many pull requests at once,
              each triggering a CI build, and hammer your GitHub App&rsquo;s rate limits. Runs over
              the limit wait in the queue until an active one finishes; none are dropped, and
              in-flight runs keep going. Leave blank (or 0) for no limit.
            </p>
          </div>
          <label className="block max-w-[220px]">
            <span className="mb-1.5 block text-reported text-ink-3">Max runs at once</span>
            <input
              type="number"
              min="0"
              max={MAX_CONCURRENT_RUNS_CEILING}
              step="1"
              inputMode="numeric"
              placeholder="Unlimited"
              aria-invalid={concurrentRunsErr !== null}
              aria-describedby={concurrentRunsErr !== null ? 'concurrent-runs-error' : undefined}
              value={draft.org.max_concurrent_runs}
              onChange={(e) =>
                patch({ org: { ...draft.org, max_concurrent_runs: e.target.value } })
              }
              className={`${inputClass} ${
                concurrentRunsErr !== null ? 'border-alarm/60 focus:border-alarm/60' : ''
              }`}
            />
            {concurrentRunsErr !== null && (
              <span id="concurrent-runs-error" className="mt-1.5 block text-reported text-alarm">
                {concurrentRunsErr}
              </span>
            )}
          </label>
        </div>
      </SettingsSection>

      {/* ── Claude credentials ── Save drives the selected provider's validated
          bind route (connectAnthropic / connectBedrock — never the org settings
          PATCH). Local: source radio, then provider + credentials when BYOK.
          Multi: provider + credentials only. */}
      <SettingsSection
        title="Claude credentials"
        summary={claudeSummary}
        dirty={claudeDirty}
        saving={isSaving('claude')}
        saveDisabled={claudeSaveDisabled}
        onSave={async () => {
          if (!orgId) return false
          setSavingKey('claude', true)
          try {
            const useSystem = isLocal && draft.anthropicKeySource === 'system'

            // Amazon Bedrock: the selected shape's fields go to that shape's
            // bind route, which replaces the credential — so the secret is
            // always required, even for a region-only edit.
            if (!useSystem && draft.claudeProvider === 'bedrock') {
              if (bedrockErr !== null) {
                toast.error(bedrockErr)
                return false
              }
              const r = await connectBedrock(orgId, bedrockPayloadFromForm(draft.org))
              if (!r.ok) {
                toast.error(r.error)
                return false
              }
              // The bind persisted its key ref onto the settings row.
              await refreshOrgVersion()
              const clearSecrets = {
                bedrock_bearer_token: '',
                aws_access_key_id: '',
                aws_secret_access_key: '',
                aws_session_token: '',
                anthropic_api_key: '',
              }
              const storedConfig = {
                bedrock_auth_method: draft.org.bedrock_auth_method,
                bedrock_role_arn: draft.org.bedrock_role_arn,
                bedrock_external_id: draft.org.bedrock_external_id,
                bedrock_region: draft.org.bedrock_region,
                bedrock_model_id: draft.org.bedrock_model_id,
                bedrock_base_url: draft.org.bedrock_base_url,
              }
              // An Anthropic key the org already holds is untouched by this
              // bind — both providers stay connected, and each run resolves the
              // one its model is served by.
              const apply = (s: WizardState): WizardState => ({
                ...s,
                claudeProvider: 'bedrock',
                bedrockConnected: true,
                bedrockStoredMethod: draft.org.bedrock_auth_method,
                anthropicKeySource: 'byok',
                org: { ...s.org, ...storedConfig, ...clearSecrets },
              })
              setBaseline(apply)
              setDraft(apply)
              toast.success('Amazon Bedrock credentials saved')
              return true
            }

            // "System credentials" is a stored selection with a precondition:
            // the removals first (two, because they are two credentials), then
            // the settings write that records the choice — which the backend
            // refuses while any provider material is still bound. The choice is
            // what a reload reads back; the removals only make it legal.
            if (useSystem) {
              const removed = await disconnectLLM(orgId)
              if (!removed.ok) {
                toast.error(removed.error)
                return false
              }
              const version = (await refreshOrgVersion()) ?? draft.org.version
              const saved = await patchOrgSettings(orgId, version, { llm_auth_method: 'system' })
              if (!saved.ok) {
                toast.error(saved.error)
                return false
              }
              const apply = (s: WizardState): WizardState => ({
                ...s,
                anthropicConnected: false,
                anthropicKeySource: 'system',
                bedrockConnected: false,
                bedrockStoredMethod: null,
                org: { ...s.org, anthropic_api_key: '', version: saved.settings.version },
              })
              setBaseline(apply)
              setDraft(apply)
              toast.success('Using system Claude credentials')
              return true
            }

            const key = draft.org.anthropic_api_key.trim()
            // BYOK + blank + already configured is a no-op: nothing was typed,
            // so there is nothing to rotate. The bind requires a key, so there
            // is no blank call to make by accident.
            if (key === '') {
              if (baseline.anthropicConnected) {
                patch({ org: { ...draft.org, anthropic_api_key: '' } })
                return true
              }
              toast.error('Paste an Anthropic API key.')
              return false
            }
            const r = await connectAnthropic(orgId, key)
            if (!r.ok) {
              toast.error(r.error)
              return false
            }
            // The bind rewrote the key ref on the settings row, and recorded
            // that the org is on its own credentials — an org holding a key is
            // not running on the machine's, so nothing asks it to say so twice.
            await refreshOrgVersion()
            // Binding an Anthropic key leaves stored Bedrock material alone:
            // both providers stay connected, and each run resolves the one its
            // model is served by.
            const apply = (s: WizardState): WizardState => ({
              ...s,
              anthropicConnected: true,
              anthropicKeySource: 'byok',
              claudeProvider: 'anthropic',
              org: { ...s.org, anthropic_api_key: '' },
            })
            setBaseline(apply)
            setDraft(apply)
            toast.success('Claude API key saved')
            return true
          } finally {
            setSavingKey('claude', false)
          }
        }}
        onCancel={() =>
          setDraft((d) => ({
            ...d,
            anthropicKeySource: baseline.anthropicKeySource,
            claudeProvider: baseline.claudeProvider,
            org: {
              ...d.org,
              anthropic_api_key: '',
              bedrock_auth_method: baseline.org.bedrock_auth_method,
              bedrock_bearer_token: '',
              aws_access_key_id: '',
              aws_secret_access_key: '',
              aws_session_token: '',
              bedrock_role_arn: baseline.org.bedrock_role_arn,
              bedrock_external_id: baseline.org.bedrock_external_id,
              bedrock_region: baseline.org.bedrock_region,
              bedrock_model_id: baseline.org.bedrock_model_id,
              bedrock_base_url: baseline.org.bedrock_base_url,
            },
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
              <ClaudeProviderCards
                selected={draft.claudeProvider}
                onSelect={(kind) => patch({ claudeProvider: kind })}
              />
              {draft.claudeProvider === 'anthropic' ? (
                <AnthropicKeyField
                  value={draft.org.anthropic_api_key}
                  onChange={(v) => patch({ org: { ...draft.org, anthropic_api_key: v } })}
                  hasKey={baseline.anthropicConnected}
                />
              ) : (
                <BedrockFields
                  form={draft.org}
                  onChange={(p) => patch({ org: { ...draft.org, ...p } })}
                  hasStored={
                    baseline.bedrockConnected &&
                    baseline.bedrockStoredMethod === draft.org.bedrock_auth_method
                  }
                />
              )}
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

      {/* Filesystem skill import is local-only: it scans the server
          process's own ~/.claude/skills, which is only meaningful when
          that process runs on the user's machine (the backend 501s it
          in multi mode). Multi mode imports a skill by pasting or
          uploading its SKILL.md — it becomes a prompt row scoped to the
          org/team, the design of record for skills in multi mode. */}
      <SettingsSection title="Integrations" summary="Import Claude Code skills">
        <div className="space-y-5">
          {isLocal && <SkillsImport />}
          <SkillPasteImport />
        </div>
      </SettingsSection>

      {/* ── Parked events ── The operator surface over routing work the event
          queue gave up on. Sits with the other operational sections rather
          than in the danger zone: requeueing is a recovery, not a destructive
          act. The summary carries the count so a non-zero population is
          visible without expanding — the whole point of a diagnostics panel
          is that you notice it before you go looking. */}
      <SettingsSection title="Parked events" summary={<ParkedEventsBadge state={failedEvents} />}>
        <FailedEventsPanel state={failedEvents} />
      </SettingsSection>

      <SettingsSection title="Danger zone" summary="Clear stored tokens">
        <DangerZone orgId={orgId} />
      </SettingsSection>
    </div>
  )
}

// ParkedEventsBadge is the Parked events section's collapsed summary. Zero is
// the healthy state and reads as plain text; anything above zero gets a
// warning-toned count pill, because a parked row is work that was silently
// dropped and will not come back on its own.
function ParkedEventsBadge({ state }: { state: UseFailedEvents }) {
  if (state.loading || state.error) return <>Dropped events</>
  const n = state.events.length
  if (n === 0) return <>None parked</>
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="rounded-full bg-alarm/10 px-2 py-0.5 text-reported font-medium text-alarm">
        {n}
      </span>
      never routed
    </span>
  )
}

// SkillsImport — the action body for the Integrations section.
function SkillsImport() {
  const [importing, setImporting] = useState(false)
  const run = async () => {
    if (importing) return
    setImporting(true)
    try {
      const result = await apiJSON<{
        errors?: string[]
        imported: number
        skipped: number
        scanned: number
      }>('/api/prompts/from-disk', { method: 'POST' })
      if (result.errors?.length) {
        toast.error(
          `${result.errors.length} skill${result.errors.length !== 1 ? 's' : ''} failed to import: ${result.errors[0]}`,
        )
      }
      if (result.imported > 0) {
        toast.success(
          `Imported ${result.imported} skill${result.imported !== 1 ? 's' : ''} (${result.skipped} already imported)`,
        )
      } else if (!result.errors?.length) {
        toast.info(
          `No new skills found (${result.scanned} scanned, ${result.skipped} already imported)`,
        )
      }
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not import the skills.'))
    } finally {
      setImporting(false)
    }
  }
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <p className="text-body text-ink-1">Import Claude Code Skills</p>
        <p className="mt-0.5 text-reported text-ink-3">
          Import SKILL.md files from ~/.claude/skills/ as delegation prompts
        </p>
      </div>
      <button
        type="button"
        onClick={() => void run()}
        disabled={importing}
        className="shrink-0 rounded-xl border border-warm/20 px-4 py-2 text-body text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
      >
        {importing ? 'Importing…' : 'Import Skills'}
      </button>
    </div>
  )
}

// SkillPasteImport — paste or upload a single SKILL.md; it becomes an
// imported-source prompt scoped to the org/team, exactly like a
// manually created prompt. Works in both modes (in multi it's the ONLY
// import path — there is no per-tenant filesystem to scan). The acting
// team comes from useWriteTeam + TeamPicker (renders only at ≥2 teams;
// single-team callers submit team_id='' and the server resolves the
// sole team) so a multi-team user doesn't hit the backend's
// team-selection 400.
function SkillPasteImport() {
  const [content, setContent] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const fileRef = useRef<HTMLInputElement | null>(null)
  const { team, setTeam, multi, ready } = useWriteTeam()

  const readFile = async (file: File) => {
    try {
      setContent(await file.text())
    } catch (err) {
      toast.error(`Failed to read file: ${(err as Error).message}`)
    }
  }

  const submit = async () => {
    if (submitting || !content.trim() || !ready) return
    setSubmitting(true)
    try {
      const created = await apiJSON<{ name: string }>('/api/prompts/upload', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content, team_id: team }),
      })
      if (team) noteWrittenTeam(team)
      toast.success(`Imported "${created.name}" into the prompt library`)
      setContent('')
      if (fileRef.current) fileRef.current.value = ''
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not import the skill.'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="space-y-2">
      <div>
        <p className="text-body text-ink-1">Add a skill from SKILL.md</p>
        <p className="mt-0.5 text-reported text-ink-3">
          Paste (or upload) a skill&apos;s markdown — it becomes a prompt in your library, named
          from its <code>name:</code> frontmatter
        </p>
      </div>
      <textarea
        value={content}
        onChange={(e) => setContent(e.target.value)}
        placeholder={'---\nname: my-skill\ndescription: …\n---\n\nInstructions…'}
        rows={5}
        className="w-full rounded-xl border border-line-1 bg-raised px-3 py-2 font-mono text-ui text-ink-1 placeholder:text-ink-3/60 focus:outline-none focus:border-warm/40"
      />
      <TeamPicker value={team} onChange={setTeam} className="max-w-xs" />
      <div className="flex items-center justify-between gap-4">
        <input
          ref={fileRef}
          type="file"
          accept=".md,text/markdown"
          onChange={(e) => {
            const f = e.target.files?.[0]
            if (f) void readFile(f)
          }}
          className="text-reported text-ink-3 file:mr-3 file:rounded-lg file:border file:border-line-1 file:bg-transparent file:px-3 file:py-1 file:text-reported file:text-ink-2"
        />
        <button
          type="button"
          onClick={() => void submit()}
          disabled={submitting || !content.trim() || !ready || (multi && !team)}
          className="shrink-0 rounded-xl border border-warm/20 px-4 py-2 text-body text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
        >
          {submitting ? 'Importing…' : 'Import Skill'}
        </button>
      </div>
    </div>
  )
}

// DangerZone — clear the org's stored integration tokens. Credentials are
// re-entered through the access sections above; the tenant itself is untouched.
//
// It drives the two per-credential DELETEs in sequence rather than a bulk
// route. The bulk route was a second way to remove the same two credentials,
// with no discriminator and its own audit path, so the two spellings could (and
// did) drift; sequencing the real ones keeps exactly one way to unbind each.
// Both are idempotent, so an org with only one bound still ends up clean.
function DangerZone({ orgId }: { orgId: string | null }) {
  return (
    <button
      type="button"
      disabled={!orgId}
      onClick={async () => {
        if (!orgId) return
        if (!confirm('Clear all stored tokens? You will need to re-authenticate.')) return
        const github = await disconnectGitHubPAT(orgId)
        if (!github.ok) {
          toast.error(github.error)
          return
        }
        const jira = await disconnectJira(orgId)
        if (!jira.ok) {
          toast.error(jira.error)
          return
        }
        window.location.reload()
      }}
      className="rounded-xl border border-alarm/20 px-4 py-2 text-body text-alarm transition-colors hover:border-alarm/30 hover:text-alarm/80"
    >
      Clear All Tokens
    </button>
  )
}
