// The org's Slack connections and the deployment's Slack app (ee/slack).
//
// An org connects a Slack workspace one of two ways: through the deployment's
// app, by the install ceremony (a top-level navigation to slackInstallStartURL
// that comes back with ?slack=installed|staged or ?slack_error=<code>), or
// through a Slack app of its own, by pasting its tokens to the import route.
// A workspace can hold several of the org's connections; one is active, and
// the rest are inactive until activate swaps one in. activate takes the
// fingerprint of the activation preview it follows.

import { apiFetch, apiJSON, HttpError } from '../../lib/apiClient'

// SlackConnectionStatus is one app's live Socket Mode connection status —
// present only when the backend's socket manager has a connection running for
// the row's api_app_id (rows sharing an app share the same connection). Absent
// for a webhook-only app, an unentitled org, or before the reconciler's first
// pass. A connection through the deployment's app carries only `state`,
// because its socket serves every org.
export interface SlackConnectionStatus {
  state: 'dialing' | 'open' | 'draining' | 'backing_off' | 'auth_failed'
  connected_at?: string
  last_event_at?: string
  consecutive_failures?: number
  last_error?: string
}

export interface SlackConnection {
  workspace_id: string
  api_app_id: string
  // source is whose Slack app the connection is through: the org's own, or
  // the deployment's. Of an org's connections in one workspace, the active
  // one is the one that acts.
  source: 'org' | 'deployment'
  active: boolean
  workspace_name: string
  app_name: string
  enterprise_id?: string
  // transport is null on a deployment connection, which receives through the
  // transport the deployment's operator configured.
  transport: 'socket' | 'events_api' | null
  bot_user_id: string
  registered_by_user_id?: string
  created_at: string
  updated_at: string
  connection?: SlackConnectionStatus
}

export type SlackTransport = 'socket' | 'events_api'

/** SlackDeploymentApp mirrors GET /api/slack/deployment-app. */
export interface SlackDeploymentApp {
  app_id: string
  transport: SlackTransport
}

export type SlackChannelStatus = 'member' | 'will_join' | 'needs_invite' | 'unknown'

export interface SlackActivationChannel {
  channel_id: string
  name: string
  status: SlackChannelStatus
}

export interface SlackActivationBlocker {
  code: string
  /** The scopes the token lacks; set on missing_scopes only. */
  scopes?: string[]
}

export interface SlackActivationNote {
  code: string
  message: string
}

/** SlackActivationPreview mirrors GET …/activation-preview. `from` is null
 *  when the workspace has no active connection. */
export interface SlackActivationPreview {
  from: SlackConnection | null
  to: SlackConnection
  channels: SlackActivationChannel[]
  blockers: SlackActivationBlocker[]
  notes: SlackActivationNote[]
  fingerprint: string
}

/** A watched channel the activated connection's bot did not join. */
export interface SlackChannelWarning {
  code?: string
  channel_id?: string
  reason?: string
}

/** SlackActivationResult mirrors POST …/activate's answer. */
export interface SlackActivationResult {
  connection: SlackConnection
  warnings: SlackChannelWarning[]
}

export interface SlackImportBody {
  bot_token: string
  signing_secret?: string
  app_token?: string
  transport?: SlackTransport
}

// fetchSlackDeploymentApp reads the deployment's Slack app: null when the
// deployment has none (404).
export async function fetchSlackDeploymentApp(): Promise<SlackDeploymentApp | null> {
  try {
    return await apiJSON<SlackDeploymentApp>('/api/slack/deployment-app')
  } catch (e) {
    if (e instanceof HttpError && e.status === 404) return null
    throw e
  }
}

export async function importSlackConnection(
  orgId: string,
  body: SlackImportBody,
): Promise<SlackConnection> {
  return apiJSON<SlackConnection>(
    `/api/orgs/${encodeURIComponent(orgId)}/slack/connections/import`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  )
}

export async function disconnectSlackConnection(
  orgId: string,
  conn: Pick<SlackConnection, 'workspace_id' | 'api_app_id'>,
): Promise<void> {
  await apiFetch(
    `/api/orgs/${encodeURIComponent(orgId)}/slack/connections/${encodeURIComponent(conn.workspace_id)}/${encodeURIComponent(conn.api_app_id)}`,
    { method: 'DELETE' },
  )
}

// fetchSlackManifest reads the manifest an org's own app is created from, as
// the YAML text Slack's "Create an app from a manifest" takes.
export async function fetchSlackManifest(orgId: string, transport: SlackTransport) {
  const resp = await apiFetch(
    `/api/orgs/${encodeURIComponent(orgId)}/slack/manifest?transport=${transport}`,
  )
  return resp.text()
}

export async function fetchSlackActivationPreview(
  orgId: string,
  conn: Pick<SlackConnection, 'workspace_id' | 'api_app_id'>,
): Promise<SlackActivationPreview> {
  return apiJSON<SlackActivationPreview>(
    `/api/orgs/${encodeURIComponent(orgId)}/slack/connections/${encodeURIComponent(conn.workspace_id)}/${encodeURIComponent(conn.api_app_id)}/activation-preview`,
  )
}

export async function activateSlackConnection(
  orgId: string,
  conn: Pick<SlackConnection, 'workspace_id' | 'api_app_id'>,
  fingerprint: string,
): Promise<SlackActivationResult> {
  return apiJSON<SlackActivationResult>(
    `/api/orgs/${encodeURIComponent(orgId)}/slack/connections/${encodeURIComponent(conn.workspace_id)}/${encodeURIComponent(conn.api_app_id)}/activate`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ fingerprint }),
    },
  )
}

// slackInstallStartURL is where Add to Slack sends the browser: the start leg
// of the deployment app's install ceremony, which redirects to Slack's consent
// page and comes back to returnTo. workspaceId preselects the workspace in
// Slack's picker, for a switch.
export function slackInstallStartURL(orgId: string, returnTo: string, workspaceId?: string) {
  const query = new URLSearchParams({ return_to: returnTo })
  if (workspaceId) query.set('workspace_id', workspaceId)
  return `/api/orgs/${encodeURIComponent(orgId)}/slack/install/start?${query.toString()}`
}

// The query parameters the install ceremony comes back with.
const SLACK_RETURN_PARAMS = ['slack', 'slack_workspace', 'slack_error']

// slackInstallReturnTo is the page the install ceremony comes back to: the
// one at href as it is now, query included, because the query carries the
// routing state (the Org page's tab) that shows the Slack section again. A
// previous ceremony's outcome is dropped so it does not come back with this
// one.
export function slackInstallReturnTo(href: string): string {
  const url = new URL(href)
  for (const k of SLACK_RETURN_PARAMS) url.searchParams.delete(k)
  return url.pathname + url.search
}

/** What the install ceremony reported on the URL it came back to. */
export interface SlackInstallReturn {
  /** The workspace a connection was stored in: active ('installed') or
   *  inactive beside the active one ('staged'). */
  outcome: { kind: 'installed' | 'staged'; workspaceId: string } | null
  error: string | null
}

export function readSlackInstallReturn(search: string): SlackInstallReturn {
  const q = new URLSearchParams(search)
  const result = q.get('slack')
  const workspaceId = q.get('slack_workspace') ?? ''
  return {
    outcome:
      (result === 'installed' || result === 'staged') && workspaceId
        ? { kind: result, workspaceId }
        : null,
    error: q.get('slack_error') || null,
  }
}

// stripSlackInstallReturn drops the ceremony's parameters from the current
// URL, so a reload does not report the outcome again.
export function stripSlackInstallReturn() {
  const url = new URL(window.location.href)
  if (!SLACK_RETURN_PARAMS.some((k) => url.searchParams.has(k))) return
  for (const k of SLACK_RETURN_PARAMS) url.searchParams.delete(k)
  window.history.replaceState(window.history.state, '', url.pathname + url.search + url.hash)
}

const INSTALL_FAILED_TEXT =
  'Slack did not complete the install. Try again, or ask your operator to check the logs.'

// slackInstallErrorText maps an install's ?slack_error= code to the card's
// banner. null for no error.
export function slackInstallErrorText(code: string | null): string | null {
  switch (code) {
    case null:
    case '':
      return null
    case 'state':
      return 'That install link expired. Start again.'
    case 'sso_required':
      return "Sign in with your organization's SSO to connect Slack."
    case 'denied':
      return 'The install was cancelled in Slack.'
    case 'no_app':
      return 'This deployment has no Slack app configured.'
    case 'enterprise_install':
      return 'Install into a single workspace. Organization-wide installs are not supported.'
    case 'workspace_taken':
      return "Another organization on this deployment has connected that workspace through the deployment's app. Connect your own Slack app instead."
    default:
      return INSTALL_FAILED_TEXT
  }
}

// slackAppLabel names a connection's app. Slack can answer an install with no
// app name; the id stands in then.
export function slackAppLabel(conn: Pick<SlackConnection, 'app_name' | 'api_app_id'>): string {
  return conn.app_name || conn.api_app_id
}

export function slackWorkspaceLabel(
  conn: Pick<SlackConnection, 'workspace_name' | 'workspace_id'>,
): string {
  return conn.workspace_name || conn.workspace_id
}

/** One Slack workspace's connections, as the card shows them. */
export interface SlackWorkspaceGroup {
  workspaceId: string
  active: SlackConnection | null
  inactive: SlackConnection[]
}

// groupSlackWorkspaces folds the org's connections into one group per
// workspace, in the order the list served the workspaces.
export function groupSlackWorkspaces(conns: SlackConnection[]): SlackWorkspaceGroup[] {
  const groups = new Map<string, SlackWorkspaceGroup>()
  for (const c of conns) {
    let g = groups.get(c.workspace_id)
    if (!g) {
      g = { workspaceId: c.workspace_id, active: null, inactive: [] }
      groups.set(c.workspace_id, g)
    }
    if (c.active) g.active = c
    else g.inactive.push(c)
  }
  return [...groups.values()]
}

// slackBlockerText is the preview's line for one thing that stops the
// activation, in the terms activate's own refusal uses.
export function slackBlockerText(b: SlackActivationBlocker): string {
  switch (b.code) {
    case 'token_rejected':
      return 'Slack rejected the bot token of this app. Connect the app again before switching to it.'
    case 'missing_scopes':
      return `This app lacks the Slack scopes ${(b.scopes ?? []).join(', ')}. Reinstall it with them before switching to it.`
    case 'app_token_rejected':
      return 'Slack rejected the app-level token of this app, so it would receive no events. Connect the app again with a working app-level token before switching to it.'
    default:
      return 'This app cannot be switched to.'
  }
}

// slackJoinWarningText says why the activated bot is not in a channel.
export function slackJoinWarningText(reason: string | undefined): string {
  switch (reason) {
    case 'invite_required':
      return 'It is private. Invite the new bot to it in Slack.'
    default:
      return 'Slack did not let the new bot join. Invite it in Slack.'
  }
}
