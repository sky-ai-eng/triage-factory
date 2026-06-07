// The Organization group of the Settings stack — rendered only for org admins.
// Composes the SAME flush primitives the setup wizard uses (UrlField,
// ChoiceCards, the `bare` field groups, ModelTierSelector) so Settings and
// /setup read as one material — no carded field groups, no card-in-card. GitHub
// is split into its wizard-granular items (URL · access method · clone
// protocol), each its own collapsible.
//
// The org-form sections all persist through the single POST /api/settings/org,
// so each saves {...baseline, ...ownSlice} against the LIVE baseline: saving one
// section never flushes another's unsaved edits, and every success folds its
// slice back into the baseline. Jira connect/disconnect, add-team, integrations
// and danger commit inline (their own endpoints), so they carry no Save footer.

import { useCallback, useEffect, useState } from 'react'
import { toast } from '../../../components/Toast/toastStore'
import { readError } from '../../../lib/api'
import {
  hostOf,
  normalizeBaseUrl,
  checkGitHubReachability,
  reachabilityMessage,
} from '../../../lib/reachability'
import { UrlField, ChoiceCards } from '../../setup/parts'
import GitHubAccessGroup from '../GitHubAccessGroup'
import GitHubAppPanel from '../GitHubAppPanel'
import PollerTimingGroup from '../PollerTimingGroup'
import JiraAccessGroup from '../JiraAccessGroup'
import ModelTierSelector from '../ModelTierSelector'
import { MODEL_CAP_OPTIONS } from '../modelTiers'
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

type AccessMode = 'app' | 'pat'

const ACCESS_CARDS: { kind: AccessMode; title: string; detail: string }[] = [
  {
    kind: 'app',
    title: 'GitHub App',
    detail: 'Polls under its own bot identity and supports multiple installations.',
  },
  {
    kind: 'pat',
    title: 'Personal access token',
    detail: 'A classic token with repo + read:org scopes — the simpler setup.',
  },
]

const CLONE_CARDS: { kind: CloneProtocol; title: string; detail: string }[] = [
  {
    kind: 'ssh',
    title: 'SSH',
    detail: 'Clone over SSH using your key (an SSH agent must be configured).',
  },
  {
    kind: 'https',
    title: 'HTTPS',
    detail: 'Clone over HTTPS using your token — no SSH setup needed.',
  },
]

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

  // Per-section drafts.
  const [ghUrl, setGhUrl] = useState('')
  const [urlError, setUrlError] = useState<string | null>(null)
  const [savingUrl, setSavingUrl] = useState(false)

  const [accessMode, setAccessMode] = useState<AccessMode>('app')
  const [ghPat, setGhPat] = useState('')
  const [savingPat, setSavingPat] = useState(false)

  const [ghClone, setGhClone] = useState<CloneProtocol>('ssh')
  const [savingClone, setSavingClone] = useState(false)

  const [ghPoll, setGhPoll] = useState('5m0s')
  const [savingGhPoll, setSavingGhPoll] = useState(false)

  const [jiraUrl, setJiraUrl] = useState('')
  const [jiraPat, setJiraPat] = useState('')

  const [jiraPoll, setJiraPoll] = useState('5m0s')
  const [savingJiraPoll, setSavingJiraPoll] = useState(false)

  const [modelCap, setModelCap] = useState('')
  const [savingCap, setSavingCap] = useState(false)

  const seed = useCallback((form: OrgConfigForm, org: OrgSettingsData) => {
    setGhUrl(form.github_url)
    setUrlError(null)
    setAccessMode(org.has_github_pat ? 'pat' : 'app')
    setGhPat('')
    setGhClone(form.github_clone_protocol)
    setGhPoll(form.github_poll_interval)
    setJiraUrl(form.jira_url)
    setJiraPat('')
    setJiraPoll(form.jira_poll_interval)
    setModelCap(form.max_llm_model_tier)
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
        seed(form, org)
        setJiraConnected(org.has_jira_pat && !!org.jira_base_url)
      })
      .catch(() =>
        setLoadError('Could not load organization settings. Check your connection and try again.'),
      )
  }, [seed])

  useEffect(() => {
    load()
  }, [load])

  // saveOrgSlice persists one section's fields merged onto the live baseline,
  // folding the result back in on success. Returns true so the shell collapses.
  const saveOrgSlice = useCallback(
    async (slice: Partial<OrgConfigForm>, label: string): Promise<boolean> => {
      const next: OrgConfigForm = { ...baseline, ...slice, github_pat: slice.github_pat ?? '' }
      const res = await saveOrgConfig(next)
      if (!res.ok) {
        toast.error(res.error)
        return false
      }
      // Keep the stored-PAT blank in the baseline (presence is tracked on data).
      setBaseline({ ...next, github_pat: '' })
      if (res.warning) toast.info(res.warning)
      toast.success(`${label} saved`)
      return true
    },
    [baseline],
  )

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
  if (!data) {
    return (
      <div className="px-1 py-3 text-[13px] text-text-tertiary">Loading organization settings…</div>
    )
  }

  const capSummary = modelCap ? `Capped at ${TIER_LABELS[modelCap] ?? modelCap}` : 'No cap'

  return (
    <div className="divide-y divide-border-subtle">
      {/* ── GitHub URL ── */}
      <SettingsSection
        title="GitHub URL"
        summary={hostOf(baseline.github_url || 'github.com')}
        dirty={ghUrl !== baseline.github_url}
        saving={savingUrl}
        onSave={async () => {
          setSavingUrl(true)
          setUrlError(null)
          try {
            const url = normalizeBaseUrl(ghUrl)
            const probe = await checkGitHubReachability(url)
            if (!probe.reachable) {
              setUrlError(reachabilityMessage(probe))
              return false
            }
            const ok = await saveOrgSlice({ github_url: url }, 'GitHub URL')
            if (ok) {
              setGhUrl(url)
              setData((d) => (d ? { ...d, github_base_url: url } : d))
            }
            return ok
          } finally {
            setSavingUrl(false)
          }
        }}
        onCancel={() => {
          setGhUrl(baseline.github_url)
          setUrlError(null)
        }}
      >
        <UrlField
          label="GitHub URL"
          value={ghUrl}
          onChange={(v) => {
            setGhUrl(v)
            if (urlError) setUrlError(null)
          }}
          placeholder="https://github.com"
          helpText="github.com for the common case; a *.ghe.com data-residency subdomain or your GitHub Enterprise Server host otherwise."
          invalid={!!urlError}
        />
        {urlError && <p className="text-[12px] text-dismiss">{urlError}</p>}
      </SettingsSection>

      {/* ── GitHub access (App vs PAT) ── */}
      <SettingsSection
        title="GitHub access"
        summary={data.has_github_pat ? 'Personal access token' : 'GitHub App'}
        // Only PAT entry persists here; the App panel registers out of band.
        dirty={accessMode === 'pat' && ghPat.trim() !== ''}
        saving={savingPat}
        onSave={
          accessMode === 'pat'
            ? async () => {
                setSavingPat(true)
                try {
                  const ok = await saveOrgSlice({ github_pat: ghPat }, 'GitHub token')
                  if (ok) {
                    setGhPat('')
                    setData((d) => (d ? { ...d, has_github_pat: true } : d))
                  }
                  return ok
                } finally {
                  setSavingPat(false)
                }
              }
            : undefined
        }
        onCancel={() => setGhPat('')}
      >
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          How Triage Factory connects to your organization&rsquo;s GitHub — the identity its bots
          poll and open pull requests under. This is the org-wide connection, not your personal
          access.
        </p>
        <ChoiceCards
          ariaLabel="GitHub access method"
          options={ACCESS_CARDS}
          selected={accessMode}
          onChoose={setAccessMode}
        />
        {accessMode === 'pat' ? (
          <GitHubAccessGroup
            value={{ github_url: ghUrl, github_pat: ghPat, github_clone_protocol: ghClone }}
            onChange={(p) => {
              if (p.github_pat !== undefined) setGhPat(p.github_pat)
            }}
            hasToken={data.has_github_pat}
            isLocal={isLocal}
            orgId={orgId}
            showAppPanel={false}
            showBaseUrl={false}
            showHeading={false}
            showCloneProtocol={false}
            bare
          />
        ) : (
          <GitHubAppPanel orgId={orgId} showHeading={false} bare />
        )}
      </SettingsSection>

      {/* ── Clone protocol (local only; multi hardwires HTTPS) ── */}
      {isLocal && (
        <SettingsSection
          title="Clone protocol"
          summary={`Clone via ${baseline.github_clone_protocol.toUpperCase()}`}
          dirty={ghClone !== baseline.github_clone_protocol}
          saving={savingClone}
          onSave={async () => {
            setSavingClone(true)
            try {
              return await saveOrgSlice({ github_clone_protocol: ghClone }, 'Clone protocol')
            } finally {
              setSavingClone(false)
            }
          }}
          onCancel={() => setGhClone(baseline.github_clone_protocol)}
        >
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            Only affects how Triage Factory clones repos to this machine — not the API connection.
          </p>
          <ChoiceCards
            ariaLabel="Clone protocol"
            options={CLONE_CARDS}
            selected={ghClone}
            onChoose={setGhClone}
          />
        </SettingsSection>
      )}

      {/* ── GitHub polling ── */}
      <SettingsSection
        title="GitHub polling"
        summary={`Every ${intervalLabel(baseline.github_poll_interval)}`}
        dirty={ghPoll !== baseline.github_poll_interval}
        saving={savingGhPoll}
        onSave={async () => {
          setSavingGhPoll(true)
          try {
            return await saveOrgSlice({ github_poll_interval: ghPoll }, 'GitHub polling')
          } finally {
            setSavingGhPoll(false)
          }
        }}
        onCancel={() => setGhPoll(baseline.github_poll_interval)}
      >
        <PollerTimingGroup
          value={{ github_poll_interval: ghPoll, jira_poll_interval: jiraPoll }}
          onChange={(p) => {
            if (p.github_poll_interval !== undefined) setGhPoll(p.github_poll_interval)
          }}
          showJira={false}
          showHeading={false}
          bare
        />
      </SettingsSection>

      {/* ── Jira connection (connect/disconnect inline) ── */}
      <SettingsSection
        title="Jira connection"
        summary={jiraConnected ? `Connected · ${hostOf(baseline.jira_url)}` : 'Not connected'}
      >
        <JiraAccessGroup
          value={{ jira_url: jiraUrl, jira_pat: jiraPat }}
          onChange={(p) => {
            if (p.jira_url !== undefined) setJiraUrl(p.jira_url)
            if (p.jira_pat !== undefined) setJiraPat(p.jira_pat)
          }}
          connected={jiraConnected}
          onConnected={(url) => {
            setJiraConnected(true)
            setBaseline((b) => ({ ...b, jira_url: url }))
            setJiraUrl(url)
            setJiraPat('')
            setData((d) => (d ? { ...d, jira_base_url: url, has_jira_pat: true } : d))
          }}
          onDisconnected={() => {
            setJiraConnected(false)
            setBaseline((b) => ({ ...b, jira_url: '' }))
            setJiraUrl('')
            setJiraPat('')
            setData((d) => (d ? { ...d, jira_base_url: '', has_jira_pat: false } : d))
          }}
          bare
        />
      </SettingsSection>

      {/* ── Jira polling (only once connected) ── */}
      {jiraConnected && (
        <SettingsSection
          title="Jira polling"
          summary={`Every ${intervalLabel(baseline.jira_poll_interval)}`}
          dirty={jiraPoll !== baseline.jira_poll_interval}
          saving={savingJiraPoll}
          onSave={async () => {
            setSavingJiraPoll(true)
            try {
              return await saveOrgSlice({ jira_poll_interval: jiraPoll }, 'Jira polling')
            } finally {
              setSavingJiraPoll(false)
            }
          }}
          onCancel={() => setJiraPoll(baseline.jira_poll_interval)}
        >
          <PollerTimingGroup
            value={{ github_poll_interval: ghPoll, jira_poll_interval: jiraPoll }}
            onChange={(p) => {
              if (p.jira_poll_interval !== undefined) setJiraPoll(p.jira_poll_interval)
            }}
            showGitHub={false}
            showHeading={false}
            bare
          />
        </SettingsSection>
      )}

      {/* ── Model cap ── */}
      <SettingsSection
        title="Model cap"
        summary={capSummary}
        dirty={modelCap !== baseline.max_llm_model_tier}
        saving={savingCap}
        onSave={async () => {
          setSavingCap(true)
          try {
            const ok = await saveOrgSlice({ max_llm_model_tier: modelCap }, 'Model cap')
            if (ok) setData((d) => (d ? { ...d, max_llm_model_tier: modelCap } : d))
            return ok
          } finally {
            setSavingCap(false)
          }
        }}
        onCancel={() => setModelCap(baseline.max_llm_model_tier)}
      >
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          A hard ceiling for the whole workspace. A team default above the cap is clamped down to it
          — the team is told, but the cap wins.
        </p>
        <ModelTierSelector
          value={modelCap}
          onChange={setModelCap}
          options={MODEL_CAP_OPTIONS}
          ariaLabel="Maximum model tier (workspace cap)"
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

// SkillsImport — the action body for the Integrations section.
function SkillsImport() {
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
