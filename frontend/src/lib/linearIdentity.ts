// Per-user Linear access — the shared network path behind the gate page
// (ConnectLinear) and the setup wizard's Linear user step.
//
// A pasted personal API key POSTs to the per-org identity endpoint, which
// validates it against Linear, refuses one from another workspace, and STORES
// it in the user's own secret scope: Triage Factory acts as the user in
// Linear, so the credential has to outlive the request. One-click Connect is a
// top-level navigation to linearConnectStartURL instead, which comes back to
// returnTo, or to the gate page with ?error=<code> (linearConnectErrorText).

import { apiJSON, httpErrorMessage } from './apiClient'

export interface LinearIdentityCaptured {
  account: string
  workspace_url_key: string
}

// captureLinearIdentityApiKey binds the caller's own Linear API key. Rejects
// with an Error carrying the endpoint's user-facing message (a key Linear
// refused, one from another workspace, or Linear unreachable).
export async function captureLinearIdentityApiKey(
  orgId: string,
  apiKey: string,
): Promise<LinearIdentityCaptured> {
  try {
    return await apiJSON<LinearIdentityCaptured>(
      `/api/orgs/${encodeURIComponent(orgId)}/linear/identity/api-key`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: apiKey.trim() }),
      },
    )
  } catch (err) {
    throw new Error(httpErrorMessage(err, "Couldn't verify that API key."))
  }
}

// linearConnectStartURL is where the Connect button sends the browser: the
// start leg, which redirects to Linear's consent page and comes back to
// returnTo.
export function linearConnectStartURL(orgId: string, returnTo: string): string {
  return (
    '/api/orgs/' +
    encodeURIComponent(orgId) +
    '/linear/connect/start?return_to=' +
    encodeURIComponent(returnTo)
  )
}

// linearConnectErrorText maps a Connect's ?error= code to the banner copy.
// null for no error.
export function linearConnectErrorText(code: string | null): string | null {
  switch (code) {
    case null:
    case '':
      return null
    case 'state':
      return 'That connection attempt expired. Please try again.'
    case 'denied':
      // Linear lets an admin require approval before members authorize a
      // third-party app; whether installing the app for the workspace counts
      // as that approval is untested.
      return 'Linear authorization was cancelled before it finished. If you did not cancel it, your Linear workspace may require an admin to approve Triage Factory first. Ask them, or paste an API key below.'
    case 'no_app':
      return 'One-click Connect needs a Linear OAuth app, which isn’t configured for this workspace. Paste an API key below instead, or ask your admin to add the app.'
    case 'not_configured':
      return 'Your workspace hasn’t connected Linear yet. Ask an admin to connect it in Workspace Settings first.'
    case 'workspace_mismatch':
      return 'The Linear account you authorized belongs to a different Linear workspace. Sign in to Linear with your account in the connected workspace, or paste an API key from it below.'
    case 'sso_required':
      return 'This organization requires signing in through its single sign-on. Sign in that way, then connect Linear again.'
    default:
      return 'Something went wrong connecting your Linear account. Please try again, or paste an API key below.'
  }
}
