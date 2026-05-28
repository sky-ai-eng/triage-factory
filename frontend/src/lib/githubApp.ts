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

export async function getGitHubAppInstallURL(orgId: string): Promise<string> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github-app/install-url`)
  if (!res.ok) throw new Error(await readError(res, 'Failed to load install URL'))
  const body = (await res.json()) as { url: string }
  return body.url
}

// startGitHubAppRegistration kicks off the manifest flow with a top-level
// navigation to the backend launch endpoint. The SPA's global CSP sets
// `form-action 'self'`, which would block an in-page form POST to
// github.com — but it does NOT govern top-level navigation. The launch
// page then renders the manifest form under its own per-response CSP
// (scoped to the org's GitHub host) and does the cross-origin POST when
// the user clicks "Continue to GitHub". GitHub redirects back to the
// callback, landing the browser on Settings with #github-app.
export function startGitHubAppRegistration(
  orgId: string,
  payload: { owner_type: 'user' | 'org'; owner_login: string },
): void {
  const params = new URLSearchParams({
    owner_type: payload.owner_type,
    owner_login: payload.owner_login,
  })
  window.location.assign(
    `/api/orgs/${encodeURIComponent(orgId)}/github-app/register/launch?${params.toString()}`,
  )
}
