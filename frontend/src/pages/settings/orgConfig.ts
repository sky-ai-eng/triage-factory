// Shared types + persistence helpers for the org-level configuration
// surface (GitHub + Jira access, poller timing, model cap). The same
// helpers back the Settings workspace tab and the create-time setup wizard
// (both modes) — every surface round-trips the identical org_settings
// shape via the existing endpoints, so there is no parallel persistence path
// to drift.

import { readError } from '../../lib/api'

export type CloneProtocol = 'ssh' | 'https'

// BedrockAuthMethod mirrors the connect endpoint's auth_method wire values:
// an assumed IAM role (short-lived session credentials, no static secret), a
// Bedrock API key (bearer), or the IAM access-key pair (+ optional session
// token) served by the SigV4 re-signing proxy.
export type BedrockAuthMethod = 'role' | 'bearer' | 'access_keys'

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
  // ── Amazon Bedrock (alternative Claude provider) ── Captured ONLY via the
  // validated connectBedrock endpoint, never sent by saveOrgConfig — same rule
  // as anthropic_api_key. The secret fields (bearer token / key pair / session
  // token) stay blank on load; presence rides has_bedrock_credentials +
  // bedrock_auth_method. The non-secret config (method, region, model,
  // endpoint) round-trips so the form shows current values.
  bedrock_auth_method: BedrockAuthMethod
  bedrock_bearer_token: string
  aws_access_key_id: string
  aws_secret_access_key: string
  aws_session_token: string
  // IAM-role method (no static secret): the role ARN Triage Factory assumes to
  // mint per-run session credentials (editable), plus the server-generated
  // external ID the role's trust policy must pin (read-only — surfaced by the
  // GET echo / role-setup response, never typed by the user).
  bedrock_role_arn: string
  bedrock_external_id: string
  bedrock_region: string
  bedrock_model_id: string
  bedrock_base_url: string
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
  // Bedrock non-secret config — echoed by the GET so the form renders the
  // stored method / region / model / endpoint. The credential itself never
  // leaves the vault.
  bedrock_auth_method?: string
  // IAM-role method: the stored role ARN and the org's generated external ID
  // (both omitempty — absent unless the role method is configured).
  bedrock_role_arn?: string
  bedrock_external_id?: string
  bedrock_region?: string
  bedrock_model_id?: string
  bedrock_base_url?: string
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
  bedrock_auth_method: 'bearer',
  bedrock_bearer_token: '',
  aws_access_key_id: '',
  aws_secret_access_key: '',
  aws_session_token: '',
  bedrock_role_arn: '',
  bedrock_external_id: '',
  // us-east-1 is Bedrock's primary region for Anthropic models and the
  // resolver's own fallback — pre-filled so the common case is zero-typing.
  bedrock_region: 'us-east-1',
  bedrock_model_id: '',
  bedrock_base_url: '',
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
    // Bedrock secrets stay blank like the key above; the non-secret config
    // seeds from the GET echo so the form shows what's stored.
    bedrock_auth_method:
      org.bedrock_auth_method === 'access_keys'
        ? 'access_keys'
        : org.bedrock_auth_method === 'role'
          ? 'role'
          : 'bearer',
    bedrock_bearer_token: '',
    aws_access_key_id: '',
    aws_secret_access_key: '',
    aws_session_token: '',
    // Role method: the ARN + external ID are non-secret config, so they round-
    // trip from the GET echo (external ID is server-generated, never typed).
    bedrock_role_arn: org.bedrock_role_arn || '',
    bedrock_external_id: org.bedrock_external_id || '',
    bedrock_region: org.bedrock_region || 'us-east-1',
    bedrock_model_id: org.bedrock_model_id || '',
    bedrock_base_url: org.bedrock_base_url || '',
  }
}

export async function fetchOrgSettings(): Promise<OrgSettingsData | null> {
  const res = await fetch('/api/settings/org')
  return res.ok ? ((await res.json()) as OrgSettingsData) : null
}

// dailyCapError is the frontend input-layer validation for the daily spend cap.
// "No cap" is expressed by clearing the field (blank), so blank is always valid.
// A typed value must parse to a positive, finite number — 0 and negatives are
// rejected: a $0/day cap is meaningless (it reads as "no cap", which the blank
// field already expresses) and a negative is nonsensical and would 400 at the
// API anyway. Returns the user-facing message, or null when the input is
// acceptable. Mirrors the backend's `>= 0` guard so the Save button blocks
// before the round-trip instead of bouncing off a 400.
export function dailyCapError(raw: string): string | null {
  const trimmed = raw.trim()
  if (trimmed === '') return null
  const n = Number(trimmed)
  if (!Number.isFinite(n)) return 'Enter a number, or leave blank for no cap.'
  if (n <= 0) return 'Enter an amount above $0, or leave blank for no cap.'
  return null
}

// dailyCapToWire converts the daily-cap text input to the wire value for
// max_daily_cost_usd. Validation happens upstream (dailyCapError gates the Save
// button), so by the time this runs the input is either blank or a positive
// number:
//   - blank        → 0 (an explicit "no cap" clear)
//   - valid number → passed straight through, fractions and all. A stray
//     non-positive value is still left intact so the backend's `>= 0` check is
//     the backstop for a direct API call (the UI never reaches here with one).
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
