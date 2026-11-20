import { useCallback, useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { Dialog } from '../../ui/dialog/Dialog'
import { HttpError, httpErrorMessage } from '../../lib/apiClient'
import { toast } from '../../components/Toast/toastStore'
import {
  activateSlackConnection,
  fetchSlackActivationPreview,
  slackAppLabel,
  slackBlockerText,
  slackJoinWarningText,
  slackWorkspaceLabel,
  type SlackActivationChannel,
  type SlackActivationPreview,
  type SlackChannelStatus,
  type SlackChannelWarning,
  type SlackConnection,
} from './slackConnect'
import { SourceChip } from './SlackChips'

/**
 * SlackActivationDialog is the switch preview: what making `target` its
 * workspace's active connection changes, read from activation-preview, and the
 * Confirm that posts activate with the preview's fingerprint. A preview with a
 * blocker cannot be confirmed. When activate reports channels the new bot did
 * not join, the dialog stays open to list them.
 *
 * It portals to <body>, because the settings groups sit inside a
 * backdrop-blurred section, which would hold a fixed overlay inside itself.
 */
export default function SlackActivationDialog(props: {
  orgId: string
  target: SlackConnection
  /** The workspace has an active connection to switch from. Read from the
   *  card's list, for the heading while the preview loads. */
  switching: boolean
  onClose: () => void
  /** Runs once activate answers 200, before any warnings are shown. */
  onActivated: (conn: SlackConnection) => void
}) {
  return createPortal(<ActivationDialog {...props} />, document.body)
}

type Phase =
  | { kind: 'loading' }
  | { kind: 'failed'; message: string }
  | { kind: 'ready'; preview: SlackActivationPreview }
  | { kind: 'done'; preview: SlackActivationPreview; warnings: SlackChannelWarning[] }

const STALE_TEXT = 'This changed since you opened it. Review it again.'

function ActivationDialog({
  orgId,
  target,
  switching,
  onClose,
  onActivated,
}: {
  orgId: string
  target: SlackConnection
  switching: boolean
  onClose: () => void
  onActivated: (conn: SlackConnection) => void
}) {
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [submitting, setSubmitting] = useState(false)
  // Why the last Confirm did not switch, shown above the reloaded preview.
  const [refusal, setRefusal] = useState<string | null>(null)

  // Keyed on the connection's identity rather than the row object, so a
  // reread of the card's list cannot restart the preview.
  const { workspace_id, api_app_id } = target
  const read = useCallback(
    () =>
      fetchSlackActivationPreview(orgId, { workspace_id, api_app_id }).then(
        (preview) => setPhase({ kind: 'ready', preview }),
        (e) =>
          setPhase({
            kind: 'failed',
            message: httpErrorMessage(e, 'Could not read what the switch changes.'),
          }),
      ),
    [orgId, workspace_id, api_app_id],
  )

  useEffect(() => {
    void read()
  }, [read])

  const preview = phase.kind === 'ready' || phase.kind === 'done' ? phase.preview : null
  const hasFrom = preview ? preview.from !== null : switching
  const workspace = slackWorkspaceLabel(preview?.to ?? target)

  const confirm = async () => {
    if (phase.kind !== 'ready' || submitting) return
    setSubmitting(true)
    setRefusal(null)
    try {
      const result = await activateSlackConnection(
        orgId,
        { workspace_id, api_app_id },
        phase.preview.fingerprint,
      )
      onActivated(result.connection)
      if (result.warnings.length > 0) {
        setPhase({ kind: 'done', preview: phase.preview, warnings: result.warnings })
        return
      }
      toast.success(`${workspace} now uses ${slackAppLabel(result.connection)}`)
      onClose()
    } catch (e) {
      // 409: the stored state moved since the preview was read. 422: a
      // blocker appeared since. Either way the reloaded preview is the one
      // to decide on.
      if (e instanceof HttpError && (e.status === 409 || e.status === 422)) {
        setRefusal(e.status === 409 ? STALE_TEXT : httpErrorMessage(e, STALE_TEXT))
        setPhase({ kind: 'loading' })
        await read()
      } else {
        setRefusal(httpErrorMessage(e, 'Could not switch the workspace’s app.'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  if (phase.kind === 'done') {
    // Its own key, so the focus trap starts again and lands on Done: the
    // Switch control that held focus is gone.
    return (
      <Dialog
        key="done"
        open
        build="none"
        noConfirm
        cancelLabel="Done"
        onCancel={onClose}
        title={hasFrom ? 'Switched' : 'Activated'}
        body={`The new bot is not in ${phase.warnings.length === 1 ? 'this watched channel' : 'these watched channels'}, so messages there are not seen until it is.`}
        width={480}
      >
        <ul className="space-y-1.5">
          {phase.warnings.map((w, i) => (
            <li key={w.channel_id ?? i} className="text-ui leading-snug text-ink-2">
              <span className="font-medium text-ink-1">
                {channelName(phase.preview.channels, w.channel_id)}
              </span>
              : {slackJoinWarningText(w.reason)}
            </li>
          ))}
        </ul>
      </Dialog>
    )
  }

  const blocked = (preview?.blockers.length ?? 0) > 0
  return (
    <Dialog
      open
      build="quiet"
      title={hasFrom ? 'Switch apps' : 'Activate'}
      body={
        hasFrom
          ? `Triage Factory acts in ${workspace} through the new app from then on, and ignores what the current app receives. The current app stays connected, so you can switch back without reinstalling.`
          : `Triage Factory acts in ${workspace} through this app from then on.`
      }
      confirmLabel={
        hasFrom ? (submitting ? 'Switching…' : 'Switch') : submitting ? 'Activating…' : 'Activate'
      }
      confirmDisabled={phase.kind !== 'ready' || blocked || submitting}
      onConfirm={() => void confirm()}
      // Once activate is sent the switch happens whether or not the dialog
      // stays, so it stays: closing it would drop the channels the new bot
      // did not join.
      onCancel={submitting ? undefined : onClose}
      width={480}
    >
      {refusal && (
        <p role="alert" className="mb-4 text-ui leading-snug text-alarm">
          {refusal}
        </p>
      )}
      {/* The reading scrolls within the dialog, which is centered in a fixed
          frame that does not scroll: a preview taller than the window would
          otherwise put Cancel and Confirm out of reach. */}
      <div className="max-h-[50vh] space-y-4 overflow-y-auto pr-1">
        {phase.kind === 'loading' && <p className="text-ui text-ink-3">Reading what changes…</p>}
        {phase.kind === 'failed' && (
          <p role="alert" className="text-ui leading-snug text-alarm">
            {phase.message}
          </p>
        )}
        {preview && <PreviewReading preview={preview} />}
      </div>
      {/* Outside the scrolling reading, beside the Confirm they disable. */}
      {preview && blocked && (
        <ul aria-label="Blockers" className="mt-4 space-y-1.5">
          {preview.blockers.map((b) => (
            <li key={b.code} className="text-ui leading-snug text-alarm">
              {slackBlockerText(b)}
            </li>
          ))}
        </ul>
      )}
    </Dialog>
  )
}

function PreviewReading({ preview }: { preview: SlackActivationPreview }) {
  return (
    <>
      <div className={`grid gap-3 ${preview.from ? 'grid-cols-2' : 'grid-cols-1'}`}>
        {preview.from && <AppSide label="From" conn={preview.from} />}
        <AppSide label="To" conn={preview.to} />
      </div>

      <ChannelGroups channels={preview.channels} />

      <p className="text-ui leading-snug text-ink-2">People @-mention the new bot from then on.</p>

      {preview.notes.length > 0 && (
        <ul className="space-y-1.5">
          {preview.notes.map((n) => (
            <li key={n.code} className="text-reported leading-snug text-ink-3">
              {n.message}
            </li>
          ))}
        </ul>
      )}
    </>
  )
}

function AppSide({ label, conn }: { label: string; conn: SlackConnection }) {
  return (
    <div className="min-w-0 space-y-1 rounded-lg bg-tint-2 px-3 py-2.5">
      <p className="text-label font-medium uppercase tracking-wide text-ink-3">{label}</p>
      <p className="truncate text-ui font-medium text-ink-1">{slackAppLabel(conn)}</p>
      <div className="flex flex-wrap items-center gap-1.5">
        <SourceChip source={conn.source} />
        <span className="font-mono text-label text-ink-3">{conn.bot_user_id}</span>
      </div>
    </div>
  )
}

// The preview's channel groups, in the order a reader acts on them. A group
// lists its channels by name only where the reader has something to do about
// each one.
const CHANNEL_GROUPS: {
  status: SlackChannelStatus
  label: string
  hint?: string
  listed: boolean
}[] = [
  { status: 'member', label: 'Already a member', listed: false },
  {
    status: 'will_join',
    label: 'Will join',
    hint: 'The new bot joins these on confirm.',
    listed: false,
  },
  {
    status: 'needs_invite',
    label: 'Needs an invite',
    hint: 'Invite the new bot to these in Slack. Until then, messages there are not seen.',
    listed: true,
  },
  { status: 'unknown', label: 'Unknown', hint: 'Could not be checked.', listed: true },
]

function ChannelGroups({ channels }: { channels: SlackActivationChannel[] }) {
  if (channels.length === 0) {
    return <p className="text-ui text-ink-3">No team watches a channel in this workspace.</p>
  }
  return (
    <div className="space-y-2">
      <p className="text-label font-medium uppercase tracking-wide text-ink-3">Watched channels</p>
      <ul className="space-y-2">
        {CHANNEL_GROUPS.map((g) => {
          const inGroup = channels.filter((c) => c.status === g.status)
          if (inGroup.length === 0) return null
          return (
            <li key={g.status} aria-label={g.label} className="space-y-0.5">
              <p className="text-ui text-ink-1">
                <span className="font-medium">{g.label}</span>
                <span className="ml-1.5 rounded bg-tint-3 px-1.5 py-px text-label text-ink-2">
                  {inGroup.length}
                </span>
              </p>
              {g.hint && <p className="text-reported leading-snug text-ink-3">{g.hint}</p>}
              {g.listed && (
                <p className="text-reported leading-snug text-ink-2">
                  {inGroup.map((c) => `#${c.name || c.channel_id}`).join(', ')}
                </p>
              )}
            </li>
          )
        })}
      </ul>
    </div>
  )
}

function channelName(channels: SlackActivationChannel[], id: string | undefined): string {
  const ch = channels.find((c) => c.channel_id === id)
  return `#${ch?.name || id || 'unknown channel'}`
}
