import { useEffect, useMemo } from 'react'
import NotifyStrip, { type StripItem } from '../ui/notifystrip/NotifyStrip'
import { Dialog } from '../ui/dialog/Dialog'
import { LiveText } from '../ui/shared/LiveText'
import { ago } from '../ui/table/cells'
import type { NotificationFeed } from '../hooks/useNotificationFeed'
import {
  cancelInvoke,
  confirmInvoke,
  runningAction,
  runningLabel,
  useNotificationActions,
} from '../hooks/useNotificationActions'
import { useApiOrgId } from '../hooks/useApiOrgId'
import type { NotificationItem } from '../lib/notifications'

// The notification feed drawn in the shell: NotifyStrip in the banner slot,
// and the confirmation an invoke action may ask for, wherever it was asked.
// The confirmation renders beside the shell rather than in the slot, which is
// its own stacking context.

type Team = { id: string; name: string }

/** The audience as the strip labels it: 'ORG', 'TEAM INFRA', 'YOU'. */
function scopeLabel(n: NotificationItem, teams: Team[]): string {
  switch (n.audience.scope) {
    case 'org':
      return 'ORG'
    case 'user':
      return 'YOU'
    case 'team': {
      const name = teams.find((t) => t.id === n.audience.team_id)?.name
      return name ? `TEAM ${name.toUpperCase()}` : 'TEAM'
    }
  }
}

function stripItem(
  n: NotificationItem,
  teams: Team[],
  act: NotificationFeed['act'],
  running: ReadonlyMap<string, string>,
): StripItem {
  // The first action this viewer can take: a link, or an invoke they may run.
  // One they may not is left out rather than offered and refused.
  const action = n.actions.find((a) => (a.type === 'link' ? !!a.href : a.allowed === true))
  // An invoke of this row already out, from here or from the page, holds
  // every invoke the row offers.
  const out = action?.type === 'invoke' ? runningAction(n, running) : undefined
  const since = Date.parse(n.first_seen_at)
  return {
    id: n.id,
    scope: scopeLabel(n, teams),
    title: n.title,
    since: <LiveText compute={(now) => ago(Math.max(0, (now - since) / 60_000))} />,
    read: n.read,
    action:
      action &&
      (out
        ? { label: runningLabel(out), onSelect: () => {}, busy: true }
        : { label: action.label, onSelect: () => act(n, action) }),
  }
}

export function NotificationStrip({
  feed,
  teams,
  held,
}: {
  feed: NotificationFeed
  teams: Team[]
  /** The shell's chrome is hidden: the strip keeps what it shows until it is back. */
  held: boolean
}) {
  const { urgent: items, arrival: next, act } = feed
  const { running } = useNotificationActions()
  const urgent = useMemo(
    () => items.map((n) => stripItem(n, teams, act, running)),
    [items, teams, act, running],
  )
  const arrival = useMemo(
    () => (next ? stripItem(next, teams, act, running) : null),
    [next, teams, act, running],
  )
  return (
    <NotifyStrip
      urgent={urgent}
      arrival={arrival}
      onArrivalDone={feed.arrivalDone}
      onOpen={feed.open}
      held={held}
    />
  )
}

/** The one confirmation an invoke asks for, from the strip or the page. Every
 *  invoke that asks first today gives something up, so it is asked as a
 *  destructive decision. */
export function NotificationConfirm() {
  const { pending } = useNotificationActions()
  const org = useApiOrgId() ?? ''
  // An invoke asked about in one org is never confirmed in another: the
  // dialog is outside the routed page, and stays open across a switch.
  const here = pending && pending.org === org ? pending : null
  useEffect(() => {
    if (pending && pending.org !== org) cancelInvoke()
  }, [pending, org])
  return (
    <Dialog
      open={here !== null}
      kind="destructive"
      build="quiet"
      title={here ? `${here.action.label}?` : undefined}
      body={here?.action.confirm}
      confirmLabel={here?.action.label}
      onConfirm={confirmInvoke}
      onCancel={cancelInvoke}
    />
  )
}
