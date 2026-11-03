import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { toast } from '../../components/Toast/toastStore'
import { httpErrorMessage } from '../../lib/apiClient'
import {
  BULK_MARKS_MAX,
  dismissNotifications,
  markNotificationsRead,
  setNotificationMarks,
  type NotificationItem,
} from '../../lib/notifications'

// The viewer's own marks, as the page writes them. Each is shown at once, over
// what the server last answered, and held there until the server agrees, so a
// read already in flight cannot put back a mark the viewer just changed. Read
// marks from dwell are batched: what was seen since the last call goes in one.

/** How often the rows seen since the last call are sent as read. */
const FLUSH_MS = 2000
/** How long a mark the page wrote is held over the server's answer without
 *  the server agreeing. Past it the server wins, as it does for a row that
 *  escalated after it was read. */
const HOLD_MS = 30_000

type Mark = { read?: boolean; dismissed?: boolean; at: number }

export interface Marks {
  /** The served rows with the page's marks laid over them. */
  items: NotificationItem[]
  /** Mark rows read: in the next batch, or `now`. */
  markRead: (ids: string[], now?: boolean) => void
  markUnread: (id: string) => void
  setDismissed: (id: string, dismissed: boolean) => void
  /** Send a bulk verb's marks, as its undo window closes. `keepalive` when
   *  the page is going away. */
  commitRead: (ids: string[], keepalive: boolean) => void
  commitDismissed: (ids: string[], keepalive: boolean) => void
}

/** The page's marks, minus those the server has caught up with or that have
 *  been held long enough. */
function prune(marks: ReadonlyMap<string, Mark>, served: NotificationItem[]): Map<string, Mark> {
  const byId = new Map(served.map((n) => [n.id, n]))
  const now = Date.now()
  const out = new Map<string, Mark>()
  for (const [id, m] of marks) {
    const n = byId.get(id)
    const agrees =
      n !== undefined &&
      (m.read === undefined || n.read === m.read) &&
      (m.dismissed === undefined || n.dismissed === m.dismissed)
    if (!agrees && now - m.at < HOLD_MS) out.set(id, m)
  }
  return out
}

type BulkSend = (ids: string[], init: { keepalive?: boolean }) => Promise<unknown>

/** Send ids through a bulk verb, BULK_MARKS_MAX to a call. */
function sendChunks(
  send: BulkSend,
  ids: string[],
  keepalive: boolean,
  onFail: (ids: string[], e: unknown) => void,
) {
  for (let i = 0; i < ids.length; i += BULK_MARKS_MAX) {
    const chunk = ids.slice(i, i + BULK_MARKS_MAX)
    send(chunk, { keepalive }).catch((e) => onFail(chunk, e))
  }
}

/** `reload` reads the list again: what a failed write falls back on. */
export function useMarks(served: NotificationItem[], reload: () => void): Marks {
  const [marks, setMarks] = useState<ReadonlyMap<string, Mark>>(() => new Map())
  const [servedFor, setServedFor] = useState(served)
  if (served !== servedFor) {
    setServedFor(served)
    setMarks((prev) => (prev.size ? prune(prev, served) : prev))
  }
  const mark = useCallback((ids: string[], m: { read?: boolean; dismissed?: boolean }) => {
    setMarks((prev) => {
      const next = new Map(prev)
      const at = Date.now()
      for (const id of ids) next.set(id, { ...next.get(id), ...m, at })
      return next
    })
  }, [])
  const unmark = useCallback((id: string) => {
    setMarks((prev) => {
      const next = new Map(prev)
      next.delete(id)
      return next
    })
  }, [])

  const reloadRef = useRef(reload)
  useEffect(() => {
    reloadRef.current = reload
  })
  // A bulk write that did not land is put back as it was, and said.
  const bulkFailed = useCallback((ids: string[], e: unknown, fallback: string) => {
    setMarks((prev) => {
      const next = new Map(prev)
      for (const id of ids) next.delete(id)
      return next
    })
    toast.error(httpErrorMessage(e, fallback))
    reloadRef.current()
  }, [])
  const readFailed = useCallback(
    (ids: string[], e: unknown) => bulkFailed(ids, e, 'Could not mark those notifications read.'),
    [bulkFailed],
  )
  const dismissFailed = useCallback(
    (ids: string[], e: unknown) => bulkFailed(ids, e, 'Could not dismiss those notifications.'),
    [bulkFailed],
  )
  const queue = useRef(new Set<string>())
  const flush = useCallback(
    (keepalive = false) => {
      const ids = [...queue.current]
      queue.current.clear()
      sendChunks(markNotificationsRead, ids, keepalive, readFailed)
    },
    [readFailed],
  )
  useEffect(() => {
    const t = setInterval(() => {
      if (queue.current.size) flush()
    }, FLUSH_MS)
    const gone = () => {
      if (queue.current.size) flush(true)
    }
    window.addEventListener('pagehide', gone)
    return () => {
      clearInterval(t)
      window.removeEventListener('pagehide', gone)
      gone()
    }
  }, [flush])

  const markRead = useCallback(
    (ids: string[], now = false) => {
      if (!ids.length) return
      mark(ids, { read: true })
      for (const id of ids) queue.current.add(id)
      if (now) flush()
    },
    [mark, flush],
  )

  const markUnread = useCallback(
    (id: string) => {
      queue.current.delete(id)
      mark([id], { read: false })
      setNotificationMarks(id, { read: false }).catch((e) => {
        unmark(id)
        toast.error(httpErrorMessage(e, 'Could not mark that unread.'))
      })
    },
    [mark, unmark],
  )

  const setDismissed = useCallback(
    (id: string, dismissed: boolean) => {
      mark([id], { dismissed })
      setNotificationMarks(id, { dismissed }).catch((e) => {
        unmark(id)
        toast.error(
          httpErrorMessage(e, dismissed ? 'Could not dismiss that.' : 'Could not restore that.'),
        )
      })
    },
    [mark, unmark],
  )

  const commitRead = useCallback(
    (ids: string[], keepalive: boolean) => {
      mark(ids, { read: true })
      sendChunks(markNotificationsRead, ids, keepalive, readFailed)
    },
    [mark, readFailed],
  )

  const commitDismissed = useCallback(
    (ids: string[], keepalive: boolean) => {
      mark(ids, { dismissed: true })
      sendChunks(dismissNotifications, ids, keepalive, dismissFailed)
    },
    [mark, dismissFailed],
  )

  const items = useMemo(
    () =>
      served.map((n) => {
        const m = marks.get(n.id)
        if (!m) return n
        return { ...n, read: m.read ?? n.read, dismissed: m.dismissed ?? n.dismissed }
      }),
    [served, marks],
  )

  return { items, markRead, markUnread, setDismissed, commitRead, commitDismissed }
}
