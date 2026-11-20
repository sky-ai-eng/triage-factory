// SlackWorkspacesCard is the org's Slack section in Settings: the org's
// connections, one row per Slack workspace, and the ways to connect one. It is
// self-contained (it owns its list fetch, the connect draft, and every action),
// so it has no shared Save footer.
//
// There are two ways to connect. When the deployment has a Slack app, Add to
// Slack runs its install ceremony, a navigation to Slack and back whose outcome
// arrives on this page's URL. An org's own app is the alternative, and the
// only way when the deployment has no app: create it from a copied manifest,
// install it to the workspace, then paste its credentials back here. Transport
// is inferred server-side from which credentials are supplied, so the form
// asks for a choice only when both a signing secret and an app-level token are
// supplied.
//
// A workspace can hold several of the org's connections, and one of them is
// active. Connecting a second app into a workspace stages it inactive beside
// the active one, and the switch preview (SlackActivationDialog) makes it the
// active one.

import { useEffect, useRef, useState } from 'react'
import { Tooltip } from '../../ui/tooltip/Tooltip'
import { Check, Copy, ExternalLink, HelpCircle, Plus, Trash2, X } from 'lucide-react'
import { httpErrorMessage } from '../../lib/apiClient'
import { invalidateEventSources } from '../../hooks/useEventSources'
import { usePagedList } from '../../hooks/usePagedList'
import { toast } from '../../components/Toast/toastStore'
import { glassInputClass } from './primitives'
import SlackActivationDialog from './SlackActivationDialog'
import { ConnectionStatusChip, SourceChip, StatusDot, TransportChip } from './SlackChips'
import {
  disconnectSlackConnection,
  fetchSlackDeploymentApp,
  fetchSlackManifest,
  groupSlackWorkspaces,
  importSlackConnection,
  readSlackInstallReturn,
  slackAppLabel,
  slackInstallErrorText,
  slackInstallReturnTo,
  slackInstallStartURL,
  slackWorkspaceLabel,
  stripSlackInstallReturn,
  type SlackConnection,
  type SlackDeploymentApp,
  type SlackImportBody,
  type SlackWorkspaceGroup,
} from './slackConnect'

// FieldHelp is the hover [?] beside a credential label: the "where in Slack do
// I find this" navigation lives here rather than as always-on body text, so the
// form stays terse while the answer is one hover away. Children carry the path.
function FieldHelp({ children }: { children: React.ReactNode }) {
  // The [?] exists only for its hint, so the tooltip host IS the interactive
  // thing: tab stop, aria-describedby, instant open on focus. Its tap handler
  // also swallows the click, which keeps a click on the glyph from refocusing
  // the input through the label wrapping each field.
  return (
    <Tooltip content={children} wrap>
      <span
        role="img"
        aria-label="Where to find this in Slack"
        className="cursor-help text-ink-3/60 transition-colors hover:text-ink-2"
      >
        <HelpCircle size={12} />
      </span>
    </Tooltip>
  )
}

// rowKey is a connection's identity: a workspace may hold several of the org's
// connected apps, so workspace_id alone does not name one.
const rowKey = (c: Pick<SlackConnection, 'workspace_id' | 'api_app_id'>) =>
  `${c.workspace_id}:${c.api_app_id}`

const SLACK_APP_CREATE_URL = 'https://api.slack.com/apps?new_app=1'

const primaryButtonClass =
  'inline-flex items-center rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90 disabled:opacity-40 disabled:shadow-none'

const textActionClass = 'text-reported text-warm transition-colors hover:underline'

/**
 * canManage is whether the viewer is an org admin. Every action here is
 * admin-only on the server; a member sees the connections and nothing to do
 * with them.
 */
export default function SlackWorkspacesCard({
  orgId,
  canManage,
}: {
  orgId: string
  canManage: boolean
}) {
  const connections = usePagedList<SlackConnection>(
    `/api/orgs/${encodeURIComponent(orgId)}/slack/connections/list`,
    'Could not load connected Slack workspaces.',
  )
  const { load, setItems } = connections
  const workspaces = connections.items
  // The org whose list a read has answered. Only a failure before then is
  // shown: a transient poll failure must not put a banner over the list
  // already on screen.
  const [loadedFor, setLoadedFor] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    const read = (keepDepth: boolean) =>
      void load({}, { keepDepth }).then((page) => {
        if (!cancelled && page) setLoadedFor(orgId)
      })
    read(false)
    // Poll so a row's live Socket Mode status resolves (Connecting… →
    // Connected, or a reconnect) without a manual reload — the connection
    // state lives in the backend socket manager, not the websocket stream.
    const timer = setInterval(() => read(true), 5000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [load, orgId])

  const loaded = loadedFor === orgId
  const loadError = loaded ? '' : connections.error

  // The deployment's Slack app, or null when it has none; undefined until the
  // read answers, so the connect actions do not change under the viewer. A
  // failed read offers only the org's own app, which works either way.
  const [deployment, setDeployment] = useState<SlackDeploymentApp | null | undefined>(undefined)
  useEffect(() => {
    let live = true
    fetchSlackDeploymentApp().then(
      (app) => live && setDeployment(app),
      () => live && setDeployment(null),
    )
    return () => {
      live = false
    }
  }, [])

  // The install ceremony reports its outcome on the URL it returns to. Read it
  // once, then drop it from the URL so a reload does not report it again.
  const [returned] = useState(() => readSlackInstallReturn(window.location.search))
  useEffect(() => {
    stripSlackInstallReturn()
  }, [])
  const [installError, setInstallError] = useState(() => slackInstallErrorText(returned.error))
  // The deployment app's install is another org's in that workspace, so the
  // org's own app is the way left.
  const [formOpen, setFormOpen] = useState(returned.error === 'workspace_taken')
  const formRef = useRef<HTMLDivElement>(null)

  // The connection whose switch preview is open.
  const [previewFor, setPreviewFor] = useState<SlackConnection | null>(null)

  // A staged install is the first step of a switch the admin started, so its
  // preview opens as soon as the list holds the connection.
  const [stagedReturn, setStagedReturn] = useState(
    returned.outcome?.kind === 'staged' ? returned.outcome.workspaceId : null,
  )
  if (stagedReturn && loaded) {
    setStagedReturn(null)
    const staged = workspaces.find(
      (c) => c.workspace_id === stagedReturn && c.source === 'deployment' && !c.active,
    )
    if (staged && canManage) setPreviewFor(staged)
  }

  const announced = useRef(false)
  useEffect(() => {
    if (announced.current || returned.outcome?.kind !== 'installed' || !loaded) return
    announced.current = true
    const id = returned.outcome.workspaceId
    const conn = workspaces.find((c) => c.workspace_id === id)
    // The org's first workspace moves slack from unconfigured to available.
    invalidateEventSources()
    toast.success(`Connected ${conn ? slackWorkspaceLabel(conn) : id}`)
  }, [returned, loaded, workspaces])

  // A write shows on the held rows at once, then rereads them: the reread
  // supersedes a poll that began before the write and would otherwise land
  // after it with the rows as they were, and it mints the next page's token
  // against the list the write left.
  const reread = () => void load({}, { keepDepth: true })

  const removeFromList = (conn: SlackConnection) => {
    setItems((list) => list.filter((c) => rowKey(c) !== rowKey(conn)))
    reread()
  }

  const imported = (conn: SlackConnection) => {
    setItems((list) => [...list.filter((c) => rowKey(c) !== rowKey(conn)), conn])
    reread()
    invalidateEventSources()
    setInstallError(null)
    setFormOpen(false)
    // An app connected into a workspace that has an active connection is
    // staged beside it; the preview is the rest of that switch.
    if (conn.active) toast.success(`Connected ${slackWorkspaceLabel(conn)}`)
    else setPreviewFor(conn)
  }

  const activated = (conn: SlackConnection) => {
    setItems((list) =>
      list.map((c) =>
        c.workspace_id !== conn.workspace_id
          ? c
          : rowKey(c) === rowKey(conn)
            ? conn
            : { ...c, active: false },
      ),
    )
    reread()
  }

  // Scrolled once the form has expanded, so the form itself is brought into
  // view rather than the folded controls above it.
  const openOwnForm = () => {
    setFormOpen(true)
    setTimeout(
      () => formRef.current?.scrollIntoView?.({ behavior: 'smooth', block: 'nearest' }),
      COLLAPSE_MS,
    )
  }

  // The page the install ceremony comes back to.
  const returnTo = slackInstallReturnTo(window.location.href)
  const groups = groupSlackWorkspaces(workspaces)

  return (
    <div className="space-y-5">
      <p className="text-body leading-relaxed text-ink-3">
        Connect one or more Slack workspaces so Triage Factory can respond to @mentions.
        {canManage && deployment && (
          <>
            {' '}
            Add to Slack installs this deployment&rsquo;s Slack app into a workspace. You can
            connect a Slack app of your own instead.
          </>
        )}
        {canManage && deployment === null && (
          <>
            {' '}
            Create a Slack app from the manifest below, install it to your workspace, then paste its
            credentials here.
          </>
        )}
      </p>

      {installError && (
        <p
          role="alert"
          className="rounded-xl border border-alarm/30 bg-alarm/5 px-4 py-2.5 text-ui leading-snug text-alarm"
        >
          {installError}
        </p>
      )}

      {loadError && (
        <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
          {loadError}
        </p>
      )}

      {groups.length > 0 && (
        <ul className="space-y-2">
          {groups.map((g) => (
            <WorkspaceRow
              key={g.workspaceId}
              orgId={orgId}
              group={g}
              deploymentApp={!!deployment}
              canManage={canManage}
              returnTo={returnTo}
              onReview={setPreviewFor}
              onSwitchToOwn={openOwnForm}
              onRemoved={removeFromList}
            />
          ))}
        </ul>
      )}

      {connections.hasMore && (
        <button
          type="button"
          onClick={() => void connections.loadMore()}
          disabled={connections.loading}
          className="text-ui font-medium text-ink-2 transition-colors hover:text-ink-1 disabled:opacity-40"
        >
          Show more
        </button>
      )}

      {canManage && (
        <div ref={formRef}>
          <ConnectArea
            orgId={orgId}
            deployment={deployment}
            hasWorkspaces={groups.length > 0}
            formOpen={formOpen}
            setFormOpen={setFormOpen}
            returnTo={returnTo}
            onImported={imported}
          />
        </div>
      )}

      {canManage && previewFor && (
        <SlackActivationDialog
          orgId={orgId}
          target={previewFor}
          switching={workspaces.some((c) => c.workspace_id === previewFor.workspace_id && c.active)}
          onClose={() => setPreviewFor(null)}
          onActivated={activated}
        />
      )}
    </div>
  )
}

// COLLAPSE_MS is the height/opacity transition length. It's duplicated as the
// `visibility` hide-delay below and must stay in step with the `duration-300`
// utility on the animated container.
const COLLAPSE_MS = 300

// Collapsible animates its child between height 0 and its natural height (the
// grid-rows 0fr↔1fr trick over an overflow-hidden inner child — no measuring)
// with an opacity cross-fade. Keyboard/AT safety rides on `visibility`, not
// `inert`: when closed, the inner subtree flips to `visibility: hidden`, which
// every browser and screen reader honors by dropping the whole subtree from
// the tab order and the accessibility tree. That covers the connect form's
// anchor and its inputs alike — a disabled <fieldset> would leave the <a>
// tabbable, and `inert` support is uneven. The hide is delayed by the
// transition length so the content stays on screen through the collapse;
// showing is immediate so it's focusable the instant it starts expanding.
function Collapsible({ open, children }: { open: boolean; children: React.ReactNode }) {
  return (
    <div
      className={`grid transition-[grid-template-rows,opacity] duration-300 ease-out ${
        open ? 'grid-rows-[1fr] opacity-100' : 'grid-rows-[0fr] opacity-0'
      }`}
    >
      <div
        className="overflow-hidden"
        style={{
          visibility: open ? 'visible' : 'hidden',
          transition: `visibility 0s linear ${open ? '0s' : `${COLLAPSE_MS}ms`}`,
        }}
      >
        {children}
      </div>
    </div>
  )
}

// ConnectArea is how a workspace gets connected. With a deployment app, Add to
// Slack is the main action and the org's own app sits behind a link. Without
// one, the own-app form is the only way: open before the first workspace is
// connected, and folded behind "Add another workspace" after, since another
// is then the rare case.
function ConnectArea({
  orgId,
  deployment,
  hasWorkspaces,
  formOpen,
  setFormOpen,
  returnTo,
  onImported,
}: {
  orgId: string
  deployment: SlackDeploymentApp | null | undefined
  hasWorkspaces: boolean
  formOpen: boolean
  setFormOpen: (open: boolean) => void
  returnTo: string
  onImported: (conn: SlackConnection) => void
}) {
  if (deployment === undefined) return null

  if (deployment) {
    return (
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <a href={slackInstallStartURL(orgId, returnTo)} className={primaryButtonClass}>
            Add to Slack
          </a>
          <button
            type="button"
            onClick={() => setFormOpen(!formOpen)}
            aria-expanded={formOpen}
            className="text-ui text-warm transition-colors hover:underline"
          >
            Use your own Slack app instead
          </button>
        </div>
        <Collapsible open={formOpen}>
          <ConnectFlow
            orgId={orgId}
            title="Your own Slack app"
            onConnected={onImported}
            onCollapse={() => setFormOpen(false)}
          />
        </Collapsible>
      </div>
    )
  }

  if (!hasWorkspaces) {
    return <ConnectFlow orgId={orgId} onConnected={onImported} />
  }

  return (
    <div>
      {/* The trigger and the form are two halves of one control: whichever
          isn't showing collapses to height 0 and (via Collapsible) visibility
          hidden, so exactly one is interactive at a time and they cross-fade
          rather than pop. */}
      <Collapsible open={!formOpen}>
        <button
          type="button"
          onClick={() => setFormOpen(true)}
          aria-expanded={formOpen}
          className="flex w-full items-center justify-center gap-1.5 rounded-2xl border border-dashed border-[var(--color-line-1)] px-4 py-3 text-ui font-medium text-ink-2 transition-colors hover:border-warm/30 hover:text-ink-1"
        >
          <Plus size={14} />
          Add another workspace
        </button>
      </Collapsible>

      <Collapsible open={formOpen}>
        <ConnectFlow
          orgId={orgId}
          title="Add another workspace"
          onConnected={onImported}
          onCollapse={() => setFormOpen(false)}
        />
      </Collapsible>
    </div>
  )
}

// WorkspaceRow is one Slack workspace: its active connection, or the inactive
// one left when the active one was disconnected, and a staged line for each
// other connection.
function WorkspaceRow({
  orgId,
  group,
  deploymentApp,
  canManage,
  returnTo,
  onReview,
  onSwitchToOwn,
  onRemoved,
}: {
  orgId: string
  group: SlackWorkspaceGroup
  deploymentApp: boolean
  canManage: boolean
  returnTo: string
  onReview: (conn: SlackConnection) => void
  onSwitchToOwn: () => void
  onRemoved: (conn: SlackConnection) => void
}) {
  const main = group.active ?? group.inactive[0]
  const staged = group.active ? group.inactive : group.inactive.slice(1)
  const workspace = slackWorkspaceLabel(main)

  let action: React.ReactNode = null
  if (!main.active) {
    action = (
      <button type="button" onClick={() => onReview(main)} className={textActionClass}>
        Activate
      </button>
    )
  } else if (main.source === 'deployment') {
    action = (
      <button type="button" onClick={onSwitchToOwn} className={textActionClass}>
        Switch to your own app
      </button>
    )
  } else if (deploymentApp) {
    action = (
      <a
        href={slackInstallStartURL(orgId, returnTo, main.workspace_id)}
        className={textActionClass}
      >
        Switch to the deployment&rsquo;s app
      </a>
    )
  }

  return (
    <li className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="truncate text-body font-medium text-ink-1">{workspace}</span>
            {main.active ? (
              <ConnectionStatusChip conn={main} />
            ) : (
              <StatusDot label="Inactive" dot="bg-ink-3/40" text="text-ink-3" />
            )}
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-1.5">
            <span className="truncate text-reported text-ink-2">{slackAppLabel(main)}</span>
            <SourceChip source={main.source} />
            {main.source === 'org' && main.transport && (
              <TransportChip transport={main.transport} />
            )}
            {main.enterprise_id && (
              <span className="text-reported text-ink-3">Enterprise Grid</span>
            )}
          </div>
        </div>
        {canManage && (
          <div className="flex shrink-0 items-center gap-3">
            {action}
            <DisconnectButton
              key={rowKey(main)}
              orgId={orgId}
              conn={main}
              workspace={workspace}
              onRemoved={onRemoved}
            />
          </div>
        )}
      </div>

      {staged.map((s) => (
        <div
          key={rowKey(s)}
          className="mt-2.5 flex flex-wrap items-center gap-x-2 gap-y-1 border-t border-line-1 pt-2.5 text-reported text-ink-3"
        >
          <span>
            Staged: <span className="text-ink-2">{slackAppLabel(s)}</span>
          </span>
          {canManage && (
            <>
              <span aria-hidden="true">·</span>
              <button
                type="button"
                onClick={() => onReview(s)}
                aria-label={`Review switch to ${slackAppLabel(s)}`}
                className={textActionClass}
              >
                Review switch
              </button>
              <span aria-hidden="true">·</span>
              <DisconnectButton
                orgId={orgId}
                conn={s}
                workspace={workspace}
                text
                onRemoved={onRemoved}
              />
            </>
          )}
        </div>
      ))}
    </li>
  )
}

// DisconnectButton makes Triage Factory forget one connection's credential.
// Nothing is removed in Slack: the app stays installed there.
function DisconnectButton({
  orgId,
  conn,
  workspace,
  text = false,
  onRemoved,
}: {
  orgId: string
  conn: SlackConnection
  workspace: string
  /** A text action, for the staged line; an icon otherwise. */
  text?: boolean
  onRemoved: (conn: SlackConnection) => void
}) {
  const [busy, setBusy] = useState(false)
  const app = slackAppLabel(conn)

  const disconnect = async () => {
    if (busy) return
    const effect = conn.active
      ? 'Triage Factory stops receiving events from this workspace.'
      : 'It is not the workspace’s active connection, so Triage Factory’s work there is unaffected.'
    const kept =
      conn.source === 'deployment'
        ? ' The app stays installed in Slack until someone removes it there.'
        : ''
    if (!window.confirm(`Disconnect ${app} from ${workspace}? ${effect}${kept}`)) return
    setBusy(true)
    try {
      await disconnectSlackConnection(orgId, conn)
      // The org's last workspace going away moves slack from available back to
      // unconfigured; drop the cached answer either way.
      invalidateEventSources()
      toast.success(`${app} disconnected from ${workspace}`)
      onRemoved(conn)
    } catch (e) {
      toast.failed(e, 'Could not disconnect the Slack app.')
      setBusy(false)
    }
  }

  const label = `Disconnect ${app} from ${workspace}`
  if (text) {
    return (
      <button
        type="button"
        onClick={() => void disconnect()}
        disabled={busy}
        aria-label={label}
        className="text-reported text-alarm transition-colors hover:text-alarm/80 disabled:opacity-40"
      >
        Disconnect
      </button>
    )
  }
  return (
    <button
      type="button"
      onClick={() => void disconnect()}
      disabled={busy}
      aria-label={label}
      className="inline-flex shrink-0 items-center rounded-full p-1.5 text-ink-3 transition-colors hover:text-alarm disabled:opacity-40"
    >
      <Trash2 size={14} />
    </button>
  )
}

// credentialShapeError catches the credential mix-ups that otherwise fail deep
// in Slack's API with an opaque message, and the expiring bot token of an app
// with token rotation on, which the server refuses. Returns a user-facing hint, or
// null when the shapes look right.
function credentialShapeError(botToken: string, appToken: string): string | null {
  if (botToken.startsWith('xoxe.xoxb-')) {
    return 'This bot token expires, because your Slack app has token rotation turned on. Slack doesn’t allow turning it off, so create a new app from the copied manifest and use its Bot User OAuth Token.'
  }
  if (!botToken.startsWith('xoxb-')) {
    return "That doesn't look like a bot token. Copy the Bot User OAuth Token (starts “xoxb-”) from your Slack app's OAuth & Permissions page."
  }
  if (appToken && !appToken.startsWith('xapp-')) {
    return "That doesn't look like an app-level token. Generate one under Basic Information → App-Level Tokens (starts “xapp-”) with the connections:write scope."
  }
  return null
}

// humanizeConnectError rewrites the raw connect failure — often a verbatim
// Slack API error the connector has no way to interpret — into an actionable
// message. Unknown errors fall through unchanged.
function humanizeConnectError(raw: string): string {
  const m = raw.toLowerCase()
  if (m.includes('team_id')) {
    return 'Slack accepted the token but it isn’t scoped to one workspace. On Enterprise Grid, install the app to a single workspace rather than org-wide, then reconnect.'
  }
  if (m.includes('invalid_auth') || m.includes('not_authed') || m.includes('account_inactive')) {
    return 'Slack rejected the token. Confirm the app is installed to your workspace and that you copied the Bot User OAuth Token (xoxb-) from OAuth & Permissions.'
  }
  if (m.includes('missing_scope')) {
    return 'The app is missing a scope Triage Factory needs. Recreate it from the copied manifest (which includes the scopes) and reinstall.'
  }
  return raw
}

// ConnectFlow is the own-app form: the deep link + manifest copy buttons, then
// the paste-back credential fields. Fields carry the "leave blank to keep
// current" convention for re-submits (re-pasting the same bot token to update
// signing_secret/app_token without retyping the one that isn't changing).
function ConnectFlow({
  orgId,
  title,
  onConnected,
  onCollapse,
}: {
  orgId: string
  /** The heading of a collapsible panel, beside its close affordance. */
  title?: string
  onConnected: (conn: SlackConnection) => void
  // Present only when the form is a collapsible panel; renders the close
  // affordance that folds it back.
  onCollapse?: () => void
}) {
  const [botToken, setBotToken] = useState('')
  const [signingSecret, setSigningSecret] = useState('')
  const [appToken, setAppToken] = useState('')
  const [transport, setTransport] = useState<'' | 'socket' | 'events_api'>('')
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const needsTransportChoice = signingSecret.trim() !== '' && appToken.trim() !== ''
  const canSubmit =
    botToken.trim() !== '' && !connecting && (!needsTransportChoice || transport !== '')

  const connect = async () => {
    if (!canSubmit) return
    // Catch a token of the wrong shape locally, with a message that says what
    // to copy instead: a mix-up otherwise fails deep in Slack's API with an
    // opaque error (a bot token pasted from App Credentials, an app-level
    // token that isn't one). Slack still has the final say on a well-shaped
    // token.
    const shapeErr = credentialShapeError(botToken.trim(), appToken.trim())
    if (shapeErr) {
      setError(shapeErr)
      return
    }
    setConnecting(true)
    setError(null)
    try {
      const body: SlackImportBody = { bot_token: botToken.trim() }
      if (signingSecret.trim()) body.signing_secret = signingSecret.trim()
      if (appToken.trim()) body.app_token = appToken.trim()
      if (transport) body.transport = transport
      const conn = await importSlackConnection(orgId, body)
      setBotToken('')
      setSigningSecret('')
      setAppToken('')
      setTransport('')
      onConnected(conn)
    } catch (e) {
      setError(humanizeConnectError(httpErrorMessage(e, 'Could not connect the Slack workspace.')))
    } finally {
      setConnecting(false)
    }
  }

  return (
    <div className="space-y-4 rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/20 px-4 py-4">
      {onCollapse && (
        <div className="flex items-center justify-between">
          <span className="text-ui font-medium text-ink-2">{title}</span>
          <button
            type="button"
            onClick={onCollapse}
            aria-label="Collapse"
            className="inline-flex shrink-0 items-center rounded-full p-1 text-ink-3 transition-colors hover:text-ink-2"
          >
            <X size={14} />
          </button>
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <a
          href={SLACK_APP_CREATE_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1.5 rounded-full border border-warm/20 px-3.5 py-1.5 text-ui font-medium text-warm transition-colors hover:border-warm/30 hover:text-warm/80"
        >
          Create a Slack app <ExternalLink size={12} />
        </a>
        <CopyManifestButton orgId={orgId} transport="socket" label="Copy manifest (Socket Mode)" />
        <CopyManifestButton
          orgId={orgId}
          transport="events_api"
          label="Copy manifest (Events API)"
        />
      </div>
      <p className="text-reported leading-relaxed text-ink-3">
        Two ways to receive events: <strong className="font-medium text-ink-2">Socket Mode</strong>{' '}
        works anywhere — localhost, behind a firewall, no public URL — and is the usual choice;{' '}
        <strong className="font-medium text-ink-2">Events API</strong> needs a public HTTPS URL
        Slack can reach. Copy the matching manifest into &ldquo;Create an app from a
        manifest,&rdquo; install it to your workspace, then paste the credentials below — hover a{' '}
        <HelpCircle size={11} className="inline align-[-1px] text-ink-3" /> for where each lives in
        Slack.
      </p>

      <label className="block space-y-2">
        <span className="flex items-center gap-1.5 text-reported font-medium uppercase tracking-wide text-ink-3">
          Bot token
          <FieldHelp>
            Slack app → <strong>OAuth &amp; Permissions</strong> →{' '}
            <strong>Bot User OAuth Token</strong> (starts <code>xoxb-</code>). Blank until you
            install the app to a workspace.
          </FieldHelp>
        </span>
        <input
          type="password"
          value={botToken}
          autoComplete="off"
          placeholder="xoxb-…"
          onChange={(e) => setBotToken(e.target.value)}
          className={glassInputClass}
        />
      </label>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <label className="block space-y-2">
          <span className="flex items-center gap-1.5 text-reported font-medium uppercase tracking-wide text-ink-3">
            Signing secret <span className="normal-case text-ink-3/70">(Events API)</span>
            <FieldHelp>
              Slack app → <strong>Basic Information</strong> → <strong>App Credentials</strong> →{' '}
              <strong>Signing Secret</strong>. Only for Events API — leave blank for Socket Mode.
            </FieldHelp>
          </span>
          <input
            type="password"
            value={signingSecret}
            autoComplete="off"
            placeholder="Leave blank to keep current"
            onChange={(e) => setSigningSecret(e.target.value)}
            className={glassInputClass}
          />
        </label>
        <label className="block space-y-2">
          <span className="flex items-center gap-1.5 text-reported font-medium uppercase tracking-wide text-ink-3">
            App-level token <span className="normal-case text-ink-3/70">(Socket Mode)</span>
            <FieldHelp>
              Slack app → <strong>Basic Information</strong> → <strong>App-Level Tokens</strong> →
              Generate Token &amp; Scopes with the <code>connections:write</code> scope (starts{' '}
              <code>xapp-</code>). Also flip on <strong>Settings → Socket Mode</strong>.
            </FieldHelp>
          </span>
          <input
            type="password"
            value={appToken}
            autoComplete="off"
            placeholder="xapp-… — leave blank to keep current"
            onChange={(e) => setAppToken(e.target.value)}
            className={glassInputClass}
          />
        </label>
      </div>

      {needsTransportChoice && (
        <label className="block space-y-2">
          <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
            Both credentials supplied — which transport?
          </span>
          <div className="flex gap-2">
            {(['socket', 'events_api'] as const).map((t) => (
              <button
                key={t}
                type="button"
                onClick={() => setTransport(t)}
                className={`rounded-full border px-3.5 py-1.5 text-ui font-medium transition-colors ${
                  transport === t
                    ? 'border-warm/40 bg-warm/10 text-warm'
                    : 'border-[var(--color-line-1)] text-ink-2 hover:text-ink-1'
                }`}
              >
                {t === 'socket' ? 'Socket Mode' : 'Events API'}
              </button>
            ))}
          </div>
        </label>
      )}

      {error && (
        <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
          {error}
        </p>
      )}

      <button
        type="button"
        onClick={() => void connect()}
        disabled={!canSubmit}
        className={primaryButtonClass}
      >
        {connecting ? 'Connecting…' : 'Connect workspace'}
      </button>
    </div>
  )
}

function CopyManifestButton({
  orgId,
  transport,
  label,
}: {
  orgId: string
  transport: 'socket' | 'events_api'
  label: string
}) {
  const [copied, setCopied] = useState(false)
  const [busy, setBusy] = useState(false)

  const copy = async () => {
    if (busy) return
    setBusy(true)
    try {
      const text = await fetchSlackManifest(orgId, transport)
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch (e) {
      toast.failed(e, 'Could not build the manifest.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <button
      type="button"
      onClick={() => void copy()}
      disabled={busy}
      className="inline-flex items-center gap-1.5 rounded-full border border-[var(--color-line-1)] px-3.5 py-1.5 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1 disabled:opacity-40"
    >
      {copied ? <Check size={12} /> : <Copy size={12} />}
      {copied ? 'Copied' : label}
    </button>
  )
}
