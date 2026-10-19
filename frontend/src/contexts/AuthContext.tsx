/* eslint-disable react-refresh/only-export-components --
   This file is a context+hooks pair. The Provider component and the
   useAuth / useOptionalAuth hooks belong together — splitting them
   into separate files would just trade one set of imports for
   another. The HMR boundary trade-off is acceptable for stable
   plumbing. */
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import {
  apiFetch,
  apiJSON,
  HttpError,
  setSSORequiredHandler,
  setUnauthHandler,
} from '../lib/apiClient'
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
  /** Re-reads /api/me for a caller that needs the answer now — after a write
   *  that changed it, or from a Retry button. A failure other than 401 moves
   *  to 'error', which the gates show with a Retry. */
  refresh: () => Promise<void>
  /** Re-reads /api/me in the background — on a timer, a focus event, or a
   *  refusal from some other call. A failure other than 401 keeps what is
   *  already loaded: the user is looking at something that works, and an
   *  error screen nothing re-reads would replace it until they reload. */
  revalidate: () => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

/** The shortest gap between two /api/me re-reads triggered by SSO_REQUIRED. */
const SSO_REFRESH_MIN_INTERVAL_MS = 5000

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

  // readMe is the one /api/me read behind refresh and revalidate. They differ
  // only in what a failure other than 401 does: refresh reports it, revalidate
  // keeps the session already loaded and leaves the next re-read to recover.
  const readMe = useCallback(async (onFailure: 'report' | 'keep') => {
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
      if (onFailure === 'keep') {
        console.warn('[auth] background /api/me read failed; keeping the loaded session', err)
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
  const refresh = useCallback(() => readMe('report'), [readMe])
  const revalidate = useCallback(() => readMe('keep'), [readMe])

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

  // Register the apiClient SSO_REQUIRED hook. A 403 SSO_REQUIRED means an
  // org this session belongs to now requires a sign-in the session did not
  // use, so /api/me's per-org sso_required is stale: re-read it, and
  // MultiAuthGate replaces the org's pages with its sign-in. Every read on
  // the page may fail at once, so one re-read serves the burst, and a
  // re-read that answers "not required" is not repeated for a short window,
  // which keeps a disagreement between the two reads from looping. It is a
  // background re-read: a failure keeps the page the user is on.
  const ssoRefresh = useRef<{ inFlight: boolean; lastAt: number }>({ inFlight: false, lastAt: 0 })
  useEffect(() => {
    setSSORequiredHandler(() => {
      const state = ssoRefresh.current
      if (state.inFlight || Date.now() - state.lastAt < SSO_REFRESH_MIN_INTERVAL_MS) return
      state.inFlight = true
      void revalidate().finally(() => {
        state.inFlight = false
        state.lastAt = Date.now()
      })
    })
    return () => setSSORequiredHandler(null)
  }, [revalidate])

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
        // Active org may be gone, or now requires SSO — re-fetch /api/me so
        // the gates re-route (a remaining org, the org's sign-in, or
        // /onboarding if none are left). A background re-read, so a failure
        // keeps the page rather than replacing it with the error screen.
        void revalidate()
      }
    })
    return () => setWsAuthCloseHandler(null)
  }, [revalidate])

  const value = useMemo<AuthContextValue>(
    () => ({
      status,
      me,
      orgs: me?.orgs ?? [],
      serverActiveOrgId: me?.active_org_id ?? null,
      error,
      refresh,
      revalidate,
      logout,
    }),
    [status, me, error, refresh, revalidate, logout],
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
