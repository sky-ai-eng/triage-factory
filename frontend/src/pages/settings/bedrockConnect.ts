// The Bedrock connect action — the AWS sibling of connectAnthropic
// (anthropicConnect.ts): the wizard's Continue and the Settings section's
// Save both drive this one helper, so Bedrock credentials are captured
// through the single validated POST /api/bedrock/connect path (the bulk org
// POST accepts none of these fields). Validation is shape-only server-side
// — there is no cheap, permission-agnostic live probe for IAM credentials —
// so a bad credential surfaces on the first run rather than at connect time.
//
// "Leave blank to keep current": posting the same auth method with every
// secret field blank keeps the stored credential and updates only the
// non-secret config (region / model / endpoint). The backend owns that rule;
// this module just forwards the form verbatim.

import { apiFetch, apiJSON, httpErrorMessage } from '../../lib/apiClient'
import type { BedrockAuthMethod, OrgConfigForm } from './orgConfig'

// BEDROCK_AUTH_OPTIONS is the shared label set for the auth-method picker,
// the Bedrock analog of CLAUDE_SOURCE_OPTIONS — one list so the wizard step
// and the Settings section present identical choices.
export const BEDROCK_AUTH_OPTIONS: {
  kind: BedrockAuthMethod
  title: string
  detail: string
}[] = [
  {
    kind: 'role',
    title: 'IAM role (recommended)',
    detail:
      'Short-lived credentials: Triage Factory assumes a role you control and mints per-run session tokens. No static key stored.',
  },
  {
    kind: 'bearer',
    title: 'Bedrock API key',
    detail: 'A single Bedrock API key (no IAM setup). Generated in the AWS console.',
  },
  {
    kind: 'access_keys',
    title: 'IAM access keys',
    detail: 'An AWS access key ID + secret (and session token for temporary credentials).',
  },
]

// bedrockPayloadFromForm maps the shared org form to the connect endpoint's
// wire shape. Trimming happens server-side; only the selected method's
// secret fields are sent so a leftover value from the other method's inputs
// can't leak into the request.
export function bedrockPayloadFromForm(form: OrgConfigForm): Record<string, string> {
  const payload: Record<string, string> = {
    auth_method: form.bedrock_auth_method,
    region: form.bedrock_region,
    model_id: form.bedrock_model_id,
    base_url: form.bedrock_base_url,
  }
  if (form.bedrock_auth_method === 'role') {
    // Role mode carries no secret — the ARN is the only method-specific field;
    // the server mints short-lived session credentials by assuming it.
    payload.role_arn = form.bedrock_role_arn
  } else if (form.bedrock_auth_method === 'bearer') {
    payload.bearer_token = form.bedrock_bearer_token
  } else {
    payload.access_key_id = form.aws_access_key_id
    payload.secret_access_key = form.aws_secret_access_key
    payload.session_token = form.aws_session_token
  }
  return payload
}

export async function connectBedrock(
  payload: Record<string, string>,
): Promise<{ ok: true } | { ok: false; error: string }> {
  try {
    await apiFetch('/api/bedrock/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the Bedrock credentials.') }
  }
}

// fetchBedrockRoleSetup asks the control service to describe the IAM-role
// handshake for this org: the trust policy (ready-to-copy JSON) the operator
// pastes onto their role, the external ID that policy must pin, and the caller
// identity the control service assumes FROM. It's a POST (no body) so it stays
// a non-idempotent action, not a cacheable GET. A 422 means the control service
// has no ambient AWS identity (an operator-side deployment problem) — the
// returned `error` is the remediation text, surfaced inline. Mirrors
// connectBedrock's shape + fetch conventions.
export async function fetchBedrockRoleSetup(): Promise<
  | { ok: true; caller_identity_arn: string; external_id: string; trust_policy: string }
  | { ok: false; error: string }
> {
  try {
    const resBody = await apiJSON<{
      caller_identity_arn?: string
      external_id?: string
      trust_policy?: string
    }>('/api/bedrock/role-setup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    })
    return {
      ok: true,
      caller_identity_arn: resBody.caller_identity_arn || '',
      external_id: resBody.external_id || '',
      trust_policy: resBody.trust_policy || '',
    }
  } catch {
    return { ok: false, error: 'Could not connect to server' }
  }
}
