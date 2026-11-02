import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { MouseEvent } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router'
import Table from '../../ui/table/Table'
import type { TableColumn, TableCommitReason, TableRow } from '../../ui/table/Table'
import { Segmented } from '../../ui/segmented/Segmented'
import Checkbox from '../../ui/checkbox/Checkbox'
import { ago } from '../../ui/table/cells'
import { usePageHeader } from '../../contexts/ChromeContext'
import { useOptionalAuth } from '../../contexts/AuthContext'
import { usePagedList } from '../../hooks/usePagedList'
import { useApiOrgId } from '../../hooks/useApiOrgId'
import { useOrgHref } from '../../hooks/useOrgHref'
import { useOrgRole } from '../../hooks/useOrgRole'
import { useTeams } from '../../hooks/useTeams'
import { useWebSocket, useWsConnected } from '../../hooks/useWebSocket'
import {
  requestInvoke,
  runningAction,
  runningLabel,
  useNotificationActions,
} from '../../hooks/useNotificationActions'
import { toast } from '../../components/Toast/toastStore'
import { HttpError, httpErrorMessage } from '../../lib/apiClient'
import {
  NOTIFICATIONS_LIST_PATH,
  followNotificationLink,
  getNotification,
  isExternalHref,
  listNotifications,
  listOrgNotifications,
  notificationPath,
  orgNotificationsListPath,
  usableActions,
  type NotificationAction,
  type NotificationFilter,
  type NotificationItem,
  type NotificationPriority,
} from '../../lib/notifications'
import { listOrgTeams } from '../../lib/orgTeams'
import { fetchArchivedTeams } from '../../lib/teamLifecycle'
import type { WSEvent } from '../../types'
import { RowMenu, type RowMenuEntry } from './RowMenu'
import { useDwellRead } from './useDwellRead'
import { useMarks } from './useMarks'
import './notifications.css'

// Notifications: one table of what the viewer is sent, open or resolved, with
// filters beside it. Everything a row says is the API's: its title, body,
// actions and their labels, who it is for, and why it resolved. The page adds
// the reading of it: which rows have been seen (on dwell), the filters, and
// an org admin's view of every team's rows.

type Tab = 'open' | 'resolved'
type List = 'sent' | 'all'

/** The largest page the API serves. */
const PAGE = 200
/** How long a deep-linked row stays tinted. */
const TINT_MS = 2400
/** How far below the scroller's top a deep-linked row lands. */
const LINK_OFFSET = 90
/** Pings come in bursts; one read answers a burst. */
const HINT_DEBOUNCE_MS = 250

const RANK: Record<NotificationPriority, number> = { urgent: 0, high: 1, low: 2 }
const PRIORITY_LABEL: Record<NotificationPriority, string> = {
  urgent: 'Urgent',
  high: 'High',
  low: 'Low',
}
const PRIORITIES: NotificationPriority[] = ['urgent', 'high', 'low']

function listPath(list: List, org: string): string {
  return list === 'all' ? orgNotificationsListPath(org) : NOTIFICATIONS_LIST_PATH
}

/** What a tab reads. Dismissed rows are left out of Open unless asked for;
 *  Resolved shows them, tagged. */
function tabFilter(tab: Tab, showDismissed: boolean): NotificationFilter {
  if (tab === 'resolved') return { status: 'resolved' }
  return showDismissed ? { status: 'open' } : { status: 'open', dismissed: false }
}

function viewKey(path: string, tab: Tab, showDismissed: boolean): string {
  return `${path}|${tab}|${tab === 'open' && showDismissed}`
}

/** The Open tab's count: what is open and not dismissed. */
const OPEN_COUNT: NotificationFilter = { status: 'open', dismissed: false }
/** Whether anything is open at all, dismissed rows included: with nothing
 *  undismissed but dismissed rows left, the page still offers to show them. */
const OPEN_ANY: NotificationFilter = { status: 'open' }

/** Minutes between an ISO time and `now`, past or future. */
function minutes(at: string | null, now: number): number {
  if (!at) return 0
  const t = Date.parse(at)
  return Number.isNaN(t) ? 0 : Math.max(0, Math.abs(now - t) / 60_000)
}

type Team = { id: string; name: string; mine: boolean }

/** Who a row is for, as the FOR column and its filter name it. The filter
 *  groups a team's rows by team, whichever of its roles they are for. */
function audienceOf(n: NotificationItem, teamName: (id: string) => string | undefined) {
  const a = n.audience
  if (a.scope === 'user') return { facet: 'you', label: 'You' }
  if (a.scope === 'org') {
    return a.role === 'admins'
      ? { facet: 'org:admins', label: 'Org admins' }
      : { facet: 'org:members', label: 'Everyone' }
  }
  const name = (a.team_id && teamName(a.team_id)) || 'Team'
  return { facet: `team:${a.team_id}`, label: a.role === 'admins' ? `${name} admins` : name }
}

/** Re-render on a clock, so the ages in the table move while the page is open. */
function useNow(everyMs: number): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), everyMs)
    return () => clearInterval(t)
  }, [everyMs])
  return now
}

export default function Notifications() {
  const org = useApiOrgId() ?? ''
  const auth = useOptionalAuth()
  const orgName = auth?.orgs?.find((o) => o.id === org)?.name ?? ''
  const { isAdmin: orgAdmin } = useOrgRole()
  const { teams: myTeams } = useTeams()
  const orgHref = useOrgHref()
  const navigate = useNavigate()
  const now = useNow(60_000)

  const [tab, setTab] = useState<Tab>('open')
  const [chosenList, setList] = useState<List>('sent')
  const list: List = orgAdmin ? chosenList : 'sent'
  const [fPri, setFPri] = useState<NotificationPriority | null>(null)
  const [fWho, setFWho] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [showDismissed, setShowDismissed] = useState(false)
  const [tint, setTint] = useState<string | null>(null)
  const [menu, setMenu] = useState<{ id: string; anchor: HTMLElement } | null>(null)
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null)
  const { running } = useNotificationActions()

  const clearFilters = () => {
    setFPri(null)
    setFWho(null)
    setQ('')
  }

  // ---- reads --------------------------------------------------------

  const path = listPath(list, org)
  const paged = usePagedList<NotificationItem>(path, 'Could not load your notifications.', {
    keyOf: idOf,
  })
  const loadPage = paged.load
  const key = viewKey(path, tab, showDismissed)
  const [loadedKey, setLoadedKey] = useState('')
  const [openCount, setOpenCount] = useState<{ path: string; n: number; any: number } | null>(null)
  const reload = useRef<() => void>(() => {})
  const reads = useRef(0)

  useEffect(() => {
    if (!org) return
    // A view left before its read answered is not the one that loaded, and
    // neither is a read a later one replaced: only the latest says so.
    let live = true
    const read = () => {
      const n = ++reads.current
      // A reread keeps the depth "load more" reached.
      void loadPage(
        { ...tabFilter(tab, showDismissed), page_size: PAGE },
        { keepDepth: true },
      ).then(() => {
        if (live && n === reads.current) setLoadedKey(key)
      })
      const count = (f: NotificationFilter) =>
        list === 'all' ? listOrgNotifications(org, f, 0) : listNotifications(f, 0)
      Promise.all([count(OPEN_COUNT), count(OPEN_ANY)])
        .then(([open, any]) => {
          if (open.total_count !== null && any.total_count !== null) {
            setOpenCount({ path, n: open.total_count, any: any.total_count })
          }
        })
        .catch(() => {})
    }
    reload.current = read
    read()
    return () => {
      live = false
    }
  }, [org, list, path, tab, showDismissed, key, loadPage])

  // The invalidation ping, and a reconnect, which may have missed some.
  const hint = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (hint.current) clearTimeout(hint.current)
    },
    [],
  )
  useWebSocket(
    useCallback((event: WSEvent) => {
      if (event.type !== 'notifications_updated' || hint.current) return
      hint.current = setTimeout(() => {
        hint.current = null
        reload.current()
      }, HINT_DEBOUNCE_MS)
    }, []),
  )
  const connected = useWsConnected()
  const wasConnected = useRef(connected)
  useEffect(() => {
    if (connected && !wasConnected.current) reload.current()
    wasConnected.current = connected
  }, [connected])

  // Names for the teams the viewer's own list leaves out, read only when the
  // All teams view needs them. An archived team's rows can still be open, so
  // its name is read too; the view is an org admin's, as that list is.
  const [orgTeams, setOrgTeams] = useState<{ org: string; teams: Team[] } | null>(null)
  useEffect(() => {
    if (list !== 'all' || !org || orgTeams?.org === org) return
    void Promise.allSettled([listOrgTeams(org), fetchArchivedTeams()]).then(
      ([active, archived]) => {
        const ts = [
          ...(active.status === 'fulfilled'
            ? active.value.map((t) => ({ id: t.id, name: t.name, mine: !!t.role }))
            : []),
          ...(archived.status === 'fulfilled'
            ? archived.value.map((t) => ({ id: t.id, name: t.name, mine: false }))
            : []),
        ]
        setOrgTeams({ org, teams: ts })
      },
    )
  }, [list, org, orgTeams])
  const teams = useMemo<Team[]>(() => {
    const mine = myTeams.map((t) => ({ id: t.id, name: t.name, mine: true }))
    const rest = (orgTeams?.org === org ? orgTeams.teams : []).filter(
      (t) => !mine.some((m) => m.id === t.id),
    )
    return [...mine, ...rest]
  }, [myTeams, orgTeams, org])
  const teamName = useCallback((id: string) => teams.find((t) => t.id === id)?.name, [teams])

  // ---- marks --------------------------------------------------------

  const reloadNow = useCallback(() => reload.current(), [])
  const { items, markRead, ...marks } = useMarks(paged.items, reloadNow)
  const pool = useMemo(
    () => (tab === 'open' && !showDismissed ? items.filter((n) => !n.dismissed) : items),
    [items, tab, showDismissed],
  )

  // Read on dwell: an open row the viewer has not read, seen for long enough.
  const unreadOpen = useRef(new Set<string>())
  useEffect(() => {
    unreadOpen.current = new Set(pool.filter((n) => !n.read && !n.resolved_at).map((n) => n.id))
  }, [pool])
  const dwell = useDwellRead(
    scroller,
    (id) => unreadOpen.current.has(id),
    (ids) => markRead(ids),
  )

  const markUnread = (id: string) => {
    dwell.suspend(id)
    marks.markUnread(id)
  }

  // ---- deep link ----------------------------------------------------

  const [params, setParams] = useSearchParams()
  const linkId = params.get('id')
  const [seek, setSeek] = useState<{ id: string; key: string } | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const linkView = useRef({ list, tab, showDismissed, orgAdmin, org, markRead })
  useEffect(() => {
    linkView.current = { list, tab, showDismissed, orgAdmin, org, markRead }
  })
  // The link being opened, so a second run of the effect for the same one (a
  // development double-mount) does not open it twice; and the last one asked
  // for, so an earlier link answering late does not take the page from it.
  const opening = useRef<string | null>(null)
  const latestLink = useRef<string | null>(null)
  useEffect(() => {
    if (!linkId) {
      opening.current = null
      return
    }
    if (opening.current === linkId) return
    opening.current = linkId
    latestLink.current = linkId
    // The id is consumed, so the same link opened again is a new open.
    setParams(
      (p) => {
        p.delete('id')
        return p
      },
      { replace: true },
    )
    getNotification(linkId)
      .then((n) => {
        if (!mounted.current || latestLink.current !== linkId) return
        const v = linkView.current
        // A row the viewer is not sent is only on an org admin's All teams.
        const nextList: List = !n.sent && v.orgAdmin ? 'all' : n.sent ? v.list : 'sent'
        const nextTab: Tab = n.resolved_at ? 'resolved' : 'open'
        const nextDismissed = n.dismissed || v.showDismissed
        setList(nextList)
        setTab(nextTab)
        setShowDismissed(nextDismissed)
        setFPri(null)
        setFWho(null)
        setQ('')
        setSeek({ id: n.id, key: viewKey(listPath(nextList, v.org), nextTab, nextDismissed) })
        v.markRead([n.id], true)
      })
      .catch((e) => {
        if (!mounted.current || latestLink.current !== linkId) return
        toast.error(
          e instanceof HttpError && e.status === 404
            ? 'That notification is gone or not yours to read.'
            : httpErrorMessage(e, 'Could not open that notification.'),
        )
      })
  }, [linkId, setParams])

  useEffect(() => {
    if (!seek || loadedKey !== seek.key) return
    const raf = requestAnimationFrame(() => {
      setSeek(null)
      const el = scroller
        ? Array.from(scroller.querySelectorAll<HTMLElement>('[data-nid]')).find(
            (e) => e.dataset.nid === seek.id,
          )
        : undefined
      if (el && scroller) {
        scroller.scrollTop +=
          el.getBoundingClientRect().top - scroller.getBoundingClientRect().top - LINK_OFFSET
      }
      setTint(seek.id)
    })
    return () => cancelAnimationFrame(raf)
  }, [seek, loadedKey, scroller])
  useEffect(() => {
    if (!tint) return
    const t = setTimeout(() => setTint(null), TINT_MS)
    return () => clearTimeout(t)
  }, [tint])

  // ---- actions ------------------------------------------------------

  const follow = (n: NotificationItem, a: NotificationAction) => {
    if (!a.href) return
    markRead([n.id])
    followNotificationLink(a.href, navigate)
  }
  const invoke = useCallback(
    (n: NotificationItem, a: NotificationAction) =>
      requestInvoke(org, n, a, () => reload.current()),
    [org],
  )

  const copyLink = (id: string) => {
    const url = new URL(orgHref(notificationPath(id)), window.location.origin).toString()
    // The clipboard is absent outside a secure context, such as a local
    // install served over plain http to another host.
    if (!navigator.clipboard) {
      toast.error('Could not copy the link.')
      return
    }
    navigator.clipboard
      .writeText(url)
      .then(() => toast.info('Link copied.'))
      .catch(() => toast.error('Could not copy the link.'))
  }

  // The menu is data; what an entry does is decided when it is picked.
  const menuEntries = (n: NotificationItem): RowMenuEntry[] => {
    const rest = usableActions(n).slice(1)
    const out = runningAction(n, running)
    const entries: RowMenuEntry[] = rest.map((a) =>
      a.type === 'invoke' && out
        ? { kind: 'item', id: `action:${a.id}`, label: runningLabel(out), disabled: true }
        : { kind: 'item', id: `action:${a.id}`, label: a.label },
    )
    if (rest.length) entries.push({ kind: 'rule' })
    if (!n.resolved_at) {
      entries.push(
        n.read
          ? { kind: 'item', id: 'unread', label: 'Mark unread' }
          : { kind: 'item', id: 'read', label: 'Mark read' },
      )
      if (n.dismissible) {
        entries.push(
          n.dismissed
            ? { kind: 'item', id: 'restore', label: 'Restore' }
            : { kind: 'item', id: 'dismiss', label: 'Dismiss for me' },
        )
      }
    }
    entries.push({ kind: 'item', id: 'copy', label: 'Copy link' })
    return entries
  }
  const pickMenu = (n: NotificationItem, id: string) => {
    if (id.startsWith('action:')) {
      const a = n.actions.find((x) => `action:${x.id}` === id)
      if (a?.type === 'link') follow(n, a)
      else if (a) invoke(n, a)
      return
    }
    if (id === 'read') markRead([n.id], true)
    else if (id === 'unread') markUnread(n.id)
    else if (id === 'dismiss') marks.setDismissed(n.id, true)
    else if (id === 'restore') marks.setDismissed(n.id, false)
    else if (id === 'copy') copyLink(n.id)
  }

  // ---- filters ------------------------------------------------------

  const audience = useCallback((n: NotificationItem) => audienceOf(n, teamName), [teamName])
  const passPri = useCallback((n: NotificationItem) => !fPri || n.priority === fPri, [fPri])
  const passWho = useCallback(
    (n: NotificationItem) => !fWho || audience(n).facet === fWho,
    [fWho, audience],
  )

  const facets = useMemo(() => {
    const byPri = new Map<string, number>()
    for (const n of pool) if (passWho(n)) byPri.set(n.priority, (byPri.get(n.priority) ?? 0) + 1)
    const byWho = new Map<string, number>()
    for (const n of pool) {
      if (!passPri(n)) continue
      const f = audience(n).facet
      byWho.set(f, (byWho.get(f) ?? 0) + 1)
    }
    const shown = (facet: string, count: number, selected: string | null) =>
      count > 0 || selected === facet
    const priority = PRIORITIES.map((p) => ({
      facet: p,
      name: PRIORITY_LABEL[p],
      count: byPri.get(p) ?? 0,
    })).filter((o) => shown(o.facet, o.count, fPri))
    const opt = (facet: string, name: string) => ({ facet, name, count: byWho.get(facet) ?? 0 })
    const forMine = [
      opt('org:admins', 'Org admins'),
      opt('org:members', 'Everyone'),
      ...teams.filter((t) => t.mine).map((t) => opt(`team:${t.id}`, t.name)),
      opt('you', 'You'),
    ].filter((o) => shown(o.facet, o.count, fWho))
    // Teams the viewer reads only as an org admin, by the facet their rows
    // carry, so a team whose name has not loaded yet still has a row.
    const otherFacets = new Set(
      [...byWho.keys()].filter((f) => f.startsWith('team:') && !forMine.some((o) => o.facet === f)),
    )
    if (fWho?.startsWith('team:') && !forMine.some((o) => o.facet === fWho)) otherFacets.add(fWho)
    const others =
      list === 'all'
        ? [...otherFacets]
            .map((f) => opt(f, teamName(f.slice('team:'.length)) ?? 'Team'))
            .sort((a, b) => a.name.localeCompare(b.name))
        : []
    return { priority, forMine, others }
  }, [pool, passPri, passWho, audience, teams, teamName, fPri, fWho, list])

  const rows = useMemo<TableRow[]>(() => {
    const needle = q.trim().toLowerCase()
    return pool
      .filter((n) => passPri(n) && passWho(n))
      .map((n) => ({ n, who: audience(n).label }))
      .filter(
        ({ n, who }) => !needle || [n.title, n.body, who].join(' ').toLowerCase().includes(needle),
      )
      .map(({ n, who }) => ({
        id: n.id,
        item: n,
        rank: RANK[n.priority],
        who,
        unread: !n.read && !n.resolved_at,
      }))
  }, [pool, passPri, passWho, audience, q])

  // ---- the table ----------------------------------------------------

  const resolvedTab = tab === 'resolved'
  // Built once per change to what a cell draws, not per render: the table
  // re-measures its columns whenever their identity changes.
  const observe = dwell.observe
  const columns = useMemo<TableColumn[]>(
    () => [
      {
        key: 'pri',
        label: 'PRIORITY',
        width: '84px',
        sortValue: (r) => r.rank,
        render: (r) => (
          <span
            className="nt-pri"
            data-tone={resolvedTab ? 'past' : r.item.priority === 'low' ? 'low' : 'warm'}
            data-unread={r.unread || undefined}
          >
            {r.item.priority.toUpperCase()}
            {r.unread ? <span className="nt-sr"> · unread</span> : null}
          </span>
        ),
      },
      {
        key: 'note',
        label: 'NOTIFICATION',
        floor: 320,
        sortValue: (r) => r.item.title,
        render: (r) => (
          <NoteCell
            n={r.item}
            first={usableActions(r.item)[0]}
            running={running}
            observe={observe(r.item.id)}
            onOpened={(n) => markRead([n.id])}
            onInvoke={invoke}
          />
        ),
      },
      {
        key: 'who',
        label: 'FOR',
        mono: false,
        width: '112px',
        drop: 2,
        color: () => 'var(--color-ink-3)',
      },
      {
        key: 'age',
        label: resolvedTab ? 'RESOLVED' : 'TRUE FOR',
        width: '76px',
        align: 'end',
        sortValue: (r) => minutes(resolvedTab ? r.item.resolved_at : r.item.first_seen_at, now),
        render: (r) =>
          resolvedTab
            ? `${ago(minutes(r.item.resolved_at, now))} ago`
            : ago(minutes(r.item.first_seen_at, now)),
      },
      resolvedTab
        ? {
            key: 'why',
            label: 'WHY',
            width: '240px',
            align: 'end',
            drop: 1,
            color: () => 'var(--color-ink-4)',
            sortValue: (r) => r.item.resolution ?? '',
            // A row whose sentence could not be written says nothing rather
            // than something the API did not say.
            render: (r) => r.item.resolution ?? '—',
          }
        : {
            key: 'last',
            label: 'LAST SEEN',
            width: '92px',
            align: 'end',
            drop: 1,
            color: () => 'var(--color-ink-4)',
            sortValue: (r) =>
              r.item.lifecycle === 'notice' && r.item.expires_at
                ? -minutes(r.item.expires_at, now)
                : minutes(r.item.last_seen_at, now),
            // A notice is not seen again; what it has left is time.
            render: (r) =>
              r.item.lifecycle === 'notice' && r.item.expires_at
                ? `in ${ago(minutes(r.item.expires_at, now))}`
                : `${ago(minutes(r.item.last_seen_at, now))} ago`,
          },
      {
        key: 'more',
        label: '',
        width: '28px',
        align: 'end',
        sortable: false,
        render: (r) => (
          <button
            type="button"
            className="nt-more"
            data-nt-more=""
            aria-label="More actions"
            aria-haspopup="menu"
            aria-expanded={menu?.id === r.id}
            data-open={menu?.id === r.id || undefined}
            onClick={(e: MouseEvent<HTMLButtonElement>) => {
              e.stopPropagation()
              const anchor = e.currentTarget
              setMenu((m) => (m?.id === r.id ? null : { id: String(r.id), anchor }))
            }}
          >
            ⋯
          </button>
        ),
      },
    ],
    [resolvedTab, now, running, menu, observe, markRead, invoke],
  )

  const onCommit = (
    actionId: string,
    ids: Array<string | number>,
    ctx: { reason: TableCommitReason },
  ) => {
    const keepalive = ctx.reason === 'unload'
    if (actionId === 'read') marks.commitRead(ids.map(String), keepalive)
    else if (actionId === 'dismiss') marks.commitDismissed(ids.map(String), keepalive)
  }

  // ---- the page -----------------------------------------------------

  const loaded = loadedKey === key
  const counts = openCount?.path === path ? openCount : null
  const count = counts ? counts.n : null
  // Nothing open at all. With dismissed rows left, the table stays, and so
  // does the control that shows them.
  const isEmpty =
    loaded &&
    tab === 'open' &&
    !showDismissed &&
    pool.length === 0 &&
    counts?.any === 0 &&
    !paged.error
  const unreadIds = tab === 'open' ? pool.filter((n) => !n.read).map((n) => n.id) : []
  const filtering = !!(fPri || fWho || q)
  const emptyLabel = !loaded
    ? 'Loading…'
    : filtering
      ? 'Nothing matches these filters.'
      : tab === 'resolved'
        ? 'Nothing has resolved in the last 30 days.'
        : showDismissed
          ? 'Nothing is open.'
          : 'Everything open here is dismissed.'

  // The last thing that resolved, for the empty state: read only when it
  // shows, from the same list.
  const [lastResolved, setLastResolved] = useState<{ path: string; n: NotificationItem } | null>(
    null,
  )
  useEffect(() => {
    if (!isEmpty || !org) return
    const read =
      list === 'all'
        ? listOrgNotifications(org, { status: 'resolved' }, PAGE)
        : listNotifications({ status: 'resolved' }, PAGE)
    read
      .then((p) => {
        // The list is ordered by priority; the latest is the one resolved last.
        const latest = p.items.reduce<NotificationItem | null>(
          (best, n) =>
            !best || Date.parse(n.resolved_at ?? '') > Date.parse(best.resolved_at ?? '')
              ? n
              : best,
          null,
        )
        if (latest) setLastResolved({ path, n: latest })
      })
      .catch(() => {})
  }, [isEmpty, org, list, path])
  const last = lastResolved?.path === path ? lastResolved.n : null

  const subtitle = isEmpty
    ? 'all clear'
    : list === 'all'
      ? `all teams${orgName ? ` · ${orgName}` : ''}`
      : count !== null
        ? `${count} open`
        : undefined
  usePageHeader(useMemo(() => ({ title: 'Notifications', subtitle }), [subtitle]))

  const menuRow = menu ? items.find((n) => n.id === menu.id) : undefined
  // A row that left the list takes its menu with it, rather than leaving it to
  // open again if the row comes back.
  if (menu && !menuRow) setMenu(null)

  const facet = (
    o: { facet: string; name: string; count: number },
    selected: string | null,
    pick: (v: string | null) => void,
  ) => (
    <button
      key={o.facet}
      type="button"
      className="nt-opt"
      aria-pressed={selected === o.facet}
      data-on={selected === o.facet || undefined}
      data-none={o.count === 0 || undefined}
      onClick={() => pick(selected === o.facet ? null : o.facet)}
    >
      <span className="nt-opt-name">{o.name}</span>
      <span className="nt-opt-count">{o.count}</span>
    </button>
  )

  return (
    <div className="nt">
      <div className="nt-bar">
        <div className="nt-tabs" role="tablist" aria-label="Notifications">
          {(
            [
              ['open', 'Open', count === null ? '--' : String(count)],
              ['resolved', 'Resolved', '30d'],
            ] as const
          ).map(([id, name, tail]) => (
            <button
              key={id}
              type="button"
              role="tab"
              className="nt-tab"
              aria-selected={tab === id}
              data-on={tab === id || undefined}
              onClick={() => {
                setTab(id)
                setMenu(null)
              }}
            >
              {name}
              <span className="nt-tab-tail">{tail}</span>
            </button>
          ))}
        </div>
        {orgAdmin ? (
          <Segmented
            variant="plate"
            mono={false}
            size="sm"
            label="Whose notifications"
            options={[
              { value: 'sent', label: 'Sent to me' },
              { value: 'all', label: 'All teams' },
            ]}
            value={list}
            onChange={(v) => {
              setList(v as List)
              setFWho(null)
              setMenu(null)
            }}
          />
        ) : null}
        <span className="nt-spacer" />
        {unreadIds.length ? (
          <button type="button" className="nt-edge" onClick={() => markRead(unreadIds, true)}>
            Mark all read
          </button>
        ) : null}
      </div>

      {isEmpty ? (
        <div className="nt-empty">
          <div className="nt-empty-title">Nothing needs you</div>
          <div className="nt-empty-body">
            {list === 'all'
              ? `No team in ${orgName || 'this organization'} has an open notification.`
              : 'When something stops working for you or a team you are on, it shows up here until it is fixed.'}
          </div>
          {last ? (
            <div className="nt-empty-last">
              Last resolved {ago(minutes(last.resolved_at, now))} ago · {last.title}
              {last.resolution ? ` · ${last.resolution}` : ''}
            </div>
          ) : null}
        </div>
      ) : (
        <div className="nt-main">
          <div className="nt-filters">
            {facets.priority.length ? (
              <div className="nt-group" role="group" aria-label="Priority">
                <span className="nt-group-label">PRIORITY</span>
                {facets.priority.map((o) =>
                  facet(o, fPri, (v) => setFPri(v as NotificationPriority | null)),
                )}
              </div>
            ) : null}
            {facets.forMine.length || facets.others.length ? (
              <div className="nt-group" role="group" aria-label="For">
                <span className="nt-group-label">FOR</span>
                {facets.forMine.map((o) => facet(o, fWho, setFWho))}
                {facets.others.length ? (
                  <>
                    <div className="nt-divider">
                      <span className="nt-divider-rule" />
                      <span className="nt-divider-label">Other teams · visible as org admin</span>
                    </div>
                    {facets.others.map((o) => facet(o, fWho, setFWho))}
                  </>
                ) : null}
              </div>
            ) : null}
            {tab === 'open' ? (
              <div className="nt-dismissed">
                <Checkbox
                  checked={showDismissed}
                  label="Show dismissed"
                  onChange={(next) => setShowDismissed(next)}
                />
              </div>
            ) : null}
            {filtering ? (
              <button type="button" className="nt-clear" onClick={clearFilters}>
                clear filters
              </button>
            ) : null}
          </div>
          <div className="nt-scroll" ref={setScroller}>
            {paged.error ? <p className="nt-error">{paged.error}</p> : null}
            <Table
              label={
                resolvedTab ? 'RESOLVED · 30 DAYS' : list === 'all' ? 'ALL TEAMS' : 'SENT TO YOU'
              }
              rows={rows}
              columns={columns}
              lines="wrap"
              mark={(r) => r.unread}
              pageSize={0}
              selectable={tab === 'open'}
              headerRight={
                <input
                  className="nt-search"
                  type="search"
                  value={q}
                  onChange={(e) => setQ(e.target.value)}
                  placeholder="Search notifications"
                  aria-label="Search notifications"
                />
              }
              bar={
                tab === 'open'
                  ? {
                      actions: [
                        { id: 'read', label: 'Mark read', message: (k) => `${k} marked read` },
                        {
                          id: 'dismiss',
                          label: 'Dismiss',
                          enabledFor: (rs) =>
                            rs.every((r) => r.item.dismissible && !r.item.dismissed),
                          message: (k) => `${k} dismissed for you`,
                        },
                      ],
                    }
                  : null
              }
              barPosition="fixed"
              mutate={(r, id) => {
                if (id === 'read') return { ...r, unread: false, item: { ...r.item, read: true } }
                if (id === 'dismiss') {
                  return showDismissed ? { ...r, item: { ...r.item, dismissed: true } } : null
                }
                return r
              }}
              onCommit={onCommit}
              emptyLabel={emptyLabel}
              rowBg={(r) => (r.id === tint ? 'var(--color-warm-1)' : null)}
            />
            {paged.hasMore ? (
              <div className="nt-loadmore">
                {paged.items.length} of {paged.total ?? paged.items.length} ·{' '}
                <button
                  type="button"
                  disabled={paged.loading}
                  onClick={() => void paged.loadMore()}
                >
                  load more
                </button>
              </div>
            ) : null}
          </div>
        </div>
      )}

      {menu && menuRow ? (
        <RowMenu
          anchor={menu.anchor}
          entries={menuEntries(menuRow)}
          onPick={(id) => pickMenu(menuRow, id)}
          onClose={() => setMenu(null)}
        />
      ) : null}
    </div>
  )
}

function idOf(n: NotificationItem): string {
  return n.id
}

/** A row's name column: its title and tags, its full body, and the first
 *  action it offers, inline after the body. */
function NoteCell({
  n,
  first,
  running,
  observe,
  onOpened,
  onInvoke,
}: {
  n: NotificationItem
  first: NotificationAction | undefined
  running: ReadonlyMap<string, string>
  observe: (el: Element | null) => void
  /** A link was followed: the browser goes there itself. */
  onOpened: (n: NotificationItem) => void
  onInvoke: (n: NotificationItem, a: NotificationAction) => void
}) {
  const stop = (e: MouseEvent) => e.stopPropagation()
  const out = first?.type === 'invoke' ? runningAction(n, running) : undefined
  let action = null
  if (first && out) {
    action = (
      <span className="nt-act" data-busy="">
        {runningLabel(out)}
      </span>
    )
  } else if (first?.type === 'invoke') {
    action = (
      <button
        type="button"
        className="nt-act"
        onClick={(e) => {
          stop(e)
          onInvoke(n, first)
        }}
      >
        {first.label}
      </button>
    )
  } else if (first?.href && isExternalHref(first.href)) {
    action = (
      <a
        className="nt-act"
        href={first.href}
        target="_blank"
        rel="noopener noreferrer"
        onClick={(e) => {
          stop(e)
          onOpened(n)
        }}
      >
        {first.label}
      </a>
    )
  } else if (first?.href) {
    action = (
      <Link
        className="nt-act"
        to={first.href}
        onClick={(e) => {
          stop(e)
          onOpened(n)
        }}
      >
        {first.label}
      </Link>
    )
  }
  return (
    <span className="nt-note" data-nid={n.id} ref={observe}>
      <span className="nt-title" data-dismissed={n.dismissed || undefined}>
        {n.title}
        {!n.sent ? <span className="nt-tag">not sent to you</span> : null}
        {n.dismissed ? <span className="nt-tag">dismissed</span> : null}
      </span>
      <span className="tb-line2 nt-body">
        {n.body}
        {action}
      </span>
    </span>
  )
}
