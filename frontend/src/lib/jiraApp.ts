// Helpers for the Workspace Settings "Atlassian OAuth app" card — the Jira
// sibling of githubApp.ts. An admin enters a bring-your-own Atlassian OAuth
// app (client_id + client_secret) that the per-user "Connect Jira" flow runs
// against; the card also shows the callback URL the app owner must register.
// The endpoints are org-scoped under /api/orgs/{org_id}/jira/app.

import { apiErrors, apiJSON, httpErrorMessage } from './apiClient'

// asError mirrors githubApp's: the card renders `err.message` directly, so a
// raw HttpError (whose message embeds the response body) must not escape here.
function asError(e: unknown, fallback: string): Error {
  return new Error(httpErrorMessage(e, fallback))
}

// The per-org Atlassian OAuth app override summary. Only the public half
// (client_id) is surfaced; the client_secret never leaves the secret store.
export interface JiraAppInfo {
  client_id: string
  registered_at: string
  registered_by_display_name: string
}

export interface JiraAppStatus {
  // The per-org override, null when the org has none (it uses the deployment
  // app in multi, or has no app in local).
  app: JiraAppInfo | null
  // True exactly when an app resolves (override OR, in multi, the deployment
  // app) — the same bit the per-user Jira status endpoint surfaces to gate the
  // one-click "Connect" button.
  connect_available: boolean
  // True when an app resolves but via the deployment default (no per-org row)
  // — so the card can say "using the deployment app" rather than offering
  // nothing.
  using_deployment_default: boolean
  // The absolute redirect_uri the app owner must register on their Atlassian
  // OAuth app. Empty when no deployment identity is configured.
  connect_callback_url: string
}

export async function getJiraAppStatus(orgId: string): Promise<JiraAppStatus> {
  try {
    return await apiJSON<JiraAppStatus>(`/api/orgs/${encodeURIComponent(orgId)}/jira/app`)
  } catch (e) {
    throw asError(e, 'Could not load the Atlassian app status.')
  }
}

// JiraAppImportInput is the import request body — a bring-your-own Atlassian
// OAuth app's client credentials.
export interface JiraAppImportInput {
  client_id: string
  client_secret: string
}

// JiraAppImportOutcome discriminates a successful store from a rejection. On
// failure, `field` is present for a field-level rejection (missing client_id /
// client_secret).
export type JiraAppImportOutcome =
  | { ok: true; result: JiraAppStatus }
  | { ok: false; error: string; field?: string }

// importJiraApp stores (or replaces) the org's bring-your-own Atlassian OAuth
// app. Returns a structured outcome rather than throwing so the form can map a
// field-level rejection to the right input.
//
// POST /api/orgs/{org_id}/jira/app
export async function importJiraApp(
  orgId: string,
  input: JiraAppImportInput,
): Promise<JiraAppImportOutcome> {
  const fallback = 'Could not save the Atlassian app.'
  try {
    const result = await apiJSON<JiraAppStatus>(`/api/orgs/${encodeURIComponent(orgId)}/jira/app`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    })
    return { ok: true, result }
  } catch (e) {
    // `field` is what routes a rejection to the right input, so the failure is
    // read through the shared envelope parser rather than for its message
    // alone — apiErrors carries the field with it.
    const item = apiErrors(e)[0]
    if (item) return { ok: false, error: item.message || fallback, field: item.field }
    return { ok: false, error: httpErrorMessage(e, fallback) }
  }
}

// deleteJiraApp removes the org's bring-your-own Atlassian OAuth app (the row
// and its stored client_secret). Idempotent. After this the org falls back to
// the deployment app (multi) or has no app (local).
//
// DELETE /api/orgs/{org_id}/jira/app
export async function deleteJiraApp(orgId: string): Promise<JiraAppStatus> {
  try {
    return await apiJSON<JiraAppStatus>(`/api/orgs/${encodeURIComponent(orgId)}/jira/app`, {
      method: 'DELETE',
    })
  } catch (e) {
    throw asError(e, 'Could not remove the Atlassian app.')
  }
}
