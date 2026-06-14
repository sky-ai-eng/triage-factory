// Shared types + persistence helpers for the org-level configuration
// surface (GitHub + Jira access, poller timing, model cap). The same
// helpers back the Settings workspace tab and the create-time setup wizard
// (both modes) — every surface round-trips the identical org_settings
// shape via the existing endpoints, so there is no parallel persistence path
// to drift.

import { readError } from '../../lib/api'

export type CloneProtocol = 'ssh' | 'https'

// OrgConfigForm is the editable org-level field set the shared components
// drive. Field names match the GET/POST /api/settings/org wire keys so a
// container can spread component patches straight into its form state.
export interface OrgConfigForm {
  github_url: string
  github_pat: string
  github_clone_protocol: CloneProtocol
  github_poll_interval: string
  jira_url: string
  jira_pat: string
  // Jira Cloud service credential (Basic auth): the Atlassian account email +
  // API token. Data Center uses jira_pat (Bearer) instead; the onboarding
  // deployment picker decides which pair the connect sends. Both stay blank on
  // load (secrets never leave the vault) like jira_pat.
  jira_email: string
  jira_api_token: string
  jira_poll_interval: string
  max_llm_model_tier: string
}

// OrgSettingsData mirrors the GET /api/settings/org response. Token fields
// are presence booleans only — the secrets themselves never leave the
// keychain/vault, so the form shows "leave blank to keep current".
export interface OrgSettingsData {
  github_base_url: string
  github_poll_interval: string
  github_clone_protocol: CloneProtocol
  has_github_pat: boolean
  jira_base_url: string
  jira_poll_interval: string
  has_jira_pat: boolean
  max_llm_model_tier?: string
  has_anthropic_api_key: boolean
  has_bedrock_credentials: boolean
  member_count: number
}

export const emptyOrgConfig = (): OrgConfigForm => ({
  github_url: '',
  github_pat: '',
  github_clone_protocol: 'ssh',
  github_poll_interval: '5m0s',
  jira_url: '',
  jira_pat: '',
  jira_email: '',
  jira_api_token: '',
  jira_poll_interval: '5m0s',
  max_llm_model_tier: '',
})

// orgConfigFromSettings seeds the editable form from a GET response.
// Token fields stay blank (presence is carried separately via
// has_github_pat / has_jira_pat) so a save without a re-typed token
// leaves the stored secret untouched.
export function orgConfigFromSettings(org: OrgSettingsData): OrgConfigForm {
  return {
    github_url: org.github_base_url || '',
    github_pat: '',
    github_clone_protocol: org.github_clone_protocol === 'https' ? 'https' : 'ssh',
    github_poll_interval: org.github_poll_interval,
    jira_url: org.jira_base_url || '',
    jira_pat: '',
    jira_email: '',
    jira_api_token: '',
    jira_poll_interval: org.jira_poll_interval,
    max_llm_model_tier: org.max_llm_model_tier || '',
  }
}

export async function fetchOrgSettings(): Promise<OrgSettingsData | null> {
  const res = await fetch('/api/settings/org')
  return res.ok ? ((await res.json()) as OrgSettingsData) : null
}

// saveOrgConfig persists the org-level field group via POST
// /api/settings/org. Token fields are sent only when re-typed (undefined
// = don't touch, matching the backend's pointer-nil semantics). Returns a
// discriminated result; `warning` carries the backend's model-cap clamp
// notice on an otherwise-successful save.
export async function saveOrgConfig(
  form: OrgConfigForm,
): Promise<{ ok: true; warning?: string } | { ok: false; error: string }> {
  const res = await fetch('/api/settings/org', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      github_base_url: form.github_url,
      github_pat: form.github_pat || undefined,
      github_poll_interval: form.github_poll_interval,
      github_clone_protocol: form.github_clone_protocol,
      jira_base_url: form.jira_url,
      jira_pat: form.jira_pat || undefined,
      jira_poll_interval: form.jira_poll_interval,
      max_llm_model_tier: form.max_llm_model_tier,
    }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save settings') }
  }
  const body = (await res.json().catch(() => null)) as { warning?: string } | null
  return { ok: true, warning: body?.warning }
}
