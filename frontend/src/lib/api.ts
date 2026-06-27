// readError extracts a usable error message from a non-ok fetch response.
// Tries the `error` field of a JSON body first (the server's convention
// in writeJSON responses), then falls back to the status text + code so
// toasts always show *something* meaningful instead of "[object Object]".
export async function readError(res: Response, fallback: string): Promise<string> {
  try {
    const body = await res.clone().json()
    if (body && typeof body.error === 'string' && body.error.length > 0) {
      return `${fallback}: ${body.error}`
    }
  } catch {
    // Body wasn't JSON — fall through to the text path.
  }
  try {
    const text = await res.text()
    if (text) return `${fallback}: ${text}`
  } catch {
    // ignore
  }
  return `${fallback} (${res.status})`
}

// avatarProxyUrl is the same-origin URL for a user's avatar (TFAC-480). User
// avatars come from cross-origin OAuth CDNs (avatars.githubusercontent.com, the
// org's Jira host) which the app's `img-src 'self'` CSP blocks, so we render
// them through this first-party proxy endpoint instead — the server fetches +
// caches the upstream image and serves it same-origin. Callers should only
// point an <img> here when the user actually has an avatar (the endpoint 404s
// otherwise, and the consuming components fall back to a monogram on error).
export function avatarProxyUrl(userId: string): string {
  return `/api/avatars/${encodeURIComponent(userId)}`
}
