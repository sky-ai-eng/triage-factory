import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow, TableSort } from '../../ui/table/Table'
import { ago, minutesSince } from '../../ui/table/cells'
import { Odometer } from '../../ui/odometer/Odometer'
import SourceChart from '../../ui/sourcechart/SourceChart'
import { SourceFrame, FigureRow, FilterField } from './SourceFrame'
import { figureAt, labelAt } from './schedule'
import type { SourceBodyProps } from './SourceFrame'
import { apiFetch } from '../../lib/apiClient'
import type { Facet } from '../../lib/apiClient'
import { usePagedList } from '../../hooks/usePagedList'
import {
  useTeamActivity,
  activitySource,
  activitySeries,
  sinceParts,
  sinceLabel,
} from '../../hooks/useTeamActivity'
import {
  fetchTeamSlackChannelFacets,
  fetchTeamTrackedSlackChannels,
  slackChannelsListPath,
} from '../settings/teamConfig'
import type { SlackChannelFacets } from '../settings/teamConfig'
import type { SlackChannelView } from '../../types'

// Slack, as this team's event source.
//
// A mention of the factory in a watched channel becomes a task for whichever
// team is PRIMARY there. Any number of teams can watch a channel; only one can
// be primary, which is what the star column is for and why "with no primary
// team" is drawn in alarm ink — a watched channel nobody owns is a channel
// whose mentions go nowhere.
//
// Sightings are recorded org-wide the moment the app sees one, tracked or not,
// so an untracked channel legitimately shows a last event. A channel the app
// is not a member of delivers nothing and shows a dash in both columns rather
// than a zero.
//
// The flow figures come from the team activity node, and Slack is the source
// it cannot always count: a team's events are defined by its tracked set,
// which Slack does not have, so under multi mode the mention count — and the
// chart, whose outer series it is — reads `—` rather than a zero. Local mode
// is N=1, where the org-wide count is the team's and both render. Became
// tasks is task-scoped and always real. Nothing polls Slack (push), so
// `since last poll` is a dash by construction, not by omission.
//
// The table reads the channel list a page at a time, searched and ordered by
// the server, and the watched / primary figures come from the list's facets
// node: the page holds one window of the channels, so a count or a filter over
// it would answer about that window.

const PROSE =
  'A mention of the factory in a watched channel becomes a task for whichever team is primary there. ' +
  'Any number of teams can watch a channel, but only one can be primary. ' +
  'Changes do not apply to runs already in-flight.'

/**
 * Whose a channel's mentions are: this team's, another team's, nobody's while
 * some team watches the channel, or `unwatched` when no team does.
 */
type OwnerKind = 'ours' | 'theirs' | 'none' | 'unwatched'

/**
 * Who this channel's mentions belong to: the kind, which tone and sort read,
 * and the words the table prints. The kind is decided here and carried beside
 * the text, because another team's name can be any text, "no one" included.
 */
function ownerOf(c: SlackChannelView): { kind: OwnerKind; text: string } {
  if (c.is_primary) return { kind: 'ours', text: 'this team' }
  const primary = (c.tracked_by ?? []).find((t) => t.is_primary)
  if (primary) return { kind: 'theirs', text: primary.team_name }
  return (c.tracked_by ?? []).length > 0
    ? { kind: 'none', text: 'no one' }
    : { kind: 'unwatched', text: '' }
}

/** How many channels one read of the list asks for. */
const PAGE = 50

/** How long typing settles before the search is sent. */
const SEARCH_DEBOUNCE_MS = 250

/** The order the page opens on: the star's own ranking — ours, nobody's,
 *  theirs, unwatched — which is the list's `primary` key. */
const OPENING_SORT: TableSort = { key: 'primary', dir: 'asc' }

/** How many channels a facet cut counts under `value`; a value no channel
 *  carries is absent from the cut. */
function facetCount(cut: Facet[], value: string): number {
  return cut.find((f) => f.value === value)?.count ?? 0
}

const channelKey = (c: SlackChannelView) => c.channel_id

/**
 * The primary mark. Drawn rather than typed: a text star inherits its font's
 * metrics — and on a stack whose first family carries no star glyph, a
 * fallback's — so it never lands where the row's other marks do.
 */
function star(row: TableRow) {
  if (!row.watched) return null
  const kind = row.ownerKind as OwnerKind
  const tone = kind === 'unwatched' ? 'none' : kind
  return (
    <span className="sp-star" data-tone={tone} aria-hidden="true">
      <svg width="13" height="13" viewBox="0 0 24 24">
        <path d="M12 3.4 14.13 9.96 21.04 9.96 15.45 14.02 17.58 20.59 12 16.53 6.42 20.59 8.55 14.02 2.96 9.96 9.87 9.96Z" />
      </svg>
    </span>
  )
}

export default function SlackSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  const [saveError, setSaveError] = useState('')
  const [filter, setFilter] = useState('')
  const [search, setSearch] = useState('')
  const [sort, setSort] = useState<TableSort>(OPENING_SORT)
  const [loaded, setLoaded] = useState(false)
  const [facets, setFacets] = useState<SlackChannelFacets | null>(null)

  // Two windows of the same node: figures read seven days, the chart a
  // fortnight. See the header comment for which of Slack's figures can
  // answer at all.
  const activity7 = useTeamActivity(teamId, 7)
  const activity14 = useTeamActivity(teamId, 14)
  const flow = activitySource(activity7, 'slack')
  const poll = sinceParts(flow?.last_poll_at ?? null)

  // 404 is the honest answer here as well as an error: Slack is an EE surface
  // and local mode has none. Either way there is nothing to list, and a failed
  // read must not read as "watches nothing".
  const paged = usePagedList<SlackChannelView>(
    slackChannelsListPath(teamId),
    'Could not read this team’s channels.',
    { keyOf: channelKey },
  )
  const { load } = paged

  useEffect(() => {
    const t = setTimeout(() => setSearch(filter.trim()), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(t)
  }, [filter])

  const query = useMemo(
    () => ({
      ...(search ? { search } : {}),
      sort_key: sort.key,
      sort_dir: sort.dir,
      page_size: PAGE,
    }),
    [search, sort],
  )

  useEffect(() => {
    if (!teamId) return
    void load(query).then(() => setLoaded(true))
  }, [teamId, load, query])

  const loadFacets = useCallback(() => {
    if (!teamId) return
    void fetchTeamSlackChannelFacets(teamId).then(setFacets)
  }, [teamId])

  useEffect(loadFacets, [loadFacets])

  // Every verb's write, in the order the verbs fired. Each one reads the
  // tracked set when it runs rather than when the page loaded, so a verb
  // never writes back a set an earlier one has already changed.
  const writes = useRef<Promise<void>>(Promise.resolve())

  const rows: TableRow[] = useMemo(
    () =>
      paged.items.map((c) => {
        const min = minutesSince(c.last_mention_at)
        const owner = ownerOf(c)
        return {
          id: c.channel_id,
          // A bare id is what the registry has when nothing has resolved a
          // name yet. Inventing one would be worse than showing the id.
          name: c.name ? '#' + c.name : c.channel_id,
          watched: c.tracked,
          owner: owner.text,
          ownerKind: owner.kind,
          mentions: null,
          last: min === null ? null : ago(min),
          lastMin: min,
        }
      }),
    [paged.items],
  )

  const columns: TableColumn[] = useMemo(
    () => [
      // No header: the glyph IS the label, and a word over it would claim more
      // width than the mark needs.
      {
        key: 'star',
        label: '',
        width: '16px',
        render: star,
        sortKey: 'primary',
      },
      {
        key: 'name',
        label: 'CHANNEL',
        sortKey: 'name',
        color: (r) => (r.watched ? 'var(--color-ink-1)' : 'var(--color-ink-4)'),
      },
      {
        key: 'owner',
        label: 'PRIMARY TEAM',
        align: 'end',
        render: (r) => r.owner || '—',
        color: (r) =>
          r.ownerKind === 'none'
            ? 'var(--color-alarm)'
            : r.ownerKind === 'ours'
              ? 'var(--color-ink-2)'
              : 'var(--color-ink-3)',
      },
      {
        key: 'mentions',
        label: 'MENTIONS · 7D',
        align: 'end',
        drop: 2,
        render: (r) => r.mentions ?? '—',
        // A dash means not applicable; a zero means zero. Nothing counts these
        // in a window today, so every row is the first — but the rule is written
        // out so the column is right the moment one does.
        color: (r) =>
          r.mentions
            ? 'var(--color-ink-2)'
            : 'color-mix(in srgb, var(--color-ink-4) 29%, var(--color-line-2))',
      },
      {
        key: 'last',
        label: 'LAST EVENT',
        align: 'end',
        drop: 1,
        render: (r) => r.last ?? '—',
        sortKey: 'last_mention_at',
        sortFirst: 'desc',
        color: (r) =>
          r.last
            ? 'var(--color-ink-4)'
            : 'color-mix(in srgb, var(--color-ink-4) 29%, var(--color-line-2))',
      },
    ],
    [],
  )

  const commit = useCallback(
    (actionId: string, ids: Array<string | number>) => {
      const picked = [...new Set(ids.map(String))]
      const put = (channelIds: string[]) =>
        apiFetch(`/api/slack/teams/${encodeURIComponent(teamId)}/channels`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ channel_ids: channelIds }),
        })
      const run = async () => {
        // The save replaces the whole tracked set, so it is computed from all
        // of it; a failed read must not be written back as "tracks nothing".
        const tracked = await fetchTeamTrackedSlackChannels(teamId)
        if (tracked === null) throw new Error('tracked channels unreadable')
        const current = tracked.map((c) => c.channel_id)
        if (actionId === 'primary') {
          // The backend promotes an existing tracker, so a channel has to be
          // tracked before it can be claimed. One set-replace, then one
          // promotion per channel.
          const missing = picked.filter((id) => !current.includes(id))
          if (missing.length) await put(current.concat(missing))
          for (const id of picked) {
            await apiFetch(`/api/slack/channels/${encodeURIComponent(id)}/primary`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ team_id: teamId }),
            })
          }
        } else {
          const on = new Set(picked)
          await put(
            actionId === 'watch'
              ? current.concat(picked.filter((id) => !current.includes(id)))
              : current.filter((id) => !on.has(id)),
          )
        }
      }
      writes.current = writes.current
        .then(run)
        .catch(() => setSaveError('Could not save the watched channels.'))
        // Re-read rather than patch: the PUT can auto-join, and who is primary
        // where is the server's answer, not a guess this page can make. The
        // reread keeps the pages "load more" reached.
        .then(() => {
          void load(query, { keepDepth: true })
          loadFacets()
        })
    },
    [teamId, load, query, loadFacets],
  )

  const visible = facets ? facets.tracked.reduce((n, f) => n + f.count, 0) : null
  const watched = facets ? facetCount(facets.tracked, 'true') : null
  const primaryHere = facets ? facetCount(facets.primary, 'this_team') : null
  const orphaned = facets ? facetCount(facets.primary, 'no_primary') : null
  const error = saveError || paged.error

  return (
    <SourceFrame
      source="slack"
      name="Slack"
      teamName={teamName}
      onBack={onBack}
      events={flow?.events ?? null}
      tasks={flow ? flow.tasks : null}
      sincePoll={sinceLabel(flow?.last_poll_at ?? null)}
    >
      <div className="sp-cols">
        <div className="sp-left">
          <p className="sp-prose" data-source="slack">
            {PROSE}
          </p>

          <div className="sp-figrows" data-tight key={facets ? 'on' : 'off'}>
            <FigureRow
              label={visible !== null ? `watched of ${visible} visible` : 'watched'}
              at={labelAt(0)}
            >
              <Odometer value={watched} at={figureAt(0)} label={`${watched ?? 0}`} />
            </FigureRow>
            <FigureRow label="primary here" at={labelAt(1)} tone="warm">
              <Odometer value={primaryHere} at={figureAt(1)} label={`${primaryHere ?? 0}`} />
            </FigureRow>
            <FigureRow label="mentions in 7 days" at={labelAt(2)}>
              <Odometer
                value={flow ? flow.events : null}
                at={figureAt(2)}
                label={`${flow?.events ?? 0}`}
              />
            </FigureRow>
            <FigureRow label="with no primary team" at={labelAt(3)} tone="alarm">
              <Odometer value={orphaned} at={figureAt(3)} label={`${orphaned ?? 0}`} />
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

          <span className="sp-note">
            ★ marks the channels this team is primary in. Automatic delegation only runs on mentions
            in those channels.
          </span>

          {/* The outer series is the mention count, so where that count is
              undefined the chart stays unanswered too — a zeroed outer line
              would claim silence the node never measured. */}
          <SourceChart
            title="WHAT THE CHANNELS SENT"
            keys={['MENTIONS', 'TASKS']}
            units={['mentions', 'tasks']}
            series={flow && flow.events !== null ? activitySeries(activity14, 'slack', 14) : null}
          />
        </div>

        <div className="sp-right">
          <div className="sp-tablewrap">
            {error ? <p className="sp-error">{error}</p> : null}
            <Table
              label="CHANNELS"
              build={loaded}
              columns={columns}
              rows={rows}
              pageSize="auto"
              // The design opens on MENTIONS · 7D. Nothing counts them yet, so
              // the list has no key to order them by; the star's own ranking —
              // ours, nobody's, theirs, untracked — is the page's argument and
              // has data behind it. Open on the count when it ships.
              sort={sort}
              onSortChange={setSort}
              barPosition="absolute"
              emptyLabel={loaded ? 'no channels' : 'loading…'}
              selectable={isAdmin}
              headerRight={
                <FilterField value={filter} onChange={setFilter} label="Filter channels" />
              }
              actions={
                isAdmin
                  ? [
                      {
                        id: 'watch',
                        label: 'Watch',
                        message: (n) =>
                          n + (n === 1 ? ' channel is' : ' channels are') + ' now watched',
                      },
                      {
                        id: 'primary',
                        label: 'Claim as primary',
                        // The backend promotes an existing tracker; it never
                        // takes a channel off the team that already holds it,
                        // so the verb is offered only when every selected
                        // channel is unclaimed or already ours.
                        enabledFor: (sel) =>
                          sel.every(
                            (r) => !r.owner || r.owner === 'no one' || r.owner === 'this team',
                          ),
                        message: (n) =>
                          'this team is primary in ' + n + (n === 1 ? ' channel' : ' channels'),
                      },
                      {
                        id: 'unwatch',
                        label: 'Stop watching',
                        tone: 'bad',
                        message: (n) =>
                          n + (n === 1 ? ' channel' : ' channels') + ' no longer watched',
                      },
                    ]
                  : []
              }
              mutate={
                isAdmin
                  ? (row, id) =>
                      id === 'primary'
                        ? { ...row, watched: true, owner: 'this team' }
                        : {
                            ...row,
                            watched: id === 'watch',
                            owner: id === 'watch' ? row.owner : '',
                          }
                  : null
              }
              bar={isAdmin ? {} : null}
              onCommit={isAdmin ? (id, ids) => commit(id, ids) : null}
            />
            {paged.hasMore ? (
              <div className="sp-loadmore">
                {paged.items.length} of {paged.total ?? paged.items.length} ·{' '}
                <button
                  type="button"
                  disabled={paged.loading}
                  onClick={() => void paged.loadMore()}
                >
                  load more
                </button>
              </div>
            ) : null}
          </div>
        </div>
      </div>
    </SourceFrame>
  )
}
