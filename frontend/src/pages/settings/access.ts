import type { AuthOrg } from '../../types'

/** Org roles that grant write access to org-scope settings. Mirrors the
 *  backend's requireOrgAdminRole (owner + admin qualify). */
const ORG_ADMIN_ROLES = new Set(['owner', 'admin'])

export interface AccessInputs {
  /** True when no AuthProvider is mounted — i.e. local mode, which is the
   *  degenerate single-org/single-team/single-user world. Always N=1. */
  isLocal: boolean
  /** The orgs the user belongs to (from /api/me). Drives orgs.length. */
  orgs: AuthOrg[]
  /** Session active org id (/api/me.active_org_id), used to pick which
   *  org's role gates the Workspace tab. */
  activeOrgId: string | null
  /** Member counts from the scope GET responses (not /api/me). */
  orgMemberCount: number
  teamMemberCount: number
  /** The caller's role in the active team ("admin" | "member" | "").
   *  Empty when the caller isn't a team member. */
  teamRole: string
}

export interface Access {
  /** Render the flat single-page layout instead of tabs. */
  isN1: boolean
  /** Workspace (org-scope) write fields enabled. */
  isOrgAdmin: boolean
  /** Team-scope write fields enabled. Strict: mirrors the backend's
   *  tf.user_is_team_admin, which only accepts memberships.role='admin'.
   *  Org admins do NOT inherit team-admin — folding it in here would show
   *  enabled fields that then 403 on save. */
  isTeamAdmin: boolean
}

export function computeAccess(i: AccessInputs): Access {
  if (i.isLocal) {
    return { isN1: true, isOrgAdmin: true, isTeamAdmin: true }
  }
  const isN1 = i.orgs.length === 1 && i.orgMemberCount === 1 && i.teamMemberCount === 1

  const activeOrg = i.activeOrgId
    ? i.orgs.find((o) => o.id === i.activeOrgId)
    : i.orgs.length === 1
      ? i.orgs[0]
      : undefined

  return {
    isN1,
    isOrgAdmin: !!activeOrg && ORG_ADMIN_ROLES.has(activeOrg.role),
    isTeamAdmin: i.teamRole === 'admin',
  }
}
