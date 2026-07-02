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
import { Check, Copy, ExternalLink, Trash2 } from 'lucide-react'
import { apiFetch, apiJSON, httpErrorMessage } from '../../lib/apiClient'
import { toast } from '../../components/Toast/toastStore'
import { glassInputClass } from './primitives'

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
}

// rowKey is the composite (workspace, app) identity a row is now keyed on
// (TFAC-533) — a workspace may host several connected apps, so workspace_id
// alone can no longer disambiguate list/upsert/remove operations.
const rowKey = (ws: Pick<SlackWorkspace, 'workspace_id' | 'api_app_id'>) =>
  `${ws.workspace_id}:${ws.api_app_id}`

const SLACK_APP_CREATE_URL = 'https://api.slack.com/apps?new_app=1'

export default function SlackWorkspacesCard({ orgId }: { orgId: string }) {
  const [workspaces, setWorkspaces] = useState<SlackWorkspace[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    apiJSON<SlackWorkspace[]>('/api/slack/workspaces')
      .then((ws) => {
        if (!cancelled) setWorkspaces(ws)
      })
      .catch((e) => {
        if (!cancelled)
          setLoadError(httpErrorMessage(e, 'Could not load connected Slack workspaces.'))
      })
    return () => {
      cancelled = true
    }
  }, [orgId])

  const removeFromList = (key: string) =>
    setWorkspaces((ws) => (ws ?? []).filter((w) => rowKey(w) !== key))

  const upsertInList = (ws: SlackWorkspace) =>
    setWorkspaces((list) => [...(list ?? []).filter((w) => rowKey(w) !== rowKey(ws)), ws])

  return (
    <div className="space-y-5">
      <p className="text-[13px] leading-relaxed text-text-tertiary">
        Connect one or more Slack workspaces so Triage Factory can respond to @mentions. Slack has
        no one-click install for a custom app — create the app from the manifest below, install it
        to your workspace, then paste the resulting credentials here.
      </p>

      {loadError && (
        <p role="alert" className="text-[12px] leading-relaxed text-[var(--color-dismiss)]">
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

      <ConnectFlow onConnected={upsertInList} />
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
      toast.success(`${label} disconnected`)
      onRemoved()
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not disconnect the workspace.'))
      setBusy(false)
    }
  }

  return (
    <li className="flex items-center justify-between gap-3 rounded-2xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/40 px-4 py-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="truncate text-[13px] font-medium text-text-primary">
            {workspace.workspace_name || workspace.workspace_id}
          </span>
          <TransportChip transport={workspace.transport} />
        </div>
        <p className="mt-0.5 truncate text-[11px] text-text-tertiary">
          {workspace.workspace_id}
          {workspace.enterprise_id ? ' · Enterprise Grid' : ''}
          <span className="ml-1.5 rounded bg-black/[0.05] px-1 py-px font-mono text-[10px] text-text-tertiary">
            {workspace.api_app_id}
          </span>
        </p>
      </div>
      <button
        type="button"
        onClick={() => void disconnect()}
        disabled={busy}
        aria-label={`Disconnect ${workspace.workspace_name || workspace.workspace_id}`}
        className="inline-flex shrink-0 items-center rounded-full p-1.5 text-text-tertiary transition-colors hover:text-dismiss disabled:opacity-40"
      >
        <Trash2 size={14} />
      </button>
    </li>
  )
}

function TransportChip({ transport }: { transport: 'socket' | 'events_api' }) {
  return (
    <span className="inline-flex shrink-0 items-center rounded-full bg-black/[0.05] px-2 py-0.5 text-[11px] font-medium text-text-tertiary">
      {transport === 'socket' ? 'Socket Mode' : 'Events API'}
    </span>
  )
}

// ConnectFlow is the standing "connect another workspace" form: the deep
// link + manifest copy buttons, then the paste-back credential fields.
// Fields carry the "leave blank to keep current" convention for re-submits
// (re-pasting the same bot token to update signing_secret/app_token without
// retyping the one that isn't changing).
function ConnectFlow({ onConnected }: { onConnected: (ws: SlackWorkspace) => void }) {
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
      onConnected(ws)
      setBotToken('')
      setSigningSecret('')
      setAppToken('')
      setTransport('')
      toast.success(`Connected ${ws.workspace_name || ws.workspace_id}`)
    } catch (e) {
      // 409 "this workspace is already connected" and the transport/auth.test
      // 400s surface verbatim.
      setError(httpErrorMessage(e, 'Could not connect the Slack workspace.'))
    } finally {
      setConnecting(false)
    }
  }

  return (
    <div className="space-y-4 rounded-2xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/20 px-4 py-4">
      <div className="flex flex-wrap items-center gap-2">
        <a
          href={SLACK_APP_CREATE_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1.5 rounded-full border border-accent/20 px-3.5 py-1.5 text-[12px] font-medium text-accent transition-colors hover:border-accent/30 hover:text-accent/80"
        >
          Create a Slack app <ExternalLink size={12} />
        </a>
        <CopyManifestButton transport="socket" label="Copy manifest (Socket Mode)" />
        <CopyManifestButton transport="events_api" label="Copy manifest (Events API)" />
      </div>
      <p className="text-[11px] leading-relaxed text-text-tertiary">
        Create the app, paste in a copied manifest under &ldquo;Create an app from a
        manifest&rdquo;, install it to your workspace, then paste the credentials it gives you
        below.
      </p>

      <label className="block space-y-2">
        <span className="block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
          Bot token
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
          <span className="block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
            Signing secret <span className="normal-case text-text-tertiary/70">(Events API)</span>
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
          <span className="block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
            App-level token <span className="normal-case text-text-tertiary/70">(Socket Mode)</span>
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
          <span className="block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
            Both credentials supplied — which transport?
          </span>
          <div className="flex gap-2">
            {(['socket', 'events_api'] as const).map((t) => (
              <button
                key={t}
                type="button"
                onClick={() => setTransport(t)}
                className={`rounded-full border px-3.5 py-1.5 text-[12px] font-medium transition-colors ${
                  transport === t
                    ? 'border-accent/40 bg-accent/10 text-accent'
                    : 'border-[var(--color-border-glass)] text-text-secondary hover:text-text-primary'
                }`}
              >
                {t === 'socket' ? 'Socket Mode' : 'Events API'}
              </button>
            ))}
          </div>
        </label>
      )}

      {error && (
        <p role="alert" className="text-[12px] leading-relaxed text-[var(--color-dismiss)]">
          {error}
        </p>
      )}

      <button
        type="button"
        onClick={() => void connect()}
        disabled={!canSubmit}
        className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white shadow-[0_10px_28px_-10px_var(--color-accent)] transition-all hover:bg-accent/90 disabled:opacity-40 disabled:shadow-none"
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
      className="inline-flex items-center gap-1.5 rounded-full border border-[var(--color-border-glass)] px-3.5 py-1.5 text-[12px] font-medium text-text-secondary transition-colors hover:text-text-primary disabled:opacity-40"
    >
      {copied ? <Check size={12} /> : <Copy size={12} />}
      {copied ? 'Copied' : label}
    </button>
  )
}
