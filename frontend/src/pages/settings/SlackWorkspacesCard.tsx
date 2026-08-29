// SlackWorkspacesCard — the org-admin "Connect Slack" surface (TFAC-529),
// modeled on AtlassianOAuthAppCard: an own card, self-contained (owns its
// list fetch, the connect draft, and the disconnect action — no shared Save
// footer). An org may connect several workspaces, so unlike the Atlassian
// card this renders a list PLUS a standing connect form rather than a single
// override slot.
//
// Slack has no GitHub-style manifest-redirect handoff, so the connect UX is
// deep link + copy-paste manifest + paste-back credentials: open Slack's app
// creation page, paste the copied manifest to scaffold the app, install it
// to the workspace, then paste the resulting bot token (and, depending on
// transport, the signing secret or the app-level token) back here. Transport
// is inferred server-side from which credentials are supplied — see
// ee/slack's inferTransport — so this form never asks the admin to choose a
// transport up front; it only surfaces the choice when both a signing
// secret and an app-level token are supplied (the one genuinely ambiguous
// case) and the server 400s.

import { useEffect, useState } from 'react'
import { Tooltip } from '../../ui/tooltip/Tooltip'
import { Check, Copy, ExternalLink, HelpCircle, Plus, Trash2, X } from 'lucide-react'
import { apiFetch, apiJSON, httpErrorMessage } from '../../lib/apiClient'
import { invalidateEventSources } from '../../hooks/useEventSources'
import { toast } from '../../components/Toast/toastStore'
import { glassInputClass } from './primitives'

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

// SlackConnectionStatus is one app's live Socket Mode connection status
// (TFAC-534) — present only when the backend's socket manager has a
// connection running for this row's api_app_id (rows sharing an app share
// the same connection). Absent for a webhook-only app, an unentitled org,
// or before the reconciler's first pass — rendered as "n/a".
interface SlackConnectionStatus {
  state: 'dialing' | 'open' | 'draining' | 'backing_off' | 'auth_failed'
  connected_at?: string
  last_event_at?: string
  consecutive_failures: number
  last_error?: string
}

interface SlackWorkspace {
  workspace_id: string
  api_app_id: string
  workspace_name: string
  enterprise_id?: string
  transport: 'socket' | 'events_api'
  bot_user_id: string
  registered_by_user_id?: string
  created_at: string
  updated_at: string
  connection?: SlackConnectionStatus
}

// rowKey is the composite (workspace, app) identity a row is now keyed on —
// a workspace may host several connected apps, so workspace_id alone can no
// longer disambiguate list/upsert/remove operations.
const rowKey = (ws: Pick<SlackWorkspace, 'workspace_id' | 'api_app_id'>) =>
  `${ws.workspace_id}:${ws.api_app_id}`

const SLACK_APP_CREATE_URL = 'https://api.slack.com/apps?new_app=1'

export default function SlackWorkspacesCard({ orgId }: { orgId: string }) {
  const [workspaces, setWorkspaces] = useState<SlackWorkspace[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    const load = (initial: boolean) => {
      apiJSON<SlackWorkspace[]>('/api/slack/workspaces')
        .then((ws) => {
          if (cancelled) return
          setWorkspaces(ws)
          // Clear a stale banner: a later poll succeeding means the list is
          // current, so an error from the initial (failed) load must not linger.
          setLoadError(null)
        })
        .catch((e) => {
          // Only the initial fetch surfaces a load error; a transient poll
          // failure must not blow away the list already on screen.
          if (!cancelled && initial)
            setLoadError(httpErrorMessage(e, 'Could not load connected Slack workspaces.'))
        })
    }
    load(true)
    // Poll so a row's live Socket Mode status resolves (Connecting… →
    // Connected, or a reconnect) without a manual reload — the connection
    // state lives in the backend socket manager, not the websocket stream.
    const timer = setInterval(() => load(false), 5000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [orgId])

  const removeFromList = (key: string) =>
    setWorkspaces((ws) => (ws ?? []).filter((w) => rowKey(w) !== key))

  const upsertInList = (ws: SlackWorkspace) =>
    setWorkspaces((list) => [...(list ?? []).filter((w) => rowKey(w) !== rowKey(ws)), ws])

  return (
    <div className="space-y-5">
      <p className="text-body leading-relaxed text-ink-3">
        Connect one or more Slack workspaces so Triage Factory can respond to @mentions. Slack has
        no one-click install for a custom app — create the app from the manifest below, install it
        to your workspace, then paste the resulting credentials here.
      </p>

      {loadError && (
        <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
          {loadError}
        </p>
      )}

      {workspaces && workspaces.length > 0 && (
        <ul className="space-y-2">
          {workspaces.map((ws) => (
            <WorkspaceRow
              key={rowKey(ws)}
              workspace={ws}
              onRemoved={() => removeFromList(rowKey(ws))}
            />
          ))}
        </ul>
      )}

      <AddWorkspaceSection
        hasWorkspaces={!!workspaces && workspaces.length > 0}
        onConnected={upsertInList}
      />
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
        className={`overflow-hidden ${open ? 'visible' : 'invisible'}`}
        style={{ transition: `visibility 0s linear ${open ? '0s' : `${COLLAPSE_MS}ms`}` }}
      >
        {children}
      </div>
    </div>
  )
}

// AddWorkspaceSection decides how the connect form is presented. Before the
// first workspace is connected the form is the primary action and stays open.
// Once at least one workspace exists, connecting another is the rare case, so
// the form collapses behind a single "Add another workspace" row and expands
// back — with a close affordance — on demand.
function AddWorkspaceSection({
  hasWorkspaces,
  onConnected,
}: {
  hasWorkspaces: boolean
  onConnected: (ws: SlackWorkspace) => void
}) {
  const [expanded, setExpanded] = useState(false)

  // A successful connect from the "add another" flow folds the form back up so
  // the newly connected workspace lands in the list above, not under an open
  // form the user has to dismiss.
  const handleConnected = (ws: SlackWorkspace) => {
    onConnected(ws)
    setExpanded(false)
  }

  // First connect: no collapse chrome, the form is simply the content.
  if (!hasWorkspaces) {
    return <ConnectFlow onConnected={onConnected} />
  }

  return (
    <div>
      {/* The trigger and the form are two halves of one control: whichever
          isn't showing collapses to height 0 and (via Collapsible) visibility
          hidden, so exactly one is interactive at a time and they cross-fade
          rather than pop. */}
      <Collapsible open={!expanded}>
        <button
          type="button"
          onClick={() => setExpanded(true)}
          aria-expanded={expanded}
          className="flex w-full items-center justify-center gap-1.5 rounded-2xl border border-dashed border-[var(--color-line-1)] px-4 py-3 text-ui font-medium text-ink-2 transition-colors hover:border-warm/30 hover:text-ink-1"
        >
          <Plus size={14} />
          Add another workspace
        </button>
      </Collapsible>

      <Collapsible open={expanded}>
        <ConnectFlow onConnected={handleConnected} onCollapse={() => setExpanded(false)} />
      </Collapsible>
    </div>
  )
}

function WorkspaceRow({
  workspace,
  onRemoved,
}: {
  workspace: SlackWorkspace
  onRemoved: () => void
}) {
  const [busy, setBusy] = useState(false)

  const disconnect = async () => {
    if (busy) return
    const label = workspace.workspace_name || workspace.workspace_id
    if (
      !window.confirm(
        `Disconnect ${label}? Triage Factory will stop receiving events from this workspace.`,
      )
    ) {
      return
    }
    setBusy(true)
    try {
      await apiFetch(
        `/api/slack/workspaces/${encodeURIComponent(workspace.workspace_id)}/${encodeURIComponent(workspace.api_app_id)}`,
        { method: 'DELETE' },
      )
      // The org's last workspace going away moves slack from available back to
      // unconfigured; drop the cached answer either way.
      invalidateEventSources()
      toast.success(`${label} disconnected`)
      onRemoved()
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not disconnect the workspace.'))
      setBusy(false)
    }
  }

  return (
    <li className="flex items-center justify-between gap-3 rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="truncate text-body font-medium text-ink-1">
            {workspace.workspace_name || workspace.workspace_id}
          </span>
          <TransportChip transport={workspace.transport} />
          <ConnectionStatusChip workspace={workspace} />
        </div>
        <p className="mt-0.5 truncate text-reported text-ink-3">
          {workspace.workspace_id}
          {workspace.enterprise_id ? ' · Enterprise Grid' : ''}
          <span className="ml-1.5 rounded bg-tint-3 px-1 py-px font-mono text-label text-ink-3">
            {workspace.api_app_id}
          </span>
        </p>
      </div>
      <button
        type="button"
        onClick={() => void disconnect()}
        disabled={busy}
        aria-label={`Disconnect ${workspace.workspace_name || workspace.workspace_id}`}
        className="inline-flex shrink-0 items-center rounded-full p-1.5 text-ink-3 transition-colors hover:text-alarm disabled:opacity-40"
      >
        <Trash2 size={14} />
      </button>
    </li>
  )
}

// CONNECTION_STATUS_DISPLAY maps the backend's connection.state (TFAC-534)
// to the chip's label/dot color. "draining" (the brief moment between a
// disconnect frame and redialing) folds into "Reconnecting…" — it's not
// worth a distinct label for something that resolves in milliseconds.
const CONNECTION_STATUS_DISPLAY: Record<
  SlackConnectionStatus['state'],
  { label: string; dot: string; text: string }
> = {
  open: { label: 'Connected', dot: 'bg-ink-1', text: 'text-ink-1' },
  dialing: { label: 'Connecting…', dot: 'bg-warm', text: 'text-warm' },
  draining: { label: 'Reconnecting…', dot: 'bg-warm', text: 'text-warm' },
  backing_off: { label: 'Reconnecting…', dot: 'bg-warm', text: 'text-warm' },
  auth_failed: { label: 'Auth failed', dot: 'bg-alarm', text: 'text-alarm' },
}

// ConnectionStatusChip is the Socket Mode connection status indicator
// (TFAC-534) — minimal by design: a colored dot plus a short label, with
// the last error (if any) available on hover via title. Rows sharing an
// app report the same connection object. No connection object means one of
// two things: a webhook-only row (this app never gets a socket — renders
// the muted "n/a" chip), or a socket-transport row the reconciler hasn't
// started yet (a brief window right after connect, or before the first
// tick — renders "Connecting…", same as an in-progress dial).
function ConnectionStatusChip({ workspace }: { workspace: SlackWorkspace }) {
  const status = workspace.connection
  if (!status) {
    if (workspace.transport === 'socket') {
      return <StatusDot label="Connecting…" dot="bg-warm" text="text-warm" />
    }
    return <StatusDot label="n/a" dot="bg-ink-3/40" text="text-ink-3" />
  }
  const display = CONNECTION_STATUS_DISPLAY[status.state] ?? CONNECTION_STATUS_DISPLAY.dialing
  return (
    <StatusDot
      label={display.label}
      dot={display.dot}
      text={display.text}
      title={status.last_error || undefined}
    />
  )
}

// StatusDot carries its own `title` rather than being wrapped in an outer span:
// a plain inline wrapper would sit on the text baseline and nudge the pill a
// hair below the sibling TransportChip, so keeping every chip a single
// inline-flex flex item is what lets the flex row's items-center line them up.
function StatusDot({
  label,
  dot,
  text,
  title,
}: {
  label: string
  dot: string
  text: string
  title?: string
}) {
  return (
    <span
      title={title}
      className={`inline-flex shrink-0 items-center gap-1 rounded-full bg-tint-3 px-2 py-0.5 text-reported font-medium ${text}`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${dot}`} />
      {label}
    </span>
  )
}

function TransportChip({ transport }: { transport: 'socket' | 'events_api' }) {
  return (
    <span className="inline-flex shrink-0 items-center rounded-full bg-tint-3 px-2 py-0.5 text-reported font-medium text-ink-3">
      {transport === 'socket' ? 'Socket Mode' : 'Events API'}
    </span>
  )
}

// credentialShapeError catches the two credential mix-ups that otherwise fail
// deep in Slack's API with an opaque message. Returns a user-facing hint that
// names the Slack page to copy from, or null when the shapes look right.
function credentialShapeError(botToken: string, appToken: string): string | null {
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

// ConnectFlow is the standing "connect another workspace" form: the deep
// link + manifest copy buttons, then the paste-back credential fields.
// Fields carry the "leave blank to keep current" convention for re-submits
// (re-pasting the same bot token to update signing_secret/app_token without
// retyping the one that isn't changing).
function ConnectFlow({
  onConnected,
  onCollapse,
}: {
  onConnected: (ws: SlackWorkspace) => void
  // Present only when the form is a collapsible "add another" panel; renders
  // the close affordance that folds it back to the trigger row.
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
    // Catch the two credential mix-ups locally, with a message that names the
    // page to copy from — otherwise they fail deep in Slack's API with an
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
      const body: Record<string, string> = { bot_token: botToken.trim() }
      if (signingSecret.trim()) body.signing_secret = signingSecret.trim()
      if (appToken.trim()) body.app_token = appToken.trim()
      if (transport) body.transport = transport
      const ws = await apiJSON<SlackWorkspace>('/api/slack/workspaces', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      invalidateEventSources()
      onConnected(ws)
      setBotToken('')
      setSigningSecret('')
      setAppToken('')
      setTransport('')
      toast.success(`Connected ${ws.workspace_name || ws.workspace_id}`)
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
          <span className="text-ui font-medium text-ink-2">Add another workspace</span>
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
        <CopyManifestButton transport="socket" label="Copy manifest (Socket Mode)" />
        <CopyManifestButton transport="events_api" label="Copy manifest (Events API)" />
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
        className="rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90 disabled:opacity-40 disabled:shadow-none"
      >
        {connecting ? 'Connecting…' : 'Connect workspace'}
      </button>
    </div>
  )
}

function CopyManifestButton({
  transport,
  label,
}: {
  transport: 'socket' | 'events_api'
  label: string
}) {
  const [copied, setCopied] = useState(false)
  const [busy, setBusy] = useState(false)

  const copy = async () => {
    if (busy) return
    setBusy(true)
    try {
      const resp = await apiFetch(`/api/slack/manifest?transport=${transport}`)
      const text = await resp.text()
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not build the manifest.'))
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
