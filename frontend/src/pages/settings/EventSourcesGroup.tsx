// Per-source on/off switches for the org.
//
// The switch reads "on", not "connected", and off means off: the source is not
// polled, creates no tasks, and resolves no credential for an agent. Saying so
// plainly is the point — an admin who turns Jira off to take load off their
// instance would have no way to discover from this screen that the agents kept
// their access, so the row states all three effects rather than the one this
// screen happens to be about.
//
// What survives is everything stored: the credential stays bound, and existing
// tasks, in-flight runs and authored handlers are untouched. That is the
// difference from the credential disconnect above, which keeps its own meaning.
//
// Not every source is switchable. GitHub is listed and explained but carries no
// control, because it has none to carry — see hasSwitch.
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

// Two reasons a row carries no switch, and they are different facts.
//
// `disableable` is the server's: a source the product is built on has no off
// switch at all, so there is nothing to render and a PATCH naming it is a 422.
//
// The state test is this screen's: a source with no credential bound, no
// licence, or nothing shipped is already off for a reason this control does not
// govern, so flipping it would change nothing the reader can see.
function hasSwitch(src: EventSourceAvailability): boolean {
  return src.disableable && (src.state === 'available' || src.state === 'disabled')
}

// The word under each row: what this state means for the org, phrased from the
// org's point of view rather than the reader's.
function stateNote(src: EventSourceAvailability): string {
  const { kind, state } = src
  switch (state) {
    case 'available':
      // A required source is on because it cannot be anything else. Saying so
      // is what stops the missing switch reading as a bug or a permission the
      // reader lacks.
      return src.disableable
        ? 'On — polled, creating tasks, and reachable by agents.'
        : 'Always on. Triage Factory is built on it, so it has no off switch.'
    case 'disabled':
      return 'Off — not polled, creating no tasks, and agents cannot reach it. The credential is still stored.'
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
  // Keyed per source, not a single kind. Every source a deployment REGISTERS is
  // disableable — required is a core-only property — so an ee build already
  // renders more than one switch, and a shared value would re-enable source A's
  // control the moment B was clicked, letting a second PATCH go out on A while
  // its first was still open.
  const [pending, setPending] = useState<ReadonlySet<string>>(() => new Set())
  const mark = (kind: string, busy: boolean) =>
    setPending((prev) => {
      const next = new Set(prev)
      if (busy) next.add(kind)
      else next.delete(kind)
      return next
    })

  async function setDisabled(kind: string, disabled: boolean) {
    if (!orgId) return
    mark(kind, true)
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
        httpErrorMessage(err, `Could not turn ${sourceLabel(kind)} ${disabled ? 'off' : 'on'}.`),
      )
    } finally {
      mark(kind, false)
    }
  }

  if (!loaded) {
    return <p className="text-[12px] text-ink-3">Loading event sources…</p>
  }

  return (
    <div className="flex flex-col gap-3">
      <p className="text-[11px] text-ink-3">
        Turning a source off stops everything Triage Factory does with it: polling it, creating
        tasks from it, and giving agents access to it. It does not disconnect the integration, and
        it does not close tasks or stop runs that already exist. Sources the product is built on
        cannot be turned off.
      </p>
      {sources.map((src) => {
        const off = src.state === 'disabled'
        const editable = canEdit && hasSwitch(src)
        return (
          <div key={src.kind} className="flex items-center justify-between gap-4">
            <div className="min-w-0">
              <p className="text-[13px] text-ink-1">{sourceLabel(src.kind)}</p>
              <p className="mt-0.5 text-[11px] text-ink-3">{stateNote(src)}</p>
            </div>
            {editable && (
              <button
                type="button"
                role="switch"
                aria-label={sourceLabel(src.kind)}
                aria-checked={!off}
                disabled={pending.has(src.kind)}
                onClick={() => void setDisabled(src.kind, !off)}
                className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors disabled:opacity-50 ${
                  off ? 'bg-black/[0.08]' : 'bg-warm'
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
