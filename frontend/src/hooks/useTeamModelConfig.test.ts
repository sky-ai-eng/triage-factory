import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'

// What this file pins is the hook's landing discipline: every response is
// written only if it still answers the question being asked. The page tests
// cover what the panel sends; here it is about what a late answer may touch.

const api = vi.hoisted(() => ({ apiJSON: vi.fn() }))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, ...api }
})

import { useTeamModelConfig } from './useTeamModelConfig'

function resource(defaultModel: string, enabled: string[] | null) {
  return { team_settings: { DefaultModel: defaultModel, EnabledModels: enabled } }
}

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

beforeEach(() => {
  api.apiJSON.mockReset()
})

describe('useTeamModelConfig', () => {
  it('keys the answer to the team it answers for — a switch reads null, then the new team', async () => {
    api.apiJSON.mockImplementation((path: string) =>
      Promise.resolve(path.includes('/t1/') ? resource('opus', ['opus']) : resource('haiku', null)),
    )
    const { result, rerender } = renderHook(({ team }) => useTeamModelConfig(team), {
      initialProps: { team: 't1' },
    })
    await waitFor(() => expect(result.current.config?.defaultModel).toBe('opus'))

    rerender({ team: 't2' })
    // Stale data stops matching immediately — no frame renders t1's choices
    // as t2's.
    expect(result.current.config).toBeNull()
    await waitFor(() => expect(result.current.config?.defaultModel).toBe('haiku'))
    expect(result.current.config?.enabledModels).toBeNull()
  })

  it('discards a save that lands after the user left its team', async () => {
    const t1Get = deferred<unknown>()
    const t2Get = deferred<unknown>()
    const patch = deferred<unknown>()
    api.apiJSON.mockImplementation((path: string, init?: RequestInit) => {
      if (init?.method === 'PATCH') return patch.promise
      return path.includes('/t1/') ? t1Get.promise : t2Get.promise
    })
    const { result, rerender } = renderHook(({ team }) => useTeamModelConfig(team), {
      initialProps: { team: 't1' },
    })
    await act(async () => t1Get.resolve(resource('opus', ['opus'])))
    await waitFor(() => expect(result.current.config?.defaultModel).toBe('opus'))

    // A save leaves for t1, and the user navigates before it answers.
    act(() => {
      void result.current.save({ ai_model: 'sonnet' }).catch(() => {})
    })
    rerender({ team: 't2' })
    await act(async () => t2Get.resolve(resource('haiku', null)))
    await waitFor(() => expect(result.current.config?.defaultModel).toBe('haiku'))

    // t1's answer arrives late and touches nothing.
    await act(async () => patch.resolve(resource('sonnet', ['opus', 'sonnet'])))
    expect(result.current.config?.defaultModel).toBe('haiku')

    // And returning to t1 re-reads rather than showing the orphaned write.
    const t1Again = deferred<unknown>()
    api.apiJSON.mockImplementation(() => t1Again.promise)
    rerender({ team: 't1' })
    expect(result.current.config).toBeNull()
    await act(async () => t1Again.resolve(resource('sonnet', ['opus', 'sonnet'])))
    await waitFor(() => expect(result.current.config?.defaultModel).toBe('sonnet'))
  })

  it('applies optimistically and reverts to the last server truth when the save fails', async () => {
    api.apiJSON.mockImplementation((_path: string, init?: RequestInit) =>
      init?.method === 'PATCH'
        ? Promise.reject(new Error('refused'))
        : Promise.resolve(resource('opus', ['opus', 'sonnet'])),
    )
    const { result } = renderHook(() => useTeamModelConfig('t1'))
    await waitFor(() => expect(result.current.config?.defaultModel).toBe('opus'))

    let refused: unknown
    await act(async () => {
      await result.current.save({ ai_model: 'sonnet' }).catch((e) => {
        refused = e
      })
    })
    // The failure is rethrown for the caller to phrase, and the optimistic
    // frame is taken back.
    expect(refused).toBeInstanceOf(Error)
    expect(result.current.config?.defaultModel).toBe('opus')
  })
})
