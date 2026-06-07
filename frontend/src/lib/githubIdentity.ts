// Per-user GitHub identity capture (PAT_2) — the shared network path behind
// both the setup wizard's User step and the Connect gate page's token fallback.
//
// POSTs a user-supplied personal access token to the per-org identity endpoint,
// which validates it against the org's GitHub host, records the resulting login
// in user_github_identities, and DISCARDS the token (the server never stores
// it). The end state is identical to the Connect OAuth flow — a verified
// identity row, no retained credential — so this is the universal fallback to
// Connect, and the only capture path when no GitHub App is registered.
//
// Resolves to the bound login on success; rejects with an Error carrying the
// endpoint's user-facing `error` message (a bad token, or a host TF couldn't
// reach) on failure. Goes through apiClient so a stale-session 401 is funneled
// to AuthContext (re-auth) rather than surfacing as a raw status.

import { apiJSON, HttpError } from './apiClient'

export interface GitHubIdentityCaptured {
  login: string
  host: string
}

export async function captureGitHubIdentityPat(
  orgId: string,
  pat: string,
): Promise<GitHubIdentityCaptured> {
  try {
    return await apiJSON<GitHubIdentityCaptured>('/identity/github/pat', {
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
