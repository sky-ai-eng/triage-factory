// The org's integration credentials — the GitHub bot PAT and the Jira service
// credential — as bind/unbind actions against their backend resources
// (PUT/DELETE /api/orgs/{org}/github/pat and .../jira/access/credential;
// see internal/server/org_credentials.go, which this mirrors). They are not
// fields inside the bulk org-settings save, so each action is a single request
// that validates, stores, re-dues the poller, and lands an audit row.
//
// Both providers live here rather than in per-provider modules because the call
// shape is identical and shared (credentialRequest below); the Jira *bind* is
// the exception and stays in jiraConnect.ts with the deployment picker it's
// coupled to — the credential shape it sends depends on Cloud vs Data Center.
//
// The practical consequence for callers: there is no "send blank to keep the
// current token" contract to remember. If the user didn't type a token, don't
// call this — the stored one is untouched because nothing asked it to change.

import { apiFetch, httpErrorMessage } from '../../lib/apiClient'
import { invalidateEventSources } from '../../hooks/useEventSources'

// CredentialResult carries the bind/unbind outcome. `login` is the identity the
// bound credential resolved to (GitHub only, and only on a bind) — the caller
// shows it back so a rotation confirms which account the new token authenticates
// as rather than just reporting "saved".
export type CredentialResult =
  | { ok: true; warning?: string; login?: string }
  | { ok: false; error: string }

// connectGitHubPAT binds (or rotates) the org's GitHub bot token. The host is
// not ours to send: the backend validates against whatever GitHub URL the org
// has saved, so the URL must already be committed (the setup wizard's own step
// does that before this call, and a connected org has one by definition). The
// backend 422s on a token that host rejects, so a successful result IS the
// validation — there's no separate probe. 409 when the org is on a GitHub App:
// switching credentials is the dedicated switch flow's job, never a credential
// save.
//
// Rotation and first bind are the same call: the route replaces whatever was
// stored, and the previous credential survives untouched if this one fails
// validation — which is what makes it safe to offer as an in-place "replace
// this token" from Settings.
export async function connectGitHubPAT(orgId: string, pat: string): Promise<CredentialResult> {
  return credentialRequest(`/api/orgs/${orgId}/github/pat`, 'PUT', { pat: pat.trim() })
}

// disconnectGitHubPAT unbinds the org's GitHub bot token and clears the stored
// host, in one server-side transaction. Idempotent. The caller's own GitHub
// identity (PAT_2) is untouched — that's a separate surface.
export async function disconnectGitHubPAT(orgId: string): Promise<CredentialResult> {
  return credentialRequest(`/api/orgs/${orgId}/github/pat`, 'DELETE')
}

// disconnectJira unbinds the org's Jira service credential and clears the
// stored host together. This replaced a two-call client-side sequence (clear
// the secrets, then a sparse settings save to clear the URL) whose failure mode
// was a half-disconnected workspace the user had to reason about.
export async function disconnectJira(orgId: string): Promise<CredentialResult> {
  return credentialRequest(`/api/orgs/${orgId}/jira/access/credential`, 'DELETE')
}

// credentialRequest is the shared call shape for the credential resources:
// a discriminated result, and the backend's optional `warning` (today: the
// local-mode env-overlay caveat, where a delete succeeds but TRIAGE_FACTORY_*
// vars keep supplying the value) and `login` (the GitHub bind's resolved
// identity) passed through rather than swallowed.
async function credentialRequest(
  url: string,
  method: 'PUT' | 'DELETE',
  body?: unknown,
): Promise<CredentialResult> {
  try {
    // A DELETE may answer 204 with no body, so this reads the response itself
    // rather than going through apiJSON, and parses best-effort.
    const res = await apiFetch(url, {
      method,
      ...(body === undefined
        ? {}
        : { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }),
    })
    const parsed = (await res.json().catch(() => null)) as {
      warning?: string
      login?: string
    } | null
    // Binding or unbinding a credential is exactly what moves a source
    // between available and unconfigured, so the cached availability answer is
    // now stale for every surface holding it.
    invalidateEventSources()
    return { ok: true, warning: parsed?.warning, login: parsed?.login }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'Could not reach the server.') }
  }
}
