// The org GitHub bot credential (PAT_1), as its own action pair — the GitHub
// mirror of jiraConnect.ts. Both credentials are addressable resources on the
// backend (PUT/DELETE /api/orgs/{org}/github-access/pat), not fields inside the
// bulk org-settings save, so binding one is a single request that validates,
// stores, re-dues the poller, and lands an audit row.
//
// The practical consequence for callers: there is no "send blank to keep the
// current token" contract to remember. If the user didn't type a token, don't
// call this — the stored one is untouched because nothing asked it to change.

import { readError } from '../../lib/api'

export type CredentialResult = { ok: true; warning?: string } | { ok: false; error: string }

// connectGitHubPAT binds (or rotates) the org's GitHub bot token against
// `baseUrl`. The backend validates the token live and 422s on a bad one, so a
// successful result IS the validation — there's no separate probe. 409 when the
// org is on a GitHub App: switching credentials is the dedicated switch flow's
// job, never a credential save.
export async function connectGitHubPAT(
  orgId: string,
  baseUrl: string,
  pat: string,
): Promise<CredentialResult> {
  return credentialRequest(`/api/orgs/${orgId}/github-access/pat`, 'PUT', {
    base_url: baseUrl.trim(),
    pat: pat.trim(),
  })
}

// disconnectGitHubPAT unbinds the org's GitHub bot token and clears the stored
// host, in one server-side transaction. Idempotent. The caller's own GitHub
// identity (PAT_2) is untouched — that's a separate surface.
export async function disconnectGitHubPAT(orgId: string): Promise<CredentialResult> {
  return credentialRequest(`/api/orgs/${orgId}/github-access/pat`, 'DELETE')
}

// disconnectJira unbinds the org's Jira service credential and clears the
// stored host together. This replaced a two-call client-side sequence (clear
// the secrets, then a sparse settings save to clear the URL) whose failure mode
// was a half-disconnected workspace the user had to reason about.
export async function disconnectJira(orgId: string): Promise<CredentialResult> {
  return credentialRequest(`/api/orgs/${orgId}/jira-access/credential`, 'DELETE')
}

// credentialRequest is the shared call shape for the credential resources:
// a discriminated result, and the backend's optional `warning` (today: the
// local-mode env-overlay caveat, where a delete succeeds but TRIAGE_FACTORY_*
// vars keep supplying the value) passed through rather than swallowed.
async function credentialRequest(
  url: string,
  method: 'PUT' | 'DELETE',
  body?: unknown,
): Promise<CredentialResult> {
  try {
    const res = await fetch(url, {
      method,
      ...(body === undefined
        ? {}
        : { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
    })
    if (!res.ok) {
      return { ok: false, error: await readError(res, 'Request failed') }
    }
    const parsed = (await res.json().catch(() => null)) as { warning?: string } | null
    return { ok: true, warning: parsed?.warning }
  } catch {
    return { ok: false, error: 'Could not reach the server.' }
  }
}
