// Helpers for the Workspace Settings "GitHub access" panel. The
// registration ceremony is a top-level browser navigation to a backend
// bounce page, which then POSTs the manifest to github.com (manifest
// flow) — see startGitHubAppRegistration. The endpoints are org-scoped
// under /api/orgs/{org_id}/github/app.

import { readError } from './api'
import { isHttpUrl } from './reachability'

// Local mode runs single-org against the synthetic sentinel org. Mirrors
// runmode.LocalDefaultOrgID on the backend; the org-scoped App endpoints
// take the id in the path even in local mode.
export const LOCAL_DEFAULT_ORG_ID = '00000000-0000-0000-0000-000000000001'

export interface GitHubAppInstallation {
  installation_id: string
  account_type: string
  account_login: string
  installed_at: string
  // RFC3339 when the account owner has suspended the installation, '' when it
  // is live: the grant survives a suspension, but GitHub refuses every token
  // minted from it. suspended_by is the login that suspended it ('' when
  // unsuspended, or when GitHub named no one). Nothing renders these yet.
  suspended_at: string
  suspended_by: string
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
  // false while the registration is STAGED — registered during a PAT→App
  // switch but not yet cut over, so the PAT is still the live credential
  // (TFAC-328). The Setup/Settings UX reads this to resolve the live mode,
  // paint the "switch pending" mode-card state, and show the staged-switch
  // banner. true once a cutover activates it.
  active: boolean
}

export interface GitHubAppStatus {
  app: GitHubAppInfo | null
  installations: GitHubAppInstallation[]
  using_hosted_default: boolean
  // The absolute redirect_uri the App owner must register on the App for per-user
  // "Connect GitHub" OAuth to work. Same URL a manifest-created App already has
  // registered (harmless there); load-bearing for a bring-your-own App whose
  // owner registers it by hand. Empty when no deployment identity is configured.
  connect_callback_url: string
}

export async function getGitHubAppStatus(orgId: string): Promise<GitHubAppStatus> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app`)
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
    `/api/orgs/${encodeURIComponent(orgId)}/github/app/installations/refresh`,
    { method: 'POST' },
  )
  if (!res.ok) throw new Error(await readError(res, 'Failed to refresh GitHub App installations'))
  return (await res.json()) as GitHubAppStatus
}

export async function getGitHubAppInstallURL(orgId: string): Promise<string> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app/install-url`)
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

// ── Bring-your-own-App import ─────────────────────────────────────────────
// The second way into App mode: supply an App ID + private key for an App that
// already exists, validated server-side via an app-JWT GET /app round trip. The
// backend derives slug/owner/permissions from the App itself, permission-
// preflights, and persists through the same path the manifest callback uses.

// GitHubAppPermissionRow is one entry in the import preflight's granted-vs-
// required table. severity is 'required' (a hard gap blocks the import) or
// 'optional' (a soft gap blocks only until accept_partial acknowledges it);
// feature names the capability an unmet optional permission degrades.
export interface GitHubAppPermissionRow {
  permission: string
  required: string
  granted: string
  satisfied: boolean
  severity: string
  feature?: string
}

// GitHubAppImportInput is the import request body. client_secret (enables OAuth
// Connect), webhook_secret (multi mode only), and accept_partial (acknowledges
// soft permission gaps) are optional.
export interface GitHubAppImportInput {
  app_id: string
  pem: string
  client_secret?: string
  webhook_secret?: string
  accept_partial?: boolean
}

// GitHubAppImportResult is the success body: the status payload plus the
// preflight table and the client-secret validation outcome.
export interface GitHubAppImportResult extends GitHubAppStatus {
  permissions: GitHubAppPermissionRow[]
  client_secret_stored: boolean
  client_secret_validated: boolean
}

// GitHubAppImportOutcome discriminates a successful import from a rejection. On
// failure, `permissions` + `blocking` are present for a permission gap (blocking
// = a hard gap accept_partial can't override; !blocking = a soft gap the caller
// resubmits past with accept_partial); `field` is present for a field-level
// rejection (bad PEM, app-id mismatch, bad client secret).
export type GitHubAppImportOutcome =
  | { ok: true; result: GitHubAppImportResult }
  | {
      ok: false
      error: string
      permissions?: GitHubAppPermissionRow[]
      blocking?: boolean
      field?: string
    }

// importGitHubApp validates and imports an existing GitHub App. It returns a
// structured outcome rather than throwing, so the form can render the permission
// table on a gap (instead of a bare error string).
//
// POST /api/orgs/{org_id}/github/app/import
export async function importGitHubApp(
  orgId: string,
  input: GitHubAppImportInput,
): Promise<GitHubAppImportOutcome> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app/import`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
  if (res.ok) {
    return { ok: true, result: (await res.json()) as GitHubAppImportResult }
  }
  try {
    const body = (await res.json()) as {
      error?: string
      permissions?: GitHubAppPermissionRow[]
      blocking?: boolean
      field?: string
    }
    return {
      ok: false,
      error: body.error || 'Failed to import the GitHub App.',
      permissions: body.permissions,
      blocking: body.blocking,
      field: body.field,
    }
  } catch {
    return { ok: false, error: 'Failed to import the GitHub App.' }
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
    `/api/orgs/${encodeURIComponent(orgId)}/github/app/register/launch?${params.toString()}`,
  )
}

// ── PAT ↔ App switching (TFAC-328 endpoints) ──────────────────────────────
// These drive the guided switch flows in the wizard and Settings. The backend
// enforces the either/or invariant; the UI's job is to preview the consequence
// (the reachability diff), confirm, and commit.

// A repo the target credential can't reach after a switch, with the teams that
// track it — what the inform-only diff lists so an admin knows who's affected.
export interface DarkRepo {
  repo: string
  teams: string[]
}

// The inform-only reachability diff both switch preflights return: how many of
// the org's tracked repos the target credential reaches, and which go dark.
export interface AccessDiff {
  tracked: number
  reachable: number
  dark_repos: DarkRepo[]
}

// pat-preflight's body — the diff plus the login the PAT authenticates as.
export interface PatPreflight extends AccessDiff {
  login: string
}

// switch-to-pat's success body. github_app_settings_url points at the org
// host's apps page so the UI can guide deleting the (still-existing) App on
// GitHub; github_app_deleted_locally is always true on success (the local
// registration + secrets are gone).
export interface SwitchToPatResult {
  status: string
  github_app_deleted_locally: boolean
  github_app_settings_url: string
}

// switchError returns the backend's clean user-facing `error` message (no
// fallback prefix), so an inline error reads as the server wrote it (e.g.
// cutover's 409 "install the App before switching"). Falls back to a generic
// line when the body carries no error field.
async function switchError(res: Response, fallback: string): Promise<string> {
  try {
    const body = (await res.clone().json()) as { error?: string }
    if (body && typeof body.error === 'string' && body.error) return body.error
  } catch {
    // Non-JSON body — use the fallback.
  }
  return fallback
}

// cutoverToApp commits a staged PAT→App switch: the backend reconciles
// installations, verifies ≥1, then activates the App and deletes the PAT
// atomically. Rejects with the backend's message — notably the 409 "install
// the App before switching" when no installation is found yet.
//
// POST /api/orgs/{org_id}/github/app/cutover
export async function cutoverToApp(orgId: string): Promise<void> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app/cutover`, {
    method: 'POST',
  })
  if (!res.ok) throw new Error(await switchError(res, 'Failed to switch to the GitHub App.'))
}

// cutoverPreflight returns the inform-only reachability diff for a PAT→App
// cutover (which tracked repos the App's installations would leave dark). Has a
// server-side write side-effect (it reconciles the installation mirror), so the
// endpoint is uncacheable — call it right before showing the diff screen.
//
// GET /api/orgs/{org_id}/github/app/cutover-preflight
export async function cutoverPreflight(orgId: string): Promise<AccessDiff> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app/cutover-preflight`)
  if (!res.ok) throw new Error(await switchError(res, 'Failed to check repository reachability.'))
  return (await res.json()) as AccessDiff
}

// switchToPat performs the App→PAT switch — a full App teardown. The backend
// validates the PAT, stores it, and deletes the App registration + secrets in
// one transaction; the App still exists on GitHub (the result flags that).
//
// POST /api/orgs/{org_id}/github/access/switch-to-pat
export async function switchToPat(orgId: string, pat: string): Promise<SwitchToPatResult> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/access/switch-to-pat`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pat }),
  })
  if (!res.ok)
    throw new Error(await switchError(res, 'Failed to switch to a personal access token.'))
  return (await res.json()) as SwitchToPatResult
}

// patPreflight validates a candidate PAT and returns the inform-only
// reachability diff for an App→PAT switch plus the login it authenticates as.
// Stores nothing — switchToPat re-validates on commit.
//
// POST /api/orgs/{org_id}/github/access/pat-preflight
export async function patPreflight(orgId: string, pat: string): Promise<PatPreflight> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/access/pat-preflight`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pat }),
  })
  if (!res.ok) throw new Error(await switchError(res, 'That token could not be validated.'))
  return (await res.json()) as PatPreflight
}

// discardStagedApp tears down a STAGED App registration — the exit for an
// abandoned PAT→App switch. The live PAT is untouched. 409s for an active App
// (use switchToPat to remove a live one).
//
// DELETE /api/orgs/{org_id}/github/app
export async function discardStagedApp(orgId: string): Promise<void> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app`, {
    method: 'DELETE',
  })
  if (!res.ok) throw new Error(await switchError(res, 'Failed to discard the staged GitHub App.'))
}
