import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'

// Org roles that may edit the org template — owner + admin, mirroring the
// backend's requireOrgAdminRole (and pages/settings/access.ts). Kept here too
// so the hook is self-contained (it doesn't need the full computeAccess inputs).
const ORG_ADMIN_ROLES = new Set(['owner', 'admin'])

export interface TemplateScopeState {
  // available gates the nav entry + route: true only in multi mode when the
  // viewer is an admin (owner/admin) of the active org. Local mode (no
  // AuthProvider) is always false — there's no template concept at N=1.
  available: boolean
  // ready is BindingGraph's scopeReady gate for { kind: 'template' }: the
  // editor can fetch once auth has resolved and the admin check passes.
  ready: boolean
  // loading is true while auth is still resolving (multi mode). The page's
  // redirect guard waits on this so an admin isn't bounced before their role
  // is known — distinct from "confirmed non-admin" (loading false, available
  // false). False in local mode (no auth to resolve).
  loading: boolean
}

// useTemplateScope is the org-template parallel to useActiveTeam — no switcher,
// no team list, just "is the org-template editor reachable for this viewer."
// Safe to call in both modes: useOptionalAuth + useActiveOrgId both return
// null without their providers (local mode), so the hook simply reports
// unavailable there.
export function useTemplateScope(): TemplateScopeState {
  const auth = useOptionalAuth()
  const activeOrgId = useActiveOrgId()

  if (!auth) {
    return { available: false, ready: false, loading: false }
  }
  const activeOrg = activeOrgId
    ? auth.orgs.find((o) => o.id === activeOrgId)
    : auth.orgs.length === 1
      ? auth.orgs[0]
      : undefined
  const isAdmin = !!activeOrg && ORG_ADMIN_ROLES.has(activeOrg.role)
  return {
    available: isAdmin,
    ready: auth.status === 'authed' && isAdmin,
    loading: auth.status === 'loading',
  }
}
