// Client for the URL-only reachability endpoints (the setup wizard's URL
// sub-step). These confirm a host *answers* before any credential is entered —
// deliberately distinct from auth validation (the PAT connect / App register).
// The backend (POST /api/github/reachability, POST /api/jira/reachability)
// always returns 200 with a `reachable` verdict, except a malformed URL, which
// is a 400. See internal/server/reachability_handlers.go +
// internal/reachability/reachability.go.

// ReachabilityResult mirrors the backend reachability.Result. `reachable` is
// the verdict; `reason` is a short classified cause ("dns", "timeout", "tls",
// "connection refused", …) present only when unreachable; `status` is the HTTP
// status the host answered with (0 when unreachable). `invalid_url` /
// `network` are client-synthesized reasons for the 400 and our-own-server
// failure cases, so callers have a single shape to branch on.
export interface ReachabilityResult {
  reachable: boolean
  reason?: string
  status?: number
}

// reachabilityMessage turns a failed verdict into a user-facing line. Kept here
// (not in the component) so the GitHub and Jira sub-steps phrase the same
// cause identically.
export function reachabilityMessage(result: ReachabilityResult): string {
  if (result.reachable) return ''
  switch (result.reason) {
    case 'invalid_url':
      return 'That doesn’t look like a valid URL. Include https:// and the host.'
    case 'dns':
      return 'Couldn’t resolve that host — check the address for typos.'
    case 'timeout':
      return 'The host didn’t respond in time. It may be unreachable from this deployment.'
    case 'tls':
      return 'The host answered but its TLS certificate couldn’t be verified.'
    case 'network':
      return 'Couldn’t reach the server to run the check. Try again.'
    default:
      return 'Couldn’t reach that host from this deployment. Check the URL and your network.'
  }
}

// probe POSTs {url} to a reachability endpoint and normalizes the outcome.
// A 400 (malformed URL) becomes reachable:false / reason "invalid_url" so the
// "fix your URL" case shares the unreachable shape; a failure reaching our OWN
// server becomes reason "network".
async function probe(path: string, url: string): Promise<ReachabilityResult> {
  let res: Response
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: url.trim() }),
    })
  } catch {
    return { reachable: false, reason: 'network' }
  }
  if (res.status === 400) {
    return { reachable: false, reason: 'invalid_url' }
  }
  if (!res.ok) {
    return { reachable: false, reason: 'network', status: res.status }
  }
  // A parse failure on a 200 means the server reached us but sent a garbled
  // body — tag it `network` so the message doesn't claim the *host* was
  // unreachable (the default-case copy), which would be misleading.
  return (await res
    .json()
    .catch(() => ({ reachable: false, reason: 'network' }))) as ReachabilityResult
}

// hostOf renders the host (no scheme) for a confirmed-URL summary, falling
// back to the raw string (scheme stripped) when it can't be parsed. Lives here
// so the reachability sub-step's collapsed bar and the wizard's collapsed-step
// summaries render a base URL identically.
export function hostOf(url: string): string {
  try {
    return new URL(url).host || url
  } catch {
    return url.replace(/^https?:\/\//, '')
  }
}

export const checkGitHubReachability = (url: string): Promise<ReachabilityResult> =>
  probe('/api/github/reachability', url)

export const checkJiraReachability = (url: string): Promise<ReachabilityResult> =>
  probe('/api/jira/reachability', url)

// isHttpUrl is the cheap, synchronous URL-format gate the wizard's URL steps
// run in validate() — instant feedback for obviously-malformed input before
// spending a network round-trip on the reachability probe. It mirrors the
// backend's normalizeReachabilityURL contract: must parse, be http(s), and
// carry a host (the backend additionally 400s on userinfo/query/fragment, which
// the probe surfaces as invalid_url).
export function isHttpUrl(raw: string): boolean {
  try {
    const u = new URL(raw.trim())
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.host !== ''
  } catch {
    return false
  }
}

// normalizeBaseUrl trims surrounding whitespace and trailing slashes from a
// user-entered base URL, so the value we probe, persist, and later connect with
// is one canonical form. The backend stores org_settings.github_base_url /
// jira_base_url verbatim (and the reachability probe trims only internally), so
// without this an untrimmed value would pass the probe yet get persisted with
// stray whitespace/slash and break identity lookups or the App/PAT/Jira flows.
export function normalizeBaseUrl(raw: string): string {
  return raw.trim().replace(/\/+$/, '')
}
