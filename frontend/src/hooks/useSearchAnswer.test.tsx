import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { SEARCH_DELAY_MS, useSearchAnswer } from './useSearchAnswer'

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('useSearchAnswer', () => {
  it('asks nothing for a blank needle', () => {
    const read = vi.fn(async (needle: string) => [needle])
    const { result } = renderHook(() => useSearchAnswer('   ', read))
    act(() => {
      vi.advanceTimersByTime(SEARCH_DELAY_MS * 2)
    })
    expect(result.current).toBeNull()
    expect(read).not.toHaveBeenCalled()
  })

  it('reads the trimmed needle once typing holds still, and keeps the last answer until the next lands', async () => {
    const read = vi.fn(async (needle: string) => [needle.toUpperCase()])
    const { result, rerender } = renderHook(({ q }) => useSearchAnswer(q, read), {
      initialProps: { q: ' ab ' },
    })
    expect(result.current).toEqual({ needle: 'ab', value: null, pending: true })

    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DELAY_MS)
    })
    expect(read).toHaveBeenCalledTimes(1)
    expect(read.mock.calls[0][0]).toBe('ab')
    expect(result.current).toEqual({ needle: 'ab', value: ['AB'], pending: false })

    // Two keystrokes inside the delay are one read, for the last of them.
    rerender({ q: 'abc' })
    rerender({ q: 'abcd' })
    expect(result.current).toEqual({ needle: 'ab', value: ['AB'], pending: true })
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DELAY_MS)
    })
    expect(read).toHaveBeenCalledTimes(2)
    expect(read.mock.calls[1][0]).toBe('abcd')
    expect(result.current).toEqual({ needle: 'abcd', value: ['ABCD'], pending: false })
  })

  it('aborts a read the needle moved past, and never returns its answer', async () => {
    let settle: (v: string[]) => void = () => {}
    const signals: AbortSignal[] = []
    const read = vi.fn((needle: string, signal: AbortSignal) => {
      signals.push(signal)
      return needle === 'slow'
        ? new Promise<string[]>((r) => (settle = r))
        : Promise.resolve([needle])
    })
    const { result, rerender } = renderHook(({ q }) => useSearchAnswer(q, read), {
      initialProps: { q: 'slow' },
    })
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DELAY_MS)
    })
    rerender({ q: 'fast' })
    expect(signals[0].aborted).toBe(true)
    await act(async () => {
      vi.advanceTimersByTime(SEARCH_DELAY_MS)
    })
    await act(async () => {
      settle(['SLOW'])
    })
    expect(result.current).toEqual({ needle: 'fast', value: ['fast'], pending: false })
  })
})
