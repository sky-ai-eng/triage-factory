import { useSyncExternalStore, useCallback, useEffect, useRef, useState } from 'react'
import type { TeamsResponse, TeamSummary } from '../types'
import { readError } from '../lib/api'

// Shared store for GET /api/teams — the data source for both multi-team
// selectors (the per-page read filter and the write-time picker) plus
// the org-admin "add team" control. A single module-level cache
// means every selector across pages and modals shares one round-trip, and
// a mutation (set sticky default, create a team) propagates to all
// mounted selectors at once.
//
// The endpoint is session-org-scoped (the backend reads the active org
// from the session), so switching orgs must invalidate the cache —
// OrgPicker calls invalidateTeams() on switch.

type State = {
  teams: TeamSummary[]
  preferredTeamId: string
  loaded: boolean
  loading: boolean
  error: string | null
}

let state: State = { teams: [], preferredTeamId: '', loaded: false, loading: false, error: null }
const listeners = new Set<() => void>()
let inFlight: Promise<void> | null = null

function setState(next: Partial<State>) {
  state = { ...state, ...next }
  for (const l of listeners) l()
}

function load(): Promise<void> {
  if (inFlight) return inFlight
  setState({ loading: true, error: null })
  inFlight = fetch('/api/teams')
    .then(async (r) => {
      if (!r.ok) throw new Error(await readError(r, 'Failed to load teams'))
      const data = (await r.json()) as TeamsResponse
      setState({
        teams: data.teams ?? [],
        preferredTeamId: data.preferred_team_id ?? '',
        loaded: true,
        loading: false,
        error: null,
      })
    })
    .catch((err) => {
      setState({ loading: false, error: err instanceof Error ? err.message : String(err) })
    })
    .finally(() => {
      inFlight = null
    })
  return inFlight
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb)
  // Lazy first load: the first subscriber kicks the fetch.
  if (!state.loaded && !inFlight) void load()
  return () => {
    listeners.delete(cb)
  }
}

function getSnapshot(): State {
  return state
}

/** Drop the cached teams and reload for any mounted subscriber. Call on
 *  active-org switch (the teams list is org-scoped) — OrgPicker does. */
export function invalidateTeams(): void {
  state = { teams: [], preferredTeamId: '', loaded: false, loading: false, error: null }
  if (listeners.size > 0) {
    void load()
  } else {
    // No subscribers right now; emit so any future getSnapshot sees the
    // reset, and let the next subscribe() trigger the reload.
    for (const l of listeners) l()
  }
}

export interface UseTeams {
  teams: TeamSummary[]
  /** The sticky default, or '' when unset / no longer a member team. */
  preferredTeamId: string
  /** True when the viewer belongs to ≥2 teams — the gate that decides
   *  whether any team control renders at all. */
  multi: boolean
  loading: boolean
  error: string | null
  refresh: () => Promise<void>
  /** Persist the sticky default (empty clears it). Optimistically updates
   *  the shared store so seeded selectors react immediately. */
  setPreferred: (teamId: string) => Promise<void>
  /** Org-admin "add team". Resolves to the created team after the store
   *  has been refreshed (so the new team is in `teams`). */
  createTeam: (name: string) => Promise<TeamSummary>
}

export function useTeams(): UseTeams {
  const snap = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)

  const refresh = useCallback(() => {
    state = { ...state, loaded: false }
    return load()
  }, [])

  const setPreferred = useCallback(async (teamId: string) => {
    const res = await fetch('/api/me/preferred-team', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
      body: JSON.stringify({ team_id: teamId }),
    })
    if (!res.ok) throw new Error(await readError(res, 'Failed to save default team'))
    setState({ preferredTeamId: teamId })
  }, [])

  const createTeam = useCallback(async (name: string): Promise<TeamSummary> => {
    const res = await fetch('/api/teams', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
      body: JSON.stringify({ name }),
    })
    if (!res.ok) throw new Error(await readError(res, 'Failed to create team'))
    const created = (await res.json()) as TeamSummary
    state = { ...state, loaded: false }
    await load()
    return created
  }, [])

  return {
    teams: snap.teams,
    preferredTeamId: snap.preferredTeamId,
    multi: snap.teams.length >= 2,
    loading: snap.loading,
    error: snap.error,
    refresh,
    setPreferred,
    createTeam,
  }
}

// useTeamFilter is the shared per-page read-filter state: '' means "all
// my teams" (the union), a team id narrows the page. It seeds once from
// the sticky default when the teams load (the issue's "sticky default
// seeds the filter"), then stays under user control — re-selecting "all"
// is honored and never re-seeded. Pages thread the value into their read
// requests as ?team_id and render <TeamScopeSelect> bound to it.
export function useTeamFilter(): [string, (v: string) => void] {
  const { preferredTeamId, loading } = useTeams()
  const [filter, setFilter] = useState('')
  const seeded = useRef(false)
  useEffect(() => {
    if (seeded.current || loading) return
    seeded.current = true
    // Seed-once from async-loaded sticky default — the canonical
    // "sync state from an external system after it loads" effect.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (preferredTeamId) setFilter(preferredTeamId)
  }, [loading, preferredTeamId])
  return [filter, setFilter]
}

// readRecentTeam/writeRecentTeam track the most-recently-written team in
// localStorage — the soft fallback the write picker seeds to when there's
// no sticky default (the issue's "sticky default → else most-recently-
// written team"). Device-local by design: the durable cross-device
// default is the server-side sticky preference; most-recent is just a
// convenience so a picker reopens on the team you last wrote to. A single
// key is fine across orgs because pickerDefault re-validates the value
// against the current org's teams and discards a stale id.
const RECENT_TEAM_KEY = 'tf.recentTeam'

export function readRecentTeam(): string {
  try {
    return localStorage.getItem(RECENT_TEAM_KEY) ?? ''
  } catch {
    return ''
  }
}

export function writeRecentTeam(teamId: string): void {
  if (!teamId) return
  try {
    localStorage.setItem(RECENT_TEAM_KEY, teamId)
  } catch {
    // localStorage unavailable (private mode quota, etc.) — most-recent
    // is a best-effort convenience, so a write failure is non-fatal.
  }
}

// pickerDefault computes the write picker's seed selection: sticky
// default → most-recently-written → first (oldest) team. Each candidate
// is validated against the current team set so a stale sticky/recent id
// (team deleted, or from another org) falls through. Returns '' only when
// the viewer has no teams.
export function pickerDefault(teams: TeamSummary[], preferredTeamId: string): string {
  if (teams.length === 0) return ''
  const ids = new Set(teams.map((t) => t.id))
  if (preferredTeamId && ids.has(preferredTeamId)) return preferredTeamId
  const recent = readRecentTeam()
  if (recent && ids.has(recent)) return recent
  return teams[0].id
}
