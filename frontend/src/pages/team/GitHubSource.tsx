import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import { Odometer, Ticker } from '../../ui/odometer/Odometer'
import SourceChart from '../../ui/sourcechart/SourceChart'
import { SourceFrame, FigureRow, FilterField } from './SourceFrame'
import { figureAt, labelAt } from './schedule'
import type { SourceBodyProps } from './SourceFrame'
import { apiListAll } from '../../lib/apiClient'
import {
  useTeamActivity,
  activitySource,
  activitySeries,
  sinceParts,
  sinceLabel,
} from '../../hooks/useTeamActivity'
import { fetchTeamRepos, saveTeamRepos } from '../settings/teamConfig'
import type { GitHubRepo } from '../../lib/githubRepos'

// GitHub, as this team's event source.
//
// The page answers one question in two halves: what the repositories sent
// (left) and which repositories are watched (right). The verbs live on the
// table, because tracking is a property of a repository and not of the page.
//
// The page's own figures — events, what became of them, runs, the time since
// the last poll, and the fortnight chart — come from the team activity node,
// fetched at the two windows the surface reads (seven days for figures, a
// fortnight for the chart). FIGURES THE API DOES NOT ANSWER READ `—`: the
// per-repository columns (runs, last event) still need a per-repo cut that
// does not exist, and a zero there would be a claim about a repository's
// week.

const PROSE =
  'Tracked repositories spawn events this team can automate, like PRs in need of review or CI failing. ' +
  'Agents can also materialize these repos at will during a run. Changes do not apply to runs already in-flight.'

/**
 * A repository's share of the team's runs: a hatched track filled to the
 * value, with the number at the right so the column reads as a ranking and as
 * a measurement at once. An em dash where nothing counts runs yet.
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
  const [tracked, setTracked] = useState<string[] | null>(null)
  const [visible, setVisible] = useState<string[] | null>(null)
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')

  // Two windows of the same node: the figures read seven days, the chart a
  // fortnight. The node is windowed by the caller, so each is asked for
  // rather than derived from the other.
  const activity7 = useTeamActivity(teamId, 7)
  const activity14 = useTeamActivity(teamId, 14)
  const flow = activitySource(activity7, 'github')
  const poll = sinceParts(flow?.last_poll_at ?? null)

  // The authoritative tracked set, so a second verb inside one undo window
  // computes from what the last one wrote rather than from what the screen
  // happened to be showing.
  const trackedRef = useRef<string[]>([])

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
    // The org's whole visible estate, so an untracked repository is offerable
    // rather than invisible. Walked to completion because the table filters
    // client-side, so a partial set would be a filter that silently misses.
    // Best-effort: without it the page still shows and can still unwatch what
    // is tracked.
    void apiListAll<GitHubRepo>('/api/github/repos/list')
      .then((repos) => {
        if (live) setVisible(repos.map((r) => r.full_name))
      })
      .catch(() => {
        if (live) setVisible(null)
      })
    return () => {
      live = false
    }
  }, [teamId])

  const rows: TableRow[] = useMemo(() => {
    if (!tracked) return []
    const on = new Set(tracked)
    const names: string[] = []
    const seen = new Set<string>()
    for (const name of visible ?? []) {
      seen.add(name)
      names.push(name)
    }
    // A tracked repository the picker cannot see is still tracked — an App
    // installation narrowed after the fact, or a credential that stopped
    // resolving. Dropping it would make the page lie about what it watches.
    for (const name of tracked) if (!seen.has(name)) names.push(name)
    names.sort((a, b) => {
      const t = Number(on.has(b)) - Number(on.has(a))
      return t || a.localeCompare(b)
    })
    return names.map((name) => ({
      id: name,
      name,
      tracked: on.has(name),
      runs: null,
      last: null,
    }))
  }, [tracked, visible])

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return q ? rows.filter((r) => String(r.name).toLowerCase().includes(q)) : rows
  }, [rows, filter])

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
        color: (r) => (r.tracked ? 'var(--color-ink-1)' : 'var(--color-ink-4)'),
      },
      {
        key: 'runs',
        label: 'RUNS',
        width: '168px',
        render: (r) => runBar(r, maxRuns),
        // Not a claim about runs: with nothing counting them the watched set
        // is the only ordering the column can honestly offer.
        sortValue: (r) => -((r.runs as number | null) ?? (r.tracked ? 1 : 0)),
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

  // The set-replace the two verbs resolve to. Sent once, when the undo window
  // closes — an undone change is never sent at all.
  const commit = useCallback(
    (actionId: string, ids: Array<string | number>) => {
      const picked = new Set(ids.map(String))
      const next =
        actionId === 'watch'
          ? trackedRef.current.concat([...picked].filter((n) => !trackedRef.current.includes(n)))
          : trackedRef.current.filter((n) => !picked.has(n))
      trackedRef.current = next
      setTracked(next)
      void saveTeamRepos(teamId, next).then((res) => {
        if (!res.ok) setError(res.error)
      })
    },
    [teamId],
  )

  const trackedCount = tracked ? tracked.length : null

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
            <FigureRow
              label={visible ? `tracked of ${visible.length} visible` : 'tracked'}
              at={labelAt(0)}
            >
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
            <Table
              label="REPOSITORIES"
              build={!!tracked}
              columns={columns}
              rows={shown}
              pageSize="auto"
              sortKey="runs"
              sortDir={1}
              barPosition="absolute"
              emptyLabel={tracked ? 'no repositories' : 'loading…'}
              // Selection exists for the verbs, so it goes with them.
              selectable={isAdmin}
              headerRight={
                <FilterField value={filter} onChange={setFilter} label="Filter repositories" />
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
