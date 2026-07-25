// Helpers for the Workspace Settings "Atlassian OAuth app" card — the Jira
// sibling of githubApp.ts. An admin enters a bring-your-own Atlassian OAuth
// app (client_id + client_secret) that the per-user "Connect Jira" flow runs
// against; the card also shows the callback URL the app owner must register.
// The endpoints are org-scoped under /api/orgs/{org_id}/jira/app.

import { readError } from './api'

// The per-org Atlassian OAuth app override summary. Only the public half
// (client_id) is surfaced; the client_secret never leaves the secret store.
export interface JiraAppInfo {
  client_id: string
  registered_at: string
  registered_by_display_name: string
}

export interface JiraAppStatus {
  // The per-org override, null when the org has none (it uses the deployment
  // first-party app in hosted, or has no app in local).
  app: JiraAppInfo | null
  // True exactly when an app resolves (override OR, in hosted, the deployment
  // first-party) — the same bit the per-user Jira status endpoint surfaces to
  // gate the one-click "Connect" button.
  connect_available: boolean
  // True when an app resolves but via the first-party default (no per-org row)
  // — so the card can say "using the hosted app" rather than offering nothing.
  using_hosted_default: boolean
  // The absolute redirect_uri the app owner must register on their Atlassian
  // OAuth app. Empty when no deployment identity is configured.
  connect_callback_url: string
}

export async function getJiraAppStatus(orgId: string): Promise<JiraAppStatus> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/jira/app`)
  if (!res.ok) throw new Error(await readError(res, 'Failed to load Atlassian app status'))
  return (await res.json()) as JiraAppStatus
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
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/jira/app`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
  if (res.ok) {
    return { ok: true, result: (await res.json()) as JiraAppStatus }
  }
  try {
    const body = (await res.json()) as { error?: string; field?: string }
    return {
      ok: false,
      error: body.error || 'Failed to save the Atlassian app.',
      field: body.field,
    }
  } catch {
    return { ok: false, error: 'Failed to save the Atlassian app.' }
  }
}

// deleteJiraApp removes the org's bring-your-own Atlassian OAuth app (the row
// and its stored client_secret). Idempotent. After this the org falls back to
// the deployment first-party app (hosted) or has no app (local).
//
// DELETE /api/orgs/{org_id}/jira/app
export async function deleteJiraApp(orgId: string): Promise<JiraAppStatus> {
  const res = await fetch(`/api/orgs/${encodeURIComponent(orgId)}/jira/app`, { method: 'DELETE' })
  if (!res.ok) throw new Error(await readError(res, 'Failed to remove the Atlassian app'))
  return (await res.json()) as JiraAppStatus
}
