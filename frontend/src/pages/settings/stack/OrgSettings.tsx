// The Organization group of the Settings stack — rendered only for org admins.
// Each section's BODY is the actual /setup step component (GitHubUrlStep,
// GitHubModeStep, GitHubAppStep, GitHubPatStep, GitHubCloneStep, OrgModelStep,
// …) fed a WizardState draft, so Settings and /setup are literally the same
// components — same visuals, same copy, same granularity (GitHub is split into
// its individual items: URL · access method · account type · App/token · clone).
//
// What Settings adds over /setup is the per-section Save model (the approved
// design): expand a section, edit its draft, Save (or Cancel/discard). The
// org-form sections all persist through the single POST /api/settings/org, so
// each saves {...baseline.org, ...ownFields} against the LIVE baseline — saving
// one never flushes another's unsaved edits. Selector/panel sections (the
// access-method picker, the App register panel) and connect/disconnect commit
// no org-form slice, so they carry no Save footer.

import { useCallback, useEffect, useState } from 'react'
import { toast } from '../../../components/Toast/toastStore'
import { readError } from '../../../lib/api'
import {
  hostOf,
  normalizeBaseUrl,
  checkGitHubReachability,
  reachabilityMessage,
} from '../../../lib/reachability'
import {
  GitHubUrlStep,
  GitHubModeStep,
  GitHubAccountTypeStep,
  GitHubAppStep,
  GitHubPatStep,
  GitHubCloneStep,
} from '../../setup/GitHubStep'
import { OrgModelStep } from '../../setup/ModelStep'
import { initialWizardState, loadOrg } from '../../setup/steps'
import type { StepContext, WizardState } from '../../setup/types'
import PollerTimingGroup from '../PollerTimingGroup'
import JiraAccessGroup from '../JiraAccessGroup'
import TeamManagementSection from '../../../components/TeamManagementSection'
import { saveOrgConfig, type OrgConfigForm } from '../orgConfig'
import SettingsSection from './SettingsSection'

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
    loadOrg({ orgId, teamId: 'default', isLocal })
      .then((slice) => {
        if (cancelled) return
        const seeded: WizardState = { ...initialWizardState(), ...slice, isLocal }
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

  const isPat = draft.githubAccessTab === 'pat'
  const isApp = draft.githubAccessTab === 'app'
  const capSummary = baseline.org.max_llm_model_tier
    ? `Capped at ${TIER_LABELS[baseline.org.max_llm_model_tier] ?? baseline.org.max_llm_model_tier}`
    : 'No cap'

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

      {/* ── GitHub access method (selector; gates the App/PAT items below) ── */}
      <SettingsSection
        title="GitHub access"
        summary={isPat ? 'Personal access token' : 'GitHub App'}
      >
        <GitHubModeStep {...ctx} />
      </SettingsSection>

      {/* ── App account type (App only; feeds the register panel) ── */}
      {isApp && (
        <SettingsSection
          title="App account type"
          summary={draft.githubAppOwnerType === 'org' ? 'Organization account' : 'Personal account'}
        >
          <GitHubAccountTypeStep {...ctx} />
        </SettingsSection>
      )}

      {/* ── GitHub App register (App only; external register, self-managed) ── */}
      {isApp && (
        <SettingsSection
          title="GitHub App"
          summary={draft.githubReady ? 'Connected' : 'Not registered'}
        >
          <GitHubAppStep {...ctx} />
        </SettingsSection>
      )}

      {/* ── Personal access token (PAT only) ── */}
      {isPat && (
        <SettingsSection
          title="Personal access token"
          summary={baseline.hasGitHubPat ? 'Token set' : 'No token'}
          dirty={draft.org.github_pat.trim() !== ''}
          saving={isSaving('gh-pat')}
          onSave={async () => {
            const ok = await commitOrgSlice(
              'gh-pat',
              { github_pat: draft.org.github_pat },
              'GitHub token',
            )
            if (ok) {
              setDraft((d) => ({ ...d, hasGitHubPat: true, org: { ...d.org, github_pat: '' } }))
              setBaseline((b) => ({ ...b, hasGitHubPat: true }))
            }
            return ok
          }}
          onCancel={() => revertOrg(['github_pat'])}
        >
          <GitHubPatStep {...ctx} />
        </SettingsSection>
      )}

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

      {/* ── Jira connection (connect/disconnect inline) ── */}
      <SettingsSection
        title="Jira connection"
        summary={
          draft.jiraConnected ? `Connected · ${hostOf(baseline.org.jira_url)}` : 'Not connected'
        }
      >
        <JiraAccessGroup
          value={{ jira_url: draft.org.jira_url, jira_pat: draft.org.jira_pat }}
          onChange={(p) => patch({ org: { ...draft.org, ...p } })}
          connected={draft.jiraConnected}
          onConnected={(url) => {
            setDraft((d) => ({
              ...d,
              jiraConnected: true,
              org: { ...d.org, jira_url: url, jira_pat: '' },
            }))
            setBaseline((b) => ({ ...b, jiraConnected: true, org: { ...b.org, jira_url: url } }))
          }}
          onDisconnected={() => {
            setDraft((d) => ({
              ...d,
              jiraConnected: false,
              org: { ...d.org, jira_url: '', jira_pat: '' },
            }))
            setBaseline((b) => ({ ...b, jiraConnected: false, org: { ...b.org, jira_url: '' } }))
          }}
          bare
        />
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

      {/* Add-team is hosted-only (POST /api/teams 404s in local). */}
      {!isLocal && (
        <SettingsSection title="Teams" summary="Add or review teams">
          <TeamManagementSection />
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
