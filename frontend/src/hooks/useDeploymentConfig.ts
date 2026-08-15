import { useState, useEffect } from 'react'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import type { DeploymentConfig, MeResponse, TeamMember, TeamMembersResponse } from '../types'

/** In-flight Promise dedup for /api/config. The endpoint is read once at
 *  FE boot by AuthGate, but multiple components may still race to mount
 *  before the result lands; sharing one round-trip is courteous. */
let configInFlight: Promise<DeploymentConfig> | null = null

function loadConfig(): Promise<DeploymentConfig> {
  if (configInFlight) return configInFlight
  configInFlight = apiJSON<DeploymentConfig>('/api/config').finally(() => {
    configInFlight = null
  })
  return configInFlight
}

/** useDeploymentConfig fetches /api/config on every mount with in-flight
 *  dedup. The response carries only deployment_mode — AuthGate's
 *  pre-login signal for which login flow to render. */
export function useDeploymentConfig(): {
  config: DeploymentConfig | null
  loading: boolean
  error: string | null
} {
  const [config, setConfig] = useState<DeploymentConfig | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    loadConfig()
      .then((data) => {
        if (!cancelled) {
          setConfig(data)
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(httpErrorMessage(err, 'Could not load the deployment configuration.'))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { config, loading, error }
}

/** In-flight Promise dedup for /api/me. Multiple IdentityListField
 *  mounts in one render (e.g. a review-event predicate editing both
 *  author_in and reviewer_in) share one round-trip. No persistent
 *  cache — identity fields can change mid-session (user opens editor,
 *  configures Jira via Settings, returns), and caching would shadow
 *  real changes until reload. */
let meInFlight: Promise<MeResponse | null> | null = null

function loadMe(): Promise<MeResponse | null> {
  if (meInFlight) return meInFlight
  // `allow: [401]` is doing two things: a signed-out session resolves here
  // instead of throwing, and — the load-bearing half — it keeps this read out
  // of the global re-auth funnel. "Not signed in" is the answer this hook
  // exists to return, so firing the handler would turn every render of a
  // logged-out surface into a redirect.
  meInFlight = apiFetch('/api/me', { allow: [401] })
    .then((r) => (r.status === 401 ? null : (r.json() as Promise<MeResponse>)))
    .finally(() => {
      meInFlight = null
    })
  return meInFlight
}

/** useMe fetches /api/me on each mount with in-flight dedup. Works in
 *  both modes (local mode synthesizes from the users row, multi mode
 *  reads via JWT-context query). Returns `me: null` on 401 so callers
 *  can render a "not signed in" state rather than crashing on missing
 *  identity.
 *
 *  Distinct from AuthContext.useAuth: this hook works in both modes
 *  without depending on the AuthProvider (which only mounts in multi
 *  mode). Components that render in both modes — e.g. IdentityListField
 *  inside the predicate editor — use this hook. */
export function useMe(): {
  me: MeResponse | null
  loading: boolean
  error: string | null
} {
  const [me, setMe] = useState<MeResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    loadMe()
      .then((data) => {
        if (!cancelled) {
          setMe(data)
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(httpErrorMessage(err, 'Could not load your account.'))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { me, loading, error }
}

/** useTeamMembers fetches the roster for the active user's team. Used
 *  by Variant B (multi-select) of the identity-allowlist field. Fetched
 *  fresh on each component mount — the roster is mutable during a
 *  session but cache invalidation isn't worth the websocket plumbing
 *  for v1. The list is usually small (single digits to low tens). */
export function useTeamMembers(): {
  members: TeamMember[]
  loading: boolean
  error: string | null
} {
  const [members, setMembers] = useState<TeamMember[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    apiJSON<TeamMembersResponse>('/api/team/members')
      .then((data) => {
        if (!cancelled) {
          setMembers(data.members || [])
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(httpErrorMessage(err, 'Could not load the team roster.'))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { members, loading, error }
}
