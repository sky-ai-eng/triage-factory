import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { usePermissionQueues } from './usePermissionQueues'
import { jsonBody } from '../test/apiResponse'

// The queue is a projection of GET /api/agent/conversations/{id}/permissions,
// so what matters here is how it reacts to the server's answers — including the
// non-answer. A prompt that is live but momentarily unreadable must not
// disappear from the UI, because a prompt nobody can see is the exact failure
// this endpoint exists to close.

const CONVERSATION = 'conv-1'

function prompt(toolCallID: string, extra: Record<string, unknown> = {}) {
  return {
    tool_call_id: toolCallID,
    tool_name: 'Bash',
    input: { command: 'rm -rf ./build' },
    timeout_ms: 120_000,
    ...extra,
  }
}

// stubPermissions routes the pending-set read to a queue of canned responses,
// one per call, so a test can script "ok, then failure, then ok".
function stubPermissions(responses: Array<{ ok?: boolean; body?: unknown; throws?: boolean }>) {
  let i = 0
  const fetchMock = vi.fn(() => {
    const r = responses[Math.min(i, responses.length - 1)]
    i++
    if (r.throws) return Promise.reject(new Error('network down'))
    return Promise.resolve({
      ok: r.ok,
      status: r.ok ? 200 : 500,
      ...jsonBody(r.body),
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('usePermissionQueues', () => {
  it('sources a conversation’s prompts from the endpoint', async () => {
    stubPermissions([{ ok: true, body: [prompt('toolu_1')] }])
    const { result } = renderHook(() => usePermissionQueues())

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))
    expect(result.current.queues[CONVERSATION][0].tool_call_id).toBe('toolu_1')
  })

  it('clears a conversation’s queue when the server says nothing is pending', async () => {
    stubPermissions([
      { ok: true, body: [prompt('toolu_1')] },
      { ok: true, body: [] },
    ])
    const { result } = renderHook(() => usePermissionQueues())

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(result.current.queues[CONVERSATION]).toBeUndefined())
  })

  it('keeps a live prompt visible when the read fails', async () => {
    // A 500 and a network drop say nothing about what is pending. Treating
    // either as "nothing pending" would blank a prompt that is still parking an
    // agent, and it would stay blank until some later frame happened to arrive.
    stubPermissions([
      { ok: true, body: [prompt('toolu_1')] },
      { ok: false },
      { throws: true },
      { ok: true, body: [prompt('toolu_2')] },
    ])
    const { result } = renderHook(() => usePermissionQueues())

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))

    act(() => result.current.refresh(CONVERSATION)) // 500
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2))
    expect(result.current.queues[CONVERSATION]).toHaveLength(1)

    act(() => result.current.refresh(CONVERSATION)) // network failure
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(3))
    expect(result.current.queues[CONVERSATION]).toHaveLength(1)

    // A later successful read must still reconcile — and it returns a DIFFERENT
    // set so this can't pass by the hook having wedged. "Queue unchanged"
    // is what a skipped failure and a thrown one both look like; only a
    // subsequent update distinguishes them.
    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() =>
      expect(result.current.queues[CONVERSATION]?.[0].tool_call_id).toBe('toolu_2'),
    )
  })

  it('tolerates a payload with no input rather than throwing', async () => {
    // The server always sends an object, but the response is cast rather than
    // validated — so the boundary normalizes instead of trusting it. Consumers
    // index into `input` directly and would throw on undefined.
    stubPermissions([{ ok: true, body: [{ tool_call_id: 'toolu_1', tool_name: 'Bash' }] }])
    const { result } = renderHook(() => usePermissionQueues())

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))
    expect(result.current.queues[CONVERSATION][0].input).toEqual({})
  })

  it('ignores a body that is not a list', async () => {
    stubPermissions([{ ok: true, body: { error: 'nope' } }])
    const { result } = renderHook(() => usePermissionQueues())

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    expect(result.current.queues[CONVERSATION]).toBeUndefined()
  })

  // A prompt's TTL is the client's clock, and the client's clock is not the
  // server's: across a system suspend the two can disagree by minutes. So an
  // expired TTL asks the server instead of dropping the prompt on its own say.
  describe('when a prompt’s TTL expires', () => {
    beforeEach(() => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
    })
    afterEach(() => {
      vi.useRealTimers()
    })

    // expire runs a prompt's 1s deadline plus the client's 5s grace out.
    async function expire() {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(6_000)
      })
    }

    it('keeps a prompt the server still lists and re-arms it from the new deadline', async () => {
      stubPermissions([
        { ok: true, body: [prompt('toolu_1', { timeout_ms: 1_000 })] },
        // The server is still waiting — it slept through part of the window
        // the client counted — and reports how long it has left.
        { ok: true, body: [prompt('toolu_1', { timeout_ms: 120_000 })] },
      ])
      const { result } = renderHook(() => usePermissionQueues())

      act(() => result.current.refresh(CONVERSATION))
      await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))

      await expire()
      await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2))
      expect(result.current.queues[CONVERSATION]).toHaveLength(1)

      // Re-armed from the 120s the server reported, not the 1s it first said.
      await expire()
      expect(fetch).toHaveBeenCalledTimes(2)
      expect(result.current.queues[CONVERSATION]).toHaveLength(1)
    })

    it('drops a prompt the server no longer lists', async () => {
      stubPermissions([
        { ok: true, body: [prompt('toolu_1', { timeout_ms: 1_000 })] },
        { ok: true, body: [] },
      ])
      const { result } = renderHook(() => usePermissionQueues())

      act(() => result.current.refresh(CONVERSATION))
      await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))

      await expire()
      await waitFor(() => expect(result.current.queues[CONVERSATION]).toBeUndefined())
    })

    it('drops the prompt when the re-check cannot be answered', async () => {
      // Its deadline has passed and nothing can say otherwise, so it goes,
      // rather than lingering in the dock with no timer left to clear it.
      stubPermissions([
        { ok: true, body: [prompt('toolu_1', { timeout_ms: 1_000 })] },
        { ok: false },
      ])
      const { result } = renderHook(() => usePermissionQueues())

      act(() => result.current.refresh(CONVERSATION))
      await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))

      await expire()
      await waitFor(() => expect(result.current.queues[CONVERSATION]).toBeUndefined())
    })
  })

  it('drops a conversation’s queue on dropConversation', async () => {
    stubPermissions([{ ok: true, body: [prompt('toolu_1')] }])
    const { result } = renderHook(() => usePermissionQueues())

    act(() => result.current.refresh(CONVERSATION))
    await waitFor(() => expect(result.current.queues[CONVERSATION]).toHaveLength(1))

    act(() => result.current.dropConversation(CONVERSATION))
    expect(result.current.queues[CONVERSATION]).toBeUndefined()
  })
})
