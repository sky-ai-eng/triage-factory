import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import { ago } from '../../ui/table/cells'
import { Odometer } from '../../ui/odometer/Odometer'
import SourceChart from '../../ui/sourcechart/SourceChart'
import { SourceFrame, FigureRow, FilterField } from './SourceFrame'
import { figureAt, labelAt } from './schedule'
import type { SourceBodyProps } from './SourceFrame'
import { apiFetch, apiJSON } from '../../lib/apiClient'
import {
  useTeamActivity,
  activitySource,
  activitySeries,
  sinceParts,
  sinceLabel,
} from '../../hooks/useTeamActivity'
import type { SlackChannelsResponse, SlackChannelView } from '../../types'

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

const PROSE =
  'A mention of the factory in a watched channel becomes a task for whichever team is primary there. ' +
  'Any number of teams can watch a channel, but only one can be primary. ' +
  'Changes do not apply to runs already in-flight.'

/** Who this channel's mentions belong to, in the words the table prints. */
function ownerOf(c: SlackChannelView): string {
  if (c.is_primary) return 'this team'
  const primary = (c.tracked_by ?? []).find((t) => t.is_primary)
  if (primary) return primary.team_name
  return (c.tracked_by ?? []).length > 0 ? 'no one' : ''
}

/**
 * The primary mark. Drawn rather than typed: a text star inherits its font's
 * metrics — and on a stack whose first family carries no star glyph, a
 * fallback's — so it never lands where the row's other marks do.
 */
function star(row: TableRow) {
  if (!row.watched) return null
  const owner = row.owner as string
  const tone = owner === 'no one' ? 'none' : owner === 'this team' ? 'ours' : 'theirs'
  return (
    <span className="sp-star" data-tone={tone} aria-hidden="true">
      <svg width="13" height="13" viewBox="0 0 24 24">
        <path d="M12 3.4 14.13 9.96 21.04 9.96 15.45 14.02 17.58 20.59 12 16.53 6.42 20.59 8.55 14.02 2.96 9.96 9.87 9.96Z" />
      </svg>
    </span>
  )
}

/** Minutes since an ISO timestamp, or null when there has never been one. */
function minutesSince(at: string | undefined): number | null {
  if (!at) return null
  const t = Date.parse(at)
  if (Number.isNaN(t)) return null
  return Math.max(0, (Date.now() - t) / 60000)
}

export default function SlackSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  const [channels, setChannels] = useState<SlackChannelView[] | null>(null)
  const [error, setError] = useState('')
  const [filter, setFilter] = useState('')

  // Two windows of the same node: figures read seven days, the chart a
  // fortnight. See the header comment for which of Slack's figures can
  // answer at all.
  const activity7 = useTeamActivity(teamId, 7)
  const activity14 = useTeamActivity(teamId, 14)
  const flow = activitySource(activity7, 'slack')
  const poll = sinceParts(flow?.last_poll_at ?? null)

  // What the next set-replace is computed from, so two verbs inside one undo
  // window do not each write from the same stale set.
  const trackedRef = useRef<string[]>([])

  const load = useCallback(() => {
    if (!teamId) return
    void apiJSON<SlackChannelsResponse>(`/api/slack/teams/${encodeURIComponent(teamId)}/channels`)
      .then((d) => {
        const list = d.channels ?? []
        trackedRef.current = list.filter((c) => c.tracked).map((c) => c.channel_id)
        setChannels(list)
      })
      .catch(() => {
        // 404 is the honest answer here as well as an error: Slack is an EE
        // surface and local mode has none. Either way there is nothing to
        // list, and a failed read must not read as "watches nothing".
        setError('Could not read this team’s channels.')
      })
  }, [teamId])

  useEffect(load, [load])

  const rows: TableRow[] = useMemo(
    () =>
      (channels ?? []).map((c) => {
        const min = minutesSince(c.last_mention_at)
        return {
          id: c.channel_id,
          // A bare id is what the registry has when nothing has resolved a
          // name yet. Inventing one would be worse than showing the id.
          name: c.name ? '#' + c.name : c.channel_id,
          watched: c.tracked,
          owner: ownerOf(c),
          mentions: null,
          last: min === null ? null : ago(min),
          lastMin: min,
        }
      }),
    [channels],
  )

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return q ? rows.filter((r) => String(r.name).toLowerCase().includes(q)) : rows
  }, [rows, filter])

  const columns: TableColumn[] = useMemo(
    () => [
      // No header: the glyph IS the label, and a word over it would claim more
      // width than the mark needs.
      {
        key: 'star',
        label: '',
        width: '16px',
        render: star,
        sortValue: (r) =>
          r.owner === 'this team' ? 0 : r.owner === 'no one' ? 1 : r.owner ? 2 : 3,
      },
      {
        key: 'name',
        label: 'CHANNEL',
        color: (r) => (r.watched ? 'var(--color-ink-1)' : 'var(--color-ink-4)'),
      },
      {
        key: 'owner',
        label: 'PRIMARY TEAM',
        align: 'end',
        render: (r) => r.owner || '—',
        color: (r) =>
          r.owner === 'no one'
            ? 'var(--color-alarm)'
            : r.owner === 'this team'
              ? 'var(--color-ink-2)'
              : 'var(--color-ink-3)',
      },
      {
        key: 'mentions',
        label: 'MENTIONS · 7D',
        align: 'end',
        drop: 2,
        render: (r) => r.mentions ?? '—',
        sortValue: (r) => -(r.mentions ?? 0),
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
        sortValue: (r) => (r.lastMin === null ? Number.MAX_SAFE_INTEGER : r.lastMin),
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
      const run = async () => {
        if (actionId === 'primary') {
          // The backend promotes an existing tracker, so a channel has to be
          // tracked before it can be claimed. One set-replace, then one
          // promotion per channel.
          const missing = picked.filter((id) => !trackedRef.current.includes(id))
          if (missing.length) {
            trackedRef.current = trackedRef.current.concat(missing)
            await apiFetch(`/api/slack/teams/${encodeURIComponent(teamId)}/channels`, {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ channel_ids: trackedRef.current }),
            })
          }
          for (const id of picked) {
            await apiFetch(`/api/slack/channels/${encodeURIComponent(id)}/primary`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ team_id: teamId }),
            })
          }
        } else {
          const on = new Set(picked)
          trackedRef.current =
            actionId === 'watch'
              ? trackedRef.current.concat(picked.filter((id) => !trackedRef.current.includes(id)))
              : trackedRef.current.filter((id) => !on.has(id))
          await apiFetch(`/api/slack/teams/${encodeURIComponent(teamId)}/channels`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ channel_ids: trackedRef.current }),
          })
        }
        // Re-read rather than patch: the PUT can auto-join, and who is primary
        // where is the server's answer, not a guess this page can make.
        load()
      }
      void run().catch(() => setError('Could not save the watched channels.'))
    },
    [teamId, load],
  )

  const watched = channels ? channels.filter((c) => c.tracked).length : null
  const primaryHere = channels ? channels.filter((c) => c.is_primary).length : null
  const orphaned = channels
    ? channels.filter(
        (c) => (c.tracked_by ?? []).length > 0 && !(c.tracked_by ?? []).some((t) => t.is_primary),
      ).length
    : null

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

          <div className="sp-figrows" data-tight key={channels ? 'on' : 'off'}>
            <FigureRow
              label={channels ? `watched of ${channels.length} visible` : 'watched'}
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
              build={!!channels}
              columns={columns}
              rows={shown}
              pageSize="auto"
              // The design opens on MENTIONS · 7D. Nothing counts them yet, so
              // that sort is inert and the rows would land in whatever order
              // the API returned; the star's own ranking — ours, nobody's,
              // theirs, untracked — is the page's argument and has data behind
              // it. Move this back to `n` when the count ships.
              sortKey="star"
              sortDir={1}
              barPosition="absolute"
              emptyLabel={channels ? 'no channels' : 'loading…'}
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
          </div>
        </div>
      </div>
    </SourceFrame>
  )
}
