// Helpers for the Workspace Settings "GitHub access" panel. The
// registration ceremony is a top-level browser navigation to a backend
// bounce page, which then POSTs the manifest to github.com (manifest
// flow) — see startGitHubAppRegistration. The endpoints are org-scoped
// under /api/orgs/{org_id}/github-app.

import { readError } from './api'

// Local mode runs single-org against the synthetic sentinel org. Mirrors
// runmode.LocalDefaultOrgID on the backend; the org-scoped App endpoints
// take the id in the path even in local mode.
export const LOCAL_DEFAULT_ORG_ID = '00000000-0000-0000-0000-000000000001'

export interface GitHubAppInstallation {
  installation_id: string
  account_type: string
  account_login: string
  installed_at: string
}

export interface GitHubAppInfo {
  app_id: string
  slug: string
  // 'user' (personal account) or 'org' (organization) — the account the App
  // was registered under, persisted at registration and used to seed the
  // "App account type" picker so it doesn't re-default to Personal on reload.
  owner_type: string
  registered_at: string
  registered_by_display_name: string
}

export interface GitHubAppStatus {
  app: GitHubAppInfo | null
  installations: GitHubAppInstallation[]
  using_hosted_default: boolean
}

export async function getGitHubAppStatus(orgId: string): Promise<GitHubAppStatus> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github-app`)
  if (!res.ok) throw new Error(await readError(res, 'Failed to load GitHub App status'))
  return (await res.json()) as GitHubAppStatus
}

// refreshGitHubAppInstallations reconciles the org's App installation mirror
// against GitHub on demand (TFAC-324's POST endpoint) and returns the refreshed
// status. This is the authoritative install-discovery path in local mode, where
// webhook deliveries never reach the NAT'd host — a plain getGitHubAppStatus
// reads a mirror a brand-new install hasn't touched yet, so the wizard's install
// step and Settings' "Check installation" action POST here to actually find it.
export async function refreshGitHubAppInstallations(orgId: string): Promise<GitHubAppStatus> {
  const res = await fetch(
    `/api/orgs/${encodeURIComponent(orgId)}/github-app/installations/refresh`,
    { method: 'POST' },
  )
  if (!res.ok) throw new Error(await readError(res, 'Failed to refresh GitHub App installations'))
  return (await res.json()) as GitHubAppStatus
}

export async function getGitHubAppInstallURL(orgId: string): Promise<string> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github-app/install-url`)
  if (!res.ok) throw new Error(await readError(res, 'Failed to load install URL'))
  const body = (await res.json()) as { url: string }
  // The URL is backend-derived from the org's GitHub base URL. Reject any
  // non-http(s) scheme here, at the single source, so neither an <a href>
  // (RepoPickerModal) nor window.open (Settings) can ever be handed a
  // javascript: link from a misconfigured base URL.
  if (!isHttpUrl(body.url)) {
    throw new Error('GitHub App install URL has an unsupported scheme')
  }
  return body.url
}

// isHttpUrl reports whether url parses and uses an http(s) scheme.
function isHttpUrl(url: string): boolean {
  try {
    const { protocol } = new URL(url)
    return protocol === 'http:' || protocol === 'https:'
  } catch {
    return false
  }
}

// startGitHubAppRegistration kicks off the manifest flow with a top-level
// navigation to the backend launch endpoint. The SPA's global CSP sets
// `form-action 'self'`, which would block an in-page form POST to
// github.com — but it does NOT govern top-level navigation. The launch
// page then renders the manifest form under its own per-response CSP
// (scoped to the org's GitHub host) and does the cross-origin POST when
// the user clicks "Continue to GitHub". GitHub redirects back to the
// callback, which lands the browser back where registration was launched
// from: `return_to` ('setup' from the wizard, 'settings' from Settings) is
// threaded through the signed state so the user resumes where they left off
// rather than always being dropped on Settings.
export function startGitHubAppRegistration(
  orgId: string,
  payload: { owner_type: 'user' | 'org'; owner_login: string; return_to: 'setup' | 'settings' },
): void {
  const params = new URLSearchParams({
    owner_type: payload.owner_type,
    owner_login: payload.owner_login,
    return_to: payload.return_to,
  })
  window.location.assign(
    `/api/orgs/${encodeURIComponent(orgId)}/github-app/register/launch?${params.toString()}`,
  )
}
