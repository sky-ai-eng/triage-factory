import { useSyncExternalStore, useCallback, useEffect, useRef, useState } from 'react'
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
// while the write default is last_acting_team_id — the last team the user
// wrote to, maintained server-side by the acting-team resolver and only
// *read* here to seed the picker.
//
// The endpoint is session-org-scoped (the backend reads the active org
// from the session), so switching orgs must invalidate the cache —
// OrgPicker calls invalidateTeams() on switch.

type State = {
  teams: TeamSummary[]
  lastActingTeamId: string
  loaded: boolean
  loading: boolean
  error: string | null
}

let state: State = { teams: [], lastActingTeamId: '', loaded: false, loading: false, error: null }
const listeners = new Set<() => void>()
let inFlight: Promise<void> | null = null
// Bumped whenever the cache is replaced wholesale (org-switch invalidate,
// refresh, post-create reload). A fetch captures the generation at dispatch
// and discards its result if a newer supersede has since bumped it — so a
// slow request for the *previous* org can't repopulate the cache after the
// active org changed (which would surface other-org team ids in the
// pickers and 400 on submit until a later refresh).
let generation = 0

function setState(next: Partial<State>) {
  state = { ...state, ...next }
  for (const l of listeners) l()
}

// supersedeInFlight invalidates any request currently in flight: it bumps
// the generation (so that request's response is dropped when it resolves)
// and clears inFlight so the next load() dispatches a genuinely new fetch
// instead of handing back the stale promise.
function supersedeInFlight() {
  generation++
  inFlight = null
}

function load(): Promise<void> {
  if (inFlight) return inFlight
  const gen = generation
  setState({ loading: true, error: null })
  inFlight = fetch('/api/teams')
    .then(async (r) => {
      if (!r.ok) throw new Error(await readError(r, 'Failed to load teams'))
      const data = (await r.json()) as TeamsResponse
      if (gen !== generation) return // superseded — a newer invalidation won
      setState({
        teams: data.teams ?? [],
        lastActingTeamId: data.last_acting_team_id ?? '',
        loaded: true,
        loading: false,
        error: null,
      })
    })
    .catch((err) => {
      if (gen !== generation) return
      setState({ loading: false, error: err instanceof Error ? err.message : String(err) })
    })
    .finally(() => {
      // Only clear if it's still ours — supersedeInFlight may have already
      // nulled it and a newer load() installed its own promise.
      if (gen === generation) inFlight = null
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
  // Supersede first so a still-pending prior-org request can't repopulate
  // the cache after we reset it (and so load() below actually dispatches a
  // new fetch rather than returning the stale inFlight promise).
  supersedeInFlight()
  state = { teams: [], lastActingTeamId: '', loaded: false, loading: false, error: null }
  // Reload for any mounted subscriber — load() re-notifies via setState.
  // With no subscribers there's nothing to notify: the reset above already
  // flips loaded=false, so the next subscribe() kicks a fresh load.
  if (listeners.size > 0) void load()
}

/** Record that a write just landed on teamId. The backend's acting-team
 *  resolver has already persisted it as the user's last-written default;
 *  this keeps the shared cache in sync so the next write picker (or the
 *  modal still open) seeds to it without a refetch. */
export function noteWrittenTeam(teamId: string): void {
  if (!teamId || teamId === state.lastActingTeamId) return
  setState({ lastActingTeamId: teamId })
}

export interface UseTeams {
  teams: TeamSummary[]
  /** The last-written team (write-default seed), or '' when unset / no
   *  longer a member team. Read-only here — it's maintained server-side. */
  lastActingTeamId: string
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
    // Supersede any in-flight fetch so a stale response can't land after
    // the caller explicitly asked for fresh data.
    supersedeInFlight()
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
    // Supersede so the reload reflects the just-created team rather than a
    // pre-create fetch that may still be in flight.
    supersedeInFlight()
    state = { ...state, loaded: false }
    await load()
    return created
  }, [])

  return {
    teams: snap.teams,
    lastActingTeamId: snap.lastActingTeamId,
    multi: snap.teams.length >= 2,
    loading: snap.loading,
    loaded: snap.loaded,
    error: snap.error,
    refresh,
    createTeam,
  }
}

// useTeamFilter is the per-page read-filter state — a *multi-team* view
// scope. The empty set means "all my teams" (the union); a non-empty set
// narrows the page to those teams. It is device-local view state
// (persisted in localStorage), deliberately NOT seeded from the write
// default: reads and writes are decoupled, so a page opening on "all" is
// independent of which team you last wrote to. Pages thread the value
// into their reads as repeated ?team_id params and bind <TeamScopeSelect>.
//
// pageKey namespaces the persistence per page (e.g. 'board', 'cards',
// 'factory'). Each page is its own lens — the board narrowed to team A
// must NOT silently hide other teams' work when you later open Cards or
// the Factory (Codex review on PR #263). The page-specific key keeps
// those scopes independent.
const TEAM_FILTER_KEY_PREFIX = 'tf.teamFilter.'

function readStoredFilter(pageKey: string): string[] {
  try {
    const raw = localStorage.getItem(TEAM_FILTER_KEY_PREFIX + pageKey)
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

export function useTeamFilter(pageKey: string): [string[], (next: string[]) => void] {
  const { teams, loaded } = useTeams()
  const [filter, setFilterState] = useState<string[]>(() => readStoredFilter(pageKey))

  // Once the teams load, drop any stored ids that aren't current teams
  // (team deleted, or left over from another org). Validating against the
  // live set keeps a stale selection from silently emptying the page.
  useEffect(() => {
    if (!loaded) return
    const ids = new Set(teams.map((t) => t.id))
    const pruned = filter.filter((id) => ids.has(id))
    if (pruned.length !== filter.length) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setFilterState(pruned)
    }
  }, [loaded, teams, filter])

  const setFilter = useCallback(
    (next: string[]) => {
      setFilterState(next)
      try {
        localStorage.setItem(TEAM_FILTER_KEY_PREFIX + pageKey, JSON.stringify(next))
      } catch {
        // localStorage unavailable — the filter still works for this
        // session; it just won't persist across reloads.
      }
    },
    [pageKey],
  )

  return [filter, setFilter]
}

// pickerDefault computes the write picker's seed selection: the last-
// written team (last_acting_team_id, server-maintained) → else the first
// (oldest) team. The candidate is validated against the current team set
// so a stale id (team deleted, or from another org) falls through.
// Returns '' only when the viewer has no teams.
export function pickerDefault(teams: TeamSummary[], lastActingTeamId: string): string {
  if (teams.length === 0) return ''
  const ids = new Set(teams.map((t) => t.id))
  if (lastActingTeamId && ids.has(lastActingTeamId)) return lastActingTeamId
  return teams[0].id
}

export interface WriteTeam {
  /** The acting team to submit. '' when the picker is hidden (≤1 team,
   *  resolved server-side to the sole team) — only meaningful once
   *  `ready`. */
  team: string
  setTeam: (id: string) => void
  /** Whether to render the <TeamPicker> (≥2 teams). */
  multi: boolean
  /** False until /api/teams has resolved. Create modals MUST disable
   *  their submit while !ready: before the teams list loads `multi` is
   *  false and the picker is hidden, so a multi-team user could otherwise
   *  submit team_id:'' in the cold-load window — landing the write on the
   *  wrong team (the stale last-acting team) or 400-ing (ambiguous) purely because
   *  the teams request was slow. Once ready, the picker is shown for
   *  multi-team users and the normal required-selection flow applies. */
  ready: boolean
}

// useWriteTeam is the write-picker contract for create modals: it seeds
// the acting team from the last-written default once teams load, exposes
// whether the picker should render, and — critically — a `ready` flag the
// modal gates its submit on so the cold-load window can't submit an
// unresolved team. Pair with <TeamPicker value={team} onChange={setTeam}/>
// and `disabled={!ready || …}` on the submit button.
export function useWriteTeam(): WriteTeam {
  const { teams, lastActingTeamId, multi, loaded } = useTeams()
  const [team, setTeam] = useState('')
  const seeded = useRef(false)
  useEffect(() => {
    // Cache invalidated (org switch via invalidateTeams, or a refresh in
    // flight): forget the prior seed and drop the now-stale team so a slow
    // reload can't leave another org's id selected. `ready` is false
    // meanwhile, so the modal's submit stays gated through the window.
    if (!loaded) {
      seeded.current = false
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setTeam((t) => (t === '' ? t : ''))
      return
    }
    // First seed after load, or re-seed when the held team is no longer a
    // member team — e.g. the org switched out from under a still-mounted
    // create modal. Submitting the stale id would 400 server-side
    // (invalid team selection), so fall back to the default.
    const stillMember = team !== '' && teams.some((t) => t.id === team)
    if (seeded.current && stillMember) return
    seeded.current = true

    setTeam(pickerDefault(teams, lastActingTeamId))
  }, [loaded, teams, lastActingTeamId, team])
  return { team, setTeam, multi, ready: loaded }
}

// useActiveTeam is the *single-team* page scope for the write/config
// surfaces that show exactly one team at a time — today the prompts page,
// which doubles as the auto-delegation binding graph. Unlike useTeamFilter
// (a multi-team read scope for dashboards like the board), this is one
// team: the page's reads (its prompts + its triggers) and its writes (a
// new prompt / trigger / rule) all belong to it. Reads and writes coincide
// here precisely because the surface is single-team, so there's nothing to
// decouple — the ambiguity that forces the read/write split on dashboards
// doesn't exist when only one team is ever in view.
//
// Device-local sticky per pageKey (localStorage), seeded from the write
// default (last-acting team → first team) and validated against the live set.
// Count-gated: `multi` is false for solo/local users, where teamId stays
// '' (the server resolves the sole team) and no switcher renders.
const ACTIVE_TEAM_KEY_PREFIX = 'tf.activeTeam.'

function readStoredActiveTeam(pageKey: string): string {
  try {
    return localStorage.getItem(ACTIVE_TEAM_KEY_PREFIX + pageKey) ?? ''
  } catch {
    return ''
  }
}

export interface ActiveTeam {
  /** The single active team. '' for solo/local or before resolution — the
   *  server resolves the sole team. A concrete id once a multi-team user's
   *  selection resolves. */
  teamId: string
  setTeamId: (id: string) => void
  /** ≥2 teams — gates whether the <TeamSwitch> renders at all. */
  multi: boolean
  /** False until /api/teams resolves and (for multi-team users) the active
   *  team is a concrete id. Read surfaces gate their first fetch on it so
   *  the cold-load window can't fetch an unresolved/other-org team, and
   *  the trigger-create gesture can't post team_id:'' (→ 400). */
  ready: boolean
  teams: TeamSummary[]
}

export function useActiveTeam(pageKey: string): ActiveTeam {
  const { teams, lastActingTeamId, multi, loaded } = useTeams()
  const [teamId, setTeamIdState] = useState<string>(() => readStoredActiveTeam(pageKey))

  useEffect(() => {
    if (!loaded) return
    if (!multi) {
      // Solo/local: no switcher; '' lets the server resolve the sole team.
      if (teamId !== '') {
        // eslint-disable-next-line react-hooks/set-state-in-effect
        setTeamIdState('')
      }
      return
    }
    // Multi-team: seed from the write default, and re-seed when the stored
    // id isn't a current team (deleted, or left over from another org after
    // an org switch). Mirrors useTeamFilter's stale-id pruning.
    const ids = new Set(teams.map((t) => t.id))
    if (teamId === '' || !ids.has(teamId)) {
      setTeamIdState(pickerDefault(teams, lastActingTeamId))
    }
  }, [loaded, multi, teams, lastActingTeamId, teamId])

  const setTeamId = useCallback(
    (id: string) => {
      setTeamIdState(id)
      try {
        localStorage.setItem(ACTIVE_TEAM_KEY_PREFIX + pageKey, id)
      } catch {
        // localStorage unavailable — the selection still works this
        // session; it just won't persist across reloads.
      }
    },
    [pageKey],
  )

  return { teamId, setTeamId, multi, ready: loaded && (!multi || teamId !== ''), teams }
}
