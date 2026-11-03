import { useEffect, useState } from 'react'
import { Lock, X } from 'lucide-react'
import { useTeams, pickerDefault } from '../hooks/useTeams'
import { fetchSlackChannelsByID, fetchTeamTrackedSlackChannels } from '../pages/settings/teamConfig'
import type { SlackChannelView } from '../types'

/** A channel the team tracks: a tracked row always carries its registry id,
 *  which is what channel_in stores. */
type TrackedSlackChannel = SlackChannelView & { id: string }

/** The shape of a channel registry id. A stored entry of any other shape
 *  names no row, so it is never sent to the by-id read, which would refuse
 *  the whole request over it. */
const REGISTRY_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/** SlackChannelListField is the editor for slack:message's `channel_in`: a
 *  multi-select over the channels the handler's team tracks
 *  (POST /api/slack/teams/{team_id}/channels/list with `tracked: true`),
 *  submitting each channel's registry `id`. The server accepts only those,
 *  since the router delivers a team's handlers only messages from channels it
 *  tracks.
 *
 *  teamId is the handler's team. '' (create with the picker hidden) falls
 *  back to the acting team, which is the sole team in that case.
 *
 *  A stored entry the team no longer tracks stays in the value and renders
 *  as a removable "not tracked" chip, so opening and saving a handler never
 *  drops it without the user seeing it. Its name is read by registry id.
 *
 *  Wire shape: `string[] | undefined` — undefined when nothing is selected,
 *  which matches every channel the team tracks. */
export default function SlackChannelListField({
  teamId,
  value,
  onChange,
}: {
  teamId: string
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
}) {
  const { teams, lastActingTeamId, loaded } = useTeams()
  const team = teamId || (loaded ? pickerDefault(teams, lastActingTeamId) : '')
  // Keyed by team so a team switch reads as loading rather than showing the
  // previous team's list.
  const [result, setResult] = useState<{
    team: string
    channels: TrackedSlackChannel[] | null
  } | null>(null)
  // Names of stored entries the team does not track, by registry id, for the
  // team they were read under. An id the read could not name maps to itself.
  const [named, setNamed] = useState<{ team: string; byId: Record<string, string> }>({
    team: '',
    byId: {},
  })

  useEffect(() => {
    if (!team) return
    let cancelled = false
    fetchTeamTrackedSlackChannels(team).then((rows) => {
      if (cancelled) return
      // The guard drops nothing the server sends; it is what lets the
      // checkboxes below submit `id` as the string channel_in takes.
      const channels = rows && rows.flatMap((c) => (c.id ? [{ ...c, id: c.id }] : []))
      setResult({ team, channels })
    })
    return () => {
      cancelled = true
    }
  }, [team])

  const selected = value ?? []
  const ready = !!team && result?.team === team
  const tracked = ready ? (result.channels ?? []) : []
  const trackedIds = new Set(tracked.map((c) => c.id))
  const untracked = selected.filter((id) => !trackedIds.has(id))
  const known = named.team === team ? named.byId : {}
  // The entries still to name, as one string so the read below runs once per
  // set rather than once per render.
  const toName =
    ready && result.channels !== null
      ? untracked.filter((id) => REGISTRY_ID.test(id) && !(id in known)).join(',')
      : ''

  useEffect(() => {
    if (!toName) return
    let cancelled = false
    const ids = toName.split(',')
    fetchSlackChannelsByID(team, ids).then((rows) => {
      if (cancelled) return
      setNamed((prev) => {
        const byId = prev.team === team ? { ...prev.byId } : {}
        for (const id of ids) byId[id] = id
        for (const row of rows ?? []) if (row.id) byId[row.id] = displayName(row)
        return { team, byId }
      })
    })
    return () => {
      cancelled = true
    }
  }, [team, toName])

  const set = (next: string[]) => onChange(next.length > 0 ? next : undefined)
  const toggle = (id: string) =>
    set(selected.includes(id) ? selected.filter((c) => c !== id) : [...selected, id])

  if (!ready) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }
  const channels = result.channels
  const nameOf = (id: string) => known[id.toLowerCase()] ?? known[id] ?? id

  return (
    <div className="space-y-2">
      {channels === null && (
        <div
          role="alert"
          className="rounded-lg border border-alarm/40 bg-alarm/40 px-3 py-2 text-ui text-alarm"
        >
          Could not load this team&rsquo;s Slack channels. Refresh the page to retry.
        </div>
      )}
      {channels !== null && tracked.length === 0 && (
        <p className="text-ui text-ink-3 italic">This team tracks no Slack channels.</p>
      )}
      {tracked.length > 0 && (
        <div className="max-h-48 overflow-y-auto rounded-lg border border-line-1 bg-raised py-1">
          {tracked.map((channel) => (
            <label
              key={channel.id}
              className="flex items-center gap-2 px-3 py-1.5 text-body text-ink-1 cursor-pointer hover:bg-tint-2"
            >
              <input
                type="checkbox"
                checked={selected.includes(channel.id)}
                onChange={() => toggle(channel.id)}
                className="accent-warm"
              />
              <span className={`text-ui ${channel.name ? '' : 'font-mono'}`}>
                {displayName(channel)}
              </span>
              {channel.is_private && (
                <Lock size={11} className="text-ink-3" aria-label="private channel" />
              )}
            </label>
          ))}
        </div>
      )}
      {untracked.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {untracked.map((id) => (
            <span
              key={id}
              className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border bg-tint-2 text-ink-2 border-line-2"
              title="The team does not track this channel, so this filter never matches it."
            >
              {nameOf(id)} · not tracked
              <button
                type="button"
                onClick={() => set(selected.filter((c) => c !== id))}
                aria-label={`Remove ${nameOf(id)}`}
                className="hover:text-ink-1"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          ))}
        </div>
      )}
      {selected.length === 0 && tracked.length > 0 && (
        <p className="text-reported text-ink-3">
          None selected: matches every channel this team tracks.
        </p>
      )}
    </div>
  )
}

// displayName renders a channel the way the Slack tab does: #name, or the
// raw Slack id when TF has not resolved a name.
function displayName(channel: SlackChannelView): string {
  return channel.name ? `#${channel.name}` : channel.channel_id
}
