import { useMemo } from 'react'
import NotifyStrip, { type StripItem } from '../ui/notifystrip/NotifyStrip'
import { Dialog } from '../ui/dialog/Dialog'
import { LiveText } from '../ui/shared/LiveText'
import { ago } from '../ui/table/cells'
import type { NotificationFeed } from '../hooks/useNotificationFeed'
import type { NotificationItem } from '../lib/notifications'

// The notification feed drawn in the shell: NotifyStrip in the banner slot,
// and the confirmation an invoke action may ask for. The confirmation renders
// beside the shell rather than in the slot, which is its own stacking context.

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

function stripItem(n: NotificationItem, teams: Team[], act: NotificationFeed['act']): StripItem {
  // The first action this viewer can take: a link, or an invoke they may run.
  // One they may not is left out rather than offered and refused.
  const action = n.actions.find((a) => (a.type === 'link' ? !!a.href : a.allowed === true))
  const since = Date.parse(n.first_seen_at)
  return {
    id: n.id,
    scope: scopeLabel(n, teams),
    title: n.title,
    since: <LiveText compute={(now) => ago(Math.max(0, (now - since) / 60_000))} />,
    read: n.read,
    action: action && { label: action.label, onSelect: () => act(n, action) },
  }
}

export function NotificationStrip({ feed, teams }: { feed: NotificationFeed; teams: Team[] }) {
  const { urgent: items, arrival: next, act } = feed
  const urgent = useMemo(() => items.map((n) => stripItem(n, teams, act)), [items, teams, act])
  const arrival = useMemo(() => (next ? stripItem(next, teams, act) : null), [next, teams, act])
  return (
    <NotifyStrip
      urgent={urgent}
      arrival={arrival}
      onArrivalDone={feed.arrivalDone}
      onOpen={feed.open}
    />
  )
}

export function NotificationConfirm({ feed }: { feed: NotificationFeed }) {
  const p = feed.pending
  return (
    <Dialog
      open={p !== null}
      title={p?.action.label}
      body={p?.action.confirm}
      confirmLabel={p?.action.label}
      onConfirm={feed.confirm}
      onCancel={feed.cancel}
    />
  )
}
