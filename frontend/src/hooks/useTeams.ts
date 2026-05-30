import { useSyncExternalStore, useCallback, useEffect, useState } from 'react'
import type { TeamsResponse, TeamSummary } from '../types'
import { readError } from '../lib/api'

// Shared store for GET /api/teams — the data source for the multi-team
// selectors (the per-page read filter and the write-time picker) plus the
// org-admin "add team" control. A single module-level cache means every
// selector across pages and modals shares one round-trip, and a mutation
// (create a team, record a write) propagates to all mounted selectors.
//
// Reads and writes are deliberately decoupled. A read is a *multi-team
// view scope* (the board can show many teams at once); a write is always
// a *single-team ownership stamp*. They share no primitive: the read
// filter is device-local view state (localStorage, see useTeamFilter),
// while the write default is preferred_team_id — the last team the user
// wrote to, maintained server-side by the acting-team resolver and only
// *read* here to seed the picker.
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
    for (const l of listeners) l()
  }
}

/** Record that a write just landed on teamId. The backend's acting-team
 *  resolver has already persisted it as the user's last-written default;
 *  this keeps the shared cache in sync so the next write picker (or the
 *  modal still open) seeds to it without a refetch. */
export function noteWrittenTeam(teamId: string): void {
  if (!teamId || teamId === state.preferredTeamId) return
  setState({ preferredTeamId: teamId })
}

export interface UseTeams {
  teams: TeamSummary[]
  /** The last-written team (write-default seed), or '' when unset / no
   *  longer a member team. Read-only here — it's maintained server-side. */
  preferredTeamId: string
  /** True when the viewer belongs to ≥2 teams — the gate that decides
   *  whether any team control renders at all. */
  multi: boolean
  loading: boolean
  /** True once the first /api/teams fetch has resolved — useTeamFilter
   *  uses it to prune stale ids only after the real team set is known. */
  loaded: boolean
  error: string | null
  refresh: () => Promise<void>
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
    loaded: snap.loaded,
    error: snap.error,
    refresh,
    createTeam,
  }
}

// useTeamFilter is the shared per-page read-filter state — a *multi-team*
// view scope. The empty set means "all my teams" (the union); a non-empty
// set narrows the page to those teams. It is device-local view state
// (persisted in localStorage), deliberately NOT seeded from the write
// default: reads and writes are decoupled, so the board opening on "all"
// is independent of which team you last wrote to. Pages thread the value
// into their reads as repeated ?team_id params and bind <TeamScopeSelect>.
const TEAM_FILTER_KEY = 'tf.teamFilter'

function readStoredFilter(): string[] {
  try {
    const raw = localStorage.getItem(TEAM_FILTER_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === 'string') : []
  } catch {
    return []
  }
}

// teamFilterQuery renders a multi-team read filter as repeated team_id
// query params ("team_id=A&team_id=B"), or "" for the empty (all-teams)
// set. Callers prefix it with "?" or "&" as appropriate.
export function teamFilterQuery(teamIds: string[]): string {
  if (!teamIds || teamIds.length === 0) return ''
  return teamIds.map((id) => `team_id=${encodeURIComponent(id)}`).join('&')
}

export function useTeamFilter(): [string[], (next: string[]) => void] {
  const { teams, loaded } = useTeams()
  const [filter, setFilterState] = useState<string[]>(() => readStoredFilter())

  // Once the teams load, drop any stored ids that aren't current teams
  // (team deleted, or left over from another org). Validating against the
  // live set keeps a stale selection from silently emptying the board.
  useEffect(() => {
    if (!loaded) return
    const ids = new Set(teams.map((t) => t.id))
    const pruned = filter.filter((id) => ids.has(id))
    if (pruned.length !== filter.length) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setFilterState(pruned)
    }
  }, [loaded, teams, filter])

  const setFilter = useCallback((next: string[]) => {
    setFilterState(next)
    try {
      localStorage.setItem(TEAM_FILTER_KEY, JSON.stringify(next))
    } catch {
      // localStorage unavailable — the filter still works for this
      // session; it just won't persist across reloads.
    }
  }, [])

  return [filter, setFilter]
}

// pickerDefault computes the write picker's seed selection: the last-
// written team (preferred_team_id, server-maintained) → else the first
// (oldest) team. The candidate is validated against the current team set
// so a stale id (team deleted, or from another org) falls through.
// Returns '' only when the viewer has no teams.
export function pickerDefault(teams: TeamSummary[], preferredTeamId: string): string {
  if (teams.length === 0) return ''
  const ids = new Set(teams.map((t) => t.id))
  if (preferredTeamId && ids.has(preferredTeamId)) return preferredTeamId
  return teams[0].id
}
