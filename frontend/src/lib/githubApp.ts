// Helpers for the Workspace Settings "GitHub access" panel. The
// registration ceremony is a top-level browser navigation to github.com
// and back (manifest flow) — see startGitHubAppRegistration. All four
// endpoints are org-scoped under /api/orgs/{org_id}/github-app.

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

export interface RegisterStartResponse {
  manifest_post_url: string
  manifest_json: string
  state: string
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

// startGitHubAppRegistration kicks off the manifest flow: it asks the
// backend for the manifest + signed state, then submits an auto-POST form
// to GitHub. GitHub requires top-level navigation here — a hidden iframe
// won't work — so this builds a real <form>, appends it to the document,
// and submits, taking the whole tab to github.com. On confirm there,
// GitHub redirects back to the callback, which lands the browser on the
// Settings page with #github-app.
export async function startGitHubAppRegistration(
  orgId: string,
  payload: { owner_type: 'user' | 'org'; owner_login: string },
): Promise<void> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github-app/register/start`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
  if (!res.ok) throw new Error(await readError(res, 'Failed to start registration'))
  const { manifest_post_url, manifest_json, state } = (await res.json()) as RegisterStartResponse

  const form = document.createElement('form')
  form.method = 'POST'
  form.action = manifest_post_url

  const manifestField = document.createElement('input')
  manifestField.type = 'hidden'
  manifestField.name = 'manifest'
  manifestField.value = manifest_json
  form.appendChild(manifestField)

  const stateField = document.createElement('input')
  stateField.type = 'hidden'
  stateField.name = 'state'
  stateField.value = state
  form.appendChild(stateField)

  document.body.appendChild(form)
  form.submit()
}
