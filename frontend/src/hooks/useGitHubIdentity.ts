import { useCallback, useEffect, useState } from 'react'
import { apiJSON, HttpError } from '../lib/apiClient'
import type { GitHubIdentityStatus } from '../types'

export type GitHubIdentityState =
  | { status: 'loading' }
  | { status: 'ready'; data: GitHubIdentityStatus }
  | { status: 'error'; error: string }

export interface UseGitHubIdentity {
  state: GitHubIdentityState
  refresh: () => void
}

/**
 * useGitHubIdentity reads the caller's GitHub identity-binding status for an
 * org (GET /api/orgs/{org}/identity/github) — the data the onboarding gate
 * blocks on. Re-fetches whenever orgId changes; a null orgId stays at
 * 'loading' (the active org hasn't resolved yet).
 *
 * Errors surface as a retryable 'error' state rather than being swallowed:
 * the gate must not silently let an unconnected user through on a transient
 * read failure. (A 401 is separately funneled to AuthContext by apiClient,
 * which redirects to /login.)
 */
export function useGitHubIdentity(orgId: string | null): UseGitHubIdentity {
  const [state, setState] = useState<GitHubIdentityState>({ status: 'loading' })

  const fetchStatus = useCallback(
    async (signal?: AbortSignal) => {
      if (!orgId) return
      setState({ status: 'loading' })
      try {
        const data = await apiJSON<GitHubIdentityStatus>('/identity/github', { org: orgId, signal })
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
