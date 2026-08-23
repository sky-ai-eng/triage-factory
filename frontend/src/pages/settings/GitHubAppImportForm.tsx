// GitHubAppImportForm is the bring-your-own-App import surface: App ID + private
// key, an optional OAuth client secret, and (multi mode) an optional webhook
// secret. It validates server-side — an app-JWT GET /app round trip proves the
// ID+key pair and derives the App's slug/owner/permissions — then permission-
// preflights and persists through the same path the manifest flow uses.
//
// Self-contained: it owns all the transient import state (draft fields, the
// granted-vs-required permission table from a failed preflight, the soft-gap
// acknowledgment), so the wizard step and the Settings switch flow both drop it
// in and only react to onImported. The caller decides what to do on success
// (the wizard patches state + advances; Settings resumes its install→cutover
// flow).

import { useEffect, useState } from 'react'
import { ChevronDown, ChevronRight, Check, X, Copy, Upload } from 'lucide-react'
import { glassInputClass } from './primitives'
import {
  getGitHubAppStatus,
  importGitHubApp,
  type GitHubAppImportInput,
  type GitHubAppImportResult,
  type GitHubAppPermissionRow,
} from '../../lib/githubApp'

export default function GitHubAppImportForm({
  orgId,
  isLocal,
  onImported,
}: {
  orgId: string
  // Gates the webhook-secret field — multi mode receives deliveries (so a
  // webhook secret is meaningful); local is hookless (backfill discovery), so
  // the field is hidden there.
  isLocal: boolean
  // Called with the import result on success. The form has already persisted —
  // the caller reacts (patch wizard state + advance, or resume the switch flow).
  onImported: (result: GitHubAppImportResult) => void
}) {
  const [appId, setAppId] = useState('')
  const [pem, setPem] = useState('')
  const [oauthOpen, setOauthOpen] = useState(false)
  const [clientSecret, setClientSecret] = useState('')
  const [webhookSecret, setWebhookSecret] = useState('')
  // The Connect callback URL the App owner must register — derived from the org's
  // deployment, fetched from the status endpoint (which carries it even with no
  // App registered yet). Only shown inside the OAuth section.
  const [callbackUrl, setCallbackUrl] = useState('')
  const [copied, setCopied] = useState(false)
  // The granted-vs-required table from the last failed preflight (null until a
  // gap is hit). softGap=true means the gap is acknowledgeable (resubmit with
  // accept_partial); false alongside a non-null table means a hard, blocking gap.
  const [permissions, setPermissions] = useState<GitHubAppPermissionRow[] | null>(null)
  const [softGap, setSoftGap] = useState(false)
  const [ack, setAck] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    getGitHubAppStatus(orgId)
      .then((s) => {
        if (!cancelled) setCallbackUrl(s.connect_callback_url)
      })
      .catch(() => {
        // The callback URL is only needed to enable OAuth; a failed status read
        // leaves it blank and the OAuth section degrades to "no URL to show".
      })
    return () => {
      cancelled = true
    }
  }, [orgId])

  const onPemFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = () => setPem(typeof reader.result === 'string' ? reader.result : '')
    reader.readAsText(file)
    // Reset the input so re-picking the same file fires change again.
    e.target.value = ''
  }

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

  const idTrim = appId.trim()
  const pemTrim = pem.trim()
  const canSubmit = idTrim !== '' && pemTrim !== '' && !busy && !(softGap && !ack)

  const submit = async () => {
    if (idTrim === '' || pemTrim === '') {
      setError('Enter the App ID and paste the private key.')
      return
    }
    if (busy) return
    setBusy(true)
    setError(null)
    const input: GitHubAppImportInput = { app_id: idTrim, pem: pemTrim }
    const cs = clientSecret.trim()
    if (cs) input.client_secret = cs
    const wh = webhookSecret.trim()
    if (wh && !isLocal) input.webhook_secret = wh
    // accept_partial is only meaningful once the user has acknowledged a soft gap.
    if (softGap && ack) input.accept_partial = true

    const outcome = await importGitHubApp(orgId, input)
    setBusy(false)
    if (outcome.ok) {
      onImported(outcome.result)
      return
    }
    setError(outcome.error)
    if (outcome.permissions) {
      setPermissions(outcome.permissions)
      // blocking=false ⇒ a soft gap: show the ack checkbox. A hard gap (blocking)
      // can't be acknowledged past — clear any prior ack.
      setSoftGap(outcome.blocking === false)
      if (outcome.blocking) setAck(false)
    } else {
      // A field-level rejection (bad PEM / app-id mismatch / bad client secret) —
      // drop any stale permission table so only the inline error shows.
      setPermissions(null)
      setSoftGap(false)
    }
  }

  return (
    <div className="space-y-5">
      <p className="text-body leading-relaxed text-ink-3">
        Connect a GitHub App you already have. Supply its App ID and a private key (.pem) — Triage
        Factory validates the pair against GitHub and reads the App&rsquo;s permissions itself, so
        you don&rsquo;t need to be the App&rsquo;s owner or a GitHub org owner.
      </p>

      <label className="block space-y-2">
        <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
          App ID
        </span>
        <input
          type="text"
          inputMode="numeric"
          value={appId}
          placeholder="123456"
          onChange={(e) => setAppId(e.target.value)}
          className={glassInputClass}
        />
        <span className="block text-reported leading-relaxed text-ink-3">
          The numeric App ID from the App&rsquo;s GitHub settings (not the client ID).
        </span>
      </label>

      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <span className="text-reported font-medium uppercase tracking-wide text-ink-3">
            Private key (PEM)
          </span>
          <label className="inline-flex cursor-pointer items-center gap-1.5 text-reported font-medium text-warm transition-colors hover:text-warm/80">
            <Upload size={12} />
            Upload .pem
            <input type="file" accept=".pem" className="hidden" onChange={onPemFile} />
          </label>
        </div>
        <textarea
          value={pem}
          placeholder="-----BEGIN RSA PRIVATE KEY-----&#10;…&#10;-----END RSA PRIVATE KEY-----"
          onChange={(e) => setPem(e.target.value)}
          rows={5}
          spellCheck={false}
          className={`${glassInputClass} resize-y font-mono text-ui leading-snug`}
        />
        <span className="block text-reported leading-relaxed text-ink-3">
          Paste the full contents of the App&rsquo;s downloaded .pem, or upload the file. It&rsquo;s
          stored encrypted and never leaves your deployment.
        </span>
      </div>

      {/* Optional OAuth client secret — collapsible, recommended. */}
      <div className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40">
        <button
          type="button"
          onClick={() => setOauthOpen((v) => !v)}
          aria-expanded={oauthOpen}
          className="flex w-full items-center gap-2 px-4 py-3 text-left"
        >
          {oauthOpen ? (
            <ChevronDown size={14} className="shrink-0 text-ink-3" />
          ) : (
            <ChevronRight size={14} className="shrink-0 text-ink-3" />
          )}
          <span className="flex-1 text-body font-medium text-ink-2">
            Enable browser sign-in for your team{' '}
            <span className="font-normal text-ink-3">(recommended)</span>
          </span>
        </button>
        {oauthOpen && (
          <div className="space-y-3 px-4 pb-4">
            <p className="text-ui leading-relaxed text-ink-3">
              A client secret lets teammates connect their GitHub identity with one click instead of
              pasting a token. You can skip it — identity capture falls back to a personal access
              token. Apps can hold two client secrets at once, so generating one here won&rsquo;t
              disturb an existing consumer.
            </p>
            <label className="block space-y-2">
              <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
                Client secret
              </span>
              <input
                type="password"
                autoComplete="off"
                value={clientSecret}
                placeholder="Generate one in the App's settings"
                onChange={(e) => setClientSecret(e.target.value)}
                className={glassInputClass}
              />
            </label>
            {callbackUrl && (
              <div className="space-y-1.5">
                <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
                  Callback URL to register on the App
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
                    onClick={copyCallback}
                    aria-label="Copy callback URL"
                    className="inline-flex shrink-0 items-center gap-1 rounded-xl border border-[var(--color-line-1)] px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1"
                  >
                    {copied ? <Check size={13} /> : <Copy size={13} />}
                    {copied ? 'Copied' : 'Copy'}
                  </button>
                </div>
                <p className="text-reported leading-relaxed text-ink-3">
                  Add this as a Callback URL under the App&rsquo;s settings on GitHub — OAuth
                  sign-in only works once it&rsquo;s registered there.
                </p>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Webhook secret — multi mode only (local is hookless).

          The copy names the CONSEQUENCE, not the field. Skipping this is
          accepted silently and then rejects every delivery GitHub sends, which
          leaves installation tracking to the periodic sync — the field being
          "(optional)" is true of the form and misleading about the workspace.
          The Settings panel's webhook-health line is where that shows up
          afterwards; this is where it can still be avoided. */}
      {!isLocal && (
        <label className="block space-y-2">
          <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
            Webhook secret{' '}
            <span className="font-normal normal-case text-ink-3">
              (needed for live installation tracking)
            </span>
          </span>
          <input
            type="password"
            autoComplete="off"
            value={webhookSecret}
            placeholder="Match the App's webhook secret, if set"
            onChange={(e) => setWebhookSecret(e.target.value)}
            className={glassInputClass}
          />
          <span className="block text-reported leading-relaxed text-ink-3">
            Paste the secret set on the App&rsquo;s webhook so Triage Factory can verify deliveries
            are genuine. Without it every delivery is rejected: installs, uninstalls and suspensions
            reach this workspace only on the next periodic sync. Leave blank only if the App has no
            webhook secret — you can add one on GitHub and import again.
          </span>
        </label>
      )}

      {permissions && <PermissionTable rows={permissions} />}

      {softGap && (
        <label className="flex items-start gap-2 text-ui text-ink-2">
          <input
            type="checkbox"
            checked={ack}
            onChange={(e) => setAck(e.target.checked)}
            className="mt-0.5"
          />
          <span>
            I understand some features stay off until the App is granted these permissions on
            GitHub.
          </span>
        </label>
      )}

      {error && (
        <p role="alert" className="text-ui leading-relaxed text-[var(--color-alarm)]">
          {error}
        </p>
      )}

      <button
        type="button"
        onClick={() => void submit()}
        disabled={!canSubmit}
        className="rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90 disabled:opacity-40 disabled:shadow-none"
      >
        {busy ? 'Validating…' : 'Connect App'}
      </button>
    </div>
  )
}

// PermissionTable renders the granted-vs-required preflight result. Unmet
// required permissions read in the dismiss tint (they block); unmet optional
// ones in amber with the feature they degrade; satisfied rows are muted.
function PermissionTable({ rows }: { rows: GitHubAppPermissionRow[] }) {
  return (
    <div className="overflow-hidden rounded-xl border border-line-1">
      <div className="flex items-center gap-2 border-b border-line-1 bg-raised px-3 py-1.5 text-label font-medium uppercase tracking-wide text-ink-3">
        <span className="flex-1">Permission</span>
        <span className="w-16 text-right">Needs</span>
        <span className="w-16 text-right">Granted</span>
        <span className="w-4" />
      </div>
      {rows.map((p) => {
        const unmetRequired = !p.satisfied && p.severity === 'required'
        const unmetOptional = !p.satisfied && p.severity !== 'required'
        return (
          <div
            key={p.permission}
            className={`px-3 py-1.5 text-ui ${
              unmetRequired ? 'bg-alarm/[0.06]' : unmetOptional ? 'bg-warm/[0.07]' : ''
            }`}
          >
            <div className="flex items-center gap-2">
              <span className="flex-1 text-ink-1">{p.permission}</span>
              <span className="w-16 text-right text-ink-3">{p.required}</span>
              <span className={`w-16 text-right ${p.granted ? 'text-ink-2' : 'text-ink-3'}`}>
                {p.granted || '—'}
              </span>
              <span className="flex w-4 justify-end">
                {p.satisfied ? (
                  <Check size={13} className="text-warm" />
                ) : (
                  <X
                    size={13}
                    className={unmetRequired ? 'text-[var(--color-alarm)]' : 'text-warm'}
                  />
                )}
              </span>
            </div>
            {unmetOptional && p.feature && (
              <p className="mt-0.5 text-reported leading-snug text-warm dark:text-warm">
                Without this: {p.feature} won&rsquo;t work.
              </p>
            )}
          </div>
        )
      })}
    </div>
  )
}
