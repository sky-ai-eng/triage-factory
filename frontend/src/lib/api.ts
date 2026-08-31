// avatarProxyUrl is the same-origin URL for a user's avatar (TFAC-480). User
// avatars come from cross-origin OAuth CDNs (avatars.githubusercontent.com, the
// org's Jira host) which the app's `img-src 'self'` CSP blocks, so we render
// them through this same-origin proxy endpoint instead — the server fetches +
// caches the upstream image and serves it same-origin. Callers should only
// point an <img> here when the user actually has an avatar (the endpoint 404s
// otherwise, and the consuming components fall back to a monogram on error).
export function avatarProxyUrl(userId: string): string {
  return `/api/avatars/${encodeURIComponent(userId)}`
}
