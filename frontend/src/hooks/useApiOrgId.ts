import { useOptionalAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'

/**
 * useApiOrgId returns the org id to put in an `/api/orgs/{org_id}/…` path, or
 * null while multi mode is still resolving one.
 *
 * The two modes answer it differently and neither can answer for the other:
 * multi mode resolves the active org from the URL / session / storage via
 * OrgContext, while local mode never mounts that provider at all and always
 * addresses the synthetic sentinel tenant. A component that reads an org-scoped
 * route directly — rather than being handed an orgId by a page that already
 * resolved one — needs both arms, and this is them.
 *
 * A null result means "not yet", not "none": callers should hold their fetch
 * rather than fall back to some other org's data.
 */
export function useApiOrgId(): string | null {
  // useOptionalAuth is null in local mode (no AuthProvider) — the degenerate
  // single-user world, which is also the one with a fixed org id.
  const auth = useOptionalAuth()
  const ctxOrgId = useActiveOrgId()
  return auth === null ? LOCAL_DEFAULT_ORG_ID : ctxOrgId
}
