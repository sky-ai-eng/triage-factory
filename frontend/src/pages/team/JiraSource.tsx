import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import StatusRules from '../../ui/statusrules/StatusRules'
import type { StatusMap } from '../../ui/statusrules/StatusRules'
import { SourceFrame, FilterField } from './SourceFrame'
import type { SourceBodyProps } from './SourceFrame'
import { apiJSON } from '../../lib/apiClient'
import { useTeamActivity, activitySource, sinceLabel } from '../../hooks/useTeamActivity'
import { useEventSources } from '../../hooks/useEventSources'
import { sourceUnavailableReason } from '../../lib/eventSources'
import {
  fetchTeamSettings,
  saveTeamJiraProjects,
  emptyProject,
  projectIsArmed,
} from '../settings/teamConfig'
import type { JiraProjectConfig } from '../settings/teamConfig'
import type { JiraStatusRef } from '../../components/JiraStatusRule'
import { listJiraProjects, type JiraProjectCandidate } from '../../lib/jiraProjects'

// Jira, as this team's event source.
//
// JIRA'S BOARD IS ITS BUILD. This is the one source page with no chart: the
// status board already lands column by column and owns that space, so the two
// halves weigh the same rather than 2:3, and the prose and the board share a
// column because they are one argument.
//
// Watching and arming are two different gestures. The table on the right picks
// from the org credential's live project catalog, and watching is one verb —
// a project joins the tracked set carrying no rules at all. Mapping its
// statuses (arming) happens in Settings, and until then the project simply
// contributes nothing to the poller's discovery query.
//
// EVERY PROJECT CARRIES ITS OWN MAP, so the board shows one project at a time
// and the key strip above it is the selector. The prototype pinned the board
// to the first project with a note; per-project rules make that a page that
// hides every other project's mapping, which is why the strip is this page's
// own device rather than the mock's.
//
// The board reads and does not write. The stored rules now cover all four
// columns, so what keeps this surface read-only is `StatusRules` itself: it
// reports no change out to a caller, and its chips are keyed by display name
// where a write needs the status ids. Both go together when the board becomes
// an editor. Under `interactive={false}` the board is absent-not-disabled: no drag,
// no staging, no grab cursor, no tab stops. Nothing is greyed out, because the
// mapping answers "where does our work come from", which a member legitimately
// needs.

const PROSE =
  'Watched Jira projects spawn events this team can automate, like new tickets being assigned to team ' +
  'members or priorities changing. Then map your ticket lifecycle to our four states so Triage Factory ' +
  'can understand your workflows. Changes do not apply to runs already in-flight.'

/** How long after the last keystroke before asking Jira for the narrowed
 *  catalog. The filter is a round trip, not an array scan. */
const SEARCH_DEBOUNCE_MS = 250

/** The canonical form of a project key — the same uppercase-trim the server
 *  applies at the write boundary, so a stored key and a catalog key compare
 *  as one value. */
const normKey = (key: string): string => key.trim().toUpperCase()

/** The stored three rules, in the board's four columns. Rules are keyed by
 *  status id on the wire; the board is a display of names, so it reads the
 *  server-resolved name off each ref. */
function mapFor(p: JiraProjectConfig | undefined): StatusMap | null {
  if (!p) return null
  return {
    ready: { members: (p.pickup?.members ?? []).map((m) => m.name), primary: null },
    inprogress: {
      members: (p.in_progress?.members ?? []).map((m) => m.name),
      primary: p.in_progress?.canonical?.name ?? null,
    },
    // Optional rule: a team that maps nothing here keeps its review-state
    // tickets In Progress on the Jira side, and the empty column is that
    // choice rendered rather than a gap.
    review: {
      members: (p.in_review?.members ?? []).map((m) => m.name),
      primary: p.in_review?.canonical?.name ?? null,
    },
    done: {
      members: (p.done?.members ?? []).map((m) => m.name),
      primary: p.done?.canonical?.name ?? null,
    },
  }
}

export default function JiraSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  const [projects, setProjects] = useState<JiraProjectConfig[] | null>(null)
  // The live catalog page for the current filter. `null` means the read
  // failed or has not landed — the table still shows what is watched, and
  // only the Watch direction has nothing to offer.
  const [candidates, setCandidates] = useState<JiraProjectCandidate[] | null>(null)
  const [truncated, setTruncated] = useState(false)
  // Each watched project's own status vocabulary, fetched when its board is
  // first shown. Per project because statuses come from a project's workflow
  // scheme: querying several keys returns their intersection, which hides
  // statuses a project genuinely has the moment two watched projects differ.
  const [statusesByKey, setStatusesByKey] = useState<Record<string, JiraStatusRef[]>>({})
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')
  const [shownKey, setShownKey] = useState('')

  // The frame's three figures, from the team activity node at the window
  // their labels name.
  const flow = activitySource(useTeamActivity(teamId, 7), 'jira')

  // Why Jira cannot reach this org right now, or null — which is also the
  // answer while the availability read is unresolved, so the page never
  // announces an outage it has not confirmed. When off, the live reads that
  // would just collect refusals (catalog, statuses) are held; the stored
  // configuration still renders, and removal still works, because removing
  // never asks Jira anything.
  const { stateOf } = useEventSources()
  const offReason = sourceUnavailableReason('jira', stateOf('jira'))

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

  // The catalog, re-read from Jira as the filter settles. Nothing is cached
  // server-side, so this is a live view of what the org credential can see —
  // and the filter runs server-side for the same reason: the page holds one
  // page of a proxied list, so a client-side scan would silently miss.
  useEffect(() => {
    if (offReason) return
    const q = filter.trim()
    let live = true
    const controller = new AbortController()
    const run = () =>
      listJiraProjects(q, { signal: controller.signal })
        .then((page) => {
          if (!live) return
          setCandidates(page.items)
          setTruncated(page.hasMore)
        })
        .catch(() => {
          if (!live || controller.signal.aborted) return
          setCandidates(null)
          setTruncated(false)
        })
    const timer = setTimeout(run, q ? SEARCH_DEBOUNCE_MS : 0)
    return () => {
      live = false
      controller.abort()
      clearTimeout(timer)
    }
  }, [filter, offReason])

  // While off, the catalog is derived-empty rather than reset: the last
  // fetch's leftovers must not render, and the effect above refetches the
  // moment the source comes back.
  const catalog = offReason ? null : candidates
  const catalogTruncated = offReason ? false : truncated

  const watched = useMemo(() => projects ?? [], [projects])
  const watchedKeys = useMemo(() => new Set(watched.map((p) => normKey(p.key))), [watched])

  // The board's project: the picked key while it is still watched, else the
  // first watched project — so unwatching what is shown falls back rather
  // than pinning a board to a project the team no longer watches.
  const shown = useMemo(
    () => watched.find((p) => normKey(p.key) === shownKey) ?? watched[0] ?? null,
    [watched, shownKey],
  )
  const shownK = shown ? normKey(shown.key) : ''

  useEffect(() => {
    if (!shownK || offReason || statusesByKey[shownK]) return
    let live = true
    void apiJSON<JiraStatusRef[]>('/api/jira/statuses?project=' + encodeURIComponent(shownK))
      .then((list) => {
        if (live) setStatusesByKey((m) => ({ ...m, [shownK]: list }))
      })
      .catch(() => {
        // The board still draws what is mapped; only the tray goes empty.
        if (live) setStatusesByKey((m) => ({ ...m, [shownK]: [] }))
      })
    return () => {
      live = false
    }
  }, [shownK, offReason, statusesByKey])

  const board = useMemo(() => mapFor(shown ?? undefined), [shown])
  // Never null into the board: StatusRules falls back to a demo status list
  // for null, and a demo vocabulary on a real team's page would be a claim.
  const statuses = shown ? (statusesByKey[shownK] ?? []).map((s) => s.name) : []

  const rows: TableRow[] = useMemo(() => {
    const byKey = new Map((catalog ?? []).map((c) => [normKey(c.key), c]))
    const needle = filter.trim().toLowerCase()
    const out: TableRow[] = []
    for (const p of watched) {
      const key = normKey(p.key)
      if (!key) continue
      // A watched project the catalog cannot see is still watched — deleted
      // upstream, or the credential's access narrowed. Dropping it would make
      // the page lie about what it watches (and make it unremovable).
      const name = byKey.get(key)?.name ?? null
      if (
        needle !== '' &&
        !key.toLowerCase().includes(needle) &&
        !(name ?? '').toLowerCase().includes(needle)
      ) {
        continue
      }
      out.push({ id: key, key, name, issues: null, watched: true })
    }
    for (const c of catalog ?? []) {
      const key = normKey(c.key)
      if (watchedKeys.has(key)) continue
      out.push({ id: key, key, name: c.name, issues: null, watched: false })
    }
    return out
  }, [catalog, filter, watched, watchedKeys])

  const columns: TableColumn[] = useMemo(
    () => [
      {
        key: 'key',
        label: 'KEY',
        width: '78px',
        color: (r) => (r.watched ? 'var(--color-ink-1)' : 'var(--color-ink-4)'),
        // Watched first, then the catalog's alphabet — the set the team acts
        // on outranks the set it could.
        sortValue: (r) => (r.watched ? '0' : '1') + String(r.key),
      },
      {
        key: 'name',
        label: 'PROJECT',
        floor: 120,
        render: (r) => r.name ?? '—',
        color: (r) => (r.watched ? 'var(--color-ink-2)' : 'var(--color-ink-4)'),
      },
      {
        key: 'issues',
        label: 'ISSUES',
        align: 'end',
        drop: 1,
        // Nothing counts a project's issues for a team yet; a number here
        // would be a claim.
        render: (r) => r.issues ?? '—',
        sortValue: (r) => -((r.issues as number | null) ?? 0),
        color: () => 'var(--color-ink-4)',
      },
    ],
    [],
  )

  // Which commit the adopted response may land on: the PUT answers with the
  // set AS STORED (status names re-resolved from Jira), and adopting a stale
  // answer over a newer optimistic set would undo the newer verb.
  const commitSeq = useRef(0)

  const commit = useCallback(
    (actionId: string, ids: Array<string | number>) => {
      // The write is a full replace-set, so a set we never read is not a set
      // we may rewrite: committing against `null` would PUT [] and untrack
      // every project the team has.
      if (projects === null) return
      const picked = new Set(ids.map((v) => normKey(String(v))))
      const have = new Set(projects.map((p) => normKey(p.key)))
      const next =
        actionId === 'watch'
          ? projects.concat([...picked].filter((k) => !have.has(k)).map((k) => emptyProject(k)))
          : projects.filter((p) => !picked.has(normKey(p.key)))
      setProjects(next)
      const seq = ++commitSeq.current
      void saveTeamJiraProjects(teamId, next).then((res) => {
        if (commitSeq.current !== seq) return
        if (res.ok) setProjects(res.projects)
        else setError(res.error)
      })
    },
    [teamId, projects],
  )

  // The note under the board: the strip names the shown project when there is
  // one to pick, so the note only carries the key when it is the sole voice.
  const note = shown
    ? (watched.length > 1 ? '' : `showing ${shownK} · `) +
      (projectIsArmed(shown)
        ? 'edit the mapping in Settings'
        : 'statuses not mapped yet · map them in Settings')
    : ''

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
          {offReason ? <p className="sp-offnote">{offReason}</p> : null}
          <div className="sp-subhead">
            <span className="sp-subhead-t">What each status means</span>
            <span className="sp-subhead-p">
              Map ticket statuses to these primitives so the factory knows what state every ticket
              is in. ★ is the one written back to Jira when the factory moves a ticket into that
              column.
            </span>
          </div>
          {watched.length > 1 ? (
            <div className="sp-projstrip" role="tablist" aria-label="Watched projects">
              {watched.map((p) => {
                const key = normKey(p.key)
                return (
                  <button
                    key={key}
                    type="button"
                    role="tab"
                    aria-selected={key === shownK}
                    className="sp-projkey"
                    data-on={key === shownK ? '' : undefined}
                    data-unarmed={projectIsArmed(p) ? undefined : ''}
                    title={projectIsArmed(p) ? undefined : 'statuses not mapped'}
                    onClick={() => setShownKey(key)}
                  >
                    {key}
                  </button>
                )
              })}
            </div>
          ) : null}
          <div className="sp-board">
            {board ? (
              <StatusRules
                key={shownK}
                map={board}
                statuses={statuses}
                showProjects={false}
                interactive={false}
                build
                note={note}
              />
            ) : (
              <p className="sp-prose">
                {projects
                  ? 'No Jira project is watched by this team yet, so there is no lifecycle to map.'
                  : ''}
              </p>
            )}
          </div>
        </div>

        {/* The projects: what is watched, and what the credential could
            watch, in that order. */}
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
              rows={rows}
              pageSize="auto"
              sortKey="key"
              sortDir={1}
              showHeader={false}
              barPosition="absolute"
              emptyLabel={projects ? 'no projects visible' : 'loading…'}
              // The catalog is a live proxy that reports no total, so a full
              // page can only say that narrowing would reveal more.
              footer={catalogTruncated ? 'more projects exist · narrow the filter' : undefined}
              selectable={isAdmin}
              actions={
                isAdmin
                  ? [
                      {
                        id: 'watch',
                        label: 'Watch',
                        message: (n) =>
                          n + (n === 1 ? ' project is' : ' projects are') + ' now watched',
                      },
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
              mutate={isAdmin ? (row, id) => ({ ...row, watched: id === 'watch' }) : null}
              bar={isAdmin ? {} : null}
              onCommit={isAdmin ? (id, ids) => commit(id, ids) : null}
            />
          </div>
        </div>
      </div>
    </SourceFrame>
  )
}
