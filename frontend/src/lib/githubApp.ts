// Helpers for the Workspace Settings "GitHub access" panel. The
// registration ceremony is a top-level browser navigation to a backend
// bounce page, which then POSTs the manifest to github.com (manifest
// flow) — see startGitHubAppRegistration. The endpoints are org-scoped
// under /api/orgs/{org_id}/github/app.

import { apiErrors, apiFetch, apiJSON, HttpError, httpErrorMessage } from './apiClient'
import { isHttpUrl } from './reachability'

// asError turns any failed call into an Error carrying the clean user-facing
// string — the server's `{ error }` verbatim (e.g. cutover's 409 "install the
// App before switching"), else the caller's fallback. Every function here is
// consumed by a panel that renders `err.message` straight into the UI, so a
// raw HttpError — whose message embeds the whole response body — must not
// escape this module.
function asError(e: unknown, fallback: string): Error {
  return new Error(httpErrorMessage(e, fallback))
}

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

// GitHubAppWebhookState is the backend's answer to "is GitHub actually
// delivering this App's webhooks here?", probed against the App's own webhook
// configuration and delivery history (never inferred from a stored secret,
// which is true in neither direction).
//
//   - not_configured: no webhook, or the inert placeholder a deployment GitHub
//     can't reach registers. The NORMAL state in local mode — not a fault.
//   - pointing_elsewhere: the App has a webhook, and it isn't this deployment.
//   - delivering_rejected: GitHub is delivering here and the receiver refuses.
//   - no_deliveries: configured for this deployment, nothing in GitHub's
//     30-day window. Ordinary for a new or quiet App.
//   - healthy: the most recent delivery was accepted.
//   - unavailable: this GitHub host doesn't expose the App-webhook endpoints
//     (GHES below 3.2), so the question can't be answered.
export type GitHubAppWebhookState =
  | 'not_configured'
  | 'pointing_elsewhere'
  | 'delivering_rejected'
  | 'no_deliveries'
  | 'healthy'
  | 'unavailable'

// GitHubAppWebhookHealth is the probe's answer as facts, not copy — the panel
// composes the message, because the likely cause of a rejection depends on the
// status code (a 401 reads differently from a failure to connect).
//
// secret_configured is GitHub's masked presence bit for the App's own webhook
// secret. It never carries the value, and TF never compares one: the delivery
// status codes are what prove a secret matches.
export interface GitHubAppWebhookHealth {
  state: GitHubAppWebhookState
  // Origin (scheme://host) the App delivers to, '' when nothing is configured.
  hook_host: string
  secret_configured: boolean
  // RFC3339 of the most recent delivery, '' when there is none.
  last_delivery_at: string
  // What the receiving endpoint answered on that delivery; 0 when GitHub could
  // not connect at all.
  last_delivery_status_code: number
  checked_at: string
}

export interface GitHubAppStatus {
  app: GitHubAppInfo | null
  installations: GitHubAppInstallation[]
  using_deployment_default: boolean
  // null when there's no App, no deployment identity to compare a hook URL
  // against, or no probe answer yet. Absent means NOT KNOWN — never "fine".
  webhook_health: GitHubAppWebhookHealth | null
  // The absolute redirect_uri the App owner must register on the App for per-user
  // "Connect GitHub" OAuth to work. Same URL a manifest-created App already has
  // registered (harmless there); load-bearing for a bring-your-own App whose
  // owner registers it by hand. Empty when no deployment identity is configured.
  connect_callback_url: string
}

export async function getGitHubAppStatus(orgId: string): Promise<GitHubAppStatus> {
  try {
    return await apiJSON<GitHubAppStatus>(`/api/orgs/${encodeURIComponent(orgId)}/github/app`)
  } catch (e) {
    throw asError(e, 'Could not load GitHub App status.')
  }
}

// refreshGitHubAppInstallations reconciles the org's App installation mirror
// against GitHub on demand (TFAC-324's POST endpoint) and returns the refreshed
// status. This is the authoritative install-discovery path in local mode, where
// webhook deliveries never reach the NAT'd host — a plain getGitHubAppStatus
// reads a mirror a brand-new install hasn't touched yet, so the wizard's install
// step and Settings' "Check installation" action POST here to actually find it.
export async function refreshGitHubAppInstallations(orgId: string): Promise<GitHubAppStatus> {
  try {
    return await apiJSON<GitHubAppStatus>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/app/installations/refresh`,
      { method: 'POST' },
    )
  } catch (e) {
    throw asError(e, 'Could not refresh the GitHub App installations.')
  }
}

// GitHubWebhookReplayResult reports what the repair did. candidates is how many
// failed installation deliveries GitHub still holds in its 30-day window, which
// is what makes `replayed: 0` legible — nothing to replay reads very differently
// from nothing accepted.
export interface GitHubWebhookReplayResult {
  candidates: number
  replayed: number
  failed: number
}

// replayGitHubWebhookDeliveries asks GitHub to redeliver the App's failed
// installation deliveries, healing an installation mirror that went stale while
// the receiver was rejecting them (a missing or mismatched webhook secret). Only
// failed deliveries are replayed, and the receiver dedups on the delivery GUID a
// redelivery shares with its original, so this cannot double-apply anything.
//
// POST /api/orgs/{org_id}/github/app/webhook/replay
export async function replayGitHubWebhookDeliveries(
  orgId: string,
): Promise<GitHubWebhookReplayResult> {
  try {
    return await apiJSON<GitHubWebhookReplayResult>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/app/webhook/replay`,
      { method: 'POST' },
    )
  } catch (e) {
    throw asError(e, 'Could not replay the missed webhook deliveries.')
  }
}

export async function getGitHubAppInstallURL(orgId: string): Promise<string> {
  let body: { url: string }
  try {
    body = await apiJSON<{ url: string }>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/app/install-url`,
    )
  } catch (e) {
    throw asError(e, 'Could not load the install URL.')
  }
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
  const fallback = 'Could not import the GitHub App.'
  try {
    const result = await apiJSON<GitHubAppImportResult>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/app/import`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(input),
      },
    )
    return { ok: true, result }
  } catch (e) {
    // Every rejection carries its message in the standard envelope, read
    // through the shared parser so the field attribution comes with it. A
    // permission gap adds to that rather than replacing it: alongside `errors`
    // its body carries the granted-vs-required table the form renders, which no
    // envelope field can hold (see githubAppImportErrorResponse). So check for
    // the table first — the message is the same one either way.
    const item = apiErrors(e)[0]
    if (e instanceof HttpError) {
      try {
        const body = JSON.parse(e.body) as {
          permissions?: GitHubAppPermissionRow[]
          blocking?: boolean
        }
        if (body.permissions) {
          return {
            ok: false,
            error: item?.message || fallback,
            permissions: body.permissions,
            blocking: body.blocking,
          }
        }
      } catch {
        // Body wasn't JSON — fall through.
      }
    }
    if (item) return { ok: false, error: item.message || fallback, field: item.field }
    return { ok: false, error: httpErrorMessage(e, fallback) }
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

// cutoverToApp commits a staged PAT→App switch: the backend reconciles
// installations, verifies ≥1, then activates the App and deletes the PAT
// atomically. Rejects with the backend's message — notably the 409 "install
// the App before switching" when no installation is found yet.
//
// POST /api/orgs/{org_id}/github/app/cutover
export async function cutoverToApp(orgId: string): Promise<void> {
  try {
    await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app/cutover`, {
      method: 'POST',
    })
  } catch (e) {
    throw asError(e, 'Could not switch to the GitHub App.')
  }
}

// cutoverPreflight returns the inform-only reachability diff for a PAT→App
// cutover (which tracked repos the App's installations would leave dark). Has a
// server-side write side-effect (it reconciles the installation mirror), so the
// endpoint is uncacheable — call it right before showing the diff screen.
//
// GET /api/orgs/{org_id}/github/app/cutover-preflight
export async function cutoverPreflight(orgId: string): Promise<AccessDiff> {
  try {
    return await apiJSON<AccessDiff>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/app/cutover-preflight`,
    )
  } catch (e) {
    throw asError(e, 'Could not check repository reachability.')
  }
}

// switchToPat performs the App→PAT switch — a full App teardown. The backend
// validates the PAT, stores it, and deletes the App registration + secrets in
// one transaction; the App still exists on GitHub (the result flags that).
//
// POST /api/orgs/{org_id}/github/pat/switch-to
export async function switchToPat(orgId: string, pat: string): Promise<SwitchToPatResult> {
  try {
    return await apiJSON<SwitchToPatResult>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/pat/switch-to`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pat }),
      },
    )
  } catch (e) {
    throw asError(e, 'Could not switch to a personal access token.')
  }
}

// patPreflight validates a candidate PAT and returns the inform-only
// reachability diff for an App→PAT switch plus the login it authenticates as.
// Stores nothing — switchToPat re-validates on commit.
//
// POST /api/orgs/{org_id}/github/pat/preflight
export async function patPreflight(orgId: string, pat: string): Promise<PatPreflight> {
  try {
    return await apiJSON<PatPreflight>(
      `/api/orgs/${encodeURIComponent(orgId)}/github/pat/preflight`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pat }),
      },
    )
  } catch (e) {
    throw asError(e, 'That token could not be validated.')
  }
}

// discardStagedApp tears down a STAGED App registration — the exit for an
// abandoned PAT→App switch. The live PAT is untouched. 409s for an active App
// (use switchToPat to remove a live one).
//
// DELETE /api/orgs/{org_id}/github/app
export async function discardStagedApp(orgId: string): Promise<void> {
  try {
    await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/github/app`, { method: 'DELETE' })
  } catch (e) {
    throw asError(e, 'Could not discard the staged GitHub App.')
  }
}

// ── The deployment App (managed class, multi mode only) ───────────────────

// startManagedGitHubConnect starts the bind ceremony for the deployment's
// GitHub App with a top-level navigation: the backend mints the pending-bind
// record and cookie and sends the admin to the App's install page on GitHub,
// which returns them to the callback where the bind completes. Control never
// comes back here.
//
// This is ALSO the recovery for an installation that exists but is bound to no
// workspace (installed from the App's public page, approved by an owner after
// a request, reinstalled): the same button, the same ceremony. GitHub hands the
// existing installation back with the same id and the bind lands. There is no
// "adopt an existing installation" call, on purpose — one way to create a
// binding is enough.
export function startManagedGitHubConnect(orgId: string): void {
  window.location.assign(`/api/orgs/${encodeURIComponent(orgId)}/github/managed/connect`)
}
