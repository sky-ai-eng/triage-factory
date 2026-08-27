import { describe, it, expect, vi } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { usePagedList } from './usePagedList'
import { jsonBody } from '../test/apiResponse'

// The hook is the one place page tokens are threaded, so these tests are about
// the threading: that loadMore continues the query load started (same filters,
// the token the server minted), that a token never crosses a filter change,
// and that a failed page leaves the surface with something to paint.

type Row = { id: string }

/** stubPages answers each successive call to the list path with the next
 *  canned page, and records the bodies it was called with. */
function stubPages(pages: Array<{ items: Row[]; next_page_token?: string; total_count: number }>) {
  const bodies: unknown[] = []
  let call = 0
  const fetchMock = vi.fn((_input: unknown, init?: { body?: string }) => {
    bodies.push(init?.body ? JSON.parse(init.body) : null)
    const page = pages[Math.min(call, pages.length - 1)]
    call++
    return Promise.resolve({ ok: true, ...jsonBody(page) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return { fetchMock, bodies }
}

describe('usePagedList', () => {
  it('loads the first page and reports the filtered total', async () => {
    stubPages([{ items: [{ id: 'a' }, { id: 'b' }], next_page_token: 'tok', total_count: 5 }])
    const { result } = renderHook(() => usePagedList<Row>('/api/tasks/list', 'Could not load.'))

    await act(async () => {
      await result.current.load({ statuses: ['queued'] })
    })

    expect(result.current.items.map((r) => r.id)).toEqual(['a', 'b'])
    expect(result.current.total).toBe(5)
    expect(result.current.hasMore).toBe(true)
    expect(result.current.error).toBe('')
  })

  it('continues the same query with the token the server minted', async () => {
    const { bodies } = stubPages([
      { items: [{ id: 'a' }], next_page_token: 'tok-1', total_count: 3 },
      { items: [{ id: 'b' }], next_page_token: 'tok-2', total_count: 3 },
    ])
    const { result } = renderHook(() => usePagedList<Row>('/api/tasks/list', 'Could not load.'))

    await act(async () => {
      await result.current.load({ statuses: ['queued'], only_unclaimed: true })
    })
    await act(async () => {
      await result.current.loadMore()
    })

    // Appended, not replaced.
    expect(result.current.items.map((r) => r.id)).toEqual(['a', 'b'])
    // Same filters, plus the token from the previous page — a token minted
    // under one filter set is rejected under another, so loadMore must carry
    // the filters load was given rather than re-deriving them.
    expect(bodies[1]).toEqual({ statuses: ['queued'], only_unclaimed: true, page_token: 'tok-1' })
  })

  it('does not page past the last page', async () => {
    const { fetchMock } = stubPages([{ items: [{ id: 'a' }], total_count: 1 }])
    const { result } = renderHook(() => usePagedList<Row>('/api/tasks/list', 'Could not load.'))

    await act(async () => {
      await result.current.load({})
    })
    expect(result.current.hasMore).toBe(false)

    await act(async () => {
      await result.current.loadMore()
    })
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('drops the previous page token when the filters change', async () => {
    const { bodies } = stubPages([
      { items: [{ id: 'a' }], next_page_token: 'tok-1', total_count: 3 },
      { items: [{ id: 'z' }], total_count: 1 },
    ])
    const { result } = renderHook(() => usePagedList<Row>('/api/tasks/list', 'Could not load.'))

    await act(async () => {
      await result.current.load({ statuses: ['queued'] })
    })
    await act(async () => {
      await result.current.load({ statuses: ['done'] })
    })

    expect(bodies[1]).toEqual({ statuses: ['done'] })
    expect(result.current.items.map((r) => r.id)).toEqual(['z'])
    expect(result.current.hasMore).toBe(false)
  })

  it('does not pair a failed load’s filters with the previous query’s token', async () => {
    // A token is only valid for the filter set it was minted under. If a
    // failed load left its filters behind next to the surviving token, the
    // next loadMore would send that mismatched pair and earn a 400 — and the
    // items on screen still belong to the query that token continues.
    const bodies: unknown[] = []
    let fail = false
    vi.stubGlobal(
      'fetch',
      vi.fn((_input: unknown, init?: { body?: string }) => {
        bodies.push(init?.body ? JSON.parse(init.body) : null)
        return fail
          ? Promise.resolve({ ok: false, status: 500, ...jsonBody({}) })
          : Promise.resolve({
              ok: true,
              ...jsonBody({ items: [{ id: 'a' }], next_page_token: 'tok-1', total_count: 3 }),
            })
      }),
    )
    const { result } = renderHook(() => usePagedList<Row>('/api/tasks/list', 'Could not load.'))

    await act(async () => {
      await result.current.load({ statuses: ['queued'] })
    })
    fail = true
    await act(async () => {
      await result.current.load({ statuses: ['done'] })
    })
    fail = false
    await act(async () => {
      await result.current.loadMore()
    })

    expect(bodies[2]).toEqual({ statuses: ['queued'], page_token: 'tok-1' })
  })

  it('keeps the current page and surfaces a message when a load fails', async () => {
    let fail = false
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        fail
          ? Promise.resolve({
              ok: false,
              status: 500,
              ...jsonBody({ errors: [{ reason: 'INTERNAL', message: 'boom' }] }),
            })
          : Promise.resolve({ ok: true, ...jsonBody({ items: [{ id: 'a' }], total_count: 1 }) }),
      ),
    )
    const { result } = renderHook(() => usePagedList<Row>('/api/tasks/list', 'Could not load.'))

    await act(async () => {
      await result.current.load({})
    })
    fail = true
    await act(async () => {
      const page = await result.current.load({})
      expect(page).toBeNull()
    })

    await waitFor(() => expect(result.current.error).toBe('boom'))
    // The stale page is still there — blanking a column on one failed poll is
    // worse than showing it a beat late.
    expect(result.current.items.map((r) => r.id)).toEqual(['a'])
  })
})
