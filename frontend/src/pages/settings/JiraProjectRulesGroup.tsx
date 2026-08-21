import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight, Search, Trash2 } from 'lucide-react'
import { Section } from './primitives'
import JiraStatusRule from '../../components/JiraStatusRule'
import {
  dropStatus,
  emptyProject,
  projectIsArmed,
  unresolvableStatuses,
  type JiraProjectConfig,
} from './teamConfig'
import type { JiraStatusRef } from '../../components/JiraStatusRule'
import { apiJSON, httpErrorMessage } from '../../lib/apiClient'
import { listJiraProjects, type JiraProjectCandidate } from '../../lib/jiraProjects'

/** How long to wait after the last keystroke before asking the server for the
 *  filtered catalog. The filter is a round trip to Jira, not an array scan, so
 *  a request per character would be both wasteful and out of order. */
const SEARCH_DEBOUNCE_MS = 250

/** normKey is the canonical form of a project key — the same uppercase-trim the
 *  server applies at the write boundary, so a key from the catalog and a key
 *  from the stored set compare as one value. */
const normKey = (key: string): string => key.trim().toUpperCase()

/** One row of the watch table: a project the team already watches, a candidate
 *  from the catalog, or both. `inCatalog` is false for a watched project the
 *  credential can no longer see — deleted upstream, or the credential's access
 *  narrowed — which is exactly the row that still needs to be unwatchable. */
interface WatchRow {
  key: string
  name: string
  watched: boolean
  inCatalog: boolean
}

/**
 * JiraProjectRulesGroup is the team-scope Jira project surface: which projects
 * the team watches, and — per watched project — the pickup / in-progress /
 * in-review / done status rules. Pickup, in-progress and done are what arm it;
 * in-review is optional.
 *
 * Watching and arming are two steps, not one. The watch table below picks from
 * the org credential's live catalog and a project joins the tracked set on one
 * click, carrying no rules at all; mapping its workflow's statuses is a
 * separate gesture on the board above, which is what turns a watched project
 * into one the poller can actually ask Jira about. An unarmed project is a
 * valid saved state, so the board says so plainly rather than blocking the save.
 *
 * A controlled component — the container owns the projects array (`value`) and
 * the actual PUT /api/teams/{id}/jira-projects — so the same editor serves the
 * team Settings tab and the setup wizard's team steps.
 *
 * It owns only its own view state: the catalog page and its search, per-project
 * expand/collapse, and the fetched status options. Org-level Jira *access*
 * (credentials, connect/disconnect) lives in JiraAccessGroup; this is the
 * team-scoped projects only, suppressed until Jira is connected.
 */
export default function JiraProjectRulesGroup({
  value,
  onChange,
  connected,
  bare = false,
}: {
  value: JiraProjectConfig[]
  onChange: (next: JiraProjectConfig[]) => void
  connected: boolean
  // The setup wizard composes this flush (no Section card, glass project rows);
  // Settings keeps the carded default.
  bare?: boolean
}) {
  // Statuses keyed by project key. Fetched per project, on demand, because a
  // project's statuses come from ITS workflow scheme: querying several at once
  // returns their intersection, which is safe to offer in every picker but
  // hides statuses a project genuinely has the moment two watched projects
  // differ — and watching is cheap now, so they will.
  const [statusesByProject, setStatusesByProject] = useState<Record<string, JiraStatusRef[]>>({})
  // The project keys whose status fetch is in flight. Per-key rather than a
  // single boolean so fetching project A then B doesn't re-enable B's button
  // when A's request settles first.
  const [loadingKeys, setLoadingKeys] = useState<Set<string>>(new Set())
  // Per-project expand/collapse, keyed by the project key. Keys are chosen from
  // the catalog rather than typed, so unlike the free-text field this replaced
  // the key is stable for the row's lifetime and makes a safe map key.
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const [candidates, setCandidates] = useState<JiraProjectCandidate[]>([])
  const [catalogError, setCatalogError] = useState('')
  const [catalogLoading, setCatalogLoading] = useState(false)
  const [catalogTruncated, setCatalogTruncated] = useState(false)
  const [search, setSearch] = useState('')

  const watchedKeys = useMemo(() => new Set(value.map((p) => normKey(p.key))), [value])

  // Current in-flight catalog fetch. Each new one aborts the previous so
  // out-of-order resolution can't leave the table showing an older search's
  // results.
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
      const page = await listJiraProjects(q, { signal: controller.signal })
      if (!mountedRef.current || controller.signal.aborted) return
      setCandidates(page.items)
      setCatalogTruncated(page.hasMore)
      setCatalogError('')
    } catch (e) {
      if (controller.signal.aborted || !mountedRef.current) return
      setCandidates([])
      setCatalogTruncated(false)
      setCatalogError(httpErrorMessage(e, 'Could not read the Jira project list.'))
    } finally {
      if (mountedRef.current && !controller.signal.aborted) setCatalogLoading(false)
    }
  }, [])

  // Load the catalog on mount and on each settled search. Nothing is cached
  // server-side, so this is a live read of what the org credential can see —
  // which is also what makes it the right thing to re-run when the search
  // changes rather than filtering a snapshot in memory.
  useEffect(() => {
    if (!connected) return
    const timer = setTimeout(() => void fetchCandidates(search), search ? SEARCH_DEBOUNCE_MS : 0)
    return () => clearTimeout(timer)
  }, [connected, search, fetchCandidates])

  // The watch table is the union of what the team watches and what the catalog
  // offers. Watched rows come first and are always present — including one the
  // catalog no longer offers, which would otherwise become unremovable — so the
  // client-side filter applies only to them; the candidates below were already
  // narrowed by the server.
  const rows = useMemo<WatchRow[]>(() => {
    const byKey = new Map(candidates.map((c) => [normKey(c.key), c]))
    const needle = search.trim().toLowerCase()
    const out: WatchRow[] = []
    for (const p of value) {
      const key = normKey(p.key)
      if (!key) continue
      const candidate = byKey.get(key)
      const name = candidate?.name ?? ''
      if (
        needle !== '' &&
        !key.toLowerCase().includes(needle) &&
        !name.toLowerCase().includes(needle)
      ) {
        continue
      }
      out.push({ key, name, watched: true, inCatalog: !!candidate })
    }
    for (const c of candidates) {
      const key = normKey(c.key)
      if (watchedKeys.has(key)) continue
      out.push({ key, name: c.name, watched: false, inCatalog: true })
    }
    return out
  }, [candidates, search, value, watchedKeys])

  // fetchJiraStatuses loads one project's statuses. Keys are normalized here
  // regardless of source, so the results are stored under the same key the
  // lookup uses.
  const fetchJiraStatuses = async (projectKey: string) => {
    const key = normKey(projectKey)
    if (!key) return
    setLoadingKeys((prev) => new Set([...prev, key]))
    try {
      const statuses = await apiJSON<JiraStatusRef[]>(
        `/api/jira/statuses?project=${encodeURIComponent(key)}`,
      )
      setStatusesByProject((current) => ({ ...current, [key]: statuses }))
    } catch {
      // Non-critical — the picker just shows no options until a retry.
    } finally {
      setLoadingKeys((prev) => {
        const n = new Set(prev)
        n.delete(key)
        return n
      })
    }
  }

  const updateProject = (key: string, patch: Partial<JiraProjectConfig>) => {
    onChange(value.map((p) => (normKey(p.key) === key ? { ...p, ...patch } : p)))
  }

  // Removing a vanished status is an edit like any other — it lands in the
  // form and takes effect on save, so the team sees what changed before it is
  // written. Clearing a canonical this way leaves the rule incomplete on
  // purpose: the replacement write target is theirs to pick.
  const dropUnresolvable = (key: string, status: JiraStatusRef) => {
    onChange(value.map((p) => (normKey(p.key) === key ? dropStatus(p, status) : p)))
  }

  const watch = (key: string) => {
    if (watchedKeys.has(key)) return
    onChange([...value, emptyProject(key)])
  }

  const unwatch = (key: string) => {
    onChange(value.filter((p) => normKey(p.key) !== key))
    setExpanded((m) => {
      const next = { ...m }
      delete next[key]
      return next
    })
  }

  // Expanding a project is what asks for its statuses — the arming step needs
  // them, and nothing before it does.
  const toggleExpanded = (key: string) => {
    const opening = !expanded[key]
    setExpanded((m) => ({ ...m, [key]: opening }))
    if (opening && !statusesByProject[key] && !loadingKeys.has(key)) {
      void fetchJiraStatuses(key)
    }
  }

  // Shared workflow schemes are common, so a project whose statuses are all
  // present in this one's is a mapping worth offering to copy wholesale.
  //
  // The match is on the status NAMES, and the copy REMAPS to this project's own
  // ids. Two projects that spell a status the same way still hold two distinct
  // status entities with two distinct ids, so copying the source's ids across
  // would write a rule pointing at statuses this project's workflow does not
  // have — which the server would refuse, correctly.
  const copySourcesFor = (key: string): JiraProjectConfig[] => {
    const names = new Set((statusesByProject[key] ?? []).map((s) => s.name))
    if (names.size === 0) return []
    return value.filter((p) => {
      if (normKey(p.key) === key || !projectIsArmed(p)) return false
      return [
        ...p.pickup.members,
        ...p.in_progress.members,
        ...p.in_review.members,
        ...p.done.members,
      ].every((m) => names.has(m.name))
    })
  }

  const copyMapping = (key: string, source: JiraProjectConfig) => {
    const byName = new Map((statusesByProject[key] ?? []).map((s) => [s.name, s]))
    const remap = (refs: JiraStatusRef[]): JiraStatusRef[] =>
      refs.map((r) => byName.get(r.name)).filter((r): r is JiraStatusRef => r !== undefined)
    const remapOne = (ref: JiraStatusRef | null | undefined): JiraStatusRef | null =>
      (ref && byName.get(ref.name)) ?? null
    updateProject(key, {
      pickup: { members: remap(source.pickup.members) },
      in_progress: {
        members: remap(source.in_progress.members),
        canonical: remapOne(source.in_progress.canonical),
      },
      in_review: {
        members: remap(source.in_review.members),
        canonical: remapOne(source.in_review.canonical),
      },
      done: { members: remap(source.done.members), canonical: remapOne(source.done.canonical) },
    })
  }

  const watchedProjects = value.filter((p) => normKey(p.key) !== '')

  const board = (
    <div className="space-y-2">
      {watchedProjects.length === 0 && (
        <p className="text-[12px] text-text-tertiary italic">
          No Jira projects watched yet. Pick one below to start.
        </p>
      )}
      {watchedProjects.map((project) => {
        const key = normKey(project.key)
        const statuses = statusesByProject[key] || []
        const missing = unresolvableStatuses(project, statuses)
        const armed = projectIsArmed(project)
        const isOpen = expanded[key] === true
        const sources = isOpen && !armed ? copySourcesFor(key) : []
        return (
          <div
            key={key}
            className={`rounded-xl border ${
              bare
                ? 'border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/50'
                : 'border-border-subtle bg-white/40'
            }`}
          >
            <div className="flex items-center gap-2 px-3 py-2">
              <button
                type="button"
                onClick={() => toggleExpanded(key)}
                className="text-text-tertiary hover:text-text-secondary"
                aria-label={isOpen ? 'Collapse project' : 'Expand project'}
              >
                {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              </button>
              <span className="flex-1 min-w-0 truncate text-[13px] font-medium text-text-primary">
                {key}
              </span>
              <button
                type="button"
                onClick={() => toggleExpanded(key)}
                className={`text-[10px] uppercase tracking-wide ${
                  missing.length > 0
                    ? 'text-dismiss hover:text-dismiss/80'
                    : armed
                      ? 'text-claim'
                      : 'text-snooze hover:text-snooze/80'
                }`}
              >
                {missing.length > 0
                  ? `${missing.length} status${missing.length === 1 ? '' : 'es'} missing`
                  : armed
                    ? 'Ready'
                    : 'Statuses not mapped'}
              </button>
              <button
                type="button"
                onClick={() => unwatch(key)}
                className="text-text-tertiary hover:text-dismiss"
                aria-label={`Stop watching ${key}`}
              >
                <Trash2 size={14} />
              </button>
            </div>

            {isOpen && (
              <div className="px-4 pb-4 pt-1 space-y-3">
                {!armed && (
                  <p className="text-[11px] text-text-tertiary">
                    {key} is watched but not mapped, so nothing from it reaches the board yet. Map
                    its statuses below to arm it.
                  </p>
                )}
                <div className="flex items-center justify-between">
                  <p className="text-[11px] text-text-tertiary">
                    {loadingKeys.has(key)
                      ? 'Loading statuses…'
                      : statuses.length > 0
                        ? `${statuses.length} statuses available`
                        : 'No statuses loaded'}
                  </p>
                  <button
                    type="button"
                    onClick={() => void fetchJiraStatuses(key)}
                    disabled={loadingKeys.has(key)}
                    className="shrink-0 text-[11px] text-accent hover:text-accent/80 disabled:opacity-40 border border-accent/20 rounded-xl px-3 py-1 transition-colors"
                  >
                    {loadingKeys.has(key) ? 'Loading...' : 'Reload statuses'}
                  </button>
                </div>

                {sources.length > 0 && (
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-[11px] text-text-tertiary">
                      Same status names as{sources.length > 1 ? ' these' : ''}:
                    </span>
                    {sources.map((source) => (
                      <button
                        key={source.key}
                        type="button"
                        onClick={() => copyMapping(key, source)}
                        className="text-[11px] text-accent hover:text-accent/80 border border-accent/20 rounded-xl px-2.5 py-1 transition-colors"
                      >
                        Copy {normKey(source.key)}&rsquo;s mapping
                      </button>
                    ))}
                  </div>
                )}

                {missing.length > 0 && (
                  <div className="rounded-xl border border-dismiss/30 bg-dismiss/5 px-3 py-2.5 space-y-2">
                    <div className="flex items-start gap-2">
                      <AlertTriangle size={13} className="mt-0.5 shrink-0 text-dismiss" />
                      <p className="text-[11px] text-text-secondary">
                        These statuses are in {key}&rsquo;s rules but not in its Jira workflow any
                        more. Polling skips them, and a rule whose write target is gone cannot
                        transition a ticket. Remove them, then pick replacements below.
                      </p>
                    </div>
                    <div className="flex flex-wrap gap-1.5 pl-[21px]">
                      {missing.map((status) => (
                        <button
                          key={status.id || status.name}
                          type="button"
                          onClick={() => dropUnresolvable(key, status)}
                          className="group inline-flex items-center gap-1.5 rounded-xl border border-dismiss/30 px-2 py-0.5 text-[11px] text-text-secondary transition-colors hover:border-dismiss hover:text-dismiss"
                          aria-label={`Remove ${status.name || status.id} from ${key}`}
                        >
                          {status.name || status.id}
                          <Trash2 size={11} className="opacity-60 group-hover:opacity-100" />
                        </button>
                      ))}
                    </div>
                  </div>
                )}

                {statuses.length > 0 && (
                  <div className="space-y-4 pt-1">
                    <JiraStatusRule
                      label="Pickup"
                      description="Poll for unassigned tickets in these states."
                      allStatuses={statuses}
                      value={project.pickup}
                      onChange={(v) => updateProject(key, { pickup: v })}
                      requireCanonical={false}
                    />
                    <JiraStatusRule
                      label="In progress"
                      description="Count as actively being worked on."
                      allStatuses={statuses}
                      value={project.in_progress}
                      onChange={(v) => updateProject(key, { in_progress: v })}
                      requireCanonical={true}
                      canonicalPrompt="Claim →"
                    />
                    <JiraStatusRule
                      label="In review"
                      description="Optional — the status that means work awaits human review."
                      allStatuses={statuses}
                      value={project.in_review}
                      onChange={(v) => updateProject(key, { in_review: v })}
                      requireCanonical={true}
                      canonicalPrompt="Review →"
                    />
                    <JiraStatusRule
                      label="Done"
                      description="Count as complete (add every variant — e.g. Resolved + Verified)."
                      allStatuses={statuses}
                      value={project.done}
                      onChange={(v) => updateProject(key, { done: v })}
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
      <div className="flex items-center gap-2 rounded-xl border border-border-subtle bg-white/40 px-3 py-1.5">
        <Search size={13} className="shrink-0 text-text-tertiary" />
        <input
          type="search"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search Jira projects"
          aria-label="Search Jira projects"
          className="w-full bg-transparent border-0 focus:outline-none text-[13px] text-text-primary placeholder-text-tertiary"
        />
      </div>

      {catalogError !== '' && <p className="text-[11px] text-dismiss">{catalogError}</p>}
      {catalogError === '' && catalogLoading && rows.length === 0 && (
        <p className="text-[12px] text-text-tertiary italic">Loading projects…</p>
      )}
      {catalogError === '' && !catalogLoading && rows.length === 0 && (
        <p className="text-[12px] text-text-tertiary italic">
          {search.trim() === ''
            ? 'This workspace’s Jira credential can’t see any projects.'
            : `No Jira project matches “${search.trim()}”.`}
        </p>
      )}

      <div className="divide-y divide-border-subtle rounded-xl border border-border-subtle overflow-hidden">
        {rows.map((row) => (
          <div key={row.key} className="flex items-center gap-3 bg-white/40 px-3 py-2">
            <div className="min-w-0 flex-1">
              <span className="text-[13px] font-medium text-text-primary">{row.key}</span>
              {row.name !== '' && (
                <span className="ml-2 text-[12px] text-text-tertiary truncate">{row.name}</span>
              )}
              {!row.inCatalog && (
                <span className="ml-2 text-[11px] text-snooze">not visible to this credential</span>
              )}
            </div>
            <button
              type="button"
              onClick={() => (row.watched ? unwatch(row.key) : watch(row.key))}
              className={`shrink-0 text-[11px] rounded-xl border px-3 py-1 transition-colors ${
                row.watched
                  ? 'border-accent/25 bg-accent/[0.08] text-accent'
                  : 'border-accent/20 text-accent hover:text-accent/80'
              }`}
            >
              {row.watched ? 'Watching' : 'Watch'}
            </button>
          </div>
        ))}
      </div>

      {catalogTruncated && (
        <p className="text-[11px] text-text-tertiary">
          More projects match than fit here — narrow the search to reach them.
        </p>
      )}
    </div>
  )

  const inner = (
    <>
      {bare ? (
        <div className="mb-4 space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
            Jira projects
          </h2>
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            Watch the Jira projects this team works from, then map each one&rsquo;s statuses to
            pickup / in-progress / done.
          </p>
        </div>
      ) : (
        <h2 className="mb-4 text-[13px] font-medium text-text-secondary">Jira projects</h2>
      )}
      {!connected ? (
        <p className="text-[12px] text-text-tertiary italic">
          Connect Jira under Workspace settings before configuring tracked projects.
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
