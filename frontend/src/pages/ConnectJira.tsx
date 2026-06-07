import { useMemo, useState } from 'react'
import { Navigate, useLocation, useParams } from 'react-router-dom'
import { useJiraIdentity } from '../hooks/useJiraIdentity'
import { captureJiraIdentityPat } from '../lib/jiraIdentity'

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
 * DC = paste-a-PAT (this page today). One-click Connect (Cloud OAuth, 3LO) is a
 * later Cloud-tier ticket; the button below is gated on connect_available
 * (false until then), so it stays hidden and the page offers only the PAT path.
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

  // PAT capture state — the only path on Data Center. On success we refresh(),
  // which re-reads identity → connected → navigates to returnTo below.
  const [pat, setPat] = useState('')
  const [capturing, setCapturing] = useState(false)
  const [patError, setPatError] = useState<string | null>(null)

  const params = new URLSearchParams(location.search)
  const returnTo = useMemo(() => {
    const rt = params.get('return_to')
    if (rt && rt.startsWith('/') && !rt.startsWith('//')) return rt
    return orgId ? '/orgs/' + orgId : '/'
    // location.search is the only input; params is derived from it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
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

  const { connected, host, connect_available } = state.data

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

  const submitPat = async () => {
    if (pat.trim() === '' || capturing) return
    setCapturing(true)
    setPatError(null)
    try {
      await captureJiraIdentityPat(orgId, pat.trim())
      // Re-read status; the connected branch above then redirects to returnTo.
      refresh()
    } catch (e) {
      setPatError(e instanceof Error ? e.message : 'Could not verify that token.')
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

      {/* PAT path — always offered, and the sole capture path on Data Center
          (connect_available=false until Cloud OAuth lands). */}
      <div className="space-y-2">
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
              if (e.key === 'Enter') void submitPat()
            }}
            aria-invalid={!!patError || undefined}
            className={`w-full rounded-xl border bg-surface px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 transition-colors ${
              patError ? 'border-dismiss/50' : 'border-border-glass focus:border-accent/40'
            }`}
          />
        </label>
        {patError && <p className="text-[12px] text-dismiss leading-relaxed">{patError}</p>}
        <button
          type="button"
          onClick={() => void submitPat()}
          disabled={pat.trim() === '' || capturing}
          className="w-full rounded-xl border border-border-glass px-4 py-2.5 text-[13px] font-medium text-text-secondary hover:text-text-primary hover:border-accent/40 disabled:opacity-40 disabled:hover:border-border-glass transition-colors"
        >
          {capturing ? 'Verifying…' : 'Verify token'}
        </button>
      </div>

      <p className="text-[11px] text-text-tertiary leading-relaxed">
        Unlike GitHub, your token is stored — it&apos;s needed to act as you on Jira. It stays in
        your workspace&apos;s secret store and is never shared with other users.
      </p>
    </Card>
  )
}
