import { useMemo } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { useActiveOrgId } from '../contexts/OrgContext'
import { useGitHubIdentity } from '../hooks/useGitHubIdentity'

/**
 * ConnectGitHub is the blocking onboarding gate: "Connect your GitHub to
 * finish setup." It gates the *verb*, not the view — identity is the property
 * features that dereference you as a GitHub actor depend on (self-predicates,
 * the personal dashboard, routing a task back to you). Team reads work without
 * it; this only blocks until a host-verified login is bound.
 *
 * Satisfiable by Connect (one consent click against the org's host, github.com
 * OR GHES) — or, for an org admin, by pasting a PAT in Workspace Settings,
 * which captures the same identity. There is deliberately no voluntary skip:
 * a skip would silently rebuild the half-onboarded population the gate exists
 * to eliminate. The runtime still tolerates an absent row (drift after a
 * rename, etc.) — the hard gate is at onboarding, soft tolerance at runtime.
 *
 * Two failure shapes are kept distinct, per the gate's contract:
 *   - "not connected yet" — your action (the default state: click Connect).
 *   - "host unreachable"  — infra (TF's backend couldn't reach the host).
 * Collapsing them would blame the user for a network problem.
 *
 * Rendered outside RequireGitHubIdentity (its own route) so it isn't gated by
 * the very check it satisfies — no redirect loop.
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
  const orgId = useActiveOrgId()
  const location = useLocation()
  const { state, refresh } = useGitHubIdentity(orgId)

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
        <p className="text-text-tertiary text-sm">Loading…</p>
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
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Finish setup
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            We couldn&apos;t check your GitHub connection just now.
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

  return (
    <Card>
      <div className="space-y-1.5">
        <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
          Connect your GitHub
        </h1>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          One last step. Triage Factory needs to know who you are on{' '}
          <span className="text-text-secondary font-medium">{host || 'GitHub'}</span> so it can
          match your pull requests and reviews to you. This is a one-time connection.
        </p>
      </div>

      {banner && (
        <div
          className={
            banner.tone === 'infra'
              ? 'rounded-xl bg-amber-500/[0.08] border border-amber-500/20 px-4 py-2.5 text-[12px] text-amber-600 dark:text-amber-400 leading-relaxed'
              : 'rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[12px] text-dismiss leading-relaxed'
          }
        >
          {banner.text}
        </div>
      )}

      {connect_available ? (
        <button
          type="button"
          onClick={startConnect}
          className="w-full flex items-center justify-center gap-2 bg-text-primary hover:bg-text-primary/90 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          <GitHubMark />
          Connect GitHub
        </button>
      ) : (
        <div className="rounded-xl bg-surface border border-border-glass px-4 py-3 text-[12px] text-text-tertiary leading-relaxed">
          Your workspace admin needs to finish setting up GitHub access before you can connect. Once
          a GitHub App is registered for this workspace, this step will be one click. An admin can
          also bind your identity by saving a GitHub token in Workspace Settings.
        </div>
      )}

      <p className="text-[11px] text-text-tertiary leading-relaxed">
        Connecting only reads your GitHub username — it doesn&apos;t grant Triage Factory access to
        your repositories. Repository access is configured separately by your admin.
      </p>
    </Card>
  )
}
