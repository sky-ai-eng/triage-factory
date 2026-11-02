import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useNotificationFeed } from './useNotificationFeed'
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
vi.mock('./useApiOrgId', () => ({ useApiOrgId: () => 'org-1' }))
vi.mock('./useOrgHref', () => ({ useOrgHref: () => (path: string) => '/orgs/org-1' + path }))
const navigate = vi.fn()
vi.mock('react-router', () => ({ useNavigate: () => navigate }))
const toastError = vi.fn()
vi.mock('../components/Toast/toastStore', () => ({
  toast: { error: (m: string) => toastError(m) },
}))

const LIST = '/api/me/notifications/list'

// What the server holds, per read.
let badge: number
let urgent: NotificationItem[]
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
    expires_at: null,
    actions: [],
    read: false,
    dismissed: false,
    ...over,
  }
}

const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  if (url === LIST) {
    const body = JSON.parse(String(init?.body))
    if (body.page_size === 0) {
      return { ok: true, status: 200, ...jsonBody({ items: [], total_count: badge }) }
    }
    const items = body.priority_in[0] === 'urgent' ? urgent : high
    return { ok: true, status: 200, ...jsonBody({ items, total_count: items.length }) }
  }
  return { ok: true, status: 200, ...jsonBody(note(url.split('/')[4])) }
})

/** A ping, and the debounced read it starts. */
async function ping() {
  act(() => dispatch?.({ type: 'notifications_updated', data: {} }))
  await act(async () => {
    await new Promise((r) => setTimeout(r, 300))
  })
}

beforeEach(() => {
  badge = 0
  urgent = []
  high = []
  fetchMock.mockClear()
  navigate.mockClear()
  toastError.mockClear()
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
    expect(navigate).toHaveBeenCalledWith('/orgs/org-1/notifications')
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
    const { result } = renderHook(() => useNotificationFeed())
    await waitFor(() => expect(result.current.count).toBe(0))

    act(() => result.current.act(item, action))
    expect(result.current.pending?.action.id).toBe('untrack')
    const ran = () =>
      fetchMock.mock.calls.some(([url]) => url === '/api/me/notifications/u1/actions/untrack')
    expect(ran()).toBe(false)

    await act(async () => result.current.confirm())
    expect(result.current.pending).toBeNull()
    await waitFor(() => expect(ran()).toBe(true))
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
})
