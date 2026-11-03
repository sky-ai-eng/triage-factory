import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import { Odometer, Ticker } from '../../ui/odometer/Odometer'
import SourceChart from '../../ui/sourcechart/SourceChart'
import { SourceFrame, FigureRow, FilterField } from './SourceFrame'
import { figureAt, labelAt } from './schedule'
import type { SourceBodyProps } from './SourceFrame'
import { useWebSocket } from '../../hooks/useWebSocket'
import {
  useTeamActivity,
  activitySource,
  activitySeries,
  activityTracked,
  sinceParts,
  sinceLabel,
} from '../../hooks/useTeamActivity'
import { ago, minutesSince } from '../../ui/table/cells'
import { fetchTeamRepos, saveTeamRepos } from '../settings/teamConfig'
import {
  candidateKeys,
  isRetired,
  listReachableRepos,
  refFromCandidate,
  repoRefKey,
  repoRefKeys,
  repoRefSlug,
} from '../../lib/githubRepos'
import type { GitHubRepo, RepoListStatus, RepoRef } from '../../lib/githubRepos'
import type { WSEvent } from '../../types'

// GitHub, as this team's event source.
//
// The page answers one question in two halves: what the repositories sent
// (left) and which repositories are watched (right). The verbs live on the
// table, because tracking is a property of a repository and not of the page.
//
// The page's own figures — events, what became of them, runs, the time since
// the last poll, and the fortnight chart — come from the team activity node,
// fetched at the two windows the surface reads (seven days for figures, a
// fortnight for the chart). The per-repository columns (runs, last event)
// come from the node's tracked-set cut at the seven-day window, which answers
// for watched repositories only. FIGURES THE API DOES NOT ANSWER READ `—`: a
// repository the team does not watch has no row in the cut, and a zero there
// would be a claim about a repository's week.
//
// The table holds two reads. The team's tracked set is one resource, read
// whole, and is the table's first section, ranked by runs. The repositories
// the org can reach are a list route the page holds pages of: its search is
// the route's `search` and its order is the route's, so the page never
// narrows or reorders a page it holds. While the search box is empty the
// catalog follows the tracked section with the repositories the team does not
// watch; once a search is applied, the table is the route's answer to it, in
// the route's order, each watched repository marked where it falls.

const PROSE =
  'Tracked repositories spawn events this team can automate, like PRs in need of review or CI failing. ' +
  'Agents can also materialize these repos at will during a run. Changes do not apply to runs already in-flight.'

/** How long after the last keystroke before asking for the searched catalog. */
const SEARCH_DEBOUNCE_MS = 250

/** How often to re-ask while the org's reachable set has never been mirrored.
 *  The `github_repos_updated` ping is the fast path; this resolves the state
 *  where the ping does not land (a socket that reconnected mid-refresh). */
const DISCOVERY_POLL_MS = 4_000

/** The rows render in the order given: the tracked section's, then the list
 *  route's. No column names a `sortKey`, so no header reorders them, and this
 *  is never called. */
const KEEP_ORDER = () => {}

/** The catalog pages the page holds, and the search they answer. */
interface Catalog {
  search: string
  items: GitHubRepo[]
  next: string
  total: number
  status: RepoListStatus
}

/**
 * A repository's share of the team's runs: a hatched track filled to the
 * value, with the number at the right so the column reads as a ranking and as
 * a measurement at once. An em dash where nothing counts the repository's
 * runs.
 */
function runBar(row: TableRow, max: number) {
  const runs = row.runs as number | null
  return (
    <span className="sp-runs">
      <span className="sp-runs-track">
        {runs ? (
          <span
            style={{
              width: Math.round((runs / max) * 100) + '%',
              background: row.tracked
                ? 'var(--color-warm)'
                : 'color-mix(in srgb, var(--color-ink-4) 29%, var(--color-line-2))',
            }}
          />
        ) : null}
      </span>
      <span className="sp-runs-v" data-none={runs ? undefined : ''}>
        {runs ?? '—'}
      </span>
    </span>
  )
}

export default function GitHubSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  const [tracked, setTracked] = useState<RepoRef[] | null>(null)
  const [catalog, setCatalog] = useState<Catalog | null>(null)
  const [catalogError, setCatalogError] = useState('')
  // What the org can reach in all, from the last read with no search: the
  // figure names it, and a searched total would be a different number.
  const [visibleTotal, setVisibleTotal] = useState<number | null>(null)
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')

  // Two windows of the same node: the figures read seven days, the chart a
  // fortnight. The node is windowed by the caller, so each is asked for
  // rather than derived from the other.
  const activity7 = useTeamActivity(teamId, 7)
  const activity14 = useTeamActivity(teamId, 14)
  const flow = activitySource(activity7, 'github')
  const poll = sinceParts(flow?.last_poll_at ?? null)
  const perRepo = useMemo(() => activityTracked(activity7, 'github'), [activity7])

  // The authoritative tracked set, so a second verb inside one undo window
  // computes from what the last one wrote rather than from what the screen
  // happened to be showing.
  const trackedRef = useRef<RepoRef[]>([])

  useEffect(() => {
    if (!teamId) return
    let live = true
    void fetchTeamRepos(teamId).then((repos) => {
      if (!live) return
      // A failed read must never read as "tracks nothing" — writing an empty
      // set back over that would wipe the team's repositories. The helper
      // answers null for exactly that case, distinct from a real empty set.
      if (repos === null) {
        setError('Could not read this team’s tracked repositories.')
        return
      }
      trackedRef.current = repos
      setTracked(repos)
    })
    return () => {
      live = false
    }
  }, [teamId])

  // Which catalog read the held state belongs to. Searches are typed faster
  // than they resolve, and the discovery poll and the websocket ping fire on
  // their own schedule, so reads come back out of order; only the latest one
  // may touch state.
  const readSeq = useRef(0)
  // The search the reader is asking for now, for the reads that fire on
  // their own schedule: re-asking an older search would let it land over the
  // newer one.
  const wanted = useRef('')

  // Best-effort: without the catalog the page still shows and can still
  // unwatch what is tracked.
  const readCatalog = useCallback(async (search: string, pageToken = '') => {
    const seq = ++readSeq.current
    try {
      const page = await listReachableRepos(search, pageToken)
      if (seq !== readSeq.current) return
      setCatalogError('')
      setCatalog((prev) =>
        pageToken && prev && prev.search === search
          ? {
              ...prev,
              items: [...prev.items, ...page.items],
              next: page.next_page_token,
              total: page.total_count,
              status: page.status,
            }
          : {
              search,
              items: page.items,
              next: page.next_page_token,
              total: page.total_count,
              status: page.status,
            },
      )
      if (!search && page.status === 'ready') setVisibleTotal(page.total_count)
    } catch {
      if (seq !== readSeq.current) return
      // A further page that failed keeps what is on screen; a first page that
      // failed has nothing true to show for its search.
      if (!pageToken) setCatalog(null)
      setCatalogError(
        search ? 'Could not search the repositories this organization can reach.' : '',
      )
    }
  }, [])

  // The first page for the search as it settles. An empty search is asked at
  // once — it is the mount read, and clearing the box should not wait.
  useEffect(() => {
    const search = filter.trim()
    wanted.current = search
    const id = setTimeout(() => void readCatalog(search), search ? SEARCH_DEBOUNCE_MS : 0)
    return () => clearTimeout(id)
  }, [filter, readCatalog])

  // The server refreshes the reachable set out of band and says so with a
  // payload-free ping; re-reading on it is what turns "discovering" into
  // repositories without a reload.
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (event.type === 'github_repos_updated') void readCatalog(wanted.current)
      },
      [readCatalog],
    ),
  )

  const discovering = catalog?.status === 'discovering'
  useEffect(() => {
    if (!discovering) return
    const id = setInterval(() => void readCatalog(wanted.current), DISCOVERY_POLL_MS)
    return () => clearInterval(id)
  }, [discovering, readCatalog])

  const loadMore = useCallback(() => {
    if (catalog?.next) void readCatalog(catalog.search, catalog.next)
  }, [catalog, readCatalog])

  // refsByRow maps each row id to its repository, so a verb can name what it
  // selected.
  const { rows, refsByRow } = useMemo((): {
    rows: TableRow[]
    refsByRow: Map<string, RepoRef>
  } => {
    if (!tracked) return { rows: [], refsByRow: new Map() }
    // A row is keyed by its repository's selection key, so a reachable
    // repository and the tracked entry that names it — by registry id or
    // GitHub id — are one row. A name alone pairs nothing a server id could.
    const trackedByKey = new Map(tracked.flatMap((r) => repoRefKeys(r).map((k) => [k, r] as const)))
    type Entry = { key: string; name: string; ref: RepoRef; on: boolean }
    const listed: Entry[] = []
    const seen = new Set<string>()
    for (const c of catalog?.items ?? []) {
      const hit = candidateKeys(c).find((k) => trackedByKey.has(k))
      const ref = hit ? trackedByKey.get(hit)! : refFromCandidate(c)
      const key = repoRefKey(ref)
      if (seen.has(key)) continue
      seen.add(key)
      listed.push({ key, name: c.full_name, ref, on: !!hit })
    }
    // The cut keys a repository by its registry id, which every tracked ref
    // carries once saved. A watched row is matched on it alone, so a
    // repository the team does not watch never borrows a figure.
    const unitOf = (e: Entry) => (e.on && e.ref.id ? perRepo.get(e.ref.id) : undefined)

    let entries: Entry[]
    if (catalog?.search) {
      entries = listed
    } else {
      // Every tracked repository, including one the org can no longer reach —
      // an App installation narrowed after the fact, a credential that
      // stopped resolving, or a repository GitHub no longer serves at all
      // (retired, its name now another repository's). Dropping it would make
      // the page lie about what it watches. Ranked by runs, the most first;
      // a repository nothing counts yet goes after every counted one.
      const watched: Entry[] = tracked.map((ref) => ({
        key: repoRefKey(ref),
        name: repoRefSlug(ref),
        ref,
        on: true,
      }))
      const runsOf = (e: Entry) => unitOf(e)?.runs ?? -1
      watched.sort((a, b) => runsOf(b) - runsOf(a) || a.name.localeCompare(b.name))
      entries = watched.concat(listed.filter((e) => !e.on))
    }
    return {
      rows: entries.map((e) => {
        const unit = unitOf(e)
        const min = minutesSince(unit?.last_event_at)
        return {
          id: e.key,
          name: e.name,
          tracked: e.on,
          retired: isRetired(e.ref),
          runs: unit ? unit.runs : null,
          last: min === null ? null : ago(min),
        }
      }),
      refsByRow: new Map(entries.map((e) => [e.key, e.ref])),
    }
  }, [tracked, catalog, perRepo])

  // Every repository the table can rank against. Held at 1 so a page with no
  // run data divides by something.
  const maxRuns = useMemo(
    () => Math.max(1, ...rows.map((r) => (r.runs as number | null) ?? 0)),
    [rows],
  )

  const columns: TableColumn[] = useMemo(
    () => [
      {
        key: 'name',
        label: 'REPOSITORY',
        // A retired row shares its name with the repository GitHub now
        // serves under it, so the two rows have to be told apart.
        render: (r) =>
          r.retired ? (
            <>
              {r.name} <span className="sp-retired">no longer on GitHub</span>
            </>
          ) : (
            r.name
          ),
        color: (r) => (r.tracked && !r.retired ? 'var(--color-ink-1)' : 'var(--color-ink-4)'),
      },
      {
        key: 'runs',
        label: 'RUNS',
        width: '168px',
        render: (r) => runBar(r, maxRuns),
      },
      {
        key: 'last',
        label: 'LAST EVENT',
        align: 'end',
        drop: 1,
        render: (r) => r.last ?? '—',
        color: () => 'var(--color-ink-4)',
      },
    ],
    [maxRuns],
  )

  // Saves go out one at a time, in the order their windows closed. A save that
  // registers a repository first is two requests, so a later save sent beside
  // it could land first and then be overwritten by the older set.
  const saving = useRef<Promise<unknown>>(Promise.resolve())

  // The set-replace the two verbs resolve to. Sent once, when the undo window
  // closes — an undone change is never sent at all.
  const commit = useCallback(
    (actionId: string, ids: Array<string | number>) => {
      const picked = new Set(ids.map(String))
      const current = trackedRef.current
      const have = new Set(current.map(repoRefKey))
      const next =
        actionId === 'watch'
          ? current.concat(
              [...picked].filter((k) => !have.has(k)).flatMap((k) => refsByRow.get(k) ?? []),
            )
          : current.filter((r) => !picked.has(repoRefKey(r)))
      trackedRef.current = next
      setTracked(next)
      const save = saving.current.then(() => saveTeamRepos(teamId, next))
      saving.current = save
      void save.then((res) => {
        if (!res.ok) {
          setError(res.error)
          return
        }
        // Adopt the stored set — registry ids for what was just registered —
        // unless a later verb has already moved past this one.
        if (trackedRef.current === next) {
          trackedRef.current = res.repos
          setTracked(res.repos)
        }
      })
    },
    [teamId, refsByRow],
  )

  const trackedCount = tracked ? tracked.length : null
  const trackedLabel =
    visibleTotal !== null
      ? `tracked of ${visibleTotal} visible`
      : discovering
        ? 'tracked · discovering repositories'
        : 'tracked'
  const remaining = catalog && catalog.next ? catalog.total - catalog.items.length : 0

  return (
    <SourceFrame
      source="github"
      name="GitHub"
      teamName={teamName}
      onBack={onBack}
      events={flow?.events ?? null}
      tasks={flow ? flow.tasks : null}
      sincePoll={sinceLabel(flow?.last_poll_at ?? null)}
    >
      <div className="sp-cols">
        <div className="sp-left">
          <p className="sp-prose">{PROSE}</p>

          {/* Keyed on the load, so the figures roll when the answer arrives
              rather than rolling to nothing and then jumping. */}
          <div className="sp-figrows" key={tracked ? 'on' : 'off'}>
            <FigureRow label={trackedLabel} at={labelAt(0)}>
              <Odometer value={trackedCount} at={figureAt(0)} label={`${trackedCount ?? 0}`} />
            </FigureRow>
            <FigureRow label="events in 7 days" at={labelAt(1)}>
              <Odometer
                value={flow?.events ?? null}
                at={figureAt(1)}
                label={`${flow?.events ?? 0}`}
              />
            </FigureRow>
            <FigureRow label="became tasks" at={labelAt(2)} tone="warm">
              <Ticker value={flow ? flow.tasks : null} variant="row" />
            </FigureRow>
            <FigureRow label="runs against these repositories" at={labelAt(3)}>
              <Odometer
                value={flow ? flow.runs : null}
                at={figureAt(3)}
                label={`${flow?.runs ?? 0}`}
              />
            </FigureRow>
            <FigureRow label="since the last poll" at={labelAt(4)} tone="faint">
              <Odometer
                value={poll ? poll.value : null}
                suffix={poll?.unit}
                at={figureAt(4)}
                label={poll ? `${poll.value}${poll.unit}` : undefined}
              />
            </FigureRow>
          </div>

          <SourceChart
            title="WHAT THE REPOSITORIES SENT"
            keys={['EVENTS', 'TASKS']}
            units={['events', 'tasks']}
            series={activitySeries(activity14, 'github', 14)}
          />
        </div>

        <div className="sp-right">
          <div className="sp-tablewrap">
            {error ? <p className="sp-error">{error}</p> : null}
            {catalogError ? <p className="sp-error">{catalogError}</p> : null}
            <Table
              label="REPOSITORIES"
              build={!!tracked}
              columns={columns}
              rows={rows}
              pageSize="auto"
              sort={null}
              onSortChange={KEEP_ORDER}
              barPosition="absolute"
              emptyLabel={
                !tracked
                  ? 'loading…'
                  : catalog?.search
                    ? 'no repositories match'
                    : discovering
                      ? 'discovering repositories…'
                      : 'no repositories'
              }
              // Selection exists for the verbs, so it goes with them.
              selectable={isAdmin}
              headerRight={
                <FilterField value={filter} onChange={setFilter} label="Search repositories" />
              }
              footer={
                remaining > 0 ? (
                  <button type="button" className="sp-more" onClick={loadMore}>
                    {remaining} more · show them
                  </button>
                ) : discovering ? (
                  <span className="sp-foot">
                    discovering the repositories this organization can reach…
                  </span>
                ) : null
              }
              actions={
                isAdmin
                  ? [
                      {
                        id: 'watch',
                        label: 'Watch',
                        message: (n) =>
                          n + (n === 1 ? ' repository is' : ' repositories are') + ' now watched',
                      },
                      {
                        id: 'unwatch',
                        label: 'Stop watching',
                        tone: 'bad',
                        message: (n) =>
                          n + (n === 1 ? ' repository' : ' repositories') + ' no longer watched',
                      },
                    ]
                  : []
              }
              mutate={isAdmin ? (row, id) => ({ ...row, tracked: id === 'watch' }) : null}
              bar={isAdmin ? {} : null}
              onCommit={isAdmin ? (id, ids) => commit(id, ids) : null}
            />
          </div>
        </div>
      </div>
    </SourceFrame>
  )
}
