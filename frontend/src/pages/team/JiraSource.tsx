import { useCallback, useEffect, useMemo, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import StatusRules from '../../ui/statusrules/StatusRules'
import type { StatusMap } from '../../ui/statusrules/StatusRules'
import { SourceFrame, FilterField } from './SourceFrame'
import type { SourceBodyProps } from './SourceFrame'
import { apiJSON } from '../../lib/apiClient'
import { useTeamActivity, activitySource, sinceLabel } from '../../hooks/useTeamActivity'
import { fetchTeamSettings, saveTeamJiraProjects } from '../settings/teamConfig'
import type { JiraProjectConfig } from '../settings/teamConfig'

// Jira, as this team's event source.
//
// JIRA'S BOARD IS ITS BUILD. This is the one source page with no chart: the
// status board already lands column by column and owns that space, so the two
// halves weigh the same rather than 2:3, and the prose and the board share a
// column because they are one argument.
//
// The board reads and does not write. Two reasons, and both have to go before
// it can drag here: `StatusRules` reports no change out to a caller, and the
// stored rule set has three columns to the board's four — a save from this
// surface would silently drop whatever was put in IN REVIEW (backend-needs
// 14). Under `interactive={false}` the board is absent-not-disabled: no drag,
// no staging, no grab cursor, no tab stops. Nothing is greyed out, because the
// mapping answers "where does our work come from", which a member legitimately
// needs.

const PROSE =
  'Tracked Jira projects spawn events this team can automate, like new tickets being assigned to team ' +
  'members or priorities changing. Then map your ticket lifecycle to our four states so Triage Factory ' +
  'can understand your workflows. Changes do not apply to runs already in-flight.'

type JiraStatus = { id: string; name: string }

/** The stored three rules, in the board's four columns. */
function mapFor(p: JiraProjectConfig | undefined): StatusMap | null {
  if (!p) return null
  return {
    ready: { members: (p.pickup?.members ?? []).map((m) => m.name), primary: null },
    inprogress: {
      members: (p.in_progress?.members ?? []).map((m) => m.name),
      primary: p.in_progress?.canonical?.name ?? null,
    },
    // Nothing stores an in-review rule yet, so the column is genuinely empty
    // rather than unmapped-looking: every status it would hold sits in the
    // tray, which is where an unmapped status belongs.
    review: { members: [], primary: null },
    done: {
      members: (p.done?.members ?? []).map((m) => m.name),
      primary: p.done?.canonical?.name ?? null,
    },
  }
}

export default function JiraSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  const [projects, setProjects] = useState<JiraProjectConfig[] | null>(null)
  const [fetched, setFetched] = useState<string[] | null>(null)
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')

  // The frame's three figures, from the team activity node at the window
  // their labels name.
  const flow = activitySource(useTeamActivity(teamId, 7), 'jira')

  useEffect(() => {
    if (!teamId) return
    let live = true
    void fetchTeamSettings(teamId)
      .then((data) => {
        if (!live) return
        if (!data) {
          setError('Could not read this team’s Jira configuration.')
          return
        }
        setProjects(data.jira_projects ?? [])
      })
      .catch(() => {
        if (live) setError('Could not read this team’s Jira configuration.')
      })
    return () => {
      live = false
    }
  }, [teamId])

  const keys = useMemo(() => (projects ?? []).map((p) => p.key.trim()).filter(Boolean), [projects])

  // Every status the tracked projects have. The endpoint returns the
  // intersection across the keys it is given, which is the same set a
  // transition has to be legal in.
  useEffect(() => {
    if (!keys.length) return
    let live = true
    const query = keys.map((k) => 'project=' + encodeURIComponent(k)).join('&')
    void apiJSON<JiraStatus[]>('/api/jira/statuses?' + query)
      .then((list) => {
        if (live) setFetched(list.map((s) => s.name))
      })
      .catch(() => {
        // The board still draws what is mapped; only the tray goes empty.
        if (live) setFetched([])
      })
    return () => {
      live = false
    }
  }, [keys])

  // A team tracking no project has no statuses to offer, and that is an answer
  // rather than a pending read — derived rather than written, so the effect
  // above only ever handles the fetch.
  const statuses = keys.length ? fetched : projects ? [] : null

  const board = useMemo(() => mapFor((projects ?? [])[0]), [projects])
  const showing = (projects ?? [])[0]?.key ?? ''

  const rows: TableRow[] = useMemo(
    () =>
      (projects ?? []).map((p) => ({
        id: p.key,
        key: p.key,
        // Only the key is stored; nothing returns a project's display name or
        // its issue count for a team.
        name: null,
        issues: null,
        watched: true,
      })),
    [projects],
  )

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return q ? rows.filter((r) => String(r.key).toLowerCase().includes(q)) : rows
  }, [rows, filter])

  const columns: TableColumn[] = useMemo(
    () => [
      {
        key: 'key',
        label: 'KEY',
        width: '78px',
        color: (r) => (r.watched ? 'var(--color-ink-1)' : 'var(--color-ink-4)'),
      },
      {
        key: 'name',
        label: 'PROJECT',
        floor: 120,
        render: (r) => r.name ?? '—',
        color: () => 'var(--color-ink-4)',
      },
      {
        key: 'issues',
        label: 'ISSUES',
        align: 'end',
        drop: 1,
        render: (r) => r.issues ?? '—',
        sortValue: (r) => -(r.issues ?? 0),
        color: () => 'var(--color-ink-4)',
      },
    ],
    [],
  )

  const commit = useCallback(
    (actionId: string, ids: Array<string | number>) => {
      // The write is a full replace-set, so a set we never read is not a set we
      // may rewrite: committing against `null` would PUT [] and untrack every
      // project the team has.
      if (actionId !== 'unwatch' || projects === null) return
      const dropped = new Set(ids.map(String))
      const next = projects.filter((p) => !dropped.has(p.key))
      setProjects(next)
      void saveTeamJiraProjects(teamId, next).then((res) => {
        if (!res.ok) setError(res.error)
      })
    },
    [teamId, projects],
  )

  return (
    <SourceFrame
      source="jira"
      name="Jira"
      teamName={teamName}
      onBack={onBack}
      events={flow?.events ?? null}
      tasks={flow ? flow.tasks : null}
      sincePoll={sinceLabel(flow?.last_poll_at ?? null)}
    >
      <div className="sp-cols" data-split="even">
        {/* What the page is for, and the map that answers it: the description
            and the board are one argument, so they share a column. */}
        <div className="sp-jira-left">
          <p className="sp-prose">{PROSE}</p>
          <div className="sp-subhead">
            <span className="sp-subhead-t">What each status means</span>
            <span className="sp-subhead-p">
              Map ticket statuses to these primitives so the factory knows what state every ticket
              is in. ★ is the one written back to Jira when the factory moves a ticket into that
              column.
            </span>
          </div>
          <div className="sp-board">
            {board ? (
              <StatusRules
                map={board}
                statuses={statuses}
                showProjects={false}
                interactive={false}
                build
                note={
                  (projects ?? []).length > 1
                    ? `showing ${showing} of ${(projects ?? []).length} tracked projects · edit the mapping in Settings`
                    : `showing ${showing} · edit the mapping in Settings`
                }
              />
            ) : (
              <p className="sp-prose">
                {projects
                  ? 'No Jira project is tracked by this team yet, so there is no lifecycle to map.'
                  : ''}
              </p>
            )}
          </div>
        </div>

        {/* The numbers and the projects they count, in that order. */}
        <div className="sp-jira-right">
          <div className="sp-jira-head">
            <span className="sp-subhead-t">Which projects</span>
            <span className="sp-grow" />
            <FilterField value={filter} onChange={setFilter} label="Filter projects" />
          </div>
          <div className="sp-tablewrap">
            {error ? <p className="sp-error">{error}</p> : null}
            <Table
              build={!!projects}
              columns={columns}
              rows={shown}
              pageSize="auto"
              sortKey="key"
              sortDir={1}
              showHeader={false}
              barPosition="absolute"
              emptyLabel={projects ? 'no projects tracked' : 'loading…'}
              selectable={isAdmin}
              // Only one direction is offered, because only one is reachable:
              // nothing lists the Jira projects this team COULD watch, so
              // there are no unwatched rows for a Watch verb to act on.
              actions={
                isAdmin
                  ? [
                      {
                        id: 'unwatch',
                        label: 'Stop watching',
                        tone: 'bad',
                        message: (n) =>
                          n + (n === 1 ? ' project' : ' projects') + ' no longer watched',
                      },
                    ]
                  : []
              }
              mutate={isAdmin ? () => null : null}
              bar={isAdmin ? {} : null}
              onCommit={isAdmin ? (id, ids) => commit(id, ids) : null}
            />
          </div>
        </div>
      </div>
    </SourceFrame>
  )
}
