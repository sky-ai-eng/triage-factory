import { Navigate, useLocation } from 'react-router-dom'
import { useAuthStatus } from './hooks/useAuthStatus'
import { useAuth, useOptionalAuth } from './contexts/AuthContext'
import { useOrgContext, useActiveOrgId } from './contexts/OrgContext'
import { useGitHubIdentity } from './hooks/useGitHubIdentity'

/**
 * AuthGate routes between the local-mode first-run/provision flow and the
 * multi-mode cookie-session flow.
 *
 *   Local mode → LocalAuthGate (/api/integrations/status; !configured →
 *                the "Start your factory" provision screen)
 *   Multi mode → MultiAuthGate (uses AuthContext)
 *
 * Multi-mode states:
 *   loading → spinner
 *   error   → error panel with retry button
 *   unauth  → redirect to /login?return_to=<current>
 *   authed + 0 orgs   → redirect to /onboarding (create-or-invite entry)
 *   authed + N orgs, URL has unknown org_id → redirect to /orgs/<active> (preserve tail)
 *   authed + N orgs, URL not under /orgs/:id → redirect to /orgs/<active>
 *   authed + N orgs, URL under /orgs/:id → render children
 *
 * /setup is a live route in BOTH modes — the single create-time wizard the
 * mandatory-setup gate redirects incomplete users to. In multi it sits
 * outside /orgs/:id and resolves its org from OrgContext (server-active /
 * localStorage / first-org), so MultiAuthGate admits it via the
 * org-resolved branch rather than redirecting it away.
 */

function Loading() {
  return (
    <div className="min-h-screen bg-surface flex items-center justify-center">
      <p className="text-text-tertiary text-sm">Loading...</p>
    </div>
  )
}

function LocalAuthGate({ children }: { children: React.ReactNode }) {
  const { configured, loading } = useAuthStatus()
  if (loading) return <Loading />
  // No provisioned tenant yet → the first-run "Start your factory"
  // screen, which fires POST /api/setup/start and then routes into the
  // /setup wizard. (configured = a tenant exists, not "GitHub creds set".)
  if (!configured) return <Navigate to="/start" replace />
  return <>{children}</>
}

function MultiAuthGate({ children }: { children: React.ReactNode }) {
  const auth = useAuth()
  const { activeOrgId, urlOrgInvalid } = useOrgContext()
  const location = useLocation()

  if (auth.status === 'loading') {
    return <Loading />
  }
  if (auth.status === 'error') {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <div className="text-center space-y-3">
          <p className="text-text-secondary text-sm">{auth.error ?? 'Failed to load session'}</p>
          <button
            type="button"
            onClick={() => void auth.refresh()}
            className="text-accent text-sm underline"
          >
            Retry
          </button>
        </div>
      </div>
    )
  }
  if (auth.status === 'unauth') {
    const target = location.pathname + location.search
    const params = target !== '/' ? '?return_to=' + encodeURIComponent(target) : ''
    return <Navigate to={'/login' + params} replace />
  }
  // Zero-membership users land on the unified onboarding entry — a
  // single "create your org / wait for an invite" page. There's no
  // policy branch here: the page itself enables or disables the create
  // affordance based on me.org_creation_enabled (TF_PREVENT_ORG_CREATION).
  // Signup provisions nothing, so this is the steady-state first-run
  // landing, not a rare error state.
  if (auth.orgs.length === 0) {
    return <Navigate to="/onboarding" replace />
  }
  // URL has an org_id the user is not a member of — swap it for the
  // active org while preserving the rest of the path.
  if (urlOrgInvalid && activeOrgId) {
    const tail = (location.pathname.replace(/^\/orgs\/[^/]+/, '') || '/') + location.search
    return <Navigate to={'/orgs/' + activeOrgId + tail} replace />
  }
  // Authed + has orgs. MultiAuthGate mounts under /orgs/:org_id/* (org in the
  // path) and the bare /setup wizard (org resolved from OrgContext —
  // server-active / localStorage / first-org), so either way an active org is
  // expected. It might still be null briefly between auth resolution and
  // OrgContext picking — render loading until both line up.
  if (!activeOrgId) {
    return <Loading />
  }
  return <>{children}</>
}

/**
 * RequireGitHubIdentity is the onboarding gate, composed *inside* the
 * authed+org-resolved MultiAuthGate and wrapping the app shell. It blocks the
 * app until the user has a host-verified GitHub identity for the active org's
 * host, redirecting to the Connect page (which lives on its own route, NOT
 * behind this gate, so there's no loop) otherwise.
 *
 * "Couldn't read the status" (TF-side error) is held as a retryable panel
 * rather than rendered through — the gate must not silently admit an
 * unconnected user on a transient failure. The Connect-failure shapes
 * (including host-unreachable) are surfaced on the Connect page itself.
 *
 * Multi-mode only by construction (MultiRoutes is the sole mounter). Local
 * mode binds identity via the PAT setup wizard, so LocalAuthGate covers it.
 */
export function RequireGitHubIdentity({ children }: { children: React.ReactNode }) {
  const { activeOrgId } = useOrgContext()
  const location = useLocation()
  const { state, refresh } = useGitHubIdentity(activeOrgId)

  if (!activeOrgId || state.status === 'loading') {
    return <Loading />
  }
  if (state.status === 'error') {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <div className="text-center space-y-3">
          <p className="text-text-secondary text-sm">Couldn&apos;t check your GitHub connection.</p>
          <button type="button" onClick={refresh} className="text-accent text-sm underline">
            Retry
          </button>
        </div>
      </div>
    )
  }
  if (!state.data.connected) {
    const target = location.pathname + location.search
    const rt = target && target !== '/' ? '?return_to=' + encodeURIComponent(target) : ''
    return <Navigate to={'/orgs/' + activeOrgId + '/connect-github' + rt} replace />
  }
  return <>{children}</>
}

/**
 * SetupPendingNotice is the holding screen a non-admin sees when their org
 * hasn't finished first-run configuration. They can't action GitHub access
 * or tracked repos (admin-only), so we never route them into the configure
 * flow — we tell them who can. An admin completing setup flips the org to
 * setup_complete and the gate lets everyone in on the next poll.
 */
function SetupPendingNotice() {
  return (
    <div className="min-h-screen bg-surface flex items-center justify-center px-4">
      <div className="max-w-md text-center space-y-3">
        <h1 className="text-[18px] font-semibold text-text-primary tracking-tight">
          Setup isn&rsquo;t finished
        </h1>
        <p className="text-[13px] text-text-tertiary leading-relaxed">
          Contact your org administrator to finish setup before continuing. Once GitHub access and
          tracked repositories are configured, this organization will open for everyone.
        </p>
      </div>
    </div>
  )
}

/**
 * RequireSetupComplete is the first-run completeness gate. It wraps the app
 * shell (NOT the configure routes — those must stay reachable for an
 * incomplete founder, so gating them here would loop) and blocks rendering
 * until the active org is setup_complete (GitHub access configured AND ≥1
 * tracked repo). An admin/owner of an incomplete org is routed to the single
 * /setup wizard, which resumes on the first incomplete step (computed
 * client-side); a non-admin gets the holding screen since they can't action
 * the missing config. The gate no longer needs setup_step — one redirect
 * target, not two.
 *
 * Composed inside AuthGate (so the tenant/membership checks already passed)
 * and, in multi, outside RequireGitHubIdentity (org-level GitHub setup is a
 * prerequisite of the per-user Connect step, so it's checked first).
 *
 * `isLocal` only selects admin treatment now (the redirect target is a
 * constant /setup): local has no OrgContext and is always treated as admin
 * (N=1, the single user owns the install).
 */
export function RequireSetupComplete({
  children,
  isLocal = false,
}: {
  children: React.ReactNode
  isLocal?: boolean
}) {
  const auth = useOptionalAuth()
  const activeOrgId = useActiveOrgId()
  // Poll while blocked so the gate self-heals: a non-admin on the holding
  // screen is admitted automatically once an admin finishes setup (the hook
  // stops polling the moment a response reports setup_complete). 15s is brisk
  // enough to feel automatic without hammering the endpoint.
  const { setup_complete, loading } = useAuthStatus(activeOrgId ?? undefined, 15000)

  if (loading) return <Loading />
  if (setup_complete) return <>{children}</>

  // Incomplete. Local is always the implicit admin; multi reads the caller's
  // role on the active org (owner/admin may configure, everyone else waits).
  const role = auth?.orgs.find((o) => o.id === activeOrgId)?.role
  const isAdmin = isLocal || role === 'owner' || role === 'admin'
  if (!isAdmin) return <SetupPendingNotice />

  // Both modes resume in the single /setup wizard; it computes the first
  // incomplete step from the GETs it already makes, so there's one target.
  return <Navigate to="/setup" replace />
}

export default function AuthGate({
  children,
  mode,
}: {
  children: React.ReactNode
  mode: 'local' | 'multi'
}) {
  if (mode === 'multi') {
    return <MultiAuthGate>{children}</MultiAuthGate>
  }
  return <LocalAuthGate>{children}</LocalAuthGate>
}
