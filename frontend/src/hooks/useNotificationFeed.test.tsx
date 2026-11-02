import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useNotificationFeed } from './useNotificationFeed'
import { cancelInvoke, confirmInvoke, useNotificationActions } from './useNotificationActions'
import { jsonBody } from '../test/apiResponse'
import type { NotificationItem } from '../lib/notifications'
import type { WSEvent } from '../types'

// What is pinned is what decides a person's attention: an arrival is
// announced once, for something that just happened, and never replayed by a
// reload; and opening an item or taking its action marks it read.

let dispatch: ((event: WSEvent) => void) | null = null
vi.mock('./useWebSocket', () => ({
  useWebSocket: (handler: (event: WSEvent) => void) => {
    dispatch = handler
  },
  useWsConnected: () => true,
}))
let orgId: string
vi.mock('./useApiOrgId', () => ({ useApiOrgId: () => orgId }))
vi.mock('./useOrgHref', () => ({ useOrgHref: () => (path: string) => '/orgs/org-1' + path }))
const navigate = vi.fn()
vi.mock('react-router', () => ({ useNavigate: () => navigate }))
const toastError = vi.fn()
const toastInfo = vi.fn()
const toastSuccess = vi.fn()
const toastWarning = vi.fn()
vi.mock('../components/Toast/toastStore', () => ({
  toast: {
    error: (m: string) => toastError(m),
    info: (m: string) => toastInfo(m),
    success: (m: string) => toastSuccess(m),
    warning: (m: string) => toastWarning(m),
  },
}))

const LIST = '/api/me/notifications/list'

// What the server holds, per read.
let badge: number
let urgent: NotificationItem[]
// The server's clock, from its Date header. A test moves it to model a viewer
// whose own clock is off.
let serverNow: () => number
// A gate the urgent read waits on, for a read that is still in flight.
let urgentGate: Promise<void> | null
let high: NotificationItem[]

function note(id: string, over: Partial<NotificationItem> = {}): NotificationItem {
  const now = new Date().toISOString()
  return {
    id,
    kind: 'linear.team_unreadable',
    lifecycle: 'condition',
    priority: 'high',
    audience: { scope: 'org', role: 'admins', team_id: null, user_id: null },
    title: `title ${id}`,
    body: '',
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

const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const headers = new Headers({ Date: new Date(serverNow()).toUTCString() })
  if (url === LIST) {
    const body = JSON.parse(String(init?.body))
    if (body.page_size === 0) {
      return { ok: true, status: 200, headers, ...jsonBody({ items: [], total_count: badge }) }
    }
    if (body.priority_in[0] === 'urgent') {
      const items = urgent
      if (urgentGate) await urgentGate
      return { ok: true, status: 200, headers, ...jsonBody({ items, total_count: items.length }) }
    }
    return {
      ok: true,
      status: 200,
      headers,
      ...jsonBody({ items: high, total_count: high.length }),
    }
  }
  const id = url.split('/')[4]
  if (url.endsWith('/actions/gone'))
    return {
      ok: true,
      status: 200,
      headers,
      ...jsonBody({ notification: null, outcome: 'Untracked.' }),
    }
  if (url.endsWith('/actions/fixed')) {
    const resolved = note(id, { resolved_at: new Date().toISOString(), resolution: 'Fixed.' })
    return {
      ok: true,
      status: 200,
      headers,
      ...jsonBody({ notification: resolved, outcome: 'Fixed it.' }),
    }
  }
  if (url.includes('/actions/'))
    return {
      ok: true,
      status: 200,
      headers,
      ...jsonBody({ notification: note(id), outcome: 'Retrying.' }),
    }
  return { ok: true, status: 200, headers, ...jsonBody(note(id)) }
})

/** A ping, and the debounced read it starts. */
async function ping() {
  act(() => dispatch?.({ type: 'notifications_updated', data: {} }))
  await act(async () => {
    await new Promise((r) => setTimeout(r, 300))
  })
}

beforeEach(() => {
  orgId = 'org-1'
  badge = 0
  urgent = []
  high = []
  serverNow = () => Date.now()
  urgentGate = null
  fetchMock.mockClear()
  navigate.mockClear()
  toastError.mockClear()
  toastInfo.mockClear()
  toastSuccess.mockClear()
  toastWarning.mockClear()
  cancelInvoke()
  vi.stubGlobal('fetch', fetchMock)
})

describe('useNotificationFeed', () => {
  it('seeds arrivals from its first answer without announcing them', async () => {
    badge = 1
    high = [note('h1')]
    const { result } = renderHook(() => useNotificationFeed())

    await waitFor(() => expect(result.current.count).toBe(1))
    // Already there at load: a reload must never replay it.
    expect(result.current.arrival).toBeNull()
  })

  it('announces a new high item once', async () => {
    high = [note('h1')]
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    high = [note('h2'), note('h1')]
    await ping()
    expect(result.current.arrival?.id).toBe('h2')

    act(() => result.current.arrivalDone('h2'))
    await ping()
    expect(result.current.arrival).toBeNull()
  })

  it('does not announce an item that has been true for longer than a minute', async () => {
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    high = [note('h3', { first_seen_at: new Date(Date.now() - 3_600_000).toISOString() })]
    await ping()
    expect(result.current.arrival).toBeNull()
  })

  it('opens an item by marking it read, at once, and going to the notifications page', async () => {
    urgent = [note('u1', { priority: 'urgent' })]
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.urgent).toHaveLength(1))

    act(() => result.current.open('u1'))
    expect(result.current.urgent[0].read).toBe(true)
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/me/notifications/u1',
      expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ read: true }) }),
    )
    expect(navigate).toHaveBeenCalledWith('/orgs/org-1/notifications?id=u1')
  })

  it('holds an invoke that asks first until it is confirmed', async () => {
    const item = note('u1', { priority: 'urgent' })
    const action = {
      id: 'untrack',
      type: 'invoke' as const,
      label: 'Stop tracking ENG',
      confirm: 'Stops polling ENG.',
      allowed: true,
    }
    const { result } = renderHook(() => ({
      feed: useNotificationFeed(),
      actions: useNotificationActions(),
    }))
    await waitFor(() => expect(result.current.feed.count).toBe(0))

    act(() => result.current.feed.act(item, action))
    expect(result.current.actions.pending?.action.id).toBe('untrack')
    const ran = () =>
      fetchMock.mock.calls.some(([url]) => url === '/api/me/notifications/u1/actions/untrack')
    expect(ran()).toBe(false)

    await act(async () => confirmInvoke())
    expect(result.current.actions.pending).toBeNull()
    await waitFor(() => expect(ran()).toBe(true))
    // The outcome is the action's own words, as a warning while the row it
    // left open is still a condition that holds.
    await waitFor(() => expect(toastWarning).toHaveBeenCalledWith('Retrying.'))
  })

  it("tells the viewer an action's outcome, as a success once it resolved the row", async () => {
    const item = note('u1', { priority: 'urgent' })
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    await act(async () =>
      result.current.act(item, { id: 'fixed', type: 'invoke', label: 'Fix', allowed: true }),
    )
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith('Fixed it.'))
    expect(toastInfo).not.toHaveBeenCalled()
  })

  it('follows an in-app link in place and an external one in a new tab, marking it read', async () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const item = note('u1', { priority: 'urgent' })
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    act(() =>
      result.current.act(item, {
        id: 'org_settings',
        type: 'link',
        label: 'Open settings',
        href: '/orgs/org-1/org?tab=settings',
      }),
    )
    expect(navigate).toHaveBeenCalledWith('/orgs/org-1/org?tab=settings')

    act(() =>
      result.current.act(item, {
        id: 'external',
        type: 'link',
        label: 'Open the App',
        href: 'https://github.com/settings/apps/tf',
      }),
    )
    expect(open).toHaveBeenCalledWith('https://github.com/settings/apps/tf', '_blank', 'noopener')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/me/notifications/u1',
      expect.objectContaining({ method: 'PATCH' }),
    )
    open.mockRestore()
  })

  it("measures an arrival on the server's clock, not the viewer's", async () => {
    // The viewer's clock runs two minutes fast: measured on it, an item the
    // server has just seen would already be two minutes old.
    serverNow = () => Date.now() - 120_000
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    high = [note('h1', { first_seen_at: new Date(serverNow()).toISOString() })]
    await ping()
    expect(result.current.arrival?.id).toBe('h1')
  })

  it('drops a waiting arrival once it is no longer unread', async () => {
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    high = [note('h3'), note('h2')]
    await ping()
    expect(result.current.arrival?.id).toBe('h2')

    // h3 was read in another tab before its turn came.
    high = [note('h2')]
    await ping()
    act(() => result.current.arrivalDone('h2'))
    expect(result.current.arrival).toBeNull()
  })

  it('does not let a read already in flight undo an item it has just opened', async () => {
    urgent = [note('u1', { priority: 'urgent' })]
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.urgent).toHaveLength(1))

    let release = () => {}
    urgentGate = new Promise((r) => (release = r))
    await ping()
    act(() => result.current.open('u1'))

    // The parked read answers with the item as it was before the mark.
    await act(async () => {
      release()
      await new Promise((r) => setTimeout(r, 0))
    })
    expect(result.current.urgent[0].read).toBe(true)
  })

  it('sends no read mark for an action whose row has left what the viewer may read', async () => {
    const item = note('u1', { priority: 'urgent' })
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    await act(async () =>
      result.current.act(item, { id: 'gone', type: 'invoke', label: 'Untrack', allowed: true }),
    )
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/me/notifications/u1/actions/gone',
        expect.anything(),
      ),
    )
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(false)
    expect(toastInfo).toHaveBeenCalledWith('Untracked.')
  })
})
