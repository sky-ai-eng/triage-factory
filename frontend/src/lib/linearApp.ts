// Helpers for the Workspace Settings "Linear OAuth app" card — the Linear
// sibling of jiraApp.ts. An admin creates an OAuth app in Linear from a
// pre-filled link, pastes its client id and secret here, and a Linear
// workspace admin can then install Triage Factory as an app user. The
// endpoints are org-scoped under /api/orgs/{org_id}/linear/app (see
// internal/server/linear_app_handlers.go).

import { apiErrors, apiJSON, httpErrorMessage } from './apiClient'

// asError mirrors jiraApp's: the card renders `err.message` directly, so a raw
// HttpError (whose message embeds the response body) must not escape here.
function asError(e: unknown, fallback: string): Error {
  return new Error(httpErrorMessage(e, fallback))
}

// The org's own app, public half only; the client secret never leaves the
// secret store.
export interface LinearAppInfo {
  client_id: string
  registered_at: string
  registered_by_display_name: string
}

export interface LinearAppStatus {
  // The org's own app, null when it has none or its secret has gone missing.
  app: LinearAppInfo | null
  // True exactly when an app resolves (the org's own, or the deployment's), so
  // the install ceremony can run.
  install_available: boolean
  // True when the app that resolves is the deployment's rather than the org's.
  using_deployment_default: boolean
  // Linear's app-creation page, pre-filled for this deployment. Linear does
  // not hand the credentials back, so the admin copies them from the app's
  // page afterwards. Empty when no deployment identity is configured.
  create_url: string
  // The two redirect URIs the app registers: the install callback and the
  // per-user Connect callback.
  redirect_uris: string[]
}

export async function getLinearAppStatus(orgId: string): Promise<LinearAppStatus> {
  try {
    return await apiJSON<LinearAppStatus>(`/api/orgs/${encodeURIComponent(orgId)}/linear/app`)
  } catch (e) {
    throw asError(e, 'Could not load the Linear app status.')
  }
}

export interface LinearAppImportInput {
  client_id: string
  client_secret: string
}

export type LinearAppImportOutcome =
  | { ok: true; result: LinearAppStatus }
  | { ok: false; error: string; field?: string }

// importLinearApp stores (or replaces) the org's Linear OAuth app. Returns an
// outcome rather than throwing so the form can route a field-level rejection.
// A replacement by a different app while an install is live is refused 409.
//
// POST /api/orgs/{org_id}/linear/app
export async function importLinearApp(
  orgId: string,
  input: LinearAppImportInput,
): Promise<LinearAppImportOutcome> {
  const fallback = 'Could not save the Linear app.'
  try {
    const result = await apiJSON<LinearAppStatus>(
      `/api/orgs/${encodeURIComponent(orgId)}/linear/app`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(input),
      },
    )
    return { ok: true, result }
  } catch (e) {
    const item = apiErrors(e)[0]
    if (item) return { ok: false, error: item.message || fallback, field: item.field }
    return { ok: false, error: httpErrorMessage(e, fallback) }
  }
}

// deleteLinearApp removes the org's Linear OAuth app and its secret.
// Idempotent. Refused 409 while an install minted by the app is live: the
// install's refreshes need the secret.
//
// DELETE /api/orgs/{org_id}/linear/app
export async function deleteLinearApp(orgId: string): Promise<LinearAppStatus> {
  try {
    return await apiJSON<LinearAppStatus>(`/api/orgs/${encodeURIComponent(orgId)}/linear/app`, {
      method: 'DELETE',
    })
  } catch (e) {
    throw asError(e, 'Could not remove the Linear app.')
  }
}
