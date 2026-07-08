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
  if (form.bedrock_auth_method === 'bearer') {
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
    const res = await fetch('/api/bedrock/connect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
    const resBody = await res.json().catch(() => ({}))
    if (!res.ok) {
      return { ok: false, error: resBody.error || 'Could not save the Bedrock credentials' }
    }
    return { ok: true }
  } catch {
    return { ok: false, error: 'Could not connect to server' }
  }
}
