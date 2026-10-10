// The org's Linear service credential: its status read, its bind, and its
// unbind (GET /api/orgs/{org}/linear/access and PUT/DELETE
// /api/orgs/{org}/linear/access/credential; see
// internal/server/linear_access.go). Like Jira, the caller's Continue (setup
// wizard) / Save (Settings) performs the bind, and LinearAccessGroup owns only
// the inline disconnect.
//
// Two shapes. A personal API key binds here; Linear has one host, so there is
// no URL and no deployment to choose, and the workspace is learned from the
// key. An installed app binds through the install ceremony, a top-level
// navigation to linearInstallStartURL that comes back with ?linear=installed
// or ?linear_error=<code>; the same DELETE disconnects either.

import { apiJSON, httpErrorMessage } from '../../lib/apiClient'
import { invalidateEventSources } from '../../hooks/useEventSources'
import { credentialRequest, type CredentialResult } from './orgCredentials'

/** LinearAccess mirrors GET /api/orgs/{org}/linear/access. */
export interface LinearAccess {
  connected: boolean
  /** The credential's shape: 'api_key' or 'app_install', '' when not connected. */
  auth_method: '' | 'api_key' | 'app_install'
  /** The workspace the credential belongs to, as in linear.app/<key>. */
  workspace_url_key: string
  workspace_name?: string
  /** Who the credential validated as when it was bound. */
  bound_as: { name: string; display_name: string } | null
  /** A Linear OAuth app resolves for the org, so the install can run. */
  connect_available: boolean
  using_deployment_default: boolean
  /** Why an unconnected org lost its credential: 'install_revoked' when the
   *  app was removed from the workspace in Linear. */
  last_error?: string
}

export async function fetchLinearAccess(orgId: string): Promise<LinearAccess> {
  return apiJSON<LinearAccess>(`/api/orgs/${encodeURIComponent(orgId)}/linear/access`)
}

// connectLinear binds (or rotates) the org's Linear API key. The server
// validates the key against Linear before storing anything, so a successful
// result IS the validation, and a rejected key leaves whatever was bound
// before in place.
export async function connectLinear(
  orgId: string,
  apiKey: string,
): Promise<{ ok: true; access: LinearAccess } | { ok: false; error: string }> {
  try {
    const access = await apiJSON<LinearAccess>(
      `/api/orgs/${encodeURIComponent(orgId)}/linear/access/credential`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: apiKey.trim() }),
      },
    )
    // The org can produce linear events now.
    invalidateEventSources()
    return { ok: true, access }
  } catch (e) {
    return { ok: false, error: httpErrorMessage(e, 'The connection failed.') }
  }
}

// disconnectLinear unbinds the org's Linear credential and forgets its
// workspace, in one server-side transaction. Idempotent.
export async function disconnectLinear(orgId: string): Promise<CredentialResult> {
  return credentialRequest(
    `/api/orgs/${encodeURIComponent(orgId)}/linear/access/credential`,
    'DELETE',
  )
}

// boundAsName is the name the connected line shows for the key's owner.
export function boundAsName(access: LinearAccess): string {
  return access.bound_as?.display_name || access.bound_as?.name || ''
}

// linearInstallStartURL is where the Install button sends the browser: the
// start leg of the install ceremony, which redirects to Linear's consent page
// and comes back to returnTo.
export function linearInstallStartURL(orgId: string, returnTo: string): string {
  return (
    '/api/orgs/' +
    encodeURIComponent(orgId) +
    '/linear/install/start?return_to=' +
    encodeURIComponent(returnTo)
  )
}

// linearInstallReturnTo is the page the install ceremony comes back to: the
// one at href as it is now, query included, because the query carries the
// routing state (the Settings tab) that shows the Linear section again. A
// previous ceremony's outcome is dropped so it does not come back with this
// one.
export function linearInstallReturnTo(href: string): string {
  const url = new URL(href)
  url.searchParams.delete('linear')
  url.searchParams.delete('linear_error')
  return url.pathname + url.search
}

// linearInstallErrorText maps an install's ?linear_error= code, or the access
// read's last_error, to the banner copy. null for no error.
export function linearInstallErrorText(code: string | null | undefined): string | null {
  switch (code) {
    case null:
    case undefined:
    case '':
      return null
    case 'state':
      return 'That install attempt expired or was started by someone else. Start it again.'
    case 'denied':
      return 'The install was cancelled in Linear before it finished. Start it again to connect.'
    case 'no_app':
      return 'Installing needs a Linear OAuth app, and none is configured for this workspace. Add one in the Linear OAuth app section, or paste an API key instead.'
    case 'install_failed':
      return 'Linear did not complete the install. Start it again, or paste an API key instead.'
    case 'workspace_taken':
      return 'That Linear workspace is already connected to another Triage Factory organization. A workspace can be installed in only one.'
    case 'install_revoked':
      return 'Triage Factory was removed from the Linear workspace, so Linear access stopped. Install it again or paste an API key.'
    default:
      return 'Something went wrong connecting Linear. Try again.'
  }
}
