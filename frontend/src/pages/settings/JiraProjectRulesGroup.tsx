import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight, Search, Trash2 } from 'lucide-react'
import { Section } from './primitives'
import JiraStatusRule from '../../components/JiraStatusRule'
import {
  dropStatus,
  projectIsArmed,
  unresolvableStatuses,
  watchedProject,
  type JiraProjectConfig,
} from './teamConfig'
import type { JiraStatusRef } from '../../components/JiraStatusRule'
import { httpErrorMessage } from '../../lib/apiClient'
import {
  candidateRefs,
  jiraProjectRef,
  listJiraProjects,
  listJiraStatuses,
  type JiraProjectCandidate,
} from '../../lib/jiraProjects'

/** How long to wait after the last keystroke before asking the server for the
 *  filtered catalog. The filter is a round trip to Jira, not an array scan, so
 *  a request per character would be both wasteful and out of order. */
const SEARCH_DEBOUNCE_MS = 250

/** One row of the watch table: a project the team already watches, a candidate
 *  from the catalog, or both. `ref` is its selection key (jiraProjectRef);
 *  `key` and `name` are for display. `inCatalog` is false for a watched project
 *  the credential can no longer see — deleted upstream, or the credential's
 *  access narrowed — which is exactly the row that still needs to be
 *  unwatchable. `candidate` is the catalog entry an unwatched row adds. */
interface WatchRow {
  ref: string
  key: string
  name: string
  watched: boolean
  inCatalog: boolean
  candidate?: JiraProjectCandidate
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
 * team-scoped projects only, suppressed until Jira is connected. `orgId`
 * addresses the org's Jira catalog, which the project list and the status
 * options are read from.
 */
export default function JiraProjectRulesGroup({
  orgId,
  value,
  onChange,
  connected,
  bare = false,
}: {
  orgId: string
  value: JiraProjectConfig[]
  onChange: (next: JiraProjectConfig[]) => void
  connected: boolean
  // The setup wizard composes this flush (no Section card, glass project rows);
  // Settings keeps the carded default.
  bare?: boolean
}) {
  // Statuses keyed by the project's Jira id, which the read addresses it by.
  // Fetched per project, on demand, because a project's statuses come from ITS
  // workflow scheme, so two watched projects can offer different ones — and
  // watching is cheap now, so they will.
  const [statusesByProject, setStatusesByProject] = useState<Record<string, JiraStatusRef[]>>({})
  // The Jira ids whose status fetch is in flight. Per-project rather than a
  // single boolean so fetching project A then B doesn't re-enable B's button
  // when A's request settles first.
  const [loadingIds, setLoadingIds] = useState<Set<string>>(new Set())
  // Per-project expand/collapse, keyed by the project's selection key, which
  // a key rename in Jira does not move and a save does not change.
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const [candidates, setCandidates] = useState<JiraProjectCandidate[]>([])
  const [catalogError, setCatalogError] = useState('')
  const [catalogLoading, setCatalogLoading] = useState(false)
  const [catalogTruncated, setCatalogTruncated] = useState(false)
  const [search, setSearch] = useState('')

  const watchedRefs = useMemo(() => new Set(value.map(jiraProjectRef)), [value])

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

  const fetchCandidates = useCallback(
    async (q: string) => {
      abortRef.current?.abort()
      const controller = new AbortController()
      abortRef.current = controller
      setCatalogLoading(true)
      try {
        const page = await listJiraProjects(orgId, q, { signal: controller.signal })
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
    },
    [orgId],
  )

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
    const byRef = new Map<string, JiraProjectCandidate>()
    for (const c of candidates) {
      for (const ref of candidateRefs(c)) {
        if (!byRef.has(ref)) byRef.set(ref, c)
      }
    }
    const needle = search.trim().toLowerCase()
    const out: WatchRow[] = []
    for (const p of value) {
      const ref = jiraProjectRef(p)
      const candidate = byRef.get(ref)
      const name = candidate?.name ?? p.name ?? ''
      if (
        needle !== '' &&
        !p.key.toLowerCase().includes(needle) &&
        !name.toLowerCase().includes(needle)
      ) {
        continue
      }
      out.push({ ref, key: p.key, name, watched: true, inCatalog: !!candidate })
    }
    for (const c of candidates) {
      if (candidateRefs(c).some((ref) => watchedRefs.has(ref))) continue
      out.push({
        ref: jiraProjectRef(c),
        key: c.key,
        name: c.name,
        watched: false,
        inCatalog: true,
        candidate: c,
      })
    }
    return out
  }, [candidates, search, value, watchedRefs])

  // fetchJiraStatuses loads one project's statuses, by its Jira id.
  const fetchJiraStatuses = async (jiraId: string) => {
    setLoadingIds((prev) => new Set([...prev, jiraId]))
    try {
      const statuses = await listJiraStatuses(orgId, jiraId)
      setStatusesByProject((current) => ({ ...current, [jiraId]: statuses }))
    } catch {
      // Non-critical — the picker just shows no options until a retry.
    } finally {
      setLoadingIds((prev) => {
        const n = new Set(prev)
        n.delete(jiraId)
        return n
      })
    }
  }

  const updateProject = (ref: string, patch: Partial<JiraProjectConfig>) => {
    onChange(value.map((p) => (jiraProjectRef(p) === ref ? { ...p, ...patch } : p)))
  }

  // Removing a vanished status is an edit like any other — it lands in the
  // form and takes effect on save, so the team sees what changed before it is
  // written. Clearing a canonical this way leaves the rule incomplete on
  // purpose: the replacement write target is theirs to pick.
  const dropUnresolvable = (ref: string, status: JiraStatusRef) => {
    onChange(value.map((p) => (jiraProjectRef(p) === ref ? dropStatus(p, status) : p)))
  }

  const watch = (candidate: JiraProjectCandidate) => {
    if (candidateRefs(candidate).some((ref) => watchedRefs.has(ref))) return
    onChange([...value, watchedProject(candidate)])
  }

  const unwatch = (ref: string) => {
    onChange(value.filter((p) => jiraProjectRef(p) !== ref))
    setExpanded((m) => {
      const next = { ...m }
      delete next[ref]
      return next
    })
  }

  // Expanding a project is what asks for its statuses — the arming step needs
  // them, and nothing before it does. A project with no Jira id yet has none
  // to ask for.
  const toggleExpanded = (project: JiraProjectConfig) => {
    const ref = jiraProjectRef(project)
    const opening = !expanded[ref]
    setExpanded((m) => ({ ...m, [ref]: opening }))
    const jiraId = project.jira_id
    if (opening && jiraId && !statusesByProject[jiraId] && !loadingIds.has(jiraId)) {
      void fetchJiraStatuses(jiraId)
    }
  }

  const statusesOf = (p: JiraProjectConfig): JiraStatusRef[] =>
    (p.jira_id && statusesByProject[p.jira_id]) || []

  // Shared workflow schemes are common, so a project whose statuses are all
  // present in this one's is a mapping worth offering to copy wholesale.
  //
  // The match is on the status NAMES, and the copy REMAPS to this project's own
  // ids. Two projects that spell a status the same way still hold two distinct
  // status entities with two distinct ids, so copying the source's ids across
  // would write a rule pointing at statuses this project's workflow does not
  // have — which the server would refuse, correctly.
  const copySourcesFor = (target: JiraProjectConfig): JiraProjectConfig[] => {
    const names = new Set(statusesOf(target).map((s) => s.name))
    if (names.size === 0) return []
    const ref = jiraProjectRef(target)
    return value.filter((p) => {
      if (jiraProjectRef(p) === ref || !projectIsArmed(p)) return false
      return [
        ...p.pickup.members,
        ...p.in_progress.members,
        ...p.in_review.members,
        ...p.done.members,
      ].every((m) => names.has(m.name))
    })
  }

  const copyMapping = (target: JiraProjectConfig, source: JiraProjectConfig) => {
    const byName = new Map(statusesOf(target).map((s) => [s.name, s]))
    const remap = (refs: JiraStatusRef[]): JiraStatusRef[] =>
      refs.map((r) => byName.get(r.name)).filter((r): r is JiraStatusRef => r !== undefined)
    const remapOne = (ref: JiraStatusRef | null | undefined): JiraStatusRef | null =>
      (ref && byName.get(ref.name)) ?? null
    updateProject(jiraProjectRef(target), {
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

  const board = (
    <div className="space-y-2">
      {value.length === 0 && (
        <p className="text-ui text-ink-3 italic">
          No Jira projects watched yet. Pick one below to start.
        </p>
      )}
      {value.map((project) => {
        const ref = jiraProjectRef(project)
        const key = project.key
        const jiraId = project.jira_id ?? ''
        const statuses = statusesOf(project)
        const loading = jiraId !== '' && loadingIds.has(jiraId)
        const missing = unresolvableStatuses(project, statuses)
        const armed = projectIsArmed(project)
        const isOpen = expanded[ref] === true
        const sources = isOpen && !armed ? copySourcesFor(project) : []
        return (
          <div
            key={ref}
            className={`rounded-xl border ${
              bare
                ? 'border-[var(--color-line-1)] bg-[var(--color-raised)]/50'
                : 'border-line-1 bg-raised'
            }`}
          >
            <div className="flex items-center gap-2 px-3 py-2">
              <button
                type="button"
                onClick={() => toggleExpanded(project)}
                className="text-ink-3 hover:text-ink-2"
                aria-label={isOpen ? 'Collapse project' : 'Expand project'}
              >
                {isOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              </button>
              <span className="flex-1 min-w-0 truncate text-body font-medium text-ink-1">
                {key}
              </span>
              <button
                type="button"
                onClick={() => toggleExpanded(project)}
                className={`text-label uppercase tracking-wide ${
                  missing.length > 0
                    ? 'text-alarm hover:text-alarm/80'
                    : armed
                      ? 'text-ink-2'
                      : 'text-ink-2 hover:text-ink-2/80'
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
                onClick={() => unwatch(ref)}
                className="text-ink-3 hover:text-alarm"
                aria-label={`Stop watching ${key}`}
              >
                <Trash2 size={14} />
              </button>
            </div>

            {isOpen && (
              <div className="px-4 pb-4 pt-1 space-y-3">
                {!armed && (
                  <p className="text-reported text-ink-3">
                    {key} is watched but not mapped, so nothing from it reaches the board yet. Map
                    its statuses below to arm it.
                  </p>
                )}
                <div className="flex items-center justify-between">
                  <p className="text-reported text-ink-3">
                    {jiraId === ''
                      ? 'Not confirmed with Jira yet, so its statuses cannot be read. The next poll confirms it.'
                      : loading
                        ? 'Loading statuses…'
                        : statuses.length > 0
                          ? `${statuses.length} statuses available`
                          : 'No statuses loaded'}
                  </p>
                  <button
                    type="button"
                    onClick={() => void fetchJiraStatuses(jiraId)}
                    disabled={jiraId === '' || loading}
                    className="shrink-0 text-reported text-warm hover:text-warm/80 disabled:opacity-40 border border-warm/20 rounded-xl px-3 py-1 transition-colors"
                  >
                    {loading ? 'Loading...' : 'Reload statuses'}
                  </button>
                </div>

                {sources.length > 0 && (
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-reported text-ink-3">
                      Same status names as{sources.length > 1 ? ' these' : ''}:
                    </span>
                    {sources.map((source) => (
                      <button
                        key={jiraProjectRef(source)}
                        type="button"
                        onClick={() => copyMapping(project, source)}
                        className="text-reported text-warm hover:text-warm/80 border border-warm/20 rounded-xl px-2.5 py-1 transition-colors"
                      >
                        Copy {source.key}&rsquo;s mapping
                      </button>
                    ))}
                  </div>
                )}

                {missing.length > 0 && (
                  <div className="rounded-xl border border-alarm/30 bg-alarm/5 px-3 py-2.5 space-y-2">
                    <div className="flex items-start gap-2">
                      <AlertTriangle size={13} className="mt-0.5 shrink-0 text-alarm" />
                      <p className="text-reported text-ink-2">
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
                          onClick={() => dropUnresolvable(ref, status)}
                          className="group inline-flex items-center gap-1.5 rounded-xl border border-alarm/30 px-2 py-0.5 text-reported text-ink-2 transition-colors hover:border-alarm hover:text-alarm"
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
                      onChange={(v) => updateProject(ref, { pickup: v })}
                      requireCanonical={false}
                    />
                    <JiraStatusRule
                      label="In progress"
                      description="Count as actively being worked on."
                      allStatuses={statuses}
                      value={project.in_progress}
                      onChange={(v) => updateProject(ref, { in_progress: v })}
                      requireCanonical={true}
                      canonicalPrompt="Claim →"
                    />
                    <JiraStatusRule
                      label="In review"
                      description="Optional — the status that means work awaits human review."
                      allStatuses={statuses}
                      value={project.in_review}
                      onChange={(v) => updateProject(ref, { in_review: v })}
                      requireCanonical={true}
                      canonicalPrompt="Review →"
                    />
                    <JiraStatusRule
                      label="Done"
                      description="Count as complete (add every variant — e.g. Resolved + Verified)."
                      allStatuses={statuses}
                      value={project.done}
                      onChange={(v) => updateProject(ref, { done: v })}
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
          placeholder="Search Jira projects"
          aria-label="Search Jira projects"
          className="w-full bg-transparent border-0 focus:outline-none text-body text-ink-1 placeholder-ink-3"
        />
      </div>

      {catalogError !== '' && <p className="text-reported text-alarm">{catalogError}</p>}
      {catalogError === '' && catalogLoading && rows.length === 0 && (
        <p className="text-ui text-ink-3 italic">Loading projects…</p>
      )}
      {catalogError === '' && !catalogLoading && rows.length === 0 && (
        <p className="text-ui text-ink-3 italic">
          {search.trim() === ''
            ? 'This organization’s Jira credential can’t see any projects.'
            : `No Jira project matches “${search.trim()}”.`}
        </p>
      )}

      <div className="divide-y divide-border-subtle rounded-xl border border-line-1 overflow-hidden">
        {rows.map((row) => (
          <div key={row.ref} className="flex items-center gap-3 bg-raised px-3 py-2">
            <div className="min-w-0 flex-1">
              <span className="text-body font-medium text-ink-1">{row.key}</span>
              {row.name !== '' && (
                <span className="ml-2 text-ui text-ink-3 truncate">{row.name}</span>
              )}
              {!row.inCatalog && (
                <span className="ml-2 text-reported text-ink-2">
                  not visible to this credential
                </span>
              )}
            </div>
            <button
              type="button"
              onClick={() =>
                row.watched ? unwatch(row.ref) : row.candidate && watch(row.candidate)
              }
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
          More projects match than fit here — narrow the search to reach them.
        </p>
      )}
    </div>
  )

  const inner = (
    <>
      {bare ? (
        <div className="mb-4 space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Jira projects</h2>
          <p className="text-body leading-relaxed text-ink-3">
            Watch the Jira projects this team works from, then map each one&rsquo;s statuses to
            pickup / in-progress / done.
          </p>
        </div>
      ) : (
        <h2 className="mb-4 text-body font-medium text-ink-2">Jira projects</h2>
      )}
      {!connected ? (
        <p className="text-ui text-ink-3 italic">
          Connect Jira under organization settings before configuring tracked projects.
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
