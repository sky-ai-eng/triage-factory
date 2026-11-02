import { useEffect } from 'react'
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter, useNavigate } from 'react-router'
import type { WSEvent } from '../../types'
import Notifications from './Notifications'
import { NotificationConfirm } from '../../components/ShellNotifications'
import { cancelInvoke } from '../../hooks/useNotificationActions'
import { jsonBody } from '../../test/apiResponse'
import type { NotificationItem } from '../../lib/notifications'

// What is pinned is what the page promises a reader: a row is read once it has
// been seen, not merely rendered; a link to a row lands on it wherever it is;
// an invoke runs once and says what it is doing; the filters count what they
// would show; and the page states what it adds to the API's answer (who a row
// was not sent to) without inventing anything else.

const toasts = vi.hoisted(() => ({
  success: vi.fn(),
  warning: vi.fn(),
  info: vi.fn(),
  error: vi.fn(),
}))
vi.mock('../../components/Toast/toastStore', () => ({ toast: toasts }))

const viewer = vi.hoisted(() => ({ orgAdmin: false }))
vi.mock('../../hooks/useOrgRole', () => ({
  useOrgRole: () => ({ role: null, isAdmin: viewer.orgAdmin, loading: false }),
}))
const orgId = vi.hoisted(() => ({ v: 'org-1' }))
vi.mock('../../hooks/useApiOrgId', () => ({ useApiOrgId: () => orgId.v }))
vi.mock('../../hooks/useOrgHref', () => ({ useOrgHref: () => (p: string) => '/orgs/org-1' + p }))
vi.mock('../../hooks/useTeams', () => ({
  useTeams: () => ({ teams: [{ id: 'infra', name: 'Infra', slug: 'infra', role: 'admin' }] }),
}))
const ws = vi.hoisted(() => ({ dispatch: null as ((e: WSEvent) => void) | null }))
vi.mock('../../hooks/useWebSocket', () => ({
  useWebSocket: (handler: (e: WSEvent) => void) => {
    ws.dispatch = handler
  },
  useWsConnected: () => true,
}))

// IntersectionObserver, which jsdom lacks: a test says which rows are on screen.
let observers: Array<{ cb: IntersectionObserverCallback; els: Set<Element> }> = []
class FakeIO {
  private me: { cb: IntersectionObserverCallback; els: Set<Element> }
  constructor(cb: IntersectionObserverCallback) {
    this.me = { cb, els: new Set() }
    observers.push(this.me)
  }
  observe(el: Element) {
    this.me.els.add(el)
  }
  unobserve(el: Element) {
    this.me.els.delete(el)
  }
  disconnect() {
    this.me.els.clear()
  }
}
function onScreen(id: string, on = true) {
  for (const o of observers) {
    const el = [...o.els].find((e) => (e as HTMLElement).dataset.nid === id)
    if (el) {
      act(() =>
        o.cb([{ target: el, isIntersecting: on } as IntersectionObserverEntry], {} as never),
      )
    }
  }
}

function note(id: string, over: Partial<NotificationItem> = {}): NotificationItem {
  const now = new Date().toISOString()
  return {
    id,
    kind: 'linear.team_unreadable',
    lifecycle: 'condition',
    priority: 'high',
    audience: { scope: 'user', role: null, team_id: null, user_id: 'me' },
    title: `title ${id}`,
    body: `body ${id}`,
    dismissible: true,
    first_seen_at: now,
    last_seen_at: now,
    seen_count: 1,
    resolved_at: null,
    resolved_reason: null,
    resolution: null,
    expires_at: null,
    actions: [],
    read: false,
    dismissed: false,
    sent: true,
    ...over,
  }
}

// What the server holds.
let rows: NotificationItem[]
let orgRows: NotificationItem[]
let actionAnswer: () => { status: number; body: unknown }
let actionGate: Promise<void>
// Holds the open list's reads in turn, for a read still in flight.
let openGates: Array<Promise<void>>
// Holds a single read of one notification, by id.
let getGates: Record<string, Promise<void>>
// Holds every count-only read, or fails it.
let countGate: Promise<void>
let countFails: boolean
let markReadFails: boolean
type Call = { url: string; method: string; body: Record<string, unknown> | null }
let calls: Call[]

function matches(n: NotificationItem, f: Record<string, unknown>): boolean {
  if (f.status === 'open' && n.resolved_at) return false
  if (f.status === 'resolved' && !n.resolved_at) return false
  if (f.dismissed !== undefined && n.dismissed !== f.dismissed) return false
  return true
}

const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const method = init?.method ?? 'GET'
  const body = init?.body ? JSON.parse(String(init.body)) : null
  calls.push({ url, method, body })
  const ok = (b: unknown, status = 200) => ({
    ok: status < 400,
    status,
    headers: new Headers(),
    ...jsonBody(b),
  })
  if (url === '/api/me/notifications/list' || url === '/api/orgs/org-1/notifications/list') {
    if (body.status === 'open' && body.page_size !== 0) await openGates.shift()
    if (body.page_size === 0) {
      await countGate
      if (countFails) return ok({ errors: [{ reason: 'INTERNAL', message: 'it broke' }] }, 500)
    }
    const from = url.startsWith('/api/orgs') ? orgRows : rows
    const items = from.filter((n) => matches(n, body))
    return ok({
      items: body.page_size === 0 ? [] : items,
      next_page_token: '',
      total_count: items.length,
    })
  }
  if (url === '/api/orgs/org-1/teams/list') {
    return ok({
      items: [
        { id: 'infra', name: 'Infra', slug: 'infra', role: 'admin' },
        { id: 'platform', name: 'Platform', slug: 'platform' },
      ],
      next_page_token: '',
      total_count: 2,
    })
  }
  if (url === '/api/teams/archived/list') {
    return ok({
      items: [{ id: 'gone', name: 'Old', slug: 'old', archived_at: '2026-10-01T00:00:00Z' }],
      next_page_token: '',
      total_count: 1,
    })
  }
  if (url === '/api/me/notifications/mark-read') {
    if (markReadFails) return ok({ errors: [{ reason: 'INTERNAL', message: 'it broke' }] }, 500)
    return ok({ ids: body.ids })
  }
  const m = /^\/api\/me\/notifications\/([^/]+)(\/actions\/(.+))?$/.exec(url)
  if (m) {
    const n = [...rows, ...orgRows].find((r) => r.id === m[1])
    if (!n) return ok({ errors: [{ reason: 'NOT_FOUND', message: 'not found' }] }, 404)
    if (m[3]) {
      await actionGate
      const a = actionAnswer()
      return ok(a.body, a.status)
    }
    if (method === 'PATCH') Object.assign(n, body)
    else await getGates[n.id]
    return ok(n)
  }
  return ok({ errors: [{ reason: 'NOT_FOUND', message: 'not mocked' }] }, 404)
})

function markedRead(): string[] {
  return calls
    .filter((c) => c.url === '/api/me/notifications/mark-read')
    .flatMap((c) => c.body!.ids as string[])
}

async function settle(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

// The router's navigate, for a test that opens a second link.
const nav: { go: (to: string) => void } = { go: () => {} }
function Nav() {
  const navigate = useNavigate()
  useEffect(() => {
    nav.go = navigate
  }, [navigate])
  return null
}

async function mount(path = '/notifications') {
  const page = () => (
    <MemoryRouter initialEntries={[path]}>
      <Notifications />
      <NotificationConfirm />
      <Nav />
    </MemoryRouter>
  )
  const { rerender } = render(page())
  await settle()
  await settle()
  return { rerender: () => rerender(page()) }
}

function rowOf(title: string): HTMLElement {
  return screen.getByText(title).closest('[role="row"]') as HTMLElement
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('IntersectionObserver', FakeIO)
  vi.spyOn(document, 'hasFocus').mockReturnValue(true)
  observers = []
  calls = []
  rows = []
  orgRows = []
  viewer.orgAdmin = false
  actionGate = Promise.resolve()
  openGates = []
  getGates = {}
  countGate = Promise.resolve()
  countFails = false
  orgId.v = 'org-1'
  markReadFails = false
  ws.dispatch = null
  actionAnswer = () => ({ status: 200, body: { notification: null, outcome: 'Done.' } })
  for (const t of Object.values(toasts)) t.mockClear()
})
afterEach(() => {
  cancelInvoke()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('read on dwell', () => {
  it('marks a row read once it has been on screen for two seconds', async () => {
    rows = [note('a'), note('b')]
    await mount()

    const dot = (title: string) => rowOf(title).querySelector('.tb-mark')
    onScreen('a')
    await settle(1500)
    expect(dot('title a')).toHaveAttribute('data-on')
    await settle(1000)
    expect(dot('title a')).not.toHaveAttribute('data-on')
    await settle(2000)
    // Seen: a. Rendered and never on screen: b.
    expect(markedRead()).toEqual(['a'])
    expect(dot('title b')).toHaveAttribute('data-on')
  })

  it('does not count time the viewer is elsewhere', async () => {
    rows = [note('a')]
    await mount()
    vi.mocked(document.hasFocus).mockReturnValue(false)
    act(() => window.dispatchEvent(new Event('blur')))

    onScreen('a')
    await settle(5000)
    expect(markedRead()).toEqual([])
  })

  it('stops counting a row marked unread until it leaves the screen and comes back', async () => {
    rows = [note('a', { read: true })]
    await mount()
    onScreen('a')

    fireEvent.click(within(rowOf('title a')).getByRole('button', { name: 'More actions' }))
    fireEvent.click(screen.getByRole('menuitem', { name: 'Mark unread' }))
    await settle()
    expect(calls).toContainEqual(
      expect.objectContaining({ url: '/api/me/notifications/a', body: { read: false } }),
    )
    await settle(5000)
    expect(markedRead()).toEqual([])

    onScreen('a', false)
    onScreen('a')
    await settle(5000)
    expect(markedRead()).toEqual(['a'])
  })
})

describe('a link to one notification', () => {
  it('opens on its tab with the filters cleared, tints it and marks it read', async () => {
    rows = [
      note('open1', { priority: 'urgent' }),
      note('done', { resolved_at: new Date().toISOString(), resolution: 'Fixed.' }),
    ]
    await mount('/notifications?id=done')
    await settle(50)

    expect(screen.getByRole('tab', { name: /Resolved/ })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByText('Fixed.')).toBeInTheDocument()
    expect(rowOf('title done').style.getPropertyValue('--tb-row-bg')).toBe('var(--color-warm-1)')
    expect(markedRead()).toContain('done')

    await settle(3000)
    expect(rowOf('title done').style.getPropertyValue('--tb-row-bg')).toBe('transparent')
  })

  it('says so when the notification is gone, and leaves the list as it is', async () => {
    rows = [note('a')]
    await mount('/notifications?id=nope')
    expect(toasts.error).toHaveBeenCalledWith('That notification is gone or not yours to read.')
    expect(screen.getByText('title a')).toBeInTheDocument()
  })
})

describe('invoke actions', () => {
  const untrack = {
    id: 'untrack',
    type: 'invoke' as const,
    label: 'Stop tracking SEC',
    running_label: 'Stopping…',
    confirm: 'This team stops tracking SEC.',
    allowed: true,
  }

  it('asks first only when the action does, then shows its running label until it answers', async () => {
    rows = [note('a', { actions: [untrack] })]
    let open!: () => void
    actionGate = new Promise((r) => (open = r))
    actionAnswer = () => ({
      status: 200,
      body: {
        notification: { ...rows[0], resolved_at: 'now' },
        outcome: 'This team stopped tracking SEC.',
      },
    })
    await mount()

    fireEvent.click(screen.getByRole('button', { name: 'Stop tracking SEC' }))
    await settle()
    expect(screen.getByText('Stop tracking SEC?')).toBeInTheDocument()
    expect(calls.some((c) => c.url.includes('/actions/'))).toBe(false)

    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Stop tracking SEC' }),
    )
    await settle()
    expect(screen.getByText('Stopping…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Stop tracking SEC' })).toBeNull()

    rows[0].resolved_at = new Date().toISOString()
    await act(async () => open())
    await settle()
    expect(toasts.success).toHaveBeenCalledWith('This team stopped tracking SEC.')
    // Resolved: it left the Open tab on the read that followed.
    expect(screen.queryByText('title a')).toBeNull()
  })

  it('runs an action that does not ask at once, and offers none the viewer may not run', async () => {
    rows = [
      note('a', {
        actions: [
          {
            id: 'retry',
            type: 'invoke',
            label: 'Retry now',
            running_label: 'Retrying…',
            allowed: true,
          },
        ],
      }),
      note('b', { actions: [{ id: 'retry', type: 'invoke', label: 'Retry now', allowed: false }] }),
    ]
    actionAnswer = () => ({
      status: 200,
      body: { notification: rows[0], outcome: 'Retrying now.' },
    })
    await mount()

    expect(screen.getAllByRole('button', { name: 'Retry now' })).toHaveLength(1)
    fireEvent.click(screen.getByRole('button', { name: 'Retry now' }))
    await settle()
    expect(screen.queryByText('Retry now?')).toBeNull()
    expect(toasts.info).toHaveBeenCalledWith('Retrying now.')
  })
})

describe('filters', () => {
  it('counts each option against the other filter, and hides the empty ones', async () => {
    rows = [
      note('u', { priority: 'urgent' }),
      note('h', {
        priority: 'high',
        audience: { scope: 'team', role: 'members', team_id: 'infra', user_id: null },
      }),
      note('h2', { priority: 'high' }),
    ]
    await mount()
    const filters = screen.getByRole('group', { name: 'Priority' })
    expect(within(filters).getByRole('button', { name: /Urgent/ })).toHaveTextContent('1')
    expect(within(filters).getByRole('button', { name: /High/ })).toHaveTextContent('2')
    expect(within(filters).queryByRole('button', { name: /Low/ })).toBeNull()

    fireEvent.click(
      within(screen.getByRole('group', { name: 'For' })).getByRole('button', { name: /Infra/ }),
    )
    expect(within(filters).queryByRole('button', { name: /Urgent/ })).toBeNull()
    expect(within(filters).getByRole('button', { name: /High/ })).toHaveTextContent('1')
    expect(screen.queryByText('title u')).toBeNull()
    expect(screen.getByText('title h')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'clear filters' }))
    expect(screen.getByText('title u')).toBeInTheDocument()
  })

  it('shows an org admin every team, with the rows they are not sent tagged', async () => {
    viewer.orgAdmin = true
    rows = [note('mine')]
    orgRows = [
      note('mine'),
      note('theirs', {
        sent: false,
        audience: { scope: 'team', role: 'admins', team_id: 'platform', user_id: null },
      }),
      note('old', {
        sent: false,
        audience: { scope: 'team', role: 'members', team_id: 'gone', user_id: null },
      }),
    ]
    await mount()
    expect(screen.queryByText('not sent to you')).toBeNull()

    fireEvent.click(screen.getByRole('radio', { name: 'All teams' }))
    await settle()
    await settle()
    expect(within(rowOf('title theirs')).getByText('not sent to you')).toBeInTheDocument()
    expect(within(rowOf('title mine')).queryByText('not sent to you')).toBeNull()
    expect(screen.getByText('Other teams · visible as org admin')).toBeInTheDocument()
    expect(within(rowOf('title theirs')).getByText('Platform admins')).toBeInTheDocument()
    // An archived team's open rows are named too.
    expect(within(rowOf('title old')).getByText('Old')).toBeInTheDocument()
  })

  it('leaves dismissed rows out until asked for them', async () => {
    rows = [note('a'), note('d', { dismissed: true })]
    await mount()
    expect(screen.queryByText('title d')).toBeNull()
    expect(
      calls.find((c) => c.url === '/api/me/notifications/list' && c.body?.page_size !== 0)?.body,
    ).toMatchObject({ status: 'open', dismissed: false })

    fireEvent.click(screen.getByRole('checkbox', { name: 'Show dismissed' }))
    await settle()
    expect(within(rowOf('title d')).getByText('dismissed')).toBeInTheDocument()
  })
})

describe('the table', () => {
  it('opens the row menu without selecting the row', async () => {
    rows = [note('a')]
    await mount()
    const row = rowOf('title a')
    fireEvent.click(within(row).getByRole('button', { name: 'More actions' }))
    expect(screen.getByRole('menu')).toBeInTheDocument()
    expect(row).toHaveAttribute('aria-selected', 'false')
  })

  it('takes a row dismissed from its menu out of the list at once', async () => {
    rows = [note('a'), note('b')]
    await mount()
    fireEvent.click(within(rowOf('title a')).getByRole('button', { name: 'More actions' }))
    fireEvent.click(screen.getByRole('menuitem', { name: 'Dismiss for me' }))
    expect(screen.queryByText('title a')).toBeNull()
    await settle()
    expect(calls).toContainEqual(
      expect.objectContaining({
        url: '/api/me/notifications/a',
        method: 'PATCH',
        body: { dismissed: true },
      }),
    )
  })

  it('dismisses the selected rows once the undo window closes', async () => {
    rows = [note('a'), note('b')]
    await mount()
    fireEvent.click(within(rowOf('title a')).getByRole('checkbox'))
    fireEvent.pointerDown(screen.getByRole('button', { name: 'Dismiss' }), {
      button: 0,
      clientX: 0,
      clientY: 0,
    })
    await settle(300)
    expect(screen.queryByText('title a')).toBeNull()
    expect(calls.some((c) => c.method === 'PATCH')).toBe(false)

    await settle(10_000)
    expect(calls).toContainEqual(
      expect.objectContaining({
        url: '/api/me/notifications/a',
        method: 'PATCH',
        body: { dismissed: true },
      }),
    )
  })

  it('offers Dismiss only when every selected row can be dismissed', async () => {
    rows = [note('a'), note('fixed', { dismissible: false })]
    await mount()
    fireEvent.click(within(rowOf('title fixed')).getByRole('checkbox'))
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Mark read' })).toBeInTheDocument()
  })
})

describe('switching views', () => {
  it('settles on the view it switched to, whatever the one it left answers late', async () => {
    rows = [note('a')]
    let open!: () => void
    openGates = [new Promise((r) => (open = r))]
    await mount()

    // Resolved holds nothing, which reads as loaded only once it has answered.
    fireEvent.click(screen.getByRole('tab', { name: /Resolved/ }))
    await settle()
    expect(screen.getByText('Nothing has resolved in the last 30 days.')).toBeInTheDocument()
    // The Open read, superseded, answers late.
    await act(async () => open())
    await settle()
    expect(screen.getByText('Nothing has resolved in the last 30 days.')).toBeInTheDocument()
    expect(screen.queryByText('Loading…')).toBeNull()
  })
})

describe('switching orgs', () => {
  it('shows nothing read or chosen in the org it left', async () => {
    rows = [
      note('a', { audience: { scope: 'team', role: 'members', team_id: 'infra', user_id: null } }),
    ]
    const { rerender } = await mount()
    fireEvent.click(
      within(screen.getByRole('group', { name: 'For' })).getByRole('button', { name: /Infra/ }),
    )

    rows = [note('b')]
    let open!: () => void
    openGates = [new Promise((r) => (open = r))]
    orgId.v = 'org-2'
    rerender()
    await settle()
    expect(screen.queryByText('title a')).toBeNull()
    expect(screen.getByText('Loading…')).toBeInTheDocument()

    await act(async () => open())
    await settle()
    expect(screen.getByText('title b')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'clear filters' })).toBeNull()
  })
})

describe('rereads', () => {
  it('does not call a view loaded on a read a later one replaced', async () => {
    let first!: () => void
    let second!: () => void
    openGates = [new Promise((r) => (first = r)), new Promise((r) => (second = r))]
    await mount()
    // A ping lands while the first read is out, and its read goes out too.
    act(() => ws.dispatch?.({ type: 'notifications_updated', data: {} } as WSEvent))
    await settle(300)

    await act(async () => first())
    await settle()
    expect(screen.queryByText('Nothing needs you')).toBeNull()
    expect(screen.getByText('Loading…')).toBeInTheDocument()

    await act(async () => second())
    await settle()
    expect(screen.getByText('Nothing needs you')).toBeInTheDocument()
  })
})

describe('marks that do not land', () => {
  it('puts a read back, and says so, when the server refuses it', async () => {
    rows = [note('a')]
    markReadFails = true
    await mount()
    const dot = () => rowOf('title a').querySelector('.tb-mark')

    onScreen('a')
    await settle(2500)
    expect(dot()).not.toHaveAttribute('data-on')
    await settle(2000)
    expect(toasts.error).toHaveBeenCalledWith('it broke')
    expect(dot()).toHaveAttribute('data-on')
  })
})

describe('a second link', () => {
  it('keeps the page on the link opened last, however the first answers', async () => {
    rows = [note('x', { resolved_at: new Date().toISOString() }), note('y')]
    let slow!: () => void
    getGates = { x: new Promise((r) => (slow = r)) }
    await mount('/notifications?id=x')

    act(() => nav.go('/notifications?id=y'))
    await settle(50)
    expect(screen.getByRole('tab', { name: /Open/ })).toHaveAttribute('aria-selected', 'true')

    await act(async () => slow())
    await settle(50)
    expect(screen.getByRole('tab', { name: /Open/ })).toHaveAttribute('aria-selected', 'true')
    expect(rowOf('title y').style.getPropertyValue('--tb-row-bg')).toBe('var(--color-warm-1)')
  })
})

describe('copy link', () => {
  it('says it could not copy where there is no clipboard', async () => {
    rows = [note('a')]
    await mount()
    expect(navigator.clipboard).toBeUndefined()
    fireEvent.click(within(rowOf('title a')).getByRole('button', { name: 'More actions' }))
    fireEvent.click(screen.getByRole('menuitem', { name: 'Copy link' }))
    expect(toasts.error).toHaveBeenCalledWith('Could not copy the link.')
  })
})

describe('nothing open', () => {
  it('keeps the table, and the way to show them, while dismissed rows are open', async () => {
    rows = [note('d', { dismissed: true })]
    await mount()
    expect(screen.queryByText('Nothing needs you')).toBeNull()
    expect(screen.getByText('Everything open here is dismissed.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('checkbox', { name: 'Show dismissed' }))
    await settle()
    expect(screen.getByText('title d')).toBeInTheDocument()
  })

  it('says what nothing open means only once the counts have answered', async () => {
    let counted!: () => void
    countGate = new Promise((r) => (counted = r))
    await mount()
    expect(screen.queryByText('Everything open here is dismissed.')).toBeNull()
    expect(screen.getByText('Loading…')).toBeInTheDocument()

    await act(async () => counted())
    await settle()
    expect(screen.getByText('Nothing needs you')).toBeInTheDocument()
  })

  it('claims no dismissed rows when the counts do not answer', async () => {
    countFails = true
    await mount()
    expect(screen.queryByText('Everything open here is dismissed.')).toBeNull()
    expect(screen.getByText('Nothing open to show.')).toBeInTheDocument()
  })

  it('says nothing needs the viewer, and names the last thing that resolved', async () => {
    const day = 24 * 3600_000
    rows = [
      note('old', {
        title: 'Older',
        resolved_at: new Date(Date.now() - 6 * day).toISOString(),
        resolution: 'Fixed then.',
      }),
      note('new', {
        title: 'Newer',
        priority: 'low',
        resolved_at: new Date(Date.now() - day).toISOString(),
        resolution: 'Fixed now.',
      }),
    ]
    await mount()
    await settle()
    expect(screen.getByText('Nothing needs you')).toBeInTheDocument()
    expect(screen.getByText(/Last resolved 1d ago · Newer · Fixed now\./)).toBeInTheDocument()
  })
})
