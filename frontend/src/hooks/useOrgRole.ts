import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'

// Org roles that count as admin — owner + admin, mirroring the backend's
// tf.user_is_org_admin (and requireOrgAdminRole). Member is not admin.
const ORG_ADMIN_ROLES = new Set(['owner', 'admin'])

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
  // The active org's ship-dark marketplace toggle (org_settings.marketplace_
  // enabled, TFAC-535/537/539) — false in local mode, while loading, or with
  // no resolvable active org. Gates the Marketplace nav entry so a member
  // never lands on a page whose every request 404s.
  marketplaceEnabled: boolean
}

// useOrgRole reports the viewer's role in the active org. Safe in both modes:
// useOptionalAuth + useActiveOrgId both return null without their providers
// (local mode), so it simply reports "no role / not admin" there. Mirrors the
// active-org resolution in useTemplateScope — when no org is explicit but the
// viewer belongs to exactly one, that org is implied.
export function useOrgRole(): OrgRoleState {
  const auth = useOptionalAuth()
  const activeOrgId = useActiveOrgId()

  if (!auth) {
    return { role: null, isAdmin: false, loading: false, marketplaceEnabled: false }
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
    marketplaceEnabled: !!activeOrg?.marketplace_enabled,
  }
}
