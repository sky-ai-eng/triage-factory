// The Bedrock connect actions — the AWS sibling of connectAnthropic
// (anthropicConnect.ts): the wizard's Continue and the Settings section's Save
// both drive these helpers, so Bedrock credentials are captured through one
// validated path per shape (the org settings PATCH accepts none of these
// fields). Validation is shape-only server-side for the two static shapes —
// there is no cheap, permission-agnostic live probe for IAM credentials — so a
// bad credential surfaces on the first run rather than at connect time. Role
// mode is the exception: its bind performs a real AssumeRole.
//
// One route per shape, and no "leave blank to keep current". Each PUT carries
// exactly its own shape's fields and replaces the resource, so the secret is
// always required — a save that only means to change the region still re-enters
// the credential. That is the cost of the ambiguity going away: a blank field
// used to mean "keep" here and "delete everything" on the Anthropic sibling,
// and nothing on the wire distinguished the two intents.

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

// bedrockPayloadFromForm maps the shared org form to the selected shape's route
// and body. Only that shape's fields are sent — the server decodes strictly
// against the shape named in the path, so a leftover value from another
// method's input isn't merely tidied away here, it would be a 400.
export function bedrockPayloadFromForm(form: OrgConfigForm): {
  path: string
  body: Record<string, string>
} {
  const config: Record<string, string> = {
    region: form.bedrock_region,
    model_id: form.bedrock_model_id,
    base_url: form.bedrock_base_url,
  }
  if (form.bedrock_auth_method === 'role') {
    // Role mode carries no secret — the ARN is the only shape-specific field;
    // the server mints short-lived session credentials by assuming it.
    return { path: 'role', body: { ...config, role_arn: form.bedrock_role_arn } }
  }
  if (form.bedrock_auth_method === 'bearer') {
    return { path: 'bearer', body: { ...config, bearer_token: form.bedrock_bearer_token } }
  }
  return {
    path: 'access-keys',
    body: {
      ...config,
      access_key_id: form.aws_access_key_id,
      secret_access_key: form.aws_secret_access_key,
      session_token: form.aws_session_token,
    },
  }
}

export async function connectBedrock(
  orgId: string,
  payload: { path: string; body: Record<string, string> },
): Promise<{ ok: true } | { ok: false; error: string }> {
  try {
    await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/llm/bedrock/${payload.path}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload.body),
    })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not save the Bedrock credentials.') }
  }
}

// disconnectBedrock removes every piece of Bedrock material the org holds —
// credential, region, model, endpoint override, role ARN and External ID.
// Idempotent.
export async function disconnectBedrock(
  orgId: string,
): Promise<{ ok: true } | { ok: false; error: string }> {
  try {
    await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/llm/bedrock`, { method: 'DELETE' })
    return { ok: true }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not remove the Bedrock credentials.') }
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
