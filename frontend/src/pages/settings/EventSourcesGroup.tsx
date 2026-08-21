// Per-source event-production switches for the org.
//
// The switch reads "produce events", not "connected". Turning a source off
// leaves its credential bound, so `tfac exec jira` keeps working and an agent
// can still comment on the ticket it is working — only new events stop, and
// only going forward: existing tasks, in-flight runs and authored handlers are
// untouched. Cutting an agent's access is the credential disconnect above,
// which keeps that meaning.
//
// Admin-only, matching the route: an org admin can flip it, a member can only
// read it. The read is what makes an inert handler explainable, which is why it
// is member-gated on the server even though this control is not.

import { useState } from 'react'
import { apiJSON, httpErrorMessage } from '../../lib/apiClient'
import { sourceLabel } from '../../lib/eventSources'
import {
  invalidateEventSources,
  useEventSources,
  type EventSourceAvailability,
} from '../../hooks/useEventSources'
import { toast } from '../../components/Toast/toastStore'

// A source with no credential bound, no licence, or nothing shipped has nothing
// to pause: the switch would be a control over a thing that is already off, and
// flipping it would change nothing the reader can see. Those rows render their
// state and no control.
function isPausable(state: EventSourceAvailability['state']): boolean {
  return state === 'available' || state === 'disabled'
}

// The word under each row: what this state means for event production, phrased
// from the org's point of view rather than the reader's.
function stateNote(kind: string, state: EventSourceAvailability['state']): string {
  switch (state) {
    case 'available':
      return 'Producing events.'
    case 'disabled':
      return 'Paused. The credential is still bound, so agents can still read and write here.'
    case 'unconfigured':
      return `${sourceLabel(kind)} is not connected yet — connect it above.`
    case 'unlicensed':
      return `${sourceLabel(kind)} is not enabled for this workspace.`
    default:
      return `${sourceLabel(kind)} is not supported yet.`
  }
}

export default function EventSourcesGroup({
  orgId,
  canEdit,
}: {
  orgId: string | null
  canEdit: boolean
}) {
  const { sources, loaded } = useEventSources()
  const [pending, setPending] = useState<string | null>(null)

  async function setDisabled(kind: string, disabled: boolean) {
    if (!orgId) return
    setPending(kind)
    try {
      await apiJSON(`/api/orgs/${encodeURIComponent(orgId)}/sources/${encodeURIComponent(kind)}`, {
        method: 'PATCH',
        body: JSON.stringify({ disabled }),
      })
      // The server also broadcasts a payload-free ping for every other client;
      // invalidating here is what makes THIS one update without waiting for the
      // round trip back through the socket.
      invalidateEventSources(orgId)
    } catch (err) {
      toast.error(
        httpErrorMessage(
          err,
          `Could not ${disabled ? 'turn off' : 'turn on'} ${sourceLabel(kind)} events.`,
        ),
      )
    } finally {
      setPending(null)
    }
  }

  if (!loaded) {
    return <p className="text-[12px] text-text-tertiary">Loading event sources…</p>
  }

  return (
    <div className="flex flex-col gap-3">
      <p className="text-[11px] text-text-tertiary">
        Pausing a source stops it creating new tasks. It does not disconnect the integration, and it
        does not close tasks or stop runs that already exist.
      </p>
      {sources.map((src) => {
        const off = src.state === 'disabled'
        const editable = canEdit && isPausable(src.state)
        return (
          <div key={src.kind} className="flex items-center justify-between gap-4">
            <div className="min-w-0">
              <p className="text-[13px] text-text-primary">{sourceLabel(src.kind)}</p>
              <p className="mt-0.5 text-[11px] text-text-tertiary">
                {stateNote(src.kind, src.state)}
              </p>
            </div>
            {editable && (
              <button
                type="button"
                role="switch"
                aria-label={`${sourceLabel(src.kind)} events`}
                aria-checked={!off}
                disabled={pending === src.kind}
                onClick={() => void setDisabled(src.kind, !off)}
                className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors disabled:opacity-50 ${
                  off ? 'bg-black/[0.08]' : 'bg-accent'
                }`}
              >
                <span
                  className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow-sm transition-transform ${
                    off ? 'translate-x-0' : 'translate-x-4'
                  }`}
                />
              </button>
            )}
          </div>
        )
      })}
    </div>
  )
}
