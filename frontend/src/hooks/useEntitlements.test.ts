import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { useEntitlements, resetEntitlementsForTest } from './useEntitlements'

// useEntitlements holds a module-level cache (one fetch per page), so each test
// clears it (resetEntitlementsForTest) and stubs fetch to return the
// deployment's licensed feature list.

// stubEntitlements routes GET /api/entitlements to a canned { features } body;
// any other path resolves not-ok so an accidental call is visible.
function stubEntitlements(features: string[]) {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input).split('?')[0] === '/api/entitlements') {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ features }) })
    }
    return Promise.resolve({ ok: false, status: 404, json: () => Promise.resolve({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => {
  resetEntitlementsForTest()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useEntitlements', () => {
  it('reflects the fetched list once the probe resolves', async () => {
    stubEntitlements(['governance'])
    const { result } = renderHook(() => useEntitlements())

    // Before the fetch resolves: not loaded, nothing licensed.
    expect(result.current.loaded).toBe(false)
    expect(result.current.has('governance')).toBe(false)

    await waitFor(() => expect(result.current.loaded).toBe(true))
    // has() reflects exactly the fetched list: granted → true, anything not in
    // the list (a real but unlicensed feature, or an unknown one) → false.
    expect(result.current.has('governance')).toBe(true)
    expect(result.current.has('sso')).toBe(false)
    expect(result.current.has('nope')).toBe(false)
  })

  it('has() is false for all in a community / unlicensed build ([])', async () => {
    stubEntitlements([])
    const { result } = renderHook(() => useEntitlements())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has('governance')).toBe(false)
    expect(result.current.has('sso')).toBe(false)
  })

  it('fetches /api/entitlements once across multiple consumers', async () => {
    const fetchMock = stubEntitlements(['governance'])
    const a = renderHook(() => useEntitlements())
    const b = renderHook(() => useEntitlements())

    await waitFor(() => expect(a.result.current.loaded).toBe(true))
    await waitFor(() => expect(b.result.current.loaded).toBe(true))

    expect(b.result.current.has('governance')).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('fails closed when the probe errors (loaded, nothing licensed)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: false, status: 500, json: () => Promise.resolve({}) })),
    )
    const { result } = renderHook(() => useEntitlements())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has('governance')).toBe(false)
  })
})
