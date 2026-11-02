import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import {
  listNotifications,
  listNotificationsAt,
  markNotificationRead,
  notificationPath,
  followNotificationLink,
  type NotificationAction,
  type NotificationFilter,
  type NotificationItem,
} from '../lib/notifications'
import { requestInvoke } from './useNotificationActions'
import { useApiOrgId } from './useApiOrgId'
import { useOrgHref } from './useOrgHref'
import { useWebSocket, useWsConnected } from './useWebSocket'
import type { WSEvent } from '../types'

// The shell's notifications: the rail band's count, the strip's urgent items,
// and the high arrivals it announces. Freshness is the invalidation-ping tier,
// as for the rail counts: `notifications_updated` says something changed, this
// refetches through REST, and REST is what carries the scoping. A client that
// misses a ping is stale, never wrong, and there is no polling loop.

/** The band's count: unread, open, undismissed urgent and high. */
const BADGE: NotificationFilter = {
  status: 'open',
  dismissed: false,
  unread: true,
  priority_in: ['urgent', 'high'],
}
/** The strip's urgent line. */
const URGENT: NotificationFilter = { status: 'open', dismissed: false, priority_in: ['urgent'] }
/** What an arrival is drawn from. */
const HIGH_UNREAD: NotificationFilter = {
  status: 'open',
  dismissed: false,
  unread: true,
  priority_in: ['high'],
}

/** The urgent items the line holds, and the unread high ones a refetch compares. */
const PAGE = 10

/** How recently a high item must have been first seen to be announced. One that
 *  only now enters the list but has been true for longer (a team the viewer
 *  just joined, a row that escalated into high) is not news. */
const ARRIVAL_WINDOW_MS = 60_000

/** Pings come in bursts (a sweep resolving several rows); one read answers a burst. */
const HINT_DEBOUNCE_MS = 250

export interface NotificationFeed {
  /** The band's count. `null` until the first answer. */
  count: number | null
  /** Open, undismissed urgent items, newest first. */
  urgent: NotificationItem[]
  /** The high arrival on show. The rest wait their turn. */
  arrival: NotificationItem | null
  /** The arrival has finished; show the next. */
  arrivalDone: (id: string) => void
  /** Mark the item read and open it on the notifications page. */
  open: (id: string) => void
  /** Follow a link, or run an invoke through the shared runner, which asks
   *  first when the invoke does. */
  act: (item: NotificationItem, action: NotificationAction) => void
}

type Answer = {
  org: string
  count: number | null
  urgent: NotificationItem[]
  arrivals: NotificationItem[]
}

const blank = (org: string): Answer => ({ org, count: null, urgent: [], arrivals: [] })

export function useNotificationFeed(): NotificationFeed {
  const orgID = useApiOrgId() ?? ''
  const navigate = useNavigate()
  const orgHref = useOrgHref()

  // The answers are stored with the org they answer for, so an org switch
  // reads as nothing known until the new org's answers land.
  const [answer, setAnswer] = useState<Answer>(() => blank(''))

  const orgRef = useRef(orgID)
  useEffect(() => {
    orgRef.current = orgID
  }, [orgID])

  // The unread high ids the last answer held. The first answer for an org
  // seeds it without announcing, so a reload never replays what was already
  // there.
  const seen = useRef<{ org: string; ids: Set<string> } | null>(null)
  // Reads overlap (a ping during a reconnect's read). An answer older than one
  // already applied is dropped: applied after it, the high items that arrived
  // in between would read as new a second time.
  const issued = useRef(0)
  const applied = useRef(0)

  const read = useCallback(async (org: string) => {
    if (!org) return
    const n = ++issued.current
    const [count, urgent, high] = await Promise.allSettled([
      listNotifications(BADGE, 0),
      listNotifications(URGENT, PAGE),
      listNotificationsAt(HIGH_UNREAD, PAGE),
    ])
    if (orgRef.current !== org || n < applied.current) return
    applied.current = n

    let fresh: NotificationItem[] = []
    let unread: Set<string> | null = null
    if (high.status === 'fulfilled') {
      const { page, at } = high.value
      const items = page.items
      unread = new Set(items.map((i) => i.id))
      const prev = seen.current?.org === org ? seen.current.ids : null
      if (prev) {
        // Measured on the server's clock, which first_seen_at is also on.
        fresh = items
          .filter((i) => !prev.has(i.id) && at - Date.parse(i.first_seen_at) < ARRIVAL_WINDOW_MS)
          // The list is newest first; arrivals are shown in the order they came.
          .reverse()
      }
      seen.current = { org, ids: unread }
    }

    // A read that fails keeps what the last one answered: stale until the
    // next ping, as with every other readout.
    setAnswer((prev) => {
      const base = prev.org === org ? prev : blank(org)
      return {
        org,
        count:
          count.status === 'fulfilled' && count.value.total_count !== null
            ? count.value.total_count
            : base.count,
        urgent: urgent.status === 'fulfilled' ? urgent.value.items : base.urgent,
        arrivals: [
          // A waiting arrival that has since been read, dismissed or resolved
          // is no longer news. The one on show finishes its own close.
          ...(unread ? base.arrivals.filter((a, i) => i === 0 || unread.has(a.id)) : base.arrivals),
          ...fresh.filter((f) => !base.arrivals.some((a) => a.id === f.id)),
        ],
      }
    })
  }, [])

  // Cold load, and a fresh set on every org change. Declared after the orgRef
  // sync above so that effect has already run when this one reads it. The
  // effect only kicks the read; its state is set after the read's await.
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void read(orgID)
  }, [orgID, read])

  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current)
    },
    [],
  )
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (event.type !== 'notifications_updated' || timer.current) return
        timer.current = setTimeout(() => {
          timer.current = null
          void read(orgRef.current)
        }, HINT_DEBOUNCE_MS)
      },
      [read],
    ),
  )

  // A dropped connection may have missed pings, so a reconnect reads afresh.
  const connected = useWsConnected()
  const wasConnected = useRef(connected)
  useEffect(() => {
    if (connected && !wasConnected.current) void read(orgRef.current)
    wasConnected.current = connected
  }, [connected, read])

  // Read is shown at once, so an urgent line drops to its read size without
  // waiting on the round trip. A read already in flight predates the mark and
  // would put it back, so it is dropped; the mark's own ping brings the next.
  // A mark that fails is put right by reading back what the server holds.
  const markRead = useCallback(
    (id: string) => {
      applied.current = issued.current + 1
      setAnswer((prev) => ({
        ...prev,
        urgent: prev.urgent.map((i) => (i.id === id ? { ...i, read: true } : i)),
      }))
      markNotificationRead(id).catch(() => void read(orgRef.current))
    },
    [read],
  )

  const open = useCallback(
    (id: string) => {
      markRead(id)
      navigate(orgHref(notificationPath(id)))
    },
    [markRead, navigate, orgHref],
  )

  const arrivalDone = useCallback((id: string) => {
    setAnswer((prev) => ({ ...prev, arrivals: prev.arrivals.filter((a) => a.id !== id) }))
  }, [])

  const act = useCallback(
    (item: NotificationItem, action: NotificationAction) => {
      if (action.type === 'link') {
        if (!action.href) return
        markRead(item.id)
        followNotificationLink(action.href, navigate)
        return
      }
      requestInvoke(orgRef.current, item, action, (notification) => {
        // A row that has left what the viewer may read has nothing left to
        // mark.
        if (notification) markRead(item.id)
        void read(orgRef.current)
      })
    },
    [markRead, navigate, read],
  )

  const current = answer.org === orgID ? answer : blank(orgID)
  return {
    count: current.count,
    urgent: current.urgent,
    arrival: current.arrivals[0] ?? null,
    arrivalDone,
    open,
    act,
  }
}
