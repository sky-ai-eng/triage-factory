import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useRailCounts } from './useRailCounts'
import { ACTIVE_STATUSES } from '../lib/conversationStatus'
import type { WSEvent } from '../types'
import { jsonBody } from '../test/apiResponse'

// The hook's refetch trigger arrives through the singleton websocket. Mocking
// the module hands the test the handler the hook registered, so hints can be
// dispatched synchronously with no socket in play.
let dispatch: ((event: WSEvent) => void) | null = null
vi.mock('./useWebSocket', () => ({
  useWebSocket: (handler: (event: WSEvent) => void) => {
    dispatch = handler
  },
}))

const CONVERSATIONS = '/api/agent/conversations/list'
const TASKS = '/api/tasks/list'

/** The count each route answers with, mutable so a test can move the server's
 *  answer and then fire a hint. */
let totals: Record<string, number> = {}
let bodies: Array<{ path: string; body: Record<string, unknown> }> = []

/** A count-only page: `total_count` under the filters, and no rows. The
 *  conversations route answers with its grouped envelope rather than `items`,
 *  which is exactly why the counts read total_count and never a length. */
function countPage(total: number, path: string) {
  return path === CONVERSATIONS
    ? jsonBody({ runs: {}, total_count: total })
    : jsonBody({ items: [], next_page_token: '', total_count: total })
}

/** Which of the two conversation reads a body is: they share a path and are
 *  told apart by the filter they carry, same as the server tells them apart. */
function conversationKey(body: Record<string, unknown>): string {
  return body.attention === true ? 'needs' : 'running'
}

beforeEach(() => {
  vi.useFakeTimers()
  dispatch = null
  bodies = []
  totals = { needs: 2, running: 1, queued: 5 }
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const path = String(input)
      const body = JSON.parse(String(init?.body ?? '{}')) as Record<string, unknown>
      bodies.push({ path, body })
      const key = path === TASKS ? 'queued' : conversationKey(body)
      return Promise.resolve({ ok: true, ...countPage(totals[key], path) })
    }),
  )
})

afterEach(() => {
  vi.useRealTimers()
})

/** Drains the microtask queue past the settled reads — apiJSON awaits `text()`
 *  and then parses, so an assertion made before that has observed nothing. */
async function settle() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
}

function bodyFor(path: string, key: string) {
  return bodies.find((b) => b.path === path && (path === TASKS || conversationKey(b.body) === key))
    ?.body
}

describe('useRailCounts', () => {
  it('asks each set for its count and nothing else', async () => {
    const { result } = renderHook(() => useRailCounts())
    await settle()

    // running: the live display statuses — `running` plus every claim phase,
    // taken from the vocabulary a Go test pins to the backend's.
    expect(bodyFor(CONVERSATIONS, 'running')).toEqual({
      statuses: [...ACTIVE_STATUSES],
      page_size: 0,
    })
    // needs: the server-side attention predicate.
    expect(bodyFor(CONVERSATIONS, 'needs')).toEqual({ attention: true, page_size: 0 })
    // queued: the Queued column's own filter set, page removed.
    expect(bodyFor(TASKS, 'queued')).toEqual({
      statuses: ['queued'],
      team_ids: [],
      only_unclaimed: true,
      include_snoozed: false,
      page_size: 0,
    })

    expect(result.current).toEqual({ needs: 2, running: 1, queued: 5 })
  })

  it('claims nothing before an answer: null is not zero', async () => {
    const { result } = renderHook(() => useRailCounts())
    expect(result.current).toEqual({ needs: null, running: null, queued: null })
    await settle()
    expect(result.current.needs).toBe(2)
  })

  it('answers a real zero once the read lands', async () => {
    totals = { needs: 0, running: 0, queued: 0 }
    const { result } = renderHook(() => useRailCounts())
    await settle()
    expect(result.current).toEqual({ needs: 0, running: 0, queued: 0 })
  })

  it('coalesces a burst of hints into one round of reads', async () => {
    renderHook(() => useRailCounts())
    await settle()
    expect(bodies).toHaveLength(3)

    totals = { needs: 4, running: 2, queued: 1 }
    act(() => {
      dispatch?.({
        type: 'conversation_update',
        conversation_id: 'c1',
        data: { status: 'running' },
      })
      dispatch?.({ type: 'task_updated', data: { task_id: 't1', status: 'in_progress' } })
      dispatch?.({ type: 'permission_request', conversation_id: 'c1', data: { tool_call_id: 'x' } })
    })
    // Still nothing in flight: the window is open, not elapsed.
    expect(bodies).toHaveLength(3)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000)
    })
    await settle()
    expect(bodies).toHaveLength(6)
  })

  it('refetches on every hint that can move one of the three sets', async () => {
    const hints: WSEvent[] = [
      { type: 'tasks_updated', data: {} },
      { type: 'task_updated', data: { task_id: 't', status: 'done' } },
      {
        type: 'task_claimed',
        data: { task_id: 't', claimed_by_agent_id: '', claimed_by_user_id: 'u' },
      },
      { type: 'conversation_update', conversation_id: 'c', data: { status: 'completed' } },
      { type: 'permission_request', conversation_id: 'c', data: { tool_call_id: 'x' } },
      { type: 'permission_resolved', conversation_id: 'c', data: { tool_call_id: 'x' } },
      { type: 'conversations_updated', data: {} },
    ]
    renderHook(() => useRailCounts())
    await settle()

    for (const [i, hint] of hints.entries()) {
      act(() => {
        dispatch?.(hint)
      })
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1000)
      })
      await settle()
      expect(bodies, `hint ${hint.type} did not refetch`).toHaveLength(3 + 3 * (i + 1))
    }
  })

  it('ignores a frame that cannot move any of the three sets', async () => {
    renderHook(() => useRailCounts())
    await settle()
    act(() => {
      dispatch?.({ type: 'scoring_started', data: { task_ids: ['t'] } })
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(bodies).toHaveLength(3)
  })

  it('keeps a count it has through a failed refetch — stale, never wrong', async () => {
    const { result } = renderHook(() => useRailCounts())
    await settle()
    expect(result.current.queued).toBe(5)

    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: false, status: 500, ...jsonBody({}) })),
    )
    act(() => {
      dispatch?.({ type: 'tasks_updated', data: {} })
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000)
    })
    await settle()
    expect(result.current.queued).toBe(5)
  })

  it('does not poll: a quiet stream fires no further reads', async () => {
    renderHook(() => useRailCounts())
    await settle()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000)
    })
    expect(bodies).toHaveLength(3)
  })
})
