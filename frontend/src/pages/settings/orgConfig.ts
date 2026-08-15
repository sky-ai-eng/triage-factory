// Shared types + persistence helpers for the org-level configuration
// surface (GitHub + Jira access, poller timing, model cap). The same
// helpers back the Settings workspace tab and the create-time setup wizard
// (both modes) — every surface round-trips the identical org_settings
// shape via the existing endpoints, so there is no parallel persistence path
// to drift.

import { apiJSON, httpErrorMessage } from '../../lib/apiClient'

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
  // Org-wide concurrent-run limit, held as the raw text input. Empty (or 0) =
  // "unlimited"; a positive integer string is the ceiling on runs executing at
  // once across the fleet. Held as a string for the same reasons as
  // max_daily_cost_usd; saveOrgConfig converts it to the wire number.
  max_concurrent_runs: string
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
  // The @login the stored org PAT authenticates as — the credential's own
  // identity, not the viewer's. Absent when no PAT is bound, when the bind
  // predates the login being recorded, or when the live token comes from the
  // environment (the recorded login wouldn't be the one in use). Settings shows
  // it beside the "Replace token" control so a rotation names the account it's
  // swapping out.
  github_pat_login?: string
  // True when the live GitHub token is supplied by TRIAGE_FACTORY_GITHUB_BOT_PAT
  // (local mode only). The overlay is read-wins, so a replacement written from
  // here would be stored and then ignored — the surface reports the credential
  // instead of offering to change it.
  github_pat_env_provided?: boolean
  jira_base_url: string
  jira_poll_interval: string
  // True when a Jira service credential is stored for the org's auth-method
  // marker (DC PAT or Cloud email + API token) — not the presence of a PAT
  // specifically, so a Cloud org reports true despite having no PAT.
  has_jira_credential: boolean
  // The Jira half of github_pat_env_provided — true when the env supplies the
  // Jira host and/or service token, either of which makes an in-place rebind
  // partly or wholly unobservable.
  jira_credential_env_provided?: boolean
  max_llm_model_tier?: string
  // Org-wide daily spend cap in USD (TFAC-477); 0 = no cap. Always present.
  max_daily_cost_usd: number
  // Org-wide concurrent-run limit; 0 = unlimited. Always present.
  max_concurrent_runs: number
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
  max_concurrent_runs: '',
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
    // 0 (unlimited) renders as an empty input; any positive limit seeds the
    // numeric string the input edits.
    max_concurrent_runs: org.max_concurrent_runs ? String(org.max_concurrent_runs) : '',
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
  // Null on any failure: every caller renders a "couldn't load settings" state
  // rather than a message, so there is nothing to carry out of the error.
  return apiJSON<OrgSettingsData>('/api/settings/org').catch(() => null)
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

// MAX_CONCURRENT_RUNS_CEILING mirrors domain.MaxConcurrentRunsCeiling — a sanity
// bound far beyond any real fleet that keeps a validated value inside the
// backend's int4 column. Kept in sync with the Go constant by hand (there's no
// shared source), like the `>= 0` guard below mirrors the handler.
export const MAX_CONCURRENT_RUNS_CEILING = 1_000_000

// concurrentRunsError is the frontend input-layer validation for the
// concurrent-run limit. "Unlimited" is expressed by clearing the field (blank)
// OR by an explicit 0 — the ticket's "blank or 0 = unlimited" — so both are
// always valid. A typed value must parse to a non-negative, finite integer at
// or below the ceiling: negatives are rejected (the claim reads <= 0 as
// unlimited, so a negative would silently mean "no limit" and 400 at the API),
// fractions are rejected since the column is an integer, and an oversized value
// is rejected before it can 400 at the DB. Returns the user-facing message, or
// null when the input is acceptable. Mirrors the backend's guard so Save blocks
// before the round-trip instead of bouncing off a 400.
export function concurrentRunsError(raw: string): string | null {
  const trimmed = raw.trim()
  if (trimmed === '') return null
  const n = Number(trimmed)
  if (!Number.isFinite(n)) return 'Enter a whole number, or leave blank for unlimited.'
  if (n < 0) return 'Enter 0 or more, or leave blank for unlimited.'
  if (!Number.isInteger(n)) return 'Enter a whole number of runs.'
  if (n > MAX_CONCURRENT_RUNS_CEILING)
    return `Enter ${MAX_CONCURRENT_RUNS_CEILING.toLocaleString()} or fewer.`
  return null
}

// concurrentRunsToWire converts the concurrent-run text input to the wire value
// for max_concurrent_runs. Validation happens upstream (concurrentRunsError
// gates the Save button), so by the time this runs the input is either blank or
// a non-negative integer:
//   - blank        → 0 (an explicit "unlimited" clear)
//   - valid number → passed straight through
//   - unparseable  → undefined, which JSON.stringify drops, so the field is
//     omitted and a previously-stored limit is left UNTOUCHED rather than
//     silently cleared to 0. Effectively unreachable from a number input; the
//     guard just makes the "never stomp the limit on garbage" property explicit.
function concurrentRunsToWire(raw: string): number | undefined {
  const trimmed = raw.trim()
  if (trimmed === '') return 0
  const n = Number(trimmed)
  return Number.isFinite(n) ? n : undefined
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

// saveOrgConfig persists the org-level CONFIG via POST /api/settings/org.
//
// It sends no secrets. The GitHub PAT and the Jira service credential each
// have their own resource (connectGitHubPAT / connectJira and their
// disconnects), so there is no "" -vs- undefined "leave blank to keep current"
// dance on the wire any more: a credential you didn't retype simply isn't part
// of this request. Base URLs stay here — they're host config the GitHub App
// path needs before any credential exists — and clearing one no longer
// destroys the matching credential.
//
// Returns a discriminated result; `warning` carries the backend's model-cap
// clamp notice on an otherwise-successful save.
export async function saveOrgConfig(
  form: OrgConfigForm,
): Promise<{ ok: true; warning?: string } | { ok: false; error: string }> {
  try {
    const body = await apiJSON<{ warning?: string } | null>('/api/settings/org', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        github_base_url: form.github_url,
        github_poll_interval: form.github_poll_interval,
        github_clone_protocol: form.github_clone_protocol,
        jira_base_url: form.jira_url,
        jira_poll_interval: form.jira_poll_interval,
        max_llm_model_tier: form.max_llm_model_tier,
        max_daily_cost_usd: dailyCapToWire(form.max_daily_cost_usd),
        max_concurrent_runs: concurrentRunsToWire(form.max_concurrent_runs),
      }),
    })
    return { ok: true, warning: body?.warning }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the settings.') }
  }
}
