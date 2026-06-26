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
  // Org-wide daily LLM spend cap (TFAC-477), held as the raw text input. Empty =
  // "no cap"; a numeric string is the per-day USD ceiling. Stored as a string
  // (not a number) so the input can be cleared to "" cleanly and partial typing
  // works; saveOrgConfig converts it to the wire number.
  max_daily_cost_usd: string
  // The org's Anthropic API key (BYOK). Blank on load — it's a secret that never
  // leaves the vault, like jira_pat. It is captured ONLY via the validated
  // connectAnthropic endpoint and is deliberately NOT sent by saveOrgConfig, so
  // the bulk settings POST can't be an unvalidated write path.
  anthropic_api_key: string
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
  // True when a Jira service credential is stored for the org's auth-method
  // marker (DC PAT or Cloud email + API token) — not the presence of a PAT
  // specifically, so a Cloud org reports true despite having no PAT.
  has_jira_credential: boolean
  max_llm_model_tier?: string
  // Org-wide daily spend cap in USD (TFAC-477); 0 = no cap. Always present.
  max_daily_cost_usd: number
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
  max_daily_cost_usd: '',
  anthropic_api_key: '',
})

// orgConfigFromSettings seeds the editable form from a GET response.
// Token fields stay blank (presence is carried separately via
// has_github_pat / has_jira_credential) so a save without a re-typed token
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
    // 0 (no cap) renders as an empty input ("No cap"); any positive cap seeds
    // the numeric string the input edits.
    max_daily_cost_usd: org.max_daily_cost_usd ? String(org.max_daily_cost_usd) : '',
    // Secret — never returned by the GET (presence rides has_anthropic_api_key),
    // so it stays blank and a save without a re-typed key leaves the vault key
    // untouched.
    anthropic_api_key: '',
  }
}

export async function fetchOrgSettings(): Promise<OrgSettingsData | null> {
  const res = await fetch('/api/settings/org')
  return res.ok ? ((await res.json()) as OrgSettingsData) : null
}

// dailyCapToWire converts the daily-cap text input to the wire value for
// max_daily_cost_usd. The field is an <input type="number">, so in practice it
// only yields an empty string or a valid numeric string:
//   - blank        → 0 (an explicit "no cap" clear)
//   - valid number → passed straight through, fractions and all. A negative is
//     left intact so the backend's `>= 0` check rejects it with a 400 the user
//     sees (the input's min="0" is just the client-side first line of defense).
//   - unparseable  → undefined, which JSON.stringify drops, so the field is
//     omitted and a previously-stored cap is left UNTOUCHED rather than silently
//     cleared to 0. Effectively unreachable from a number input; the guard just
//     makes the "never stomp the cap on garbage" property explicit.
function dailyCapToWire(raw: string): number | undefined {
  const trimmed = raw.trim()
  if (trimmed === '') return 0
  const n = Number(trimmed)
  return Number.isFinite(n) ? n : undefined
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
      max_daily_cost_usd: dailyCapToWire(form.max_daily_cost_usd),
    }),
  })
  if (!res.ok) {
    return { ok: false, error: await readError(res, 'Failed to save settings') }
  }
  const body = (await res.json().catch(() => null)) as { warning?: string } | null
  return { ok: true, warning: body?.warning }
}
