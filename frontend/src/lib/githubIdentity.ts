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
// reach) on failure.

export interface GitHubIdentityCaptured {
  login: string
  host: string
}

export async function captureGitHubIdentityPat(
  orgId: string,
  pat: string,
): Promise<GitHubIdentityCaptured> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/identity/github/pat`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pat }),
  })
  if (!res.ok) {
    let msg = `Couldn't verify that token (HTTP ${res.status}).`
    try {
      const body = (await res.json()) as { error?: string }
      if (body?.error) msg = body.error
    } catch {
      // Non-JSON error body — keep the generic message.
    }
    throw new Error(msg)
  }
  return (await res.json()) as GitHubIdentityCaptured
}
