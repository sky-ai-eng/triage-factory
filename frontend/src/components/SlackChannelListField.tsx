import { useEffect, useState } from 'react'
import { Lock, X } from 'lucide-react'
import { useTeams, pickerDefault } from '../hooks/useTeams'
import { fetchTeamSlackChannels } from '../pages/settings/teamConfig'
import type { SlackChannelView } from '../types'

/** SlackChannelListField is the editor for slack:message's `channel_in`: a
 *  multi-select over the channels the handler's team tracks
 *  (GET /api/slack/teams/{team_id}/channels), submitting each channel's
 *  registry `id`. The server accepts only those, since the router delivers a
 *  team's handlers only messages from channels it tracks.
 *
 *  teamId is the handler's team. '' (create with the picker hidden) falls
 *  back to the acting team, which is the sole team in that case.
 *
 *  A stored entry the team no longer tracks stays in the value and renders
 *  as a removable "not tracked" chip, so opening and saving a handler never
 *  drops it without the user seeing it.
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
    channels: SlackChannelView[] | null
  } | null>(null)

  useEffect(() => {
    if (!team) return
    let cancelled = false
    fetchTeamSlackChannels(team).then((data) => {
      if (!cancelled) setResult({ team, channels: data?.channels ?? null })
    })
    return () => {
      cancelled = true
    }
  }, [team])

  const selected = value ?? []
  const set = (next: string[]) => onChange(next.length > 0 ? next : undefined)
  const toggle = (id: string) =>
    set(selected.includes(id) ? selected.filter((c) => c !== id) : [...selected, id])

  if (!team || result?.team !== team) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }
  const channels = result.channels
  const tracked = (channels ?? [])
    .filter((c): c is SlackChannelView & { id: string } => c.tracked && !!c.id)
    .sort((a, b) => displayName(a).localeCompare(displayName(b)))
  const untracked = selected.filter((id) => !tracked.some((c) => c.id === id))
  // The response also lists registry rows the team does not track, which
  // names a stored entry the team has since untracked.
  const nameOf = (id: string) => {
    const row = channels?.find((c) => c.id === id)
    return row ? displayName(row) : id
  }

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
