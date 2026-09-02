import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'

// Org roles that count as admin — owner + admin, mirroring the backend's
// tf.user_is_org_admin (and requireOrgAdminRole). Member is not admin.
const ORG_ADMIN_ROLES = new Set(['owner', 'admin'])

// isOrgAdminRole is the same test as useOrgRole's isAdmin, for a caller that
// holds a role string for an org other than the active one — a page offering a
// choice of workspaces reads each membership's role rather than the hook's
// single answer.
export function isOrgAdminRole(role: string | null | undefined): boolean {
  return !!role && ORG_ADMIN_ROLES.has(role)
}

export interface OrgRoleState {
  // The viewer's role in the active org ("owner" | "admin" | "member"), or
  // null when unknown (still loading, no active org, or local mode).
  role: string | null
  // Convenience: role is owner or admin. Gates the Org nav entry + the
  // roster's management controls.
  isAdmin: boolean
  // True while multi-mode auth is still resolving — callers that redirect or
  // hide on non-admin should wait this out so an admin isn't bounced mid-load.
  loading: boolean
}

// useOrgRole reports the viewer's role in the active org. Safe in both modes:
// useOptionalAuth + useActiveOrgId both return null without their providers
// (local mode), so it simply reports "no role / not admin" there. When no
// org is explicit but the viewer belongs to exactly one, that org is implied.
export function useOrgRole(): OrgRoleState {
  const auth = useOptionalAuth()
  const activeOrgId = useActiveOrgId()

  if (!auth) {
    return { role: null, isAdmin: false, loading: false }
  }
  const activeOrg = activeOrgId
    ? auth.orgs.find((o) => o.id === activeOrgId)
    : auth.orgs.length === 1
      ? auth.orgs[0]
      : undefined
  const role = activeOrg?.role ?? null
  return {
    role,
    isAdmin: !!role && ORG_ADMIN_ROLES.has(role),
    loading: auth.status === 'loading',
  }
}
