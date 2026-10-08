// The org's Linear service credential: its status read, its bind, and its
// unbind (GET /api/orgs/{org}/linear/access and PUT/DELETE
// /api/orgs/{org}/linear/access/credential; see
// internal/server/linear_access.go). Like Jira, the caller's Continue (setup
// wizard) / Save (Settings) performs the bind, and LinearAccessGroup owns only
// the inline disconnect.
//
// One shape here: a personal API key. Linear has one host, so there is no URL
// and no deployment to choose, and the workspace is learned from the key.

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
  connect_available: boolean
  using_deployment_default: boolean
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
