// TeamSlackChannelsPanel — the /team page's Slack tab body (TFAC-544): the
// last mile of both Slack channel-tracking flows — Slack-first (someone
// mentioned the bot; the channel shows up here as unclaimed-with-activity,
// an admin claims it) and TF-first (an admin picks channels before anyone
// has mentioned anything). A full tab body (TeamPage gates the tab itself
// on the `slack` entitlement — see TeamPage.tsx), not a collapsible
// Settings section: unlike the team-scope config groups that ride
// TeamSettings' per-section Save/Cancel, this is standalone enough (its own
// fetch, its own full-set-replace PUT, a primary-reassignment side action)
// to earn its own tab rather than a subheading, mirroring how Members and
// Prompts are already dedicated tabs.
//
// Any team member can view this tab (the GET only requires membership);
// SlackChannelPicker disables editing for a non-admin viewer, and "Make
// primary" is further gated on orgIsAdmin — both mirror the backend's own
// authz rather than duplicating a client-side role gate here.

import { useEffect, useMemo, useState } from 'react'
import { toast } from './Toast/toastStore'
import SlackChannelPicker from './SlackChannelPicker'
import {
  fetchTeamSlackChannels,
  reassignSlackChannelPrimary,
  saveTeamSlackChannels,
} from '../pages/settings/teamConfig'
import type { SlackChannelsWarning, SlackChannelView } from '../types'

const sameIdSet = (a: string[], b: string[]): boolean => {
  if (a.length !== b.length) return false
  const sb = new Set(b)
  return a.every((x) => sb.has(x))
}

// Process-level Slack warning codes (GET or PUT) → friendly copy. A
// per-channel PUT warning (invite_required / join_failed) is formatted by
// warningMessage below instead, since it needs the channel's name.
const WARNING_CODES: Record<string, string> = {
  slack_unreachable:
    'The live Slack channel list is unavailable right now — showing tracked and previously seen channels only.',
  slack_channels_truncated:
    'Slack has more channels than we could list — some may be missing below.',
}

const channelLabel = (channels: SlackChannelView[], channelId?: string): string => {
  if (!channelId) return 'This channel'
  const match = channels.find((ch) => ch.channel_id === channelId)
  return match?.name ? `#${match.name}` : channelId
}

// warningMessage renders one entry of a channels response's `warnings` (see
// ee/slack's channelsWarning doc: a process-level `code` OR a per-channel
// `channel_id` + `reason`, never both).
function warningMessage(w: SlackChannelsWarning, channels: SlackChannelView[]): string {
  if (w.code) return WARNING_CODES[w.code] ?? w.code
  const label = channelLabel(channels, w.channel_id)
  if (w.reason === 'invite_required') {
    return `${label} is private — invite the bot manually (/invite @bot) to finish tracking it.`
  }
  if (w.reason === 'join_failed') {
    return `Couldn't add the bot to ${label} automatically — check its permissions and try again.`
  }
  return `${label}: ${w.reason ?? 'unknown issue'}`
}

export default function TeamSlackChannelsPanel({
  teamId,
  orgIsAdmin,
}: {
  teamId: string
  orgIsAdmin: boolean
}) {
  // null = not loaded yet (or the load failed) — distinct from [] ("loaded,
  // genuinely zero candidates and zero tracked/sighting rows", the
  // "connect a workspace" empty state below).
  const [channels, setChannels] = useState<SlackChannelView[] | null>(null)
  const [warnings, setWarnings] = useState<SlackChannelsWarning[]>([])
  const [role, setRole] = useState('')
  const [loadError, setLoadError] = useState<string | null>(null)
  const [selected, setSelected] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [makingPrimaryId, setMakingPrimaryId] = useState<string | null>(null)
  // Bumped by the load-error Retry button to re-run the fetch effect below.
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => {
    let cancelled = false
    setLoadError(null)
    fetchTeamSlackChannels(teamId)
      .then((resp) => {
        if (cancelled) return
        if (!resp) {
          setLoadError('Could not load Slack channels. Check your connection and try again.')
          return
        }
        setChannels(resp.channels)
        setSelected(resp.channels.filter((c) => c.tracked).map((c) => c.channel_id))
        setWarnings(resp.warnings)
        setRole(resp.role)
      })
      .catch(() => {
        if (!cancelled) {
          setLoadError('Could not load Slack channels. Check your connection and try again.')
        }
      })
    return () => {
      cancelled = true
    }
  }, [teamId, reloadKey])

  const trackedIds = useMemo(
    () => (channels ?? []).filter((c) => c.tracked).map((c) => c.channel_id),
    [channels],
  )
  // Memoized so identity is stable across renders that don't touch the
  // selection (e.g. `saving` or `makingPrimaryId` flipping) — SlackChannelPicker
  // re-sorts its list off this Set's reference, and a fresh `new Set(...)` every
  // render would invalidate that memo for no reason.
  const selectedSet = useMemo(() => new Set(selected), [selected])
  const dirty = channels !== null && !sameIdSet(selected, trackedIds)
  const toggleChannel = (channelId: string) => {
    setSelected((prev) =>
      prev.includes(channelId) ? prev.filter((id) => id !== channelId) : [...prev, channelId],
    )
  }

  const save = async () => {
    setSaving(true)
    try {
      const res = await saveTeamSlackChannels(teamId, selected)
      if (!res.ok) {
        toast.error(res.error)
        return
      }
      setChannels(res.data.channels)
      setSelected(res.data.channels.filter((c) => c.tracked).map((c) => c.channel_id))
      setWarnings(res.data.warnings)
      setRole(res.data.role)
      toast.success('Slack channels saved')
    } finally {
      setSaving(false)
    }
  }

  const cancel = () => setSelected(trackedIds)

  // Primary reassignment is deliberately minimal (no modal, no confirm
  // beyond the click) and commits immediately rather than riding Save. A
  // clean draft (no pending pick/unpick) re-syncs to the refreshed tracked
  // set; an in-progress unsaved selection is left alone rather than
  // clobbered.
  const handleMakePrimary = async (channelId: string) => {
    setMakingPrimaryId(channelId)
    try {
      const res = await reassignSlackChannelPrimary(channelId, teamId)
      if (!res.ok) {
        toast.error(res.error)
        return
      }
      const wasClean = !dirty
      const fresh = await fetchTeamSlackChannels(teamId)
      if (fresh) {
        setChannels(fresh.channels)
        setWarnings(fresh.warnings)
        setRole(fresh.role)
        if (wasClean) {
          setSelected(fresh.channels.filter((c) => c.tracked).map((c) => c.channel_id))
        }
      } else {
        // The reassignment itself committed — only the refresh failed. Say so
        // rather than leaving the list silently stale with no indication a
        // reload is needed.
        toast.error(
          'Primary reassigned, but the list failed to refresh — reload to see the change.',
        )
      }
    } finally {
      setMakingPrimaryId(null)
    }
  }

  if (loadError) {
    return (
      <div className="px-1 py-3 text-[13px] text-text-secondary">
        {loadError}{' '}
        <button
          type="button"
          onClick={() => setReloadKey((k) => k + 1)}
          className="text-accent underline"
        >
          Retry
        </button>
      </div>
    )
  }

  if (channels === null) {
    return <div className="px-1 py-3 text-[13px] text-text-tertiary">Loading Slack channels…</div>
  }

  return (
    <div className="space-y-4">
      <p className="text-[13px] leading-relaxed text-text-tertiary">
        Channels this team tracks route Slack @mentions to its board. Claim a channel someone
        already mentioned the bot in, or pick one before anyone has.
      </p>

      {channels.length === 0 ? (
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Connect a Slack workspace in organization settings first.
        </p>
      ) : (
        <>
          <SlackChannelPicker
            channels={channels}
            selected={selectedSet}
            onToggle={toggleChannel}
            disabled={role !== 'admin' || saving}
            orgIsAdmin={orgIsAdmin}
            onMakePrimary={handleMakePrimary}
            makingPrimaryId={makingPrimaryId}
          />

          {warnings.length > 0 && (
            <ul className="space-y-1">
              {warnings.map((w, i) => (
                <li key={i} className="text-[12px] text-amber-600">
                  {warningMessage(w, channels)}
                </li>
              ))}
            </ul>
          )}

          <div className="flex items-center justify-end gap-2 border-t border-border-subtle pt-4">
            <button
              type="button"
              onClick={cancel}
              disabled={!dirty || saving}
              className="rounded-xl px-3 py-2 text-[13px] font-medium text-text-tertiary transition-colors hover:text-text-secondary disabled:opacity-40"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={() => void save()}
              disabled={!dirty || saving || role !== 'admin'}
              className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white shadow-[0_10px_28px_-10px_var(--color-accent)] transition-all hover:bg-accent/90 disabled:opacity-40 disabled:shadow-none"
            >
              {saving ? 'Saving…' : 'Save'}
            </button>
          </div>
        </>
      )}
    </div>
  )
}
