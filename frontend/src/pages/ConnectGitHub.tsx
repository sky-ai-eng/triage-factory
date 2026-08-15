import { useMemo, useState } from 'react'
import { Navigate, useLocation, useParams } from 'react-router'
import { useGitHubIdentity } from '../hooks/useGitHubIdentity'
import { captureGitHubIdentityPat } from '../lib/githubIdentity'

/**
 * ConnectGitHub is the blocking onboarding gate: "Connect your GitHub to
 * finish setup." It gates the *verb*, not the view — identity is the property
 * features that dereference you as a GitHub actor depend on (self-predicates,
 * the personal dashboard, routing a task back to you). Team reads work without
 * it; this only blocks until a host-verified login is bound.
 *
 * It is the backstop for the setup wizard's User step (the first-run capture):
 * a returning or drifted user (renamed, left the org), or a second member who
 * joined after first-run, lands here. Satisfiable two ways, both ending in the
 * same state (a verified identity row, no token retained):
 *   - Connect — one consent click against the org's host (github.com OR GHES),
 *     when a GitHub App is registered.
 *   - PAT_2 paste — always available, and the only path when no App exists.
 * The org PAT (PAT_1) is deliberately NOT a capture path here: access and
 * identity are independent, so there is no "ask your admin to save a token"
 * shortcut — every user binds their own identity. There is no voluntary skip.
 *
 * Two Connect failure shapes are kept distinct, per the gate's contract:
 *   - "not connected yet" — your action (the default state: Connect / paste).
 *   - "host unreachable"  — infra (TF's backend couldn't reach the host).
 * Collapsing them would blame the user for a network problem.
 *
 * Runs in BOTH modes — the org id comes from the route param (/orgs/:org_id/
 * connect-github), so it needs no OrgContext (absent in local mode). Rendered
 * outside RequireGitHubIdentity (its own route) so it isn't gated by the very
 * check it satisfies — no redirect loop.
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

const GitHubMark = () => (
  <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" className="fill-current">
    <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
  </svg>
)

/** errorBanner maps a callback error code to user-facing copy. host_unreachable
 *  is the distinct infra state; the rest are "your action didn't complete." */
function errorBanner(
  code: string | null,
  host: string,
): { tone: 'infra' | 'retry'; text: string } | null {
  switch (code) {
    case null:
    case '':
      return null
    case 'host_unreachable':
      return {
        tone: 'infra',
        text: `We couldn't reach ${host || 'your GitHub host'}. This is a connectivity issue between Triage Factory and your GitHub server, not something you did — try again, or ask your workspace admin to check that the host is reachable.`,
      }
    case 'denied':
      return {
        tone: 'retry',
        text: 'GitHub sign-in was cancelled before it finished. Connect again to complete setup.',
      }
    case 'bad_host':
      return {
        tone: 'retry',
        text: "Your workspace's GitHub URL looks misconfigured. Ask your admin to fix it in Workspace Settings, then try again.",
      }
    case 'state':
      return { tone: 'retry', text: 'That connection attempt expired. Please try again.' }
    default:
      return {
        tone: 'retry',
        text: 'Something went wrong connecting your GitHub. Please try again.',
      }
  }
}

export default function ConnectGitHub() {
  const { org_id: orgId } = useParams<{ org_id: string }>()
  const location = useLocation()
  const { state, refresh } = useGitHubIdentity(orgId ?? null)

  // PAT_2 fallback state — always available (the only path when no App is
  // registered). On success we refresh(), which re-reads identity → connected →
  // navigates to returnTo below.
  const [pat, setPat] = useState('')
  const [capturing, setCapturing] = useState(false)
  const [patError, setPatError] = useState<string | null>(null)

  const params = new URLSearchParams(location.search)
  const errorCode = params.get('error')
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
        <p className="text-ink-3 text-sm">Loading…</p>
      </Card>
    )
  }

  // Couldn't even read identity status (TF server/network hiccup). Distinct
  // from the GitHub-host-unreachable case below — this is TF itself. Offer a
  // retry rather than silently letting an unconnected user through.
  if (state.status === 'error') {
    return (
      <Card>
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-ink-1 tracking-tight">Finish setup</h1>
          <p className="text-body text-ink-3 leading-relaxed">
            We couldn&apos;t check your GitHub connection just now.
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

  const { connected, host, connect_available } = state.data

  // Already bound (direct navigation, or a stale tab after a successful
  // connect) — send the user where they were headed.
  if (connected) {
    return <Navigate to={returnTo} replace />
  }

  const banner = errorBanner(errorCode, host)
  const startConnect = () => {
    window.location.href =
      '/api/orgs/' +
      encodeURIComponent(orgId) +
      '/github/connect/start?return_to=' +
      encodeURIComponent(returnTo)
  }

  const submitPat = async () => {
    if (pat.trim() === '' || capturing) return
    setCapturing(true)
    setPatError(null)
    try {
      await captureGitHubIdentityPat(orgId, pat.trim())
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
        <h1 className="text-[22px] font-semibold text-ink-1 tracking-tight">Connect your GitHub</h1>
        <p className="text-body text-ink-3 leading-relaxed">
          One last step. Triage Factory needs to know who you are on{' '}
          <span className="text-ink-2 font-medium">{host || 'GitHub'}</span> so it can match your
          pull requests and reviews to you. This is a one-time connection.
        </p>
      </div>

      {banner && (
        <div
          className={
            banner.tone === 'infra'
              ? 'rounded-xl bg-warm/[0.08] border border-warm/20 px-4 py-2.5 text-ui text-warm dark:text-warm leading-relaxed'
              : 'rounded-xl bg-alarm/[0.08] border border-alarm/20 px-4 py-2.5 text-ui text-alarm leading-relaxed'
          }
        >
          {banner.text}
        </div>
      )}

      {connect_available && (
        <button
          type="button"
          onClick={startConnect}
          className="w-full flex items-center justify-center gap-2 bg-inverse hover:bg-inverse/90 text-inverse-ink font-medium rounded-xl px-4 py-2.5 text-body transition-colors"
        >
          <GitHubMark />
          Connect GitHub
        </button>
      )}

      {/* PAT_2 fallback — always offered, and the sole capture path when no App
          is registered (connect_available=false). */}
      <div className="space-y-2">
        <label className="block">
          <span className="mb-1.5 block text-reported font-medium uppercase tracking-wide text-ink-3">
            {connect_available
              ? 'Or paste a personal access token'
              : 'Paste a personal access token'}
          </span>
          <input
            type="password"
            autoComplete="off"
            value={pat}
            placeholder="ghp_…"
            onChange={(e) => {
              setPat(e.target.value)
              if (patError) setPatError(null)
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submitPat()
            }}
            aria-invalid={!!patError || undefined}
            className={`w-full rounded-xl border bg-ground px-4 py-2.5 text-body text-ink-1 placeholder-ink-3 focus:outline-none focus:ring-2 focus:ring-warm/30 transition-colors ${
              patError ? 'border-alarm/50' : 'border-line-1 focus:border-warm/40'
            }`}
          />
        </label>
        {patError && <p className="text-ui text-alarm leading-relaxed">{patError}</p>}
        <button
          type="button"
          onClick={() => void submitPat()}
          disabled={pat.trim() === '' || capturing}
          className="w-full rounded-xl border border-line-1 px-4 py-2.5 text-body font-medium text-ink-2 hover:text-ink-1 hover:border-warm/40 disabled:opacity-40 disabled:hover:border-line-1 transition-colors"
        >
          {capturing ? 'Verifying…' : 'Verify token'}
        </button>
      </div>

      <p className="text-reported text-ink-3 leading-relaxed">
        Either way, Triage Factory only reads your GitHub username — it doesn&apos;t store your
        token or gain access to your repositories. Repository access is configured separately by
        your admin.
      </p>
    </Card>
  )
}
