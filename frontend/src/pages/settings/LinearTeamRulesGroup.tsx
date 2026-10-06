import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight, Search, Trash2 } from 'lucide-react'
import { Section } from './primitives'
import JiraStatusRule from '../../components/JiraStatusRule'
import {
  dropLinearState,
  linearTeamIsArmed,
  linearTeamIsUnmapped,
  linearTeamRulesValid,
  prefillLinearRules,
  unmappedLinearTeam,
  unresolvableLinearStates,
  type LinearTeamConfig,
} from './teamConfig'
import { httpErrorMessage } from '../../lib/apiClient'
import {
  listLinearStates,
  listLinearTeams,
  type LinearStateOption,
  type LinearTeamCandidate,
} from '../../lib/linearTeams'

/** How long to wait after the last keystroke before asking the server for the
 *  filtered catalog — each search is a round trip to Linear. */
const SEARCH_DEBOUNCE_MS = 250

/** One row of the watch table: a team this team already watches, a candidate
 *  from the catalog, or both. `inCatalog` is false for a watched team the
 *  credential can no longer see, which is exactly the row that must stay
 *  removable. */
interface WatchRow {
  id: string
  key: string
  name: string
  private: boolean
  watched: boolean
  inCatalog: boolean
}

/**
 * LinearTeamRulesGroup is the team-scope Linear surface: which Linear teams
 * this team watches, and per watched team the pickup / in-progress / done
 * workflow-state rules. The Linear sibling of JiraProjectRulesGroup.
 *
 * A Linear workflow types its states, so watching a team also maps it: the
 * moment a team is watched its states are read and its rules pre-filled from
 * their types (prefillLinearRules). The pre-fill is only the form's starting
 * point — what is saved is whatever the user saves, by id — and a workflow
 * the types cannot map whole (no started or no completed state) is left
 * watched and unmapped, which the board says plainly.
 *
 * The server stores a team either watched with no rules or mapped with all
 * three, so a half-mapped team is flagged here and blocks the save.
 *
 * A controlled component: the container owns the teams (`value`) and the PUT
 * /api/teams/{id}/linear-teams. Org-level Linear access lives elsewhere; this
 * is suppressed until Linear is connected.
 */
export default function LinearTeamRulesGroup({
  value,
  onChange,
  connected,
  bare = false,
}: {
  value: LinearTeamConfig[]
  onChange: (next: LinearTeamConfig[]) => void
  connected: boolean
  bare?: boolean
}) {
  const [statesByTeam, setStatesByTeam] = useState<Record<string, LinearStateOption[]>>({})
  const [loadingIds, setLoadingIds] = useState<Set<string>>(new Set())
  const [stateErrors, setStateErrors] = useState<Record<string, string>>({})
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const [candidates, setCandidates] = useState<LinearTeamCandidate[]>([])
  const [catalogError, setCatalogError] = useState('')
  const [catalogLoading, setCatalogLoading] = useState(false)
  const [catalogTruncated, setCatalogTruncated] = useState(false)
  const [search, setSearch] = useState('')

  const watchedIds = useMemo(() => new Set(value.map((t) => t.id)), [value])

  // The latest value, for the asynchronous pre-fill: the states read resolves
  // after the click that started it, by which time the user may have unwatched
  // the team or started mapping it by hand.
  const valueRef = useRef(value)
  useEffect(() => {
    valueRef.current = value
  }, [value])

  const abortRef = useRef<AbortController | null>(null)
  const mountedRef = useRef(true)
  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      abortRef.current?.abort()
      abortRef.current = null
    }
  }, [])

  const fetchCandidates = useCallback(async (q: string) => {
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    setCatalogLoading(true)
    try {
      const page = await listLinearTeams(q, { signal: controller.signal })
      if (!mountedRef.current || controller.signal.aborted) return
      setCandidates(page.items)
      setCatalogTruncated(page.hasMore)
      setCatalogError('')
    } catch (e) {
      if (controller.signal.aborted || !mountedRef.current) return
      setCandidates([])
      setCatalogTruncated(false)
      setCatalogError(httpErrorMessage(e, 'Could not read the Linear team list.'))
    } finally {
      if (mountedRef.current && !controller.signal.aborted) setCatalogLoading(false)
    }
  }, [])

  useEffect(() => {
    if (!connected) return
    const timer = setTimeout(() => void fetchCandidates(search), search ? SEARCH_DEBOUNCE_MS : 0)
    return () => clearTimeout(timer)
  }, [connected, search, fetchCandidates])

  // The watch table: watched teams first, always present — including one the
  // catalog no longer offers — then the catalog's other candidates, which the
  // server already narrowed by the search.
  const rows = useMemo<WatchRow[]>(() => {
    const byId = new Map(candidates.map((c) => [c.id, c]))
    const needle = search.trim().toLowerCase()
    const out: WatchRow[] = []
    for (const t of value) {
      if (
        needle !== '' &&
        !t.key.toLowerCase().includes(needle) &&
        !t.name.toLowerCase().includes(needle)
      ) {
        continue
      }
      const candidate = byId.get(t.id)
      out.push({
        id: t.id,
        key: candidate?.key ?? t.key,
        name: candidate?.name ?? t.name,
        private: candidate?.private ?? false,
        watched: true,
        inCatalog: !!candidate,
      })
    }
    for (const c of candidates) {
      if (watchedIds.has(c.id)) continue
      out.push({ ...c, watched: false, inCatalog: true })
    }
    return out
  }, [candidates, search, value, watchedIds])

  /** fetchStates reads one team's workflow. With `prefill`, a team that is
   *  still watched and still unmapped when the read lands has its rules filled
   *  from the states' types. */
  const fetchStates = async (teamId: string, prefill = false) => {
    setLoadingIds((prev) => new Set([...prev, teamId]))
    try {
      const states = await listLinearStates(teamId)
      if (!mountedRef.current) return
      setStatesByTeam((current) => ({ ...current, [teamId]: states }))
      setStateErrors((current) => {
        const next = { ...current }
        delete next[teamId]
        return next
      })
      if (prefill) {
        const team = valueRef.current.find((t) => t.id === teamId)
        const rules = prefillLinearRules(states)
        if (team && rules && linearTeamIsUnmapped(team)) {
          onChange(valueRef.current.map((t) => (t.id === teamId ? { ...t, ...rules } : t)))
        }
      }
    } catch (e) {
      if (!mountedRef.current) return
      setStateErrors((current) => ({
        ...current,
        [teamId]: httpErrorMessage(e, 'Could not read this team’s workflow states.'),
      }))
    } finally {
      if (mountedRef.current) {
        setLoadingIds((prev) => {
          const n = new Set(prev)
          n.delete(teamId)
          return n
        })
      }
    }
  }

  const updateTeam = (id: string, patch: Partial<LinearTeamConfig>) => {
    onChange(value.map((t) => (t.id === id ? { ...t, ...patch } : t)))
  }

  const watch = (row: WatchRow) => {
    if (watchedIds.has(row.id)) return
    onChange([...value, unmappedLinearTeam(row.id, row.key, row.name)])
    void fetchStates(row.id, true)
  }

  const unwatch = (id: string) => {
    onChange(value.filter((t) => t.id !== id))
    setExpanded((m) => {
      const next = { ...m }
      delete next[id]
      return next
    })
  }

  const toggleExpanded = (id: string) => {
    const opening = !expanded[id]
    setExpanded((m) => ({ ...m, [id]: opening }))
    if (opening && !statesByTeam[id] && !loadingIds.has(id)) {
      void fetchStates(id)
    }
  }

  const board = (
    <div className="space-y-2">
      {value.length === 0 && (
        <p className="text-ui text-ink-3 italic">
          No Linear teams watched yet. Pick one below to start.
        </p>
      )}
      {value.map((team) => {
        const states = statesByTeam[team.id] || []
        const missing = unresolvableLinearStates(team, states)
        const armed = linearTeamIsArmed(team)
        const halfMapped = !linearTeamRulesValid(team)
        const isOpen = expanded[team.id] === true
        const prefill = isOpen && !armed && states.length > 0 ? prefillLinearRules(states) : null
        return (
          <div
            key={team.id}
            className={`rounded-xl border ${
              bare
                ? 'border-[var(--color-line-1)] bg-[var(--color-raised)]/50'
                : 'border-line-1 bg-raised'
            }`}
          >
            <div className="flex items-center gap-2 px-3 py-2">
              <button
                type="button"
                onClick={() => toggleExpanded(team.id)}
                className="text-ink-3 hover:text-ink-2"
                aria-label={isOpen ? `Collapse ${team.key}` : `Expand ${team.key}`}
              >
                {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              </button>
              <span className="text-body font-medium text-ink-1">{team.key}</span>
              <span className="flex-1 min-w-0 truncate text-ui text-ink-3">{team.name}</span>
              <button
                type="button"
                onClick={() => toggleExpanded(team.id)}
                className={`text-label uppercase tracking-wide ${
                  missing.length > 0 || halfMapped
                    ? 'text-alarm hover:text-alarm/80'
                    : 'text-ink-2 hover:text-ink-2/80'
                }`}
              >
                {missing.length > 0
                  ? `${missing.length} state${missing.length === 1 ? '' : 's'} missing`
                  : halfMapped
                    ? 'Partly mapped'
                    : armed
                      ? 'Ready'
                      : 'States not mapped'}
              </button>
              <button
                type="button"
                onClick={() => unwatch(team.id)}
                className="text-ink-3 hover:text-alarm"
                aria-label={`Stop watching ${team.key}`}
              >
                <Trash2 size={14} />
              </button>
            </div>

            {isOpen && (
              <div className="px-4 pb-4 pt-1 space-y-3">
                {!armed && !halfMapped && (
                  <p className="text-reported text-ink-3">
                    {team.key} is watched but not mapped, so nothing from it reaches the board yet.
                    Map its states below to arm it.
                  </p>
                )}
                {halfMapped && (
                  <p className="text-reported text-alarm">
                    Map pickup, in progress and done — each in-progress and done rule with a write
                    target — or clear all three to keep {team.key} watched without mapping it.
                  </p>
                )}
                <div className="flex items-center justify-between gap-3">
                  <p className="text-reported text-ink-3">
                    {loadingIds.has(team.id)
                      ? 'Loading states…'
                      : stateErrors[team.id]
                        ? stateErrors[team.id]
                        : states.length > 0
                          ? `${states.length} states available`
                          : 'No states loaded'}
                  </p>
                  <div className="flex shrink-0 items-center gap-2">
                    {prefill && (
                      <button
                        type="button"
                        onClick={() => updateTeam(team.id, prefill)}
                        className="text-reported text-warm hover:text-warm/80 border border-warm/20 rounded-xl px-3 py-1 transition-colors"
                      >
                        Map from state types
                      </button>
                    )}
                    <button
                      type="button"
                      onClick={() => void fetchStates(team.id)}
                      disabled={loadingIds.has(team.id)}
                      className="text-reported text-warm hover:text-warm/80 disabled:opacity-40 border border-warm/20 rounded-xl px-3 py-1 transition-colors"
                    >
                      {loadingIds.has(team.id) ? 'Loading...' : 'Reload states'}
                    </button>
                  </div>
                </div>

                {missing.length > 0 && (
                  <div className="rounded-xl border border-alarm/30 bg-alarm/5 px-3 py-2.5 space-y-2">
                    <div className="flex items-start gap-2">
                      <AlertTriangle size={13} className="mt-0.5 shrink-0 text-alarm" />
                      <p className="text-reported text-ink-2">
                        These states are in {team.key}&rsquo;s rules but not in its Linear workflow
                        any more. Remove them, then pick replacements below.
                      </p>
                    </div>
                    <div className="flex flex-wrap gap-1.5 pl-[21px]">
                      {missing.map((state) => (
                        <button
                          key={state.id}
                          type="button"
                          onClick={() =>
                            onChange(
                              value.map((t) => (t.id === team.id ? dropLinearState(t, state) : t)),
                            )
                          }
                          className="group inline-flex items-center gap-1.5 rounded-xl border border-alarm/30 px-2 py-0.5 text-reported text-ink-2 transition-colors hover:border-alarm hover:text-alarm"
                          aria-label={`Remove ${state.name || state.id} from ${team.key}`}
                        >
                          {state.name || state.id}
                          <Trash2 size={11} className="opacity-60 group-hover:opacity-100" />
                        </button>
                      ))}
                    </div>
                  </div>
                )}

                {states.length > 0 && (
                  <div className="space-y-4 pt-1">
                    <JiraStatusRule
                      label="Pickup"
                      description="Poll for unassigned issues in these states."
                      allStatuses={states}
                      value={team.pickup}
                      onChange={(v) => updateTeam(team.id, { pickup: { members: v.members } })}
                      requireCanonical={false}
                    />
                    <JiraStatusRule
                      label="In progress"
                      description="Count as actively being worked on."
                      allStatuses={states}
                      value={team.in_progress}
                      onChange={(v) => updateTeam(team.id, { in_progress: v })}
                      requireCanonical={true}
                      canonicalPrompt="Claim →"
                    />
                    <JiraStatusRule
                      label="Done"
                      description="Count as complete (completed and canceled states alike)."
                      allStatuses={states}
                      value={team.done}
                      onChange={(v) => updateTeam(team.id, { done: v })}
                      requireCanonical={true}
                      canonicalPrompt="Complete →"
                    />
                  </div>
                )}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )

  const picker = (
    <div className="mt-4 space-y-2">
      <div className="flex items-center gap-2 rounded-xl border border-line-1 bg-raised px-3 py-1.5">
        <Search size={13} className="shrink-0 text-ink-3" />
        <input
          type="search"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search Linear teams"
          aria-label="Search Linear teams"
          className="w-full bg-transparent border-0 focus:outline-none text-body text-ink-1 placeholder-ink-3"
        />
      </div>

      {catalogError !== '' && <p className="text-reported text-alarm">{catalogError}</p>}
      {catalogError === '' && catalogLoading && rows.length === 0 && (
        <p className="text-ui text-ink-3 italic">Loading teams…</p>
      )}
      {catalogError === '' && !catalogLoading && rows.length === 0 && (
        <p className="text-ui text-ink-3 italic">
          {search.trim() === ''
            ? 'This workspace’s Linear credential can’t see any teams.'
            : `No Linear team matches “${search.trim()}”.`}
        </p>
      )}

      <div className="divide-y divide-border-subtle rounded-xl border border-line-1 overflow-hidden">
        {rows.map((row) => (
          <div key={row.id} className="flex items-center gap-3 bg-raised px-3 py-2">
            <div className="min-w-0 flex-1">
              <span className="text-body font-medium text-ink-1">{row.key}</span>
              {row.name !== '' && (
                <span className="ml-2 text-ui text-ink-3 truncate">{row.name}</span>
              )}
              {row.private && <span className="ml-2 text-reported text-ink-3">private</span>}
              {/* Only a whole catalog can say a team is missing from it: a
                  truncated page leaves teams off that the credential can see. */}
              {!row.inCatalog && !catalogTruncated && catalogError === '' && (
                <span className="ml-2 text-reported text-ink-2">
                  not visible to this credential
                </span>
              )}
            </div>
            <button
              type="button"
              onClick={() => (row.watched ? unwatch(row.id) : watch(row))}
              className={`shrink-0 text-reported rounded-xl border px-3 py-1 transition-colors ${
                row.watched
                  ? 'border-warm/25 bg-warm/[0.08] text-warm'
                  : 'border-warm/20 text-warm hover:text-warm/80'
              }`}
            >
              {row.watched ? 'Watching' : 'Watch'}
            </button>
          </div>
        ))}
      </div>

      {catalogTruncated && (
        <p className="text-reported text-ink-3">
          More teams match than fit here — narrow the search to reach them.
        </p>
      )}
    </div>
  )

  const inner = (
    <>
      {bare ? (
        <div className="mb-4 space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Linear teams</h2>
          <p className="text-body leading-relaxed text-ink-3">
            Watch the Linear teams this team works from. Each one&rsquo;s states are mapped to
            pickup / in-progress / done from their types, and you can adjust the mapping before
            saving.
          </p>
        </div>
      ) : (
        <h2 className="mb-4 text-body font-medium text-ink-2">Linear teams</h2>
      )}
      {!connected ? (
        <p className="text-ui text-ink-3 italic">
          Connect Linear under Workspace settings before configuring tracked teams.
        </p>
      ) : (
        <>
          {board}
          {picker}
        </>
      )}
    </>
  )

  return bare ? inner : <Section>{inner}</Section>
}
