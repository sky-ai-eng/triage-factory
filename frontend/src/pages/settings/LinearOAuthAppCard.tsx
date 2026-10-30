// LinearOAuthAppCard is the org's own Linear OAuth app — the Linear sibling of
// AtlassianOAuthAppCard. Linear's app-creation page takes a pre-filled form, so
// the card links there with this deployment's name, URL and redirect URIs
// already filled in; Linear does not hand the credentials back, so the admin
// pastes the client ID and secret from the created app's page. With the app
// saved, a Linear workspace admin can install Triage Factory from the Linear
// connection section. When the deployment has a Linear app of its own, Install
// already works and an app of the org's own is an optional override, so the
// card says so and keeps the form behind a button, with no creation link.
// `openForm` skips that button for a caller whose own button was already the
// deliberate click (the setup wizard's "Set up your own Linear app").
//
// Self-contained (an action section, no Save footer): it owns its status fetch,
// the draft fields, the summary and the remove. `onStatus` tells the parent
// whether an install is now available, so the connection section can offer it
// without a reload.

import { useEffect, useRef, useState } from 'react'
import { Check, Copy, ExternalLink } from 'lucide-react'
import { toast } from '../../components/Toast/toastStore'
import { glassInputClass } from './primitives'
import {
  deleteLinearApp,
  getLinearAppStatus,
  importLinearApp,
  type LinearAppStatus,
} from '../../lib/linearApp'

function CopyField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      clearTimeout(timer.current)
      timer.current = setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard denied — the value is still selectable in the field.
    }
  }
  return (
    <div className="flex items-center gap-2">
      <input
        type="text"
        readOnly
        aria-label={label}
        value={value}
        onFocus={(e) => e.target.select()}
        className={`${glassInputClass} font-mono text-ui`}
      />
      <button
        type="button"
        onClick={() => void copy()}
        aria-label={`Copy ${label}`}
        className="inline-flex shrink-0 items-center gap-1 rounded-xl border border-[var(--color-line-1)] px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1"
      >
        {copied ? <Check size={13} /> : <Copy size={13} />}
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}

export default function LinearOAuthAppCard({
  orgId,
  onStatus,
  openForm = false,
}: {
  orgId: string
  onStatus?: (status: LinearAppStatus) => void
  openForm?: boolean
}) {
  const [status, setStatusState] = useState<LinearAppStatus | null>(null)
  const [clientId, setClientId] = useState('')
  const [clientSecret, setClientSecret] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [replacing, setReplacing] = useState(false)
  const [registering, setRegistering] = useState(false)
  const [loadFailed, setLoadFailed] = useState(false)

  const setStatus = (s: LinearAppStatus) => {
    setStatusState(s)
    onStatus?.(s)
  }

  useEffect(() => {
    let cancelled = false
    getLinearAppStatus(orgId)
      .then((s) => {
        if (cancelled) return
        setStatusState(s)
        onStatus?.(s)
      })
      .catch(() => {
        // Saving still works, but the card cannot tell whether an app is
        // already saved, so it says so rather than look like there is none.
        if (!cancelled) setLoadFailed(true)
      })
    return () => {
      cancelled = true
    }
    // onStatus is a notification, not an input: re-running the read when the
    // parent re-renders with a new closure would refetch for nothing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgId])

  const idTrim = clientId.trim()
  const secretTrim = clientSecret.trim()
  const canSubmit = idTrim !== '' && secretTrim !== '' && !busy

  // Enter in either field saves the app. It stops there: the setup wizard
  // listens for Enter to Continue, which would otherwise fire from these
  // fields too.
  const onFieldKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
    e.preventDefault()
    e.stopPropagation()
    void submit()
  }

  const submit = async () => {
    if (!canSubmit) return
    setBusy(true)
    setError(null)
    const outcome = await importLinearApp(orgId, { client_id: idTrim, client_secret: secretTrim })
    setBusy(false)
    if (outcome.ok) {
      setStatus(outcome.result)
      setClientId('')
      setClientSecret('')
      setReplacing(false)
      setRegistering(false)
      toast.success('Linear app saved')
      return
    }
    setError(outcome.error)
  }

  const remove = async () => {
    if (busy) return
    if (
      !window.confirm(
        'Remove this Linear OAuth app? Installing Triage Factory in Linear will stop being available unless a deployment app covers your organization.',
      )
    ) {
      return
    }
    setBusy(true)
    setError(null)
    try {
      setStatus(await deleteLinearApp(orgId))
      toast.success('Linear app removed')
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to remove the Linear app.')
    } finally {
      setBusy(false)
    }
  }

  const hasOverride = !!status?.app
  const onDeploymentApp = !hasOverride && !!status?.using_deployment_default
  const showForm = replacing || (!hasOverride && (!onDeploymentApp || registering || openForm))
  const redirectURIs = status?.redirect_uris ?? []

  return (
    <div className="space-y-5">
      {!onDeploymentApp && (
        <p className="text-body leading-relaxed text-ink-3">
          Register a Linear OAuth app so a Linear workspace admin can install Triage Factory as an
          app. Triage Factory then works in Linear as its own app user instead of through
          someone&rsquo;s personal API key, and the app user takes no seat. The secret is stored
          encrypted and never leaves your deployment.
        </p>
      )}

      {loadFailed && (
        <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
          Could not load this organization&rsquo;s Linear app, so an app may already be saved.
          Reload to try again.
        </p>
      )}

      {hasOverride && (
        <div className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3 text-ui text-ink-2">
          <div className="flex items-center gap-2">
            <Check size={14} className="text-warm" />
            <span className="font-medium text-ink-1">Linear app configured</span>
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
      {onDeploymentApp && (
        <div className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3 text-ui text-ink-2">
          Using this deployment&rsquo;s Triage Factory app, so a Linear workspace admin can install
          Triage Factory without registering one.{' '}
          {showForm
            ? 'Register your own below only if you need to'
            : 'Use your own Linear app only if you need one'}
          ; it then replaces the deployment&rsquo;s app for this organization.
        </div>
      )}
      {onDeploymentApp && !showForm && (
        <button
          type="button"
          onClick={() => setRegistering(true)}
          className="rounded-xl border border-[var(--color-line-1)] px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:text-ink-1"
        >
          Use your own Linear app instead…
        </button>
      )}

      {showForm && (
        <>
          {status?.create_url && (
            <div className="space-y-1.5">
              <a
                href={status.create_url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1.5 rounded-xl border border-[var(--color-line-1)] px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:text-ink-1"
              >
                Create the app in Linear
                <ExternalLink size={13} />
              </a>
              <p className="text-reported leading-relaxed text-ink-3">
                Opens Linear&rsquo;s new-application page with the name, URL and redirect URIs
                filled in. Create it, then copy its client ID and client secret here.
              </p>
            </div>
          )}

          <label className="block space-y-2">
            <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
              Client ID
            </span>
            <input
              type="text"
              value={clientId}
              autoComplete="off"
              placeholder="Your Linear OAuth app's client ID"
              onChange={(e) => setClientId(e.target.value)}
              onKeyDown={onFieldKeyDown}
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
              placeholder="Your Linear OAuth app's client secret"
              onChange={(e) => setClientSecret(e.target.value)}
              onKeyDown={onFieldKeyDown}
              className={glassInputClass}
            />
          </label>

          {redirectURIs.length > 0 && (
            <div className="space-y-1.5">
              <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
                Redirect URIs to register on the app
              </span>
              {redirectURIs.map((uri, i) => (
                <CopyField key={uri} label={`redirect URI ${i + 1}`} value={uri} />
              ))}
              <p className="text-reported leading-relaxed text-ink-3">
                {status?.create_url
                  ? 'The link above registers both. If you create the app by hand, add each one under its Callback URLs — Linear matches them exactly.'
                  : 'Add each one under the app’s Callback URLs — Linear matches them exactly.'}{' '}
                The second is for members connecting their own Linear accounts.
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
            {((hasOverride && replacing) || (registering && !openForm)) && (
              <button
                type="button"
                onClick={() => {
                  setReplacing(false)
                  setRegistering(false)
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
        <>
          {error && (
            <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
              {error}
            </p>
          )}
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
        </>
      )}
    </div>
  )
}
