import { useLocation, useNavigate } from 'react-router'
import { useAuth } from '../contexts/AuthContext'
import { useOrgContext } from '../contexts/OrgContext'
import { reconnectWebSocket } from './useWebSocket'
import { invalidateTeams } from './useTeams'
import { invalidateEntitlements } from './useEntitlements'
import { apiFetch, isSSORequired } from '../lib/apiClient'

/**
 * useSwitchOrg returns the one way the SPA moves to another org the user
 * belongs to: update OrgContext, persist the choice on the session, drop the
 * org-scoped caches, and rewrite the URL so the same sub-path opens inside the
 * new org — e.g. switching from /orgs/A/triage to /orgs/B/triage keeps
 * /triage. OrgPicker uses it, and so does the SSO-required interstitial, which
 * renders where the picker cannot.
 *
 * Callers do not offer it for an org that requires an SSO the session did not
 * come through: the server refuses that switch, and the user needs the org's
 * own sign-in (`AuthOrg.sso_start_url`), a full-page navigation.
 */
export function useSwitchOrg(): (newOrgId: string) => Promise<void> {
  const { refresh } = useAuth()
  const { activeOrgId, setActiveOrgId } = useOrgContext()
  const navigate = useNavigate()
  const location = useLocation()

  return async (newOrgId: string) => {
    if (newOrgId === activeOrgId) return
    setActiveOrgId(newOrgId)

    // Persist the choice on the session row so the server's
    // withSession picks up the new active org for every subsequent
    // request (and so cross-tab /api/me reads agree). Fire-and-log
    // failures: the local pick already updated OrgContext, and the
    // next /api/me will reconcile a transient persistence failure on
    // the user's next interaction.
    try {
      await apiFetch('/api/me/active-org', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ org_id: newOrgId }),
      })
      // Drop the cached /api/teams response only AFTER the active org
      // is persisted server-side. /api/teams is session-org-scoped, so
      // invalidating before the POST commits would race and refetch the
      // OLD org's teams; doing it here guarantees the reload reads the
      // new org's set.
      invalidateTeams()
      // Entitlements are scoped to the active org too (multi mode), so drop
      // their cache on the same commit — the new org may license a different
      // EE feature set. Same post-persist ordering rationale as teams.
      invalidateEntitlements()
      // Force the WS to re-handshake so the hub's per-(user, org)
      // Broadcast filter routes events for the just-switched
      // tenant rather than the previous one. Without this the
      // user keeps receiving the old org's events until the next
      // unrelated disconnect.
      reconnectWebSocket()
    } catch (err) {
      if (isSSORequired(err)) {
        // The org started requiring SSO after the last /api/me read, so the
        // list offered it as a plain switch. Re-read before navigating, and
        // the org's route renders its sign-in rather than pages whose reads
        // all answer 403.
        await refresh()
      } else {
        // A refusal (revoked membership) or a network drop. Rolling back the
        // local selection would conflict with the URL navigation below; log
        // and let the AuthGate / /api/me reconciliation correct it on the
        // next page render.
        console.warn('[org-switch] active-org POST failed:', err)
      }
    }

    // Rewrite the URL: swap the org_id segment, preserve everything
    // after it. Routes that don't start with /orgs/<id> fall back to
    // the new org's root.
    const swapped = location.pathname.match(/^\/orgs\/[^/]+(\/.*)?$/)
    const tail = swapped?.[1] ?? ''
    navigate('/orgs/' + newOrgId + tail, { replace: false })
  }
}
