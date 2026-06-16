import { useMemo, useState } from 'react'
import { Navigate, useLocation, useParams } from 'react-router-dom'
import { useJiraIdentity } from '../hooks/useJiraIdentity'
import { captureJiraIdentityPat, captureJiraIdentityApiToken } from '../lib/jiraIdentity'

/**
 * ConnectJira is the blocking onboarding gate for per-user Jira access — the
 * Jira sibling of ConnectGitHub. It gates the *verb*: Triage Factory acts as
 * you on Jira (board claims and ticket updates are attributed to you, not a
 * shared bot), so the per-user credential must be bound before those actions
 * are available. Team reads work without it; this only blocks until a
 * credential is stored.
 *
 * The structural difference from ConnectGitHub mirrors the bind flow: GitHub
 * captures *identity* (validate, keep the @login, discard the token); Jira
 * captures *access* (validate, STORE the credential, derive identity) — because
 * the Jira user level must act as the user. So the copy is honest about
 * retention: your token is stored, in your workspace's secret store.
 *
 * The paste path follows the org's deployment: a Data Center org binds a single
 * personal access token; a Cloud org binds the Atlassian account email + API
 * token. One-click Connect (Cloud OAuth, 3LO) is a later Cloud-tier ticket; the
 * button below is gated on connect_available (false until then), so it stays
 * hidden and the page offers only the paste path.
 *
 * Runs in BOTH modes — the org id comes from the route param
 * (/orgs/:org_id/connect-jira), so it needs no OrgContext (absent in local
 * mode). Rendered outside RequireJiraIdentity (its own route) so it isn't gated
 * by the very check it satisfies — no redirect loop.
 */

function Card({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-sm backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        {children}
      </div>
    </div>
  )
}

export default function ConnectJira() {
  const { org_id: orgId } = useParams<{ org_id: string }>()
  const location = useLocation()
  const { state, refresh } = useJiraIdentity(orgId ?? null)

  // Paste-capture state. On success we refresh(), which re-reads identity →
  // connected → navigates to returnTo below. Which fields are used follows the
  // org's deployment: DC uses pat, Cloud uses email + apiToken.
  const [pat, setPat] = useState('')
  const [email, setEmail] = useState('')
  const [apiToken, setApiToken] = useState('')
  const [capturing, setCapturing] = useState(false)
  const [patError, setPatError] = useState<string | null>(null)

  const returnTo = useMemo(() => {
    const rt = new URLSearchParams(location.search).get('return_to')
    if (rt && rt.startsWith('/') && !rt.startsWith('//')) return rt
    return orgId ? '/orgs/' + orgId : '/'
  }, [location.search, orgId])

  // Active org hasn't resolved yet (rare flash) — hold.
  if (!orgId || state.status === 'loading') {
    return (
      <Card>
        <p className="text-text-tertiary text-sm">Loading…</p>
      </Card>
    )
  }

  // Couldn't even read identity status (TF server/network hiccup). Offer a
  // retry rather than silently letting an unbound user through.
  if (state.status === 'error') {
    return (
      <Card>
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Finish setup
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            We couldn&apos;t check your Jira connection just now.
          </p>
        </div>
        <button
          type="button"
          onClick={refresh}
          className="w-full bg-text-primary hover:bg-text-primary/90 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          Try again
        </button>
      </Card>
    )
  }

  const { connected, host, connect_available, deployment } = state.data
  // The paste fields follow the org's deployment: Cloud binds an email + API
  // token, Data Center a single PAT.
  const cloud = deployment === 'cloud'

  // Already bound (direct navigation, or a stale tab after a successful
  // capture) — send the user where they were headed.
  if (connected) {
    return <Navigate to={returnTo} replace />
  }

  // Dormant until Cloud OAuth (3LO) lands: connect_available stays false on
  // Data Center, so the button this drives is hidden and the target
  // (/jira/connect/start, not yet implemented) is unreachable for now. It lights
  // up automatically when the backend flips connect_available for a Cloud org.
  const startConnect = () => {
    window.location.href =
      '/api/orgs/' +
      encodeURIComponent(orgId) +
      '/jira/connect/start?return_to=' +
      encodeURIComponent(returnTo)
  }

  const submitReady = cloud ? email.trim() !== '' && apiToken.trim() !== '' : pat.trim() !== ''
  const submit = async () => {
    if (!submitReady || capturing) return
    setCapturing(true)
    setPatError(null)
    try {
      if (cloud) {
        await captureJiraIdentityApiToken(orgId, email.trim(), apiToken.trim())
      } else {
        await captureJiraIdentityPat(orgId, pat.trim())
      }
      // Re-read status; the connected branch above then redirects to returnTo.
      refresh()
    } catch (e) {
      setPatError(e instanceof Error ? e.message : 'Could not verify that credential.')
    } finally {
      // Always clear the in-flight flag — even on success. If refresh() comes
      // back connected we navigate away regardless; if it lags (propagation) or
      // fails, the button must re-enable so the user can retry rather than be
      // stuck on a disabled "Verifying…".
      setCapturing(false)
    }
  }

  return (
    <Card>
      <div className="space-y-1.5">
        <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
          Connect your Jira
        </h1>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          One last step. Triage Factory acts as you on{' '}
          <span className="text-text-secondary font-medium">{host || 'Jira'}</span> — so the tickets
          it claims and updates are attributed to you, not a shared bot. This is a one-time
          connection.
        </p>
      </div>

      {connect_available && (
        <button
          type="button"
          onClick={startConnect}
          className="w-full flex items-center justify-center gap-2 bg-text-primary hover:bg-text-primary/90 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          Connect Jira
        </button>
      )}

      {/* Paste path — always offered, and the sole capture path until Cloud
          OAuth lands (connect_available=false). The fields follow the org's
          deployment: Cloud = email + API token, Data Center = a single PAT. */}
      <div className="space-y-2">
        {cloud ? (
          <>
            {/* When the one-click Connect button is offered above, this "Or
                paste…" preamble draws the contrast — it's a section heading for
                the email + token pair, NOT the first field's label. */}
            {connect_available && (
              <p className="text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                Or paste an email + API token
              </p>
            )}
            <label className="block">
              <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                Atlassian account email
              </span>
              <input
                type="email"
                autoComplete="email"
                value={email}
                placeholder="you@yourcompany.com"
                onChange={(e) => {
                  setEmail(e.target.value)
                  if (patError) setPatError(null)
                }}
                aria-invalid={!!patError || undefined}
                className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 transition-colors ${
                  patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
                }`}
              />
            </label>
            <label className="block">
              <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
                API token
              </span>
              <input
                type="password"
                autoComplete="off"
                value={apiToken}
                placeholder="Your Atlassian API token"
                onChange={(e) => {
                  setApiToken(e.target.value)
                  if (patError) setPatError(null)
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void submit()
                }}
                aria-invalid={!!patError || undefined}
                className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 transition-colors ${
                  patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
                }`}
              />
            </label>
          </>
        ) : (
          <label className="block">
            <span className="mb-1.5 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
              {connect_available
                ? 'Or paste a personal access token'
                : 'Paste a personal access token'}
            </span>
            <input
              type="password"
              autoComplete="off"
              value={pat}
              placeholder="Your Jira token"
              onChange={(e) => {
                setPat(e.target.value)
                if (patError) setPatError(null)
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void submit()
              }}
              aria-invalid={!!patError || undefined}
              className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 transition-colors ${
                patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
              }`}
            />
          </label>
        )}
        {patError && <p className="text-[12px] text-dismiss leading-relaxed">{patError}</p>}
        <button
          type="button"
          onClick={() => void submit()}
          disabled={!submitReady || capturing}
          className="w-full rounded-xl border border-border-glass px-4 py-2.5 text-[13px] font-medium text-text-secondary hover:text-text-primary hover:border-accent/40 disabled:opacity-40 disabled:hover:border-border-glass transition-colors"
        >
          {capturing ? 'Verifying…' : cloud ? 'Verify credential' : 'Verify token'}
        </button>
      </div>

      <p className="text-[11px] text-text-tertiary leading-relaxed">
        {cloud ? (
          <>
            Create a token at{' '}
            <a
              href="https://id.atlassian.com/manage-profile/security/api-tokens"
              target="_blank"
              rel="noreferrer"
              className="text-accent hover:underline"
            >
              Atlassian API token settings
            </a>
            . Unlike GitHub, it&apos;s stored — it&apos;s needed to act as you on Jira. It stays in
            your workspace&apos;s secret store and is never shared with other users.
          </>
        ) : (
          <>
            Unlike GitHub, your token is stored — it&apos;s needed to act as you on Jira. It stays
            in your workspace&apos;s secret store and is never shared with other users.
          </>
        )}
      </p>
    </Card>
  )
}
