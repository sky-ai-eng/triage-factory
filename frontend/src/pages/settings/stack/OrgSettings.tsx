// The Organization group of the Settings stack — rendered only for org
// admins. Composes the existing org-scope field groups (GitHub access, poller
// timing, model cap, Jira connection) plus the action surfaces (add-team,
// integrations, danger zone) into independently-collapsible sections.
//
// The two FORM sections (GitHub access, Polling & model) both persist through
// the single POST /api/settings/org, so each saves {...baseline, ...ownDraft}:
// saving one section must never flush the OTHER section's unsaved edits. The
// shared `baseline` is the last-saved org form; every successful save folds its
// slice back into it so the next change-detection compares against current
// state. The Jira / add-team / integrations / danger sections commit inline
// (their own endpoints), so they carry no draft and no Save footer.

import { useCallback, useEffect, useState } from 'react'
import { toast } from '../../../components/Toast/toastStore'
import { readError } from '../../../lib/api'
import { hostOf } from '../../../lib/reachability'
import GitHubAccessGroup from '../GitHubAccessGroup'
import PollerTimingGroup from '../PollerTimingGroup'
import ModelGroup from '../ModelGroup'
import JiraAccessGroup from '../JiraAccessGroup'
import TeamManagementSection from '../../../components/TeamManagementSection'
import {
  emptyOrgConfig,
  fetchOrgSettings,
  orgConfigFromSettings,
  saveOrgConfig,
  type CloneProtocol,
  type OrgConfigForm,
  type OrgSettingsData,
} from '../orgConfig'
import SettingsSection from './SettingsSection'

const TIER_LABELS: Record<string, string> = { haiku: 'Haiku', sonnet: 'Sonnet', opus: 'Opus' }
const intervalLabel = (d: string): string => d.replace(/m0s$/, 'm')

export default function OrgSettings({
  orgId,
  isLocal,
}: {
  orgId: string | null
  isLocal: boolean
}) {
  const [data, setData] = useState<OrgSettingsData | null>(null)
  const [baseline, setBaseline] = useState<OrgConfigForm>(emptyOrgConfig)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [jiraConnected, setJiraConnected] = useState(false)

  // ── GitHub-access section draft (URL / PAT / clone protocol) ──
  const [ghUrl, setGhUrl] = useState('')
  const [ghPat, setGhPat] = useState('')
  const [ghClone, setGhClone] = useState<CloneProtocol>('ssh')
  const [savingGitHub, setSavingGitHub] = useState(false)

  // ── Polling & model section draft ──
  const [ghPoll, setGhPoll] = useState('5m0s')
  const [jiraPoll, setJiraPoll] = useState('5m0s')
  const [modelCap, setModelCap] = useState('')
  const [savingPoll, setSavingPoll] = useState(false)

  // ── Jira connection draft (the connect form's url/pat; committed inline) ──
  const [jiraUrl, setJiraUrl] = useState('')
  const [jiraPat, setJiraPat] = useState('')

  // seedFromBaseline resets every section's draft to a freshly-loaded org form
  // (used on load). Kept stable so the load effect's dep doesn't churn. Each
  // section's Cancel resets only its OWN slice inline (below) — the sections
  // are independent, so cancelling GitHub must not revert an unsaved Polling
  // edit.
  const seedFromBaseline = useCallback((form: OrgConfigForm) => {
    setGhUrl(form.github_url)
    setGhPat('')
    setGhClone(form.github_clone_protocol)
    setGhPoll(form.github_poll_interval)
    setJiraPoll(form.jira_poll_interval)
    setModelCap(form.max_llm_model_tier)
    setJiraUrl(form.jira_url)
    setJiraPat('')
  }, [])

  const load = useCallback(() => {
    setLoadError(null)
    fetchOrgSettings()
      .then((org) => {
        if (!org) {
          setLoadError('Could not load organization settings. Check your connection and try again.')
          return
        }
        const form = orgConfigFromSettings(org)
        setData(org)
        setBaseline(form)
        seedFromBaseline(form)
        setJiraConnected(org.has_jira_pat && !!org.jira_base_url)
      })
      .catch(() =>
        setLoadError('Could not load organization settings. Check your connection and try again.'),
      )
  }, [seedFromBaseline])

  useEffect(() => {
    load()
  }, [load])

  if (loadError) {
    return (
      <div className="rounded-2xl border border-border-glass bg-surface-overlay/40 px-5 py-4 text-[13px] text-text-secondary">
        {loadError}{' '}
        <button type="button" onClick={load} className="text-accent underline">
          Retry
        </button>
      </div>
    )
  }
  if (!data) {
    return (
      <div className="rounded-2xl border border-border-glass bg-surface-overlay/40 px-5 py-4 text-[13px] text-text-tertiary">
        Loading organization settings…
      </div>
    )
  }

  // ── GitHub access save ──
  const githubDirty =
    ghPat.trim() !== '' ||
    ghUrl !== baseline.github_url ||
    ghClone !== baseline.github_clone_protocol

  const saveGitHub = async (): Promise<boolean> => {
    setSavingGitHub(true)
    try {
      const next: OrgConfigForm = {
        ...baseline,
        github_url: ghUrl,
        github_pat: ghPat,
        github_clone_protocol: ghClone,
      }
      const res = await saveOrgConfig(next)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      // Fold the saved slice into the baseline (keep PAT blank — it's stored).
      const saved: OrgConfigForm = { ...next, github_pat: '' }
      setBaseline(saved)
      setData((d) =>
        d
          ? {
              ...d,
              github_base_url: ghUrl,
              has_github_pat: d.has_github_pat || ghPat.trim() !== '',
            }
          : d,
      )
      setGhPat('')
      if (res.warning) toast.info(res.warning)
      toast.success('GitHub settings saved')
      return true
    } finally {
      setSavingGitHub(false)
    }
  }

  // ── Polling & model save ──
  const pollDirty =
    ghPoll !== baseline.github_poll_interval ||
    jiraPoll !== baseline.jira_poll_interval ||
    modelCap !== baseline.max_llm_model_tier

  const savePoll = async (): Promise<boolean> => {
    setSavingPoll(true)
    try {
      const next: OrgConfigForm = {
        ...baseline,
        github_poll_interval: ghPoll,
        jira_poll_interval: jiraPoll,
        max_llm_model_tier: modelCap,
      }
      const res = await saveOrgConfig(next)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      setBaseline(next)
      setData((d) => (d ? { ...d, max_llm_model_tier: modelCap } : d))
      if (res.warning) toast.info(res.warning)
      toast.success('Polling & model settings saved')
      return true
    } finally {
      setSavingPoll(false)
    }
  }

  // ── Jira connect/disconnect (inline; updates baseline + connected flag) ──
  const onJiraConnected = (url: string) => {
    setJiraConnected(true)
    setBaseline((b) => ({ ...b, jira_url: url }))
    setJiraUrl(url)
    setJiraPat('')
    setData((d) => (d ? { ...d, jira_base_url: url, has_jira_pat: true } : d))
  }
  const onJiraDisconnected = () => {
    setJiraConnected(false)
    setBaseline((b) => ({ ...b, jira_url: '' }))
    setJiraUrl('')
    setJiraPat('')
    setData((d) => (d ? { ...d, jira_base_url: '', has_jira_pat: false } : d))
  }

  const capSummary = modelCap ? `Capped at ${TIER_LABELS[modelCap] ?? modelCap}` : 'No model cap'

  return (
    <div className="space-y-2.5">
      <SettingsSection
        title="GitHub access"
        summary={`${hostOf(baseline.github_url || 'github.com')}${data.has_github_pat ? ' · token set' : ''}`}
        dirty={githubDirty}
        saving={savingGitHub}
        onSave={saveGitHub}
        onCancel={() => {
          setGhUrl(baseline.github_url)
          setGhPat('')
          setGhClone(baseline.github_clone_protocol)
        }}
      >
        <GitHubAccessGroup
          value={{ github_url: ghUrl, github_pat: ghPat, github_clone_protocol: ghClone }}
          onChange={(patch) => {
            if (patch.github_url !== undefined) setGhUrl(patch.github_url)
            if (patch.github_pat !== undefined) setGhPat(patch.github_pat)
            if (patch.github_clone_protocol !== undefined) setGhClone(patch.github_clone_protocol)
          }}
          hasToken={data.has_github_pat}
          isLocal={isLocal}
          orgId={orgId}
        />
      </SettingsSection>

      <SettingsSection
        title="Polling & model"
        summary={`GitHub every ${intervalLabel(baseline.github_poll_interval)} · ${capSummary}`}
        dirty={pollDirty}
        saving={savingPoll}
        onSave={savePoll}
        onCancel={() => {
          setGhPoll(baseline.github_poll_interval)
          setJiraPoll(baseline.jira_poll_interval)
          setModelCap(baseline.max_llm_model_tier)
        }}
      >
        <PollerTimingGroup
          value={{ github_poll_interval: ghPoll, jira_poll_interval: jiraPoll }}
          onChange={(patch) => {
            if (patch.github_poll_interval !== undefined) setGhPoll(patch.github_poll_interval)
            if (patch.jira_poll_interval !== undefined) setJiraPoll(patch.jira_poll_interval)
          }}
          showJira={jiraConnected}
        />
        <ModelGroup
          value={{ max_llm_model_tier: modelCap }}
          onChange={(patch) => {
            if (patch.max_llm_model_tier !== undefined) setModelCap(patch.max_llm_model_tier)
          }}
        />
      </SettingsSection>

      <SettingsSection
        title="Jira connection"
        summary={jiraConnected ? `Connected · ${hostOf(baseline.jira_url)}` : 'Not connected'}
      >
        <JiraAccessGroup
          value={{ jira_url: jiraUrl, jira_pat: jiraPat }}
          onChange={(patch) => {
            if (patch.jira_url !== undefined) setJiraUrl(patch.jira_url)
            if (patch.jira_pat !== undefined) setJiraPat(patch.jira_pat)
          }}
          connected={jiraConnected}
          onConnected={onJiraConnected}
          onDisconnected={onJiraDisconnected}
        />
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

// SkillsImport — the action body for the Integrations section (unchanged
// behaviour, lifted out of the old flat layout).
function SkillsImport() {
  return (
    <div className="flex items-center justify-between gap-4 rounded-xl border border-border-subtle bg-white/40 px-4 py-3">
      <div>
        <p className="text-[13px] text-text-primary">Import Claude Code Skills</p>
        <p className="mt-0.5 text-[11px] text-text-tertiary">
          Import SKILL.md files from ~/.claude/skills/ as delegation prompts
        </p>
      </div>
      <button
        type="button"
        onClick={async () => {
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
          }
        }}
        className="shrink-0 rounded-xl border border-accent/20 px-4 py-2 text-[13px] text-accent transition-colors hover:border-accent/30 hover:text-accent/80"
      >
        Import Skills
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
