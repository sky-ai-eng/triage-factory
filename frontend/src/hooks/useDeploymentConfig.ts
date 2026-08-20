import { useState, useEffect } from 'react'
import { apiFetch, apiJSON, httpErrorMessage, readJSON } from '../lib/apiClient'
import type { DeploymentConfig, MeResponse, TeamBot, TeamMember } from '../types'
import { fetchTeamRoster, type TeamRoster } from '../lib/teamRoster'
import { pickerDefault, useTeams } from './useTeams'

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
    .then((r) => (r.status === 401 ? null : readJSON<MeResponse>(r, '/api/me')))
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

/** useTeamMembers fetches the roster of the team the user is ACTING as — the
 *  same team the shell around the consumer resolves (last-written, else the
 *  first team). Resolving it here, from the shared teams cache, is what keeps a
 *  roster and the page it renders inside naming the same team for a user who
 *  belongs to more than one.
 *
 *  Used by Variant B (multi-select) of the identity-allowlist field and by the
 *  board's assignee picker. Fetched fresh whenever the acting team resolves or
 *  changes — the roster is mutable during a session, and cache invalidation
 *  isn't worth the websocket plumbing while the list is this small (single
 *  digits to low tens).
 *
 *  `bot` is the raw roster bot; a caller that OFFERS it (rather than describing
 *  it) filters through `usableBot`. */
export function useTeamMembers(): {
  members: TeamMember[]
  bot: TeamBot | null
  loading: boolean
  error: string | null
} {
  const { teams, lastActingTeamId, loaded } = useTeams()
  const teamId = loaded ? pickerDefault(teams, lastActingTeamId) : ''
  const [roster, setRoster] = useState<TeamRoster | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!teamId) return
    let cancelled = false
    fetchTeamRoster(teamId)
      .then((data) => {
        if (cancelled) return
        setRoster(data)
        // Clearing on success, rather than at dispatch, keeps this effect from
        // setting state synchronously — and a prior failure stays visible
        // until a later read actually succeeds.
        setError(null)
      })
      .catch((err) => {
        if (!cancelled) setError(httpErrorMessage(err, 'Could not load the team roster.'))
      })
    return () => {
      cancelled = true
    }
  }, [teamId])

  return {
    members: roster?.members ?? [],
    bot: roster?.bot ?? null,
    // Still loading while the teams cache is resolving the acting team: a
    // consumer that branches on roster SIZE (the predicate editor picks its
    // variant that way) must not read an empty roster as "solo team". A user
    // with no team at all resolves to '' and is not loading — there is
    // nothing further to wait for, and neither is there after a failure.
    loading: !loaded || (teamId !== '' && roster === null && error === null),
    error,
  }
}
