// AtlassianOAuthAppCard is the bring-your-own Atlassian OAuth app surface — the
// Jira sibling of GitHubAppImportForm. An admin enters an Atlassian OAuth app's
// client_id + client_secret (the credentials the per-user "Connect Jira" flow
// runs against) and registers the callback URL the card shows on their app. It
// mirrors the GitHub card but is far simpler: an Atlassian OAuth app has no
// App ID, no PEM, and no permission preflight, only the OAuth client pair.
//
// Self-contained (an action section, no Save footer): it owns its status fetch,
// the draft fields, the connected/override summary, and the remove action, so
// the parent only drops it in. It re-fetches status on mount and reflects the
// store/remove outcome inline.

import { useEffect, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { toast } from '../../components/Toast/toastStore'
import { glassInputClass } from './primitives'
import {
  getJiraAppStatus,
  importJiraApp,
  deleteJiraApp,
  type JiraAppStatus,
} from '../../lib/jiraApp'

export default function AtlassianOAuthAppCard({ orgId }: { orgId: string }) {
  const [status, setStatus] = useState<JiraAppStatus | null>(null)
  const [clientId, setClientId] = useState('')
  const [clientSecret, setClientSecret] = useState('')
  const [copied, setCopied] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Reveal the entry form even when an override is already set, so an admin can
  // replace the credentials in place.
  const [replacing, setReplacing] = useState(false)

  useEffect(() => {
    let cancelled = false
    getJiraAppStatus(orgId)
      .then((s) => {
        if (!cancelled) setStatus(s)
      })
      .catch(() => {
        // A failed status read leaves the card in its entry state — the import
        // still works; only the summary + callback URL are unavailable.
      })
    return () => {
      cancelled = true
    }
  }, [orgId])

  const callbackUrl = status?.connect_callback_url ?? ''
  const copyCallback = async () => {
    if (!callbackUrl) return
    try {
      await navigator.clipboard.writeText(callbackUrl)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard denied — the URL is still selectable in the field.
    }
  }

  const idTrim = clientId.trim()
  const secretTrim = clientSecret.trim()
  const canSubmit = idTrim !== '' && secretTrim !== '' && !busy

  const submit = async () => {
    if (!canSubmit) return
    setBusy(true)
    setError(null)
    const outcome = await importJiraApp(orgId, { client_id: idTrim, client_secret: secretTrim })
    setBusy(false)
    if (outcome.ok) {
      setStatus(outcome.result)
      setClientId('')
      setClientSecret('')
      setReplacing(false)
      toast.success('Atlassian app saved')
      return
    }
    setError(outcome.error)
  }

  const remove = async () => {
    if (busy) return
    if (
      !window.confirm(
        'Remove this Atlassian OAuth app? The one-click Connect option will stop working unless a deployment app covers your workspace.',
      )
    ) {
      return
    }
    setBusy(true)
    setError(null)
    try {
      const next = await deleteJiraApp(orgId)
      setStatus(next)
      toast.success('Atlassian app removed')
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to remove the Atlassian app.')
    } finally {
      setBusy(false)
    }
  }

  const hasOverride = !!status?.app
  const showForm = !hasOverride || replacing

  return (
    <div className="space-y-5">
      <p className="text-body leading-relaxed text-ink-3">
        Register an Atlassian OAuth app so teammates can connect their Jira account with one click
        instead of pasting an API token. Create a 3LO app in the{' '}
        <span className="font-medium text-ink-2">Atlassian developer console</span>, then paste its
        client ID and secret here. The secret is stored encrypted and never leaves your deployment.
      </p>

      {/* Current state: a per-org override, the deployment default, or nothing. */}
      {hasOverride && (
        <div className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3 text-ui text-ink-2">
          <div className="flex items-center gap-2">
            <Check size={14} className="text-warm" />
            <span className="font-medium text-ink-1">Atlassian app configured</span>
          </div>
          <p className="mt-1 break-all font-mono text-reported text-ink-3">
            {status?.app?.client_id}
          </p>
          {status?.app?.registered_by_display_name && (
            <p className="mt-1 text-reported text-ink-3">
              Added by {status.app.registered_by_display_name}
            </p>
          )}
        </div>
      )}
      {!hasOverride && status?.using_deployment_default && (
        <div className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3 text-ui text-ink-2">
          Using this deployment's Atlassian app. Add your own below to override it for this
          workspace.
        </div>
      )}

      {showForm && (
        <>
          <label className="block space-y-2">
            <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
              Client ID
            </span>
            <input
              type="text"
              value={clientId}
              autoComplete="off"
              placeholder="Your Atlassian OAuth app's client ID"
              onChange={(e) => setClientId(e.target.value)}
              className={glassInputClass}
            />
          </label>

          <label className="block space-y-2">
            <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
              Client secret
            </span>
            <input
              type="password"
              value={clientSecret}
              autoComplete="off"
              placeholder="Your Atlassian OAuth app's client secret"
              onChange={(e) => setClientSecret(e.target.value)}
              className={glassInputClass}
            />
          </label>

          {callbackUrl && (
            <div className="space-y-1.5">
              <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
                Callback URL to register on the app
              </span>
              <div className="flex items-center gap-2">
                <input
                  type="text"
                  readOnly
                  value={callbackUrl}
                  onFocus={(e) => e.target.select()}
                  className={`${glassInputClass} font-mono text-ui`}
                />
                <button
                  type="button"
                  onClick={() => void copyCallback()}
                  aria-label="Copy callback URL"
                  className="inline-flex shrink-0 items-center gap-1 rounded-xl border border-[var(--color-line-1)] px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1"
                >
                  {copied ? <Check size={13} /> : <Copy size={13} />}
                  {copied ? 'Copied' : 'Copy'}
                </button>
              </div>
              <p className="text-reported leading-relaxed text-ink-3">
                Add this as a Callback URL (redirect URI) under your Atlassian app&rsquo;s
                Authorization settings — one-click Connect only works once it&rsquo;s registered
                there.
              </p>
            </div>
          )}

          {error && (
            <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
              {error}
            </p>
          )}

          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => void submit()}
              disabled={!canSubmit}
              className="rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90 disabled:opacity-40 disabled:shadow-none"
            >
              {busy ? 'Saving…' : hasOverride ? 'Replace app' : 'Save app'}
            </button>
            {hasOverride && replacing && (
              <button
                type="button"
                onClick={() => {
                  setReplacing(false)
                  setClientId('')
                  setClientSecret('')
                  setError(null)
                }}
                className="rounded-xl px-3 py-2 text-body font-medium text-ink-3 transition-colors hover:text-ink-2"
              >
                Cancel
              </button>
            )}
          </div>
        </>
      )}

      {hasOverride && !replacing && (
        <div className="flex items-center gap-3">
          <button
            type="button"
            onClick={() => setReplacing(true)}
            className="rounded-xl border border-[var(--color-line-1)] px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:text-ink-1"
          >
            Replace
          </button>
          <button
            type="button"
            onClick={() => void remove()}
            disabled={busy}
            className="rounded-xl px-3 py-2 text-body font-medium text-[var(--color-alarm)] transition-colors hover:opacity-80 disabled:opacity-40"
          >
            Remove
          </button>
        </div>
      )}
    </div>
  )
}
