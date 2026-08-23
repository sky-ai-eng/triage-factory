import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import StatusRules from '../../ui/statusrules/StatusRules'
import type { StatusItem, StatusMap, StatusRule } from '../../ui/statusrules/StatusRules'
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
import type { JiraStatusRef, JiraStatusRuleValue } from '../../components/JiraStatusRule'
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
// FOR A TEAM ADMIN, THE BOARD IS THE EDITOR. Every gesture — a chip landing
// in a column, a ★ moving, a suggested mapping — reports the next map, which
// maps back onto the four stored rules and saves as a replace-set the moment
// it lands. The rules stay valid by the board's own construction: a non-empty
// write-target column always carries a ★ (one is minted on first landing and
// reassigned when its chip leaves), READY never carries one, and a ★ is
// always a member. For a member — or while the source is off — the board is
// read-only and absent-not-disabled: no drag, no staging, no grab cursor, no
// tab stops. Nothing is greyed out, because the mapping answers "where does
// our work come from", which a member legitimately needs.

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

/** A stored ref as a board item. A legacy name-only ref — stored before
 *  statuses were identified — falls back to its name as the identity: stable
 *  for display and drag, and resolved to a real id at write time against the
 *  live vocabulary, which is how such a rule gains its ids on its next save. */
const asItem = (m: JiraStatusRef): StatusItem => ({ id: m.id || m.name, label: m.name })

const primaryOf = (c: JiraStatusRef | null | undefined): string | null =>
  c ? c.id || c.name : null

/** The four stored rules, in the board's four columns. */
function mapFor(p: JiraProjectConfig | undefined): StatusMap | null {
  if (!p) return null
  return {
    ready: { members: (p.pickup?.members ?? []).map(asItem), primary: null },
    inprogress: {
      members: (p.in_progress?.members ?? []).map(asItem),
      primary: primaryOf(p.in_progress?.canonical),
    },
    // Optional rule: a team that maps nothing here keeps its review-state
    // tickets In Progress on the Jira side, and the empty column is that
    // choice rendered rather than a gap.
    review: {
      members: (p.in_review?.members ?? []).map(asItem),
      primary: primaryOf(p.in_review?.canonical),
    },
    done: {
      members: (p.done?.members ?? []).map(asItem),
      primary: primaryOf(p.done?.canonical),
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
  const boardStatuses: StatusItem[] = shown
    ? (statusesByKey[shownK] ?? []).map((st) => ({ id: st.id, label: st.name }))
    : []

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

  // Which write the adopted response may land on: the PUT answers with the
  // set AS STORED (status names re-resolved from Jira), and adopting a stale
  // answer over a newer optimistic set would undo the newer gesture.
  const commitSeq = useRef(0)

  // The one write door for this page — the table's verbs and the board's
  // gestures alike. Optimistic, latest-wins, and honest on failure: the
  // optimistic set rolls back rather than leaving the page rendering a
  // configuration the server never accepted.
  const persist = useCallback(
    (next: JiraProjectConfig[], prev: JiraProjectConfig[]) => {
      setProjects(next)
      const seq = ++commitSeq.current
      void saveTeamJiraProjects(teamId, next).then((res) => {
        if (commitSeq.current !== seq) return
        if (res.ok) setProjects(res.projects)
        else {
          setProjects(prev)
          setError(res.error)
        }
      })
    },
    [teamId],
  )

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
      persist(next, projects)
    },
    [projects, persist],
  )

  // A board gesture, mapped back onto the four stored rules and saved. The
  // board's construction keeps the result valid — a non-empty write-target
  // column always carries a ★ and READY never does — so this is a projection,
  // not a validation site.
  const applyBoard = useCallback(
    (next: StatusMap) => {
      if (!shown || projects === null) return
      const vocab = statusesByKey[shownK] ?? []
      const prior = [
        ...(shown.pickup?.members ?? []),
        ...(shown.in_progress?.members ?? []),
        ...(shown.in_review?.members ?? []),
        ...(shown.done?.members ?? []),
      ]
      // An item resolves back to a full ref through the live vocabulary — by
      // id, then by name for a legacy chip that predates ids — falling back to
      // the ref it was drawn from, so a status the vocabulary can no longer
      // see survives gestures it was not part of.
      const toRef = (it: StatusItem): JiraStatusRef =>
        vocab.find((v) => v.id === it.id) ??
        vocab.find((v) => v.name === it.label) ??
        prior.find((r) => (r.id || r.name) === it.id) ?? { id: '', name: it.label }
      const rule = (r: StatusRule, withCanonical: boolean): JiraStatusRuleValue => {
        const members = r.members.map(toRef)
        if (!withCanonical) return { members }
        const at = r.primary === null ? -1 : r.members.findIndex((m) => m.id === r.primary)
        return { members, canonical: at >= 0 ? members[at] : null }
      }
      const edited: JiraProjectConfig = {
        ...shown,
        pickup: rule(next.ready, false),
        in_progress: rule(next.inprogress, true),
        in_review: rule(next.review, true),
        done: rule(next.done, true),
      }
      persist(
        projects.map((p) => (normKey(p.key) === shownK ? edited : p)),
        projects,
      )
    },
    [shown, shownK, projects, statusesByKey, persist],
  )

  // The note under the board: the strip names the shown project when there is
  // one to pick, so the note only carries the key when it is the sole voice.
  // An admin is told the board saves; a member is only told what is missing.
  const note = (() => {
    if (!shown) return ''
    const parts: string[] = []
    if (watched.length <= 1) parts.push(`showing ${shownK}`)
    if (isAdmin && !offReason) {
      parts.push(
        projectIsArmed(shown)
          ? 'drag to remap · ★ is what TF writes back · saves as it lands'
          : 'not armed yet · drag statuses into the columns to arm it',
      )
    } else if (!projectIsArmed(shown)) {
      parts.push('statuses not mapped yet')
    }
    return parts.join(' · ')
  })()

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
                value={board}
                onChange={isAdmin && !offReason ? applyBoard : undefined}
                statuses={boardStatuses}
                showProjects={false}
                interactive={isAdmin && !offReason}
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
