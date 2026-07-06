// SlackChannelPicker — the merged searchable checklist body for the /team
// page's Slack tab (TFAC-544). Mirrors RepoPickerModal's inline searchable-
// checkbox idiom (controlled selection, no footer of its own — the host owns
// Save/Cancel) but purely presentational: it renders whatever `channels` the
// host already fetched (TeamSlackChannelsPanel owns the fetch/save via
// teamConfig's fetchTeamSlackChannels/saveTeamSlackChannels), rather than
// fetching its own candidate list the way RepoPickerModal does.
//
// Each row surfaces this team's relationship to the channel via one status
// chip (unclaimed / tracked-by-another-team / watching / primary) plus,
// independently, a "bot not in channel" warning when a tracked row's bot
// isn't a member — that must read as *not working*, so it layers on top of
// the ownership chip rather than replacing it. Rows this team doesn't track
// yet, with recent @mention activity and zero trackers, are the Slack-first
// claim signal — they sort to the top of the unselected portion.

import { useMemo, useState } from 'react'
import { Lock } from 'lucide-react'
import SearchField from './SearchField'
import { timeAgo } from '../lib/relativeTime'
import type { SlackChannelView } from '../types'

interface Props {
  channels: SlackChannelView[]
  /** Currently-checked channel_ids (the unsaved draft selection). */
  selected: Set<string>
  onToggle: (channelId: string) => void
  /** Disables every row — a non-admin viewer, or a save in flight. */
  disabled?: boolean
  /** Gates the per-row "Make primary" affordance (org-admin only). */
  orgIsAdmin: boolean
  onMakePrimary: (channelId: string) => void
  /** channel_id currently reassigning primary, for its row's busy state. */
  makingPrimaryId?: string | null
}

type ChipKind = 'unclaimed' | 'tracked-other' | 'watching' | 'primary'

interface ChipInfo {
  kind: ChipKind
  label: string
}

// unclaimed: zero trackers, the claim affordance. tracked-other: another
// team's binding (name + role only — no activity/config of that team).
// watching: this team tracks, another team is primary. primary: this team
// is primary.
function statusChip(channel: SlackChannelView): ChipInfo {
  if (channel.tracked) {
    return channel.is_primary
      ? { kind: 'primary', label: 'primary' }
      : { kind: 'watching', label: 'watching' }
  }
  if (channel.tracked_by.length === 0) {
    return { kind: 'unclaimed', label: 'unclaimed' }
  }
  const primary = channel.tracked_by.find((t) => t.is_primary) ?? channel.tracked_by[0]
  return {
    kind: 'tracked-other',
    label: primary.is_primary
      ? `tracked by ${primary.team_name} (primary)`
      : `tracked by ${primary.team_name}`,
  }
}

const CHIP_STYLE: Record<ChipKind, { dot: string; text: string }> = {
  unclaimed: { dot: 'bg-amber-500', text: 'text-amber-700' },
  'tracked-other': { dot: 'bg-text-tertiary/40', text: 'text-text-tertiary' },
  watching: { dot: 'bg-sky-500', text: 'text-sky-700' },
  primary: { dot: 'bg-emerald-500', text: 'text-emerald-700' },
}

function Chip({ label, dot, text }: { label: string; dot: string; text: string }) {
  return (
    <span
      className={`inline-flex shrink-0 items-center gap-1 rounded-full bg-black/[0.05] px-2 py-0.5 text-[11px] font-medium ${text}`}
    >
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${dot}`} />
      {label}
    </span>
  )
}

// The Slack-first claim signal: nobody tracks this channel yet, but the bot
// has seen an @mention in it. Also the sort key that floats these rows to
// the top of the unselected portion.
function isUnclaimedWithActivity(c: SlackChannelView): boolean {
  return c.tracked_by.length === 0 && !!c.last_mention_at
}

export default function SlackChannelPicker({
  channels,
  selected,
  onToggle,
  disabled = false,
  orgIsAdmin,
  onMakePrimary,
  makingPrimaryId = null,
}: Props) {
  const [search, setSearch] = useState('')

  // A workspace chip only earns its keep when there's more than one connected
  // workspace to disambiguate — derived from the distinct workspace_ids this
  // response actually carries (the only workspace signal this endpoint gives
  // the frontend; org-settings' workspace list is out of scope here).
  const showWorkspaceChip = useMemo(
    () => new Set(channels.map((c) => c.workspace_id).filter(Boolean)).size > 1,
    [channels],
  )

  const sorted = useMemo(() => {
    return [...channels].sort((a, b) => {
      const aSel = selected.has(a.channel_id)
      const bSel = selected.has(b.channel_id)
      if (aSel !== bSel) return aSel ? -1 : 1
      if (!aSel) {
        const aAct = isUnclaimedWithActivity(a)
        const bAct = isUnclaimedWithActivity(b)
        if (aAct !== bAct) return aAct ? -1 : 1
        if (aAct && bAct) {
          // Most recently mentioned first.
          return (b.last_mention_at ?? '').localeCompare(a.last_mention_at ?? '')
        }
      }
      const an = a.name || a.channel_id
      const bn = b.name || b.channel_id
      return an.localeCompare(bn)
    })
  }, [channels, selected])

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return sorted
    return sorted.filter(
      (c) => (c.name || '').toLowerCase().includes(q) || c.channel_id.toLowerCase().includes(q),
    )
  }, [sorted, search])

  return (
    <div className="space-y-3">
      <SearchField
        value={search}
        onChange={setSearch}
        placeholder="Search channels…"
        ariaLabel="Search Slack channels"
      />
      <div className="max-h-80 overflow-y-auto">
        {filtered.length === 0 ? (
          <p className="py-8 text-center text-[13px] text-text-tertiary">
            {search ? `No channels match "${search}"` : 'No channels found'}
          </p>
        ) : (
          filtered.map((channel) => {
            const checked = selected.has(channel.channel_id)
            const chip = statusChip(channel)
            const chipStyle = CHIP_STYLE[chip.kind]
            const botMissing = channel.tracked && !channel.bot_is_member
            const canMakePrimary =
              orgIsAdmin && channel.tracked && !channel.is_primary && channel.tracked_by.length >= 2
            const activity = isUnclaimedWithActivity(channel)
            const displayName = channel.name ? `#${channel.name}` : channel.channel_id

            return (
              <div
                key={channel.channel_id}
                role="checkbox"
                aria-checked={checked}
                aria-disabled={disabled}
                aria-label={`Track ${displayName}`}
                tabIndex={disabled ? -1 : 0}
                onClick={() => !disabled && onToggle(channel.channel_id)}
                onKeyDown={(e) => {
                  if (disabled) return
                  if (e.key === ' ' || e.key === 'Enter') {
                    e.preventDefault()
                    onToggle(channel.channel_id)
                  }
                }}
                className={`flex items-start gap-3 rounded-xl px-3 py-2.5 transition-colors ${
                  disabled
                    ? 'cursor-not-allowed opacity-60'
                    : 'cursor-pointer hover:bg-black/[0.02]'
                } ${checked ? 'bg-accent/[0.04]' : ''}`}
              >
                <span
                  aria-hidden
                  className={`mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded border transition-colors ${
                    checked ? 'border-accent bg-accent text-white' : 'border-border-subtle'
                  }`}
                >
                  {checked && (
                    <svg
                      width="10"
                      height="10"
                      viewBox="0 0 10 10"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    >
                      <polyline points="2 5 4 7 8 3" />
                    </svg>
                  )}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span
                      className={`truncate text-[12.5px] font-medium text-text-primary ${
                        channel.name ? '' : 'font-mono'
                      }`}
                    >
                      {displayName}
                    </span>
                    {channel.is_private && (
                      <Lock
                        size={11}
                        aria-label="private channel"
                        className="shrink-0 text-text-tertiary"
                      />
                    )}
                    {showWorkspaceChip && channel.workspace_id && (
                      <span className="shrink-0 rounded bg-black/[0.05] px-1 py-0.5 text-[10px] text-text-tertiary">
                        {channel.workspace_id}
                      </span>
                    )}
                    <Chip label={chip.label} dot={chipStyle.dot} text={chipStyle.text} />
                    {botMissing && (
                      <Chip
                        label="bot not in channel — /invite @bot"
                        dot="bg-rose-500"
                        text="text-rose-700"
                      />
                    )}
                    {canMakePrimary && (
                      <button
                        type="button"
                        onClick={(e) => {
                          e.stopPropagation()
                          onMakePrimary(channel.channel_id)
                        }}
                        disabled={makingPrimaryId === channel.channel_id}
                        className="shrink-0 text-[11px] font-medium text-accent underline-offset-2 hover:underline disabled:opacity-40"
                      >
                        {makingPrimaryId === channel.channel_id
                          ? 'Making primary…'
                          : 'Make primary'}
                      </button>
                    )}
                  </div>
                  {activity && (
                    <p className="mt-0.5 text-[11px] text-text-tertiary">
                      mentions seen · {timeAgo(channel.last_mention_at!)}
                    </p>
                  )}
                </div>
              </div>
            )
          })
        )}
      </div>
    </div>
  )
}
