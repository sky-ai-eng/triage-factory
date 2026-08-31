import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useConversationSets, useTranscriptTick } from './hooks'
import { ACTIVE_STATUSES } from '../../lib/conversationStatus'
import type { Message, WSEvent } from '../../types'
import { jsonBody } from '../../test/apiResponse'

// The ticks arrive through the singleton websocket. Mocking the module hands
// the test every handler the hooks registered, so frames can be dispatched
// synchronously with no socket in play. An array, not a single slot: a
// re-render registers again, and every copy shares the one debounce timer.
const handlers: Array<(event: WSEvent) => void> = []
vi.mock('../../hooks/useWebSocket', () => ({
  useWebSocket: (handler: (event: WSEvent) => void) => {
    handlers.push(handler)
  },
}))

function dispatch(event: WSEvent) {
  for (const fn of [...handlers]) fn(event)
}

/** One streamed transcript row, in the wire shape broadcastMessage sends. */
function row(over: Partial<Message>): WSEvent {
  const data: Message = {
    id: 1,
    conversation_id: 'c1',
    role: 'assistant',
    content: '',
    subtype: '',
    tool_calls: [{ id: 'call-1', name: 'Bash', input: { command: 'go test ./...' } }],
    created_at: '2026-08-31T12:00:00Z',
    ...over,
  }
  return { type: 'message', conversation_id: 'c1', data }
}

let bodies: Array<Record<string, unknown>> = []

beforeEach(() => {
  vi.useFakeTimers()
  handlers.length = 0
  bodies = []
  vi.stubGlobal(
    'fetch',
    vi.fn((_input: unknown, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body ?? '{}')) as Record<string, unknown>)
      return Promise.resolve({ ok: true, ...jsonBody({ runs: {}, total_count: 0 }) })
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

describe('useTranscriptTick', () => {
  it('coalesces a burst of assistant rows into one bump per window', async () => {
    const { result } = renderHook(() => useTranscriptTick())
    expect(result.current).toBe(0)

    act(() => {
      dispatch(row({ id: 1 }))
      dispatch(row({ id: 2 }))
      dispatch(row({ id: 3 }))
    })
    // The window is open, not elapsed.
    expect(result.current).toBe(0)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000)
    })
    expect(result.current).toBe(1)

    // The next row opens the next window — armed by a match, never by expiry.
    act(() => {
      dispatch(row({ id: 4 }))
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000)
    })
    expect(result.current).toBe(2)
  })

  it('bumps on a text-only assistant row: the line honestly falls back to Working', async () => {
    const { result } = renderHook(() => useTranscriptTick())
    act(() => {
      dispatch(row({ content: 'Now the tests pass, so I will push.', tool_calls: undefined }))
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000)
    })
    expect(result.current).toBe(1)
  })

  it('skips rows no assistant wrote, and frames that are not messages', async () => {
    const { result } = renderHook(() => useTranscriptTick())
    act(() => {
      dispatch(row({ role: 'user', tool_calls: undefined }))
      dispatch(row({ role: 'tool', tool_call_id: 'call-1', tool_calls: undefined }))
      dispatch({ type: 'tasks_updated', data: {} })
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000)
    })
    expect(result.current).toBe(0)
  })

  it('does not poll: a quiet stream never bumps', async () => {
    const { result } = renderHook(() => useTranscriptTick())
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000)
    })
    expect(result.current).toBe(0)
  })
})

describe('useConversationSets', () => {
  const mountSets = () =>
    renderHook(
      ({ tick, transcriptTick }: { tick: number; transcriptTick: number }) =>
        useConversationSets('t1', tick, transcriptTick),
      { initialProps: { tick: 0, transcriptTick: 0 } },
    )

  const runningBodies = () => bodies.filter((b) => Array.isArray(b.statuses))
  const needsBodies = () => bodies.filter((b) => b.attention === true)

  it('refetches only the running read on a transcript tick', async () => {
    const { rerender } = mountSets()
    await settle()
    expect(needsBodies()).toHaveLength(1)
    expect(runningBodies()).toHaveLength(1)

    rerender({ tick: 0, transcriptTick: 1 })
    await settle()
    // A transcript row moves current_action and nothing in the attention set.
    expect(needsBodies()).toHaveLength(1)
    expect(runningBodies()).toHaveLength(2)
    expect(runningBodies()[1]).toEqual({
      team_ids: ['t1'],
      statuses: [...ACTIVE_STATUSES, 'queued'],
      page_size: 20,
    })
  })

  it('refetches both reads on the page tick', async () => {
    const { rerender } = mountSets()
    await settle()

    rerender({ tick: 1, transcriptTick: 0 })
    await settle()
    expect(needsBodies()).toHaveLength(2)
    expect(runningBodies()).toHaveLength(2)
  })
})
