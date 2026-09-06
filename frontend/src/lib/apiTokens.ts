import { apiFetch, apiJSON, apiListAll } from './apiClient'
import type { ApiToken, ApiTokenCreateRequest, ApiTokenCreated, ApiTokenPolicy } from '../types'

// The caller's own API tokens — the headless credential surface — and the
// org policy that binds them. Multi-mode only: every route here 404s in local,
// and the page that mounts this never renders there.

/** Every token the caller holds, across their orgs. The set is bounded (fifty
 *  per org) so it is read whole, and the table pages it client-side. */
export function listApiTokens(): Promise<ApiToken[]> {
  return apiListAll<ApiToken>('/api/me/tokens/list', {})
}

/** Mints one token. The response is the only place the plaintext ever exists;
 *  a 409 (the per-org limit) or 422 (a field fault, the cap among them) throws
 *  an HttpError whose message the form prints verbatim. */
export function createApiToken(body: ApiTokenCreateRequest): Promise<ApiTokenCreated> {
  return apiJSON<ApiTokenCreated>('/api/me/tokens', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

/** Renames in place. Name is the only editable field on a token by design. */
export function renameApiToken(id: string, name: string): Promise<ApiToken> {
  return apiJSON<ApiToken>(`/api/me/tokens/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  })
}

/** Revokes. A 204, so apiFetch rather than apiJSON. `keepalive` is for the
 *  table's undo window closing on unload — an ordinary fetch is killed with
 *  the page. */
export async function revokeApiToken(
  id: string,
  opts: { keepalive?: boolean } = {},
): Promise<void> {
  await apiFetch(`/api/me/tokens/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    keepalive: opts.keepalive,
  })
}

/** The cap that binds the caller's tokens in one org — readable by any member. */
export function readApiTokenPolicy(orgId: string): Promise<ApiTokenPolicy> {
  return apiJSON<ApiTokenPolicy>(`/api/orgs/${encodeURIComponent(orgId)}/api-token-policy`)
}
