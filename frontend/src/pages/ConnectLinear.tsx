import { useMemo, useState } from 'react'
import { Navigate, useLocation, useParams } from 'react-router'
import { useLinearIdentity } from '../hooks/useLinearIdentity'
import {
  captureLinearIdentityApiKey,
  linearConnectErrorText,
  linearConnectStartURL,
  linearDisconnectedText,
} from '../lib/linearIdentity'
import { safeReturnTo } from '../lib/returnTo'

/**
 * ConnectLinear is the blocking onboarding gate for per-user Linear access —
 * the Linear sibling of ConnectJira. Every write a person makes in Linear
 * through Triage Factory is made as that person, so their own credential is
 * bound before the app opens, once the org has Linear.
 *
 * Two ways to bind: one-click Connect (Linear's consent page, offered when a
 * Linear OAuth app resolves for the org) and a pasted personal API key, always
 * offered. Either way the credential is stored, so the copy says so.
 *
 * Runs in BOTH modes — the org id comes from the route param
 * (/orgs/:org_id/connect-linear), so it needs no OrgContext (absent in local
 * mode). Rendered outside RequireLinearIdentity (its own route) so it isn't
 * gated by the very check it satisfies.
 */

function Card({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-h-screen bg-ground flex items-center justify-center p-4">
      <div className="w-full max-w-sm backdrop-blur-xl bg-raised border border-line-1 rounded-2xl p-8 space-y-6 shadow-float shadow-black/[0.04]">
        {children}
      </div>
    </div>
  )
}

export default function ConnectLinear() {
  const { org_id: orgId } = useParams<{ org_id: string }>()
  const location = useLocation()
  const { state, refresh } = useLinearIdentity(orgId ?? null)

  const [apiKey, setApiKey] = useState('')
  const [capturing, setCapturing] = useState(false)
  const [keyError, setKeyError] = useState<string | null>(null)

  const errorCode = new URLSearchParams(location.search).get('error')
  const returnTo = useMemo(
    () =>
      safeReturnTo(
        new URLSearchParams(location.search).get('return_to'),
        orgId ? '/orgs/' + orgId : '/',
      ),
    [location.search, orgId],
  )

  if (!orgId || state.status === 'loading') {
    return (
      <Card>
        <p className="text-ink-3 text-sm">Loading…</p>
      </Card>
    )
  }

  if (state.status === 'error') {
    return (
      <Card>
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-ink-1 tracking-tight">Finish setup</h1>
          <p className="text-body text-ink-3 leading-relaxed">
            We couldn&apos;t check your Linear connection just now.
          </p>
        </div>
        <button
          type="button"
          onClick={refresh}
          className="w-full bg-inverse hover:bg-inverse/90 text-inverse-ink font-medium rounded-xl px-4 py-2.5 text-body transition-colors"
        >
          Try again
        </button>
      </Card>
    )
  }

  const {
    connected,
    workspace_url_key: workspace,
    connect_available,
    disconnected_reason,
  } = state.data

  // Already bound, or the org has no Linear to bind against: nothing to do
  // here, so send the user where they were headed.
  if (connected || workspace === '') {
    return <Navigate to={returnTo} replace />
  }

  const startConnect = () => {
    window.location.href = linearConnectStartURL(orgId, returnTo)
  }

  // A Connect that just failed explains itself first; otherwise say why a
  // connection the user had stopped working.
  const banner = linearConnectErrorText(errorCode) ?? linearDisconnectedText(disconnected_reason)
  const submit = async () => {
    if (apiKey.trim() === '' || capturing) return
    setCapturing(true)
    setKeyError(null)
    try {
      await captureLinearIdentityApiKey(orgId, apiKey)
      // Re-read status; the connected branch above then redirects to returnTo.
      refresh()
    } catch (e) {
      setKeyError(e instanceof Error ? e.message : "Couldn't verify that API key.")
    } finally {
      setCapturing(false)
    }
  }

  return (
    <Card>
      <div className="space-y-1.5">
        <h1 className="text-[22px] font-semibold text-ink-1 tracking-tight">Connect your Linear</h1>
        <p className="text-body text-ink-3 leading-relaxed">
          One last step. Triage Factory acts as you in{' '}
          <span className="text-ink-2 font-medium">linear.app/{workspace}</span> — so the issues it
          claims and updates are attributed to you, not a shared bot. This is a one-time connection.
        </p>
      </div>

      {banner && (
        <div
          role="alert"
          className="rounded-xl border border-alarm/30 bg-alarm/[0.06] px-4 py-2.5 text-ui leading-relaxed text-alarm"
        >
          {banner}
        </div>
      )}

      {connect_available && (
        <button
          type="button"
          onClick={startConnect}
          className="w-full flex items-center justify-center gap-2 bg-inverse hover:bg-inverse/90 text-inverse-ink font-medium rounded-xl px-4 py-2.5 text-body transition-colors"
        >
          Connect Linear
        </button>
      )}

      <div className="space-y-2">
        <label className="block">
          <span className="mb-1.5 block text-reported font-medium uppercase tracking-wide text-ink-3">
            {connect_available ? 'Or paste a personal API key' : 'Paste a personal API key'}
          </span>
          <input
            type="password"
            autoComplete="off"
            value={apiKey}
            placeholder="lin_api_…"
            onChange={(e) => {
              setApiKey(e.target.value)
              if (keyError) setKeyError(null)
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submit()
            }}
            aria-invalid={!!keyError || undefined}
            className={`w-full rounded-xl border bg-ground px-4 py-2.5 text-body text-ink-1 placeholder-ink-3 focus:outline-none focus:ring-2 focus:ring-warm/30 transition-colors ${
              keyError ? 'border-alarm/50' : 'border-line-1 focus:border-warm/40'
            }`}
          />
        </label>
        {keyError && <p className="text-ui text-alarm leading-relaxed">{keyError}</p>}
        <button
          type="button"
          onClick={() => void submit()}
          disabled={apiKey.trim() === '' || capturing}
          className="w-full rounded-xl border border-line-1 px-4 py-2.5 text-body font-medium text-ink-2 hover:text-ink-1 hover:border-warm/40 disabled:opacity-40 disabled:hover:border-line-1 transition-colors"
        >
          {capturing ? 'Verifying…' : 'Verify key'}
        </button>
      </div>

      <p className="text-reported text-ink-3 leading-relaxed">
        Create a key in Linear under Settings → Account → Security &amp; Access. Whichever way you
        connect, your key or token is stored in your workspace&apos;s secret store so Triage Factory
        can act as you, and is never shared with other users.
      </p>
    </Card>
  )
}
