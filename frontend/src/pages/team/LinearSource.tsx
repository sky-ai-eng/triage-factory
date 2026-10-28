import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import StatusRules from '../../ui/statusrules/StatusRules'
import type { StatusItem, StatusMap, StatusRule } from '../../ui/statusrules/StatusRules'
import SourceChart from '../../ui/sourcechart/SourceChart'
import Tooltip from '../../ui/tooltip/Tooltip'
import { SourceFrame, FilterField } from './SourceFrame'
import type { SourceBodyProps } from './SourceFrame'
import { EMPTY_STATUS_MAP, boardNote, eventsColumn } from './mapping'
import {
  fetchTeamSettings,
  linearTeamFromWire,
  linearTeamIsArmed,
  saveTeamLinearTeams,
  unmappedLinearTeam,
} from '../settings/teamConfig'
import type { LinearTeamConfig } from '../settings/teamConfig'
import type { JiraStatusRuleValue } from '../../components/JiraStatusRule'
import { useActiveOrgId } from '../../contexts/OrgContext'
import {
  useTeamActivity,
  activitySource,
  activityTracked,
  activitySeries,
  sinceLabel,
} from '../../hooks/useTeamActivity'
import { useEventSources } from '../../hooks/useEventSources'
import type { EventSourceState } from '../../hooks/useEventSources'
import { sourceUnavailableReason } from '../../lib/eventSources'
import { HttpError } from '../../lib/apiClient'
import {
  listLinearStates,
  listLinearTeams,
  lookupLinearTeam,
  type LinearStateOption,
  type LinearStateRef,
  type LinearTeamCandidate,
} from '../../lib/linearTeams'

// Linear, as this team's event source: Jira's page in Linear's vocabulary.
//
// The table on the right watches Linear teams from the org credential's live
// catalog; watching adds a team with every rule empty. The board on the left
// maps one watched team's workflow states into the four columns, and every
// gesture saves as it lands, as a replace-set of the whole watched set. Each
// rule is complete or empty on its own, so a team mapped in part is stored,
// and is polled once pickup, in progress and done are mapped. In review is
// optional and arms nothing.
//
// A member sees the same page with the verbs absent: no selection, no drag, no
// ★ moves. Nothing is greyed out.
//
// A paused Linear keeps its credential, so the catalog and the workflows still
// read and the board stays editable. An unconnected one has neither: the page
// shows what is stored, and a team can still be unwatched, which asks Linear
// nothing.
//
// LINEAR CAN BREAK A STORED MAPPING, in two ways the page names under the
// board and marks on the strip: a team the credential can no longer see (out
// of reach), and a ★ whose state was deleted from the workflow.

const PROSE =
  'Watched Linear teams send events this team can automate, like issues being assigned to team ' +
  'members or moving between states. Map each team’s workflow states to pickup, in progress and ' +
  'done so Triage Factory knows where an issue stands. Changes do not apply to runs already in-flight.'

/** How long after the last keystroke before asking Linear for the narrowed
 *  catalog. The filter is a round trip, not an array scan. */
const SEARCH_DEBOUNCE_MS = 250

const TIP_PRIVATE =
  'Private in Linear: only the team’s own members there can see its issues. The tasks they ' +
  'become here are visible to everyone on this team.'
const TIP_OUT_OF_REACH =
  'This team is still watched, but Linear no longer shows it to this organization’s Linear ' +
  'connection. It may have been deleted or made private, or the connection lost access. Nothing ' +
  'is polled from it until access returns; stop watching it to drop its mapping.'

/** The sentence under the prose for a Linear that cannot be read or polled. */
function offNote(state: EventSourceState | undefined): string | null {
  if (state === 'disabled') {
    return 'Linear is paused for this organization. Nothing is polled until it resumes; the mapping stays editable.'
  }
  const reason = sourceUnavailableReason('linear', state)
  return reason
    ? `${reason} The mapping below is what is stored; a team can still be unwatched.`
    : null
}

const asItem = (s: LinearStateRef): StatusItem => ({ id: s.id, label: s.name || s.id })

/** The four stored rules, in the board's four columns. */
function mapFor(t: LinearTeamConfig): StatusMap {
  const rule = (r: JiraStatusRuleValue<LinearStateRef>): StatusRule => ({
    members: r.members.map(asItem),
    primary: r.canonical?.id ?? null,
  })
  return {
    ready: { members: t.pickup.members.map(asItem), primary: null },
    inprogress: rule(t.in_progress),
    // Optional: a team that maps nothing here keeps its issues In Progress in
    // Linear while they await review.
    review: rule(t.in_review),
    done: rule(t.done),
  }
}

const sameRule = (a: StatusRule, b: StatusRule): boolean =>
  a.primary === b.primary &&
  a.members.length === b.members.length &&
  a.members.every((m) => b.members.some((n) => n.id === m.id))

/** A ★ whose state is gone from the team's live workflow, and the column it
 *  marks. Only a workflow that was read can say a state is missing from it. */
function deletedStar(
  t: LinearTeamConfig,
  states: LinearStateOption[] | null | undefined,
): { name: string; column: 'IN PROGRESS' | 'IN REVIEW' | 'DONE' } | null {
  if (!states || states.length === 0) return null
  const live = new Set(states.map((s) => s.id))
  const rules: Array<[JiraStatusRuleValue<LinearStateRef>, 'IN PROGRESS' | 'IN REVIEW' | 'DONE']> =
    [
      [t.in_progress, 'IN PROGRESS'],
      [t.in_review, 'IN REVIEW'],
      [t.done, 'DONE'],
    ]
  for (const [rule, column] of rules) {
    const c = rule.canonical
    if (c && !live.has(c.id)) return { name: c.name || c.id, column }
  }
  return null
}

/** What a deleted ★ means, by the column it marked: the admin's sentence names
 *  the repair, the member's who can make it. Only IN PROGRESS's ★ is written
 *  onto an issue (a claim moves the issue there), so only it names something
 *  the deletion stops. */
const STAR_CONSEQUENCE: Record<
  'IN PROGRESS' | 'IN REVIEW' | 'DONE',
  { admin: string; member: (key: string) => string }
> = {
  'IN PROGRESS': {
    admin: 'A claim can’t move an issue until another state there is starred.',
    member: (key) => `Claims can’t move ${key} issues until an admin stars another state.`,
  },
  'IN REVIEW': {
    admin: 'Star a state there to replace it.',
    member: () => 'An admin can star a state there to replace it.',
  },
  DONE: {
    admin: 'Star a state there to replace it.',
    member: () => 'An admin can star a state there to replace it.',
  },
}

/** A team's name is ink-2 while it is watched and reachable, and recedes
 *  otherwise. */
const nameInk = (r: TableRow): string =>
  r.watched && !r.gone ? 'var(--color-ink-2)' : 'var(--color-ink-4)'

/** A tag after a team's name, with the tooltip that says what it means. */
function Tag({ text, tip, alarm }: { text: string; tip: string; alarm?: boolean }) {
  return (
    <Tooltip content={tip} wrap={260} side="top" className="sp-taghost">
      <span className="sp-tag" data-tone={alarm ? 'alarm' : undefined}>
        {text}
      </span>
    </Tooltip>
  )
}

export default function LinearSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  // The org whose Linear catalog the live reads address; null while it is
  // still resolving, which holds them.
  const orgId = useActiveOrgId()
  const [teams, setTeams] = useState<LinearTeamConfig[] | null>(null)
  // The live catalog page for the current filter. `null` means the read failed
  // or has not landed — the table still shows what is watched, and only the
  // Watch direction has nothing to offer.
  const [candidates, setCandidates] = useState<LinearTeamCandidate[] | null>(null)
  const [truncated, setTruncated] = useState(false)
  // Each watched team's workflow, keyed by its Linear id and read when its
  // board is shown until a read answers; null when Linear answered that the
  // credential cannot see the team.
  const [statesByTeam, setStatesByTeam] = useState<Record<string, LinearStateOption[] | null>>({})
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')
  const [shownId, setShownId] = useState('')

  // What the page has learned about whether the org credential can see each
  // Linear team: every team a read has served, as it served it; the teams
  // Linear answered it has no such team for; and the unfiltered catalog,
  // which decides absence only when it fit on one page.
  const [seen, setSeen] = useState<Record<string, LinearTeamCandidate>>({})
  const [missing, setMissing] = useState<ReadonlySet<string>>(new Set())
  const [unfiltered, setUnfiltered] = useState<{ ids: Set<string>; complete: boolean } | null>(null)

  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  const { stateOf } = useEventSources()
  const state = stateOf('linear')
  // A paused source keeps its credential, so it still reads.
  const connected = state === 'available' || state === 'disabled'
  const note = offNote(state)

  // The frame's three figures and each watched team's events, from the team
  // activity node at the window their labels name; the chart reads its own
  // fortnight. Without a connection they hold their dashes.
  const activity = useTeamActivity(teamId, 7)
  const activity14 = useTeamActivity(teamId, 14)
  const flow = connected ? activitySource(activity, 'linear') : null
  const perTeam = useMemo(
    () => activityTracked(connected ? activity : null, 'linear'),
    [activity, connected],
  )

  useEffect(() => {
    if (!teamId) return
    let live = true
    void fetchTeamSettings(teamId).then((data) => {
      if (!live) return
      if (!data) {
        setError('Could not read this team’s Linear configuration.')
        return
      }
      const stored = (data.linear_teams ?? []).map(linearTeamFromWire)
      confirmed.current = { seq: 0, set: stored }
      setTeams(stored)
    })
    return () => {
      live = false
    }
  }, [teamId])

  const learn = useCallback((items: LinearTeamCandidate[]) => {
    if (items.length === 0) return
    setSeen((m) => ({ ...m, ...Object.fromEntries(items.map((c) => [c.id, c])) }))
    setMissing((g) => {
      if (!items.some((c) => g.has(c.id))) return g
      const next = new Set(g)
      for (const c of items) next.delete(c.id)
      return next
    })
  }, [])

  const markMissing = useCallback((id: string) => {
    setMissing((g) => (g.has(id) ? g : new Set(g).add(id)))
  }, [])

  // The catalog, re-read from Linear as the filter settles. The filter runs
  // server-side, because the page holds one page of a proxied list and a
  // client-side scan would silently miss.
  useEffect(() => {
    if (!connected || !orgId) return
    const q = filter.trim()
    let live = true
    const controller = new AbortController()
    const run = () =>
      listLinearTeams(orgId, q, { signal: controller.signal })
        .then((page) => {
          if (!live) return
          setCandidates(page.items)
          setTruncated(page.hasMore)
          learn(page.items)
          if (q === '') {
            setUnfiltered({ ids: new Set(page.items.map((c) => c.id)), complete: !page.hasMore })
          }
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
  }, [filter, connected, orgId, learn])

  // Without a connection the catalog is derived-empty rather than reset, and
  // nothing is said about reach: every team is out of it, and the note above
  // the board already says why.
  const catalog = connected ? candidates : null
  const catalogTruncated = connected ? truncated : false
  const isMissing = useCallback(
    (id: string) =>
      connected &&
      (missing.has(id) ||
        (!!unfiltered?.complete && !unfiltered.ids.has(id) && seen[id] === undefined)),
    [connected, missing, unfiltered, seen],
  )

  // A workspace with more teams than one catalog page cannot answer for a
  // watched team the page left off, so each such team is asked about by id,
  // once. A read that fails establishes nothing, and the team keeps no tag.
  const probed = useRef(new Set<string>())
  useEffect(() => {
    if (!connected || !orgId || teams === null || !unfiltered || unfiltered.complete) return
    for (const t of teams) {
      if (seen[t.id] || missing.has(t.id) || probed.current.has(t.id)) continue
      probed.current.add(t.id)
      void lookupLinearTeam(orgId, t.id).then((found) => {
        if (!mounted.current) return
        if (found === 'gone') markMissing(t.id)
        else if (found) learn([found])
      })
    }
  }, [connected, orgId, teams, unfiltered, seen, missing, learn, markMissing])

  const watched = useMemo(() => teams ?? [], [teams])
  const watchedIds = useMemo(() => new Set(watched.map((t) => t.id)), [watched])

  // The board's team: the picked one while it is still watched, else the
  // first watched team.
  const shown = useMemo(
    () => watched.find((t) => t.id === shownId) ?? watched[0] ?? null,
    [watched, shownId],
  )
  const shownTeamId = shown?.id ?? ''

  useEffect(() => {
    if (!shownTeamId || !connected || !orgId || shownTeamId in statesByTeam) return
    let live = true
    void listLinearStates(orgId, shownTeamId)
      .then((list) => {
        if (live) setStatesByTeam((m) => ({ ...m, [shownTeamId]: list }))
      })
      .catch((e) => {
        if (!live) return
        // A 404 is Linear saying the credential cannot see the team at all,
        // which is an answer, and is kept. Any other failure records nothing,
        // so the read is asked again the next time this board is shown; until
        // then the board still draws what is mapped, and only the tray is
        // empty.
        if (e instanceof HttpError && e.status === 404) {
          setStatesByTeam((m) => ({ ...m, [shownTeamId]: null }))
          markMissing(shownTeamId)
        }
      })
    return () => {
      live = false
    }
  }, [shownTeamId, connected, orgId, statesByTeam, markMissing])

  const shownStates = shown && connected ? statesByTeam[shown.id] : undefined
  const shownMissing = shown ? isMissing(shown.id) : false
  const shownStar = shown ? deletedStar(shown, shownStates) : null
  // A team out of reach cannot be mapped: every change to it asks Linear to
  // confirm states it will no longer answer for.
  const editable = isAdmin && connected && !shownMissing

  const board = useMemo(() => (shown ? mapFor(shown) : null), [shown])
  // Never null into the board: StatusRules falls back to a demo vocabulary for
  // null, and a demo vocabulary on a real team's page would be a claim.
  const boardStatuses: StatusItem[] = (shownStates ?? []).map((st) => ({
    id: st.id,
    label: st.name,
  }))

  const rows: TableRow[] = useMemo(() => {
    const needle = filter.trim().toLowerCase()
    const out: TableRow[] = []
    for (const t of watched) {
      // A watched team the credential cannot see is still watched, and keeps
      // the key and name it was stored with: dropping it would make the page
      // lie about what it watches, and leave it unremovable.
      const gone = isMissing(t.id)
      const live = gone ? undefined : seen[t.id]
      const key = live?.key ?? t.key
      const name = live?.name ?? t.name
      if (
        needle !== '' &&
        !key.toLowerCase().includes(needle) &&
        !name.toLowerCase().includes(needle)
      ) {
        continue
      }
      out.push({
        id: t.id,
        key,
        name,
        private: live?.private ?? false,
        gone,
        events: perTeam.get(t.id)?.events ?? null,
        watched: true,
      })
    }
    for (const c of catalog ?? []) {
      if (watchedIds.has(c.id)) continue
      out.push({
        id: c.id,
        key: c.key,
        name: c.name,
        private: c.private,
        gone: false,
        events: null,
        watched: false,
      })
    }
    return out
  }, [catalog, filter, watched, watchedIds, seen, isMissing, perTeam])

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
        label: 'TEAM',
        floor: 120,
        // Plain text unless a tag rides along: a text cell keeps the table's
        // type, ink, ellipsis and clipped-name hover, and a node cell owns its
        // own, so the name then carries the cell's type itself.
        render: (r) =>
          r.private || r.gone ? (
            <>
              <span className="sp-cellname" style={{ color: nameInk(r) }}>
                {r.name || '—'}
              </span>
              {r.private ? <Tag text="private" tip={TIP_PRIVATE} /> : null}
              {r.gone ? <Tag text="out of reach" tip={TIP_OUT_OF_REACH} alarm /> : null}
            </>
          ) : (
            r.name || '—'
          ),
        measure: (r) =>
          `${r.name || '—'}${r.private ? '  private' : ''}${r.gone ? '  out of reach' : ''}`,
        color: nameInk,
      },
      eventsColumn,
    ],
    [],
  )

  // Which write the adopted response may land on: the PUT answers with the set
  // AS STORED (keys, names and state names re-resolved from Linear), and
  // adopting a stale answer over a newer optimistic set would undo the newer
  // gesture.
  const commitSeq = useRef(0)
  // The newest set the server has confirmed, and the write that confirmed it.
  // A failure rolls back to this rather than to the set the gesture was built
  // on: with two writes out, that set carries the first one's change, which
  // the server may have refused as well.
  const confirmed = useRef<{ seq: number; set: LinearTeamConfig[] }>({ seq: 0, set: [] })

  // The one write door for this page — the table's verbs and the board's
  // gestures alike. Optimistic, latest-wins, and honest on failure: the
  // optimistic set rolls back rather than leaving the page rendering a
  // configuration the server never accepted.
  const persist = useCallback(
    (next: LinearTeamConfig[]) => {
      setTeams(next)
      const seq = ++commitSeq.current
      void saveTeamLinearTeams(teamId, next).then((res) => {
        if (res.ok && seq > confirmed.current.seq) {
          confirmed.current = { seq, set: res.teams }
        }
        if (!mounted.current || commitSeq.current !== seq) return
        if (res.ok) {
          setTeams(res.teams)
          setError('')
        } else {
          setTeams(confirmed.current.set)
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
      // every team this team watches.
      if (teams === null) return
      const picked = new Set(ids.map(String))
      const next =
        actionId === 'watch'
          ? teams.concat(
              (catalog ?? [])
                .filter((c) => picked.has(c.id) && !watchedIds.has(c.id))
                .map((c) => unmappedLinearTeam(c.id, c.key, c.name)),
            )
          : teams.filter((t) => !picked.has(t.id))
      persist(next)
    },
    [teams, catalog, watchedIds, persist],
  )

  // A board gesture, mapped back onto the four stored rules and saved. The
  // board's construction keeps the result valid — a non-empty write-target
  // column always carries a ★ and READY never does.
  const applyBoard = useCallback(
    (next: StatusMap) => {
      if (!shown || teams === null) return
      const vocab = statesByTeam[shown.id] ?? []
      const live = new Set(vocab.map((v) => v.id))
      const prior = [
        ...shown.pickup.members,
        ...shown.in_progress.members,
        ...shown.in_review.members,
        ...shown.done.members,
      ]
      // An item resolves back to a full ref through the live workflow, falling
      // back to the ref it was drawn from.
      const toRef = (it: StatusItem): LinearStateRef => {
        const v = vocab.find((s) => s.id === it.id)
        if (v) return { id: v.id, name: v.name, type: v.type }
        return prior.find((r) => r.id === it.id) ?? { id: it.id, name: it.label, type: '' }
      }
      // The server checks a changed rule against the team's live workflow and
      // refuses a state Linear no longer has, so a gesture on a rule that
      // still names a deleted state — starring a replacement for a deleted ★,
      // say — would fail whole. The state is gone, so no issue is in it, and a
      // rule the gesture changes sheds it; a ★ it held passes to the next
      // member, as it does when its chip is dragged out. A rule the gesture
      // left alone keeps it as stored, since the server carries an unchanged
      // rule without asking.
      const was = mapFor(shown)
      const settle = (r: StatusRule, before: StatusRule): StatusRule => {
        if (live.size === 0 || sameRule(r, before)) return r
        const members = r.members.filter((m) => live.has(m.id))
        if (members.length === r.members.length) return r
        const primary =
          r.primary === null
            ? null
            : members.some((m) => m.id === r.primary)
              ? r.primary
              : (members[0]?.id ?? null)
        return { members, primary }
      }
      const rule = (r: StatusRule): JiraStatusRuleValue<LinearStateRef> => {
        const members = r.members.map(toRef)
        const at = r.primary === null ? -1 : r.members.findIndex((m) => m.id === r.primary)
        return { members, canonical: at >= 0 ? members[at] : null }
      }
      const edited: LinearTeamConfig = {
        ...shown,
        pickup: { members: settle(next.ready, was.ready).members.map(toRef) },
        in_progress: rule(settle(next.inprogress, was.inprogress)),
        in_review: rule(settle(next.review, was.review)),
        done: rule(settle(next.done, was.done)),
      }
      persist(teams.map((t) => (t.id === shown.id ? edited : t)))
    },
    [shown, teams, statesByTeam, persist],
  )

  const boardNoteText = shown
    ? boardNote({
        shownKey: shown.key,
        sole: watched.length <= 1,
        editable,
        armed: linearTeamIsArmed(shown),
        quiet: !!shownStar,
        noun: 'states',
      })
    : ''

  // The one sentence under the board for what Linear changed beneath the
  // shown team's mapping.
  const flag: { text: string; faint?: boolean } | null = !shown
    ? null
    : shownMissing
      ? {
          text: `Linear no longer shows ${shown.key} to this organization’s Linear connection, so nothing is polled from it. The stored mapping stays until it is unwatched.`,
        }
      : shownStar
        ? {
            text:
              `${shownStar.name}, the ★ in ${shownStar.column}, was deleted in Linear. ` +
              (isAdmin
                ? STAR_CONSEQUENCE[shownStar.column].admin
                : STAR_CONSEQUENCE[shownStar.column].member(shown.key)),
          }
        : connected && seen[shown.id]?.private
          ? {
              text: `${shown.key} is private in Linear. Its issues still become tasks every member of this team can read.`,
              faint: true,
            }
          : null

  return (
    <SourceFrame
      source="linear"
      name="Linear"
      teamName={teamName}
      onBack={onBack}
      events={flow?.events ?? null}
      tasks={flow ? flow.tasks : null}
      sincePoll={sinceLabel(flow?.last_poll_at ?? null)}
    >
      <div className="sp-cols" data-split="even">
        {/* What the page is for, and the map that answers it. */}
        <div className="sp-mapcol">
          <p className="sp-prose">{PROSE}</p>
          {note ? <p className="sp-offnote">{note}</p> : null}
          <div className="sp-subhead">
            <span className="sp-subhead-t">What each state means</span>
            {watched.length > 1 ? (
              <div className="sp-projstrip" role="tablist" aria-label="Watched teams">
                {watched.map((t) => {
                  const armed = linearTeamIsArmed(t)
                  const alarm =
                    isMissing(t.id) || !!deletedStar(t, connected ? statesByTeam[t.id] : undefined)
                  return (
                    <button
                      key={t.id}
                      type="button"
                      role="tab"
                      aria-selected={t.id === shownTeamId}
                      className="sp-projkey"
                      data-on={t.id === shownTeamId ? '' : undefined}
                      data-unarmed={armed || alarm ? undefined : ''}
                      data-alarm={alarm ? '' : undefined}
                      title={alarm ? 'needs attention' : armed ? undefined : 'states not mapped'}
                      onClick={() => setShownId(t.id)}
                    >
                      {t.key}
                    </button>
                  )
                })}
              </div>
            ) : null}
          </div>
          <div className="sp-board">
            {board ? (
              <StatusRules
                key={shownTeamId}
                value={board}
                onChange={editable ? applyBoard : undefined}
                statuses={boardStatuses}
                showProjects={false}
                interactive={editable}
                build
                note={boardNoteText}
              />
            ) : teams ? (
              // Nothing watched: the board's shape, faded, under what to do
              // about it — or, for a reader with no verb to follow, under what
              // is true.
              <StatusRules
                key="placeholder"
                value={EMPTY_STATUS_MAP}
                statuses={[]}
                showProjects={false}
                build
                placeholder={
                  isAdmin && connected
                    ? 'Track a Linear team to the right to map workflow statuses.'
                    : 'No Linear team is tracked by this team yet.'
                }
              />
            ) : null}
            {flag ? (
              <p className="sp-flag" data-tone={flag.faint ? 'faint' : undefined}>
                {flag.text}
              </p>
            ) : null}
          </div>
        </div>

        {/* What the teams sent, then the teams: what is watched, and what the
            credential could watch, in that order. */}
        <div className="sp-listcol">
          <div className="sp-chartwrap">
            <SourceChart
              title="WHAT THE TEAMS SENT"
              keys={['EVENTS', 'TASKS']}
              units={['events', 'tasks']}
              series={connected ? activitySeries(activity14, 'linear', 14) : null}
            />
          </div>
          <div className="sp-listhead">
            <span className="sp-subhead-t">Which teams</span>
            <span className="sp-grow" />
            <FilterField value={filter} onChange={setFilter} label="Filter teams" />
          </div>
          <div className="sp-tablewrap">
            {error ? <p className="sp-error">{error}</p> : null}
            <Table
              build={!!teams}
              columns={columns}
              rows={rows}
              pageSize="auto"
              sortKey="key"
              sortDir={1}
              showHeader={false}
              barPosition="absolute"
              emptyLabel={
                teams === null
                  ? 'loading…'
                  : state === 'unconfigured'
                    ? 'Linear is not connected'
                    : 'no teams visible'
              }
              // The catalog is a live proxy that reports no total, so a full
              // page can only say that narrowing would reveal more.
              footer={catalogTruncated ? 'more teams exist · narrow the filter' : undefined}
              selectable={isAdmin}
              actions={
                isAdmin
                  ? [
                      {
                        id: 'watch',
                        label: 'Watch',
                        message: (n) => n + (n === 1 ? ' team is' : ' teams are') + ' now watched',
                      },
                      {
                        id: 'unwatch',
                        label: 'Stop watching',
                        tone: 'bad',
                        message: (n) => n + (n === 1 ? ' team' : ' teams') + ' no longer watched',
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
