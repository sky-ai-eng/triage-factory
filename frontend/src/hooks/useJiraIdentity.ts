import { useCallback, useEffect, useState } from 'react'
import { apiJSON, HttpError } from '../lib/apiClient'
import type { JiraIdentityStatus } from '../types'

export type JiraIdentityState =
  | { status: 'loading' }
  | { status: 'ready'; data: JiraIdentityStatus }
  | { status: 'error'; error: string }

export interface UseJiraIdentity {
  state: JiraIdentityState
  refresh: () => void
}

/**
 * useJiraIdentity reads the caller's Jira access-binding status for an org
 * (GET /api/orgs/{org}/identity/jira) — the Jira sibling of useGitHubIdentity.
 * `connected` reflects a stored per-user credential (Jira's user level holds
 * access, not just identity). Re-fetches whenever orgId changes; a null orgId
 * stays at 'loading' (the active org hasn't resolved yet).
 *
 * Errors surface as a retryable 'error' state rather than being swallowed: the
 * gate must not silently let an unconnected user through on a transient read
 * failure. (A 401 is separately funneled to AuthContext by apiClient, which
 * redirects to /login.)
 */
export function useJiraIdentity(orgId: string | null): UseJiraIdentity {
  const [state, setState] = useState<JiraIdentityState>({ status: 'loading' })

  const fetchStatus = useCallback(
    async (signal?: AbortSignal) => {
      if (!orgId) return
      setState({ status: 'loading' })
      try {
        const data = await apiJSON<JiraIdentityStatus>('/identity/jira', { org: orgId, signal })
        if (signal?.aborted) return
        setState({ status: 'ready', data })
      } catch (err) {
        if (signal?.aborted) return
        const msg =
          err instanceof HttpError
            ? `HTTP ${err.status}`
            : err instanceof Error
              ? err.message
              : String(err)
        setState({ status: 'error', error: msg })
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
