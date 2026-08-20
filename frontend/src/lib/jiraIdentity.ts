// Per-user Jira access capture — the shared network path behind both the setup
// wizard's Jira-access step and the Settings Jira-identity section.
//
// POSTs the user's own Jira credential to the per-org Jira identity endpoint,
// which validates it against the org's Jira host, derives the resulting account,
// and — unlike the GitHub sibling — STORES the credential (the user acts as
// themselves on board claims, so it must be retained). The end state is a bound
// account with a stored per-user credential.
//
// The credential shape follows the org's deployment: a Data Center org binds a
// personal access token (captureJiraIdentityPat); a Cloud org binds the
// Atlassian account email + API token (captureJiraIdentityApiToken). The route
// is the same — the server branches on the org's deployment.
//
// Resolves to the bound account on success; rejects with an Error carrying the
// endpoint's user-facing `error` message (a bad credential, or a host TF
// couldn't reach) on failure. Goes through apiClient so a stale-session 401 is
// funneled to AuthContext (re-auth) rather than surfacing as a raw status.

import { apiJSON, httpErrorMessage } from './apiClient'

export interface JiraIdentityCaptured {
  account: string
  host: string
}

// postJiraIdentity is the shared poster: it sends the credential body and maps a
// non-2xx into an Error carrying the endpoint's user-facing `error` message.
async function postJiraIdentity(
  orgId: string,
  body: Record<string, string>,
): Promise<JiraIdentityCaptured> {
  try {
    return await apiJSON<JiraIdentityCaptured>(
      `/api/orgs/${encodeURIComponent(orgId)}/jira/identity/pat`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    )
  } catch (err) {
    // Surface the endpoint's user-facing `error` field rather than a bare HTTP
    // status. (A 401 has already been routed to AuthContext by apiClient.)
    throw new Error(httpErrorMessage(err, "Couldn't verify that credential."))
  }
}

// captureJiraIdentityPat binds a Data Center personal access token.
export async function captureJiraIdentityPat(
  orgId: string,
  pat: string,
): Promise<JiraIdentityCaptured> {
  return postJiraIdentity(orgId, { pat })
}

// captureJiraIdentityApiToken binds a Cloud Atlassian email + API token pair.
export async function captureJiraIdentityApiToken(
  orgId: string,
  email: string,
  token: string,
): Promise<JiraIdentityCaptured> {
  return postJiraIdentity(orgId, { email, token })
}
