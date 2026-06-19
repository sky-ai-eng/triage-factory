/* eslint-disable react-refresh/only-export-components --
   This file is a context+hooks pair. The Provider component and the
   useAuth / useOptionalAuth hooks belong together — splitting them
   into separate files would just trade one set of imports for
   another. The HMR boundary trade-off is acceptable for stable
   plumbing. */
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { apiFetch, apiJSON, HttpError, setUnauthHandler } from '../lib/apiClient'
import {
  setWsAuthCloseHandler,
  WS_CLOSE_MEMBERSHIP_CHANGED,
  WS_CLOSE_SESSION_REVOKED,
} from '../hooks/useWebSocket'
import type { AuthOrg, MeResponse } from '../types'

/**
 * AuthContext owns the multi-mode session state machine.
 *
 * State transitions:
 *   loading  → authed   (GET /api/me returned 200 with body)
 *   loading  → unauth   (GET /api/me returned 401)
 *   loading  → error    (network failure or non-401 5xx)
 *   authed   → unauth   (401 from any subsequent API call via setUnauthHandler)
 *   authed   → unauth   (logout completed)
 *
 * Only mounted in multi mode (see main.tsx). Local mode never
 * constructs this provider — the existing keychain AuthGate handles
 * its own flow.
 *
 * 401 funneling: AuthContext registers a global handler on apiClient
 * so any 401 from any endpoint flips state to 'unauth' and lets the
 * router redirect to /login. Avoids each consumer having to wire its
 * own 401 → reauth logic.
 */

export type AuthStatus = 'loading' | 'authed' | 'unauth' | 'error'

interface AuthContextValue {
  status: AuthStatus
  /** Full /api/me payload. The context value also exposes orgs +
   *  serverActiveOrgId as top-level selectors for consumers that don't
   *  need the rest of the user; both are derived from `me`. */
  me: MeResponse | null
  orgs: AuthOrg[]
  /** Session's active org id as reported by /api/me. Null when absent
   *  from the response. OrgContext prefers this over localStorage so
   *  a cross-tab switch (POST /api/me/active-org) wins over stale
   *  client state. */
  serverActiveOrgId: string | null
  error: string | null
  refresh: () => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>('loading')
  const [me, setMe] = useState<MeResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  // statusRef tracks the latest status so the 401 handler (a closure
  // captured at mount) can branch on it without re-binding every
  // render. Without this, an unauth flip-while-already-unauth would
  // log a redundant transition.
  const statusRef = useRef<AuthStatus>('loading')
  useEffect(() => {
    statusRef.current = status
  }, [status])

  const refresh = useCallback(async () => {
    try {
      const data = await apiJSON<MeResponse>('/api/me')
      setMe(data)
      setError(null)
      setStatus('authed')
    } catch (err) {
      if (err instanceof HttpError && err.status === 401) {
        setMe(null)
        setError(null)
        setStatus('unauth')
        return
      }
      // Network failure or unexpected status — surface as 'error'
      // rather than 'unauth' so the UI can distinguish transient
      // server issues from genuine not-logged-in state.
      setMe(null)
      setError(err instanceof Error ? err.message : String(err))
      setStatus('error')
    }
  }, [])

  const logout = useCallback(async () => {
    try {
      await apiFetch('/api/auth/logout', { method: 'POST' })
    } catch {
      // Best-effort. Even if the server call fails, the user clicked
      // logout — we should reflect that locally. The cookie may
      // persist; the next request will surface the failure.
    }
    setMe(null)
    setError(null)
    setStatus('unauth')
  }, [])

  // Initial /api/me fetch on mount. refresh() handles its own
  // setState calls; the effect just kicks the first call. The lint
  // rule is meant to catch render-loop setState; an event-driven
  // fetch-on-mount is the canonical safe pattern.
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void refresh()
  }, [refresh])

  // Register the apiClient 401 hook. A 401 from any API call flips
  // us to 'unauth' so the router can redirect. statusRef.current
  // guards against redundant transitions when we're already unauth.
  useEffect(() => {
    setUnauthHandler(() => {
      if (statusRef.current === 'unauth') return
      setMe(null)
      setStatus('unauth')
    })
    return () => setUnauthHandler(null)
  }, [])

  // Register the websocket auth-kick hook (TFAC-75). The server closes a
  // socket when its session is revoked or the user loses the org it's
  // scoped to; the hook maps that to the same state transitions a 401
  // (revoked) or an org change (membership) would drive — so revocation
  // takes effect without waiting for the next HTTP round-trip.
  useEffect(() => {
    setWsAuthCloseHandler((code) => {
      if (code === WS_CLOSE_SESSION_REVOKED) {
        // Mirror a 401: drop the user and let MultiAuthGate send us to
        // /login. Guard against a redundant flip if we're already unauth.
        if (statusRef.current === 'unauth') return
        setMe(null)
        setStatus('unauth')
      } else if (code === WS_CLOSE_MEMBERSHIP_CHANGED) {
        // Active org may be gone — re-fetch /api/me so the gates re-route
        // (a remaining org, or /onboarding if none are left).
        void refresh()
      }
    })
    return () => setWsAuthCloseHandler(null)
  }, [refresh])

  const value = useMemo<AuthContextValue>(
    () => ({
      status,
      me,
      orgs: me?.orgs ?? [],
      serverActiveOrgId: me?.active_org_id ?? null,
      error,
      refresh,
      logout,
    }),
    [status, me, error, refresh, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const v = useContext(AuthContext)
  if (!v) {
    throw new Error('useAuth called outside AuthProvider (local mode does not mount it)')
  }
  return v
}

/** useOptionalAuth returns null when no AuthProvider is mounted.
 *  Components rendered in both modes (e.g. Shell) use this to branch
 *  on whether multi-mode auth state is available. */
export function useOptionalAuth(): AuthContextValue | null {
  return useContext(AuthContext)
}
