/* eslint-disable react-refresh/only-export-components --
   This file is a context+hooks pair. The Provider component and the
   useActiveOrgId / useOrgContext hooks belong together. HMR
   boundary trade-off is acceptable for stable plumbing. */
import { createContext, useContext, useEffect, useMemo, useState } from 'react'
import { useLocation } from 'react-router'
import { useOptionalAuth } from './AuthContext'

/**
 * OrgContext owns the "which org is the user looking at right now"
 * decision. Only mounted in multi mode (after AuthProvider).
 *
 * Source of truth precedence:
 *   1. URL path — if at /orgs/:org_id/*, that org_id wins.
 *   2. /api/me's active_org_id — the session's server-side choice.
 *   3. localStorage[ACTIVE_ORG_KEY] — sticky pick from last session.
 *   4. First org in the user's org list.
 *
 * The URL is authoritative when present so deep links survive across
 * sessions. The server value beats localStorage so a switch made in
 * another tab (POST /api/me/active-org persists on the session row)
 * wins over this tab's cached pick on the next /api/me load.
 *
 * Stale localStorage is silently ignored: if the stored org_id isn't
 * in the current user's org list, we fall through. Avoids surfacing a
 * "you don't have access" error for an org the user used to belong to.
 */

const ACTIVE_ORG_KEY = 'triagefactory.activeOrgId'

interface OrgContextValue {
  activeOrgId: string | null
  /** True when the URL contains an /orgs/:org_id the authenticated user
   *  is not a member of. AuthGate uses this to redirect to the active
   *  org rather than silently showing a different org's data. */
  urlOrgInvalid: boolean
  /** Persists a new active org. Doesn't navigate — the caller (e.g.
   *  OrgPicker) handles the route swap. */
  setActiveOrgId: (id: string) => void
}

const OrgContext = createContext<OrgContextValue | null>(null)

function readStored(): string | null {
  try {
    return window.localStorage.getItem(ACTIVE_ORG_KEY)
  } catch {
    return null
  }
}

function writeStored(id: string | null) {
  try {
    if (id) {
      window.localStorage.setItem(ACTIVE_ORG_KEY, id)
    } else {
      window.localStorage.removeItem(ACTIVE_ORG_KEY)
    }
  } catch {
    // localStorage can throw in private-mode Safari etc. Silent
    // failure is fine — context still works in-memory.
  }
}

/** Extracts an org_id from a path like '/orgs/abc/triage'. Returns
 *  null when the path doesn't start with /orgs/<id>. */
function orgIdFromPath(pathname: string): string | null {
  const match = pathname.match(/^\/orgs\/([^/]+)/)
  return match ? match[1] : null
}

export function OrgProvider({ children }: { children: React.ReactNode }) {
  const auth = useOptionalAuth()
  const location = useLocation()
  const [storedId, setStoredId] = useState<string | null>(() => readStored())

  const urlOrgId = orgIdFromPath(location.pathname)

  // Resolve the active org: URL wins, then the session's server-side
  // active_org_id, then localStorage, then first org.
  const activeOrgId = useMemo(() => {
    if (!auth) return null
    const validIds = new Set(auth.orgs.map((o) => o.id))
    if (urlOrgId && validIds.has(urlOrgId)) return urlOrgId
    if (auth.serverActiveOrgId && validIds.has(auth.serverActiveOrgId))
      return auth.serverActiveOrgId
    if (storedId && validIds.has(storedId)) return storedId
    if (auth.orgs.length > 0) return auth.orgs[0].id
    return null
  }, [auth, urlOrgId, storedId])

  // Detect when the URL names an org the authenticated user isn't in.
  // Only meaningful once we have a confirmed org list (authed state).
  const urlOrgInvalid = useMemo(() => {
    if (!auth || !urlOrgId) return false
    if (auth.status !== 'authed') return false
    return !auth.orgs.some((o) => o.id === urlOrgId)
  }, [auth, urlOrgId])

  // Sync localStorage with the resolved active org so a fresh tab
  // picks it up. Skip when null (no orgs yet or pre-auth).
  useEffect(() => {
    if (activeOrgId) writeStored(activeOrgId)
  }, [activeOrgId])

  const value = useMemo<OrgContextValue>(
    () => ({
      activeOrgId,
      urlOrgInvalid,
      setActiveOrgId: (id: string) => {
        writeStored(id)
        setStoredId(id)
      },
    }),
    [activeOrgId, urlOrgInvalid],
  )

  return <OrgContext.Provider value={value}>{children}</OrgContext.Provider>
}

export function useActiveOrgId(): string | null {
  const v = useContext(OrgContext)
  return v ? v.activeOrgId : null
}

export function useOrgContext(): OrgContextValue {
  const v = useContext(OrgContext)
  if (!v) {
    throw new Error('useOrgContext called outside OrgProvider (local mode does not mount it)')
  }
  return v
}
