import { useCallback, useEffect, useState } from 'react'
import { apiJSON, httpErrorMessage } from '../lib/apiClient'
import type { LinearIdentityStatus } from '../types'

export type LinearIdentityState =
  | { status: 'loading' }
  | { status: 'ready'; data: LinearIdentityStatus }
  | { status: 'error'; error: string }

export interface UseLinearIdentity {
  state: LinearIdentityState
  refresh: () => void
}

/**
 * useLinearIdentity reads the caller's Linear access-binding status for an org
 * (GET /api/orgs/{org}/linear/identity) — the Linear sibling of useJiraIdentity.
 * `connected` reflects a stored per-user credential the server can act as the
 * user with. Re-fetches whenever orgId changes; a null orgId stays at
 * 'loading' (the active org hasn't resolved yet).
 *
 * Errors surface as a retryable 'error' state rather than being swallowed: the
 * gate must not silently let an unconnected user through on a transient read
 * failure. (A 401 is separately funneled to AuthContext by apiClient, which
 * redirects to /login.)
 */
export function useLinearIdentity(orgId: string | null): UseLinearIdentity {
  const [state, setState] = useState<LinearIdentityState>({ status: 'loading' })

  const fetchStatus = useCallback(
    async (signal?: AbortSignal) => {
      if (!orgId) return
      setState({ status: 'loading' })
      try {
        const data = await apiJSON<LinearIdentityStatus>(
          `/api/orgs/${encodeURIComponent(orgId)}/linear/identity`,
          { signal },
        )
        if (signal?.aborted) return
        setState({ status: 'ready', data })
      } catch (err) {
        if (signal?.aborted) return
        setState({
          status: 'error',
          error: httpErrorMessage(err, 'Could not load your Linear connection.'),
        })
      }
    },
    [orgId],
  )

  useEffect(() => {
    const ctrl = new AbortController()
    // Fetch-on-mount/orgId-change: fetchStatus owns its own setState calls;
    // the effect just kicks it. Same safe pattern AuthContext uses.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void fetchStatus(ctrl.signal)
    return () => ctrl.abort()
  }, [fetchStatus])

  const refresh = useCallback(() => {
    void fetchStatus()
  }, [fetchStatus])

  return { state, refresh }
}
