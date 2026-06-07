// Per-user Jira access capture — the shared network path behind both the setup
// wizard's Jira-access step and the Settings Jira-identity section.
//
// POSTs a user-supplied personal access token to the per-org Jira identity
// endpoint, which validates it against the org's Jira host, derives the
// resulting account, and — unlike the GitHub sibling — STORES the credential
// (the user acts as themselves on board claims, so the token must be retained).
// The end state is a bound account with a stored per-user credential.
//
// Resolves to the bound account on success; rejects with an Error carrying the
// endpoint's user-facing `error` message (a bad token, or a host TF couldn't
// reach) on failure. Goes through apiClient so a stale-session 401 is funneled
// to AuthContext (re-auth) rather than surfacing as a raw status.

import { apiJSON, HttpError } from './apiClient'

export interface JiraIdentityCaptured {
  account: string
  host: string
}

export async function captureJiraIdentityPat(
  orgId: string,
  pat: string,
): Promise<JiraIdentityCaptured> {
  try {
    return await apiJSON<JiraIdentityCaptured>('/identity/jira/pat', {
      org: orgId,
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pat }),
    })
  } catch (err) {
    // Surface the endpoint's user-facing `error` field instead of a bare HTTP
    // status. (A 401 has already been routed to AuthContext by apiClient.)
    if (err instanceof HttpError) {
      let msg = `Couldn't verify that token (HTTP ${err.status}).`
      try {
        const body = JSON.parse(err.body) as { error?: string }
        if (body?.error) msg = body.error
      } catch {
        // Non-JSON error body — keep the generic message.
      }
      throw new Error(msg)
    }
    throw err
  }
}
