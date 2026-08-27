import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import {
  useEntitlements,
  invalidateEntitlements,
  resetEntitlementsForTest,
  FeatureGovernance,
} from './useEntitlements'
import { jsonBody } from '../test/apiResponse'

// useEntitlements holds a module-level cache (one fetch per page), so each test
// clears it (resetEntitlementsForTest) and stubs fetch to return the
// deployment's licensed feature list.

// stubEntitlements routes GET /api/entitlements to a canned { features } body;
// any other path resolves not-ok so an accidental call is visible.
function stubEntitlements(features: string[]) {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input).split('?')[0] === '/api/entitlements') {
      return Promise.resolve({ ok: true, ...jsonBody({ features }) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => {
  resetEntitlementsForTest()
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

  it('refetches the new org feature set on invalidate (org switch)', async () => {
    // Org A licenses governance; after switching to org B (which does not), an
    // invalidate must drop the stale set and reflect B's empty entitlements.
    let features = ['governance']
    const fetchMock = vi.fn((input: unknown) => {
      if (String(input).split('?')[0] === '/api/entitlements') {
        return Promise.resolve({ ok: true, ...jsonBody({ features }) })
      }
      return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
    })
    vi.stubGlobal('fetch', fetchMock)

    const { result } = renderHook(() => useEntitlements())
    await waitFor(() => expect(result.current.has('governance')).toBe(true))

    features = [] // org B
    invalidateEntitlements()
    await waitFor(() => expect(result.current.has('governance')).toBe(false))
    expect(result.current.loaded).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('drops a previous org slow response that lands after an org switch', async () => {
    // The generation guard's reason for existing: org A's probe is slow; we hold
    // its resolver so it lands AFTER we switch to org B. Without the guard, A's
    // stale governance set would clobber B's empty one.
    let landA!: () => void
    const aResponse = { ok: true, ...jsonBody({ features: ['governance'] }) }
    let call = 0
    const fetchMock = vi.fn(() => {
      call += 1
      if (call === 1) {
        return new Promise((resolve) => {
          landA = () => resolve(aResponse)
        })
      }
      // Org B resolves immediately with no entitlements.
      return Promise.resolve({ ok: true, ...jsonBody({ features: [] }) })
    })
    vi.stubGlobal('fetch', fetchMock)

    const { result } = renderHook(() => useEntitlements())
    expect(result.current.loaded).toBe(false) // org A's fetch is still in flight

    // Switch orgs: invalidate supersedes A (bumps the generation) and dispatches
    // org B's fetch, which resolves to an empty set.
    invalidateEntitlements()
    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has(FeatureGovernance)).toBe(false) // org B

    // Now A's slow response finally lands. The generation guard must drop it, so
    // org B's empty set stays put — no governance bleed-through from the old org.
    landA()
    await new Promise((r) => setTimeout(r, 0)) // flush the superseded chain
    expect(result.current.has(FeatureGovernance)).toBe(false)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('fails closed when the probe errors (loaded, nothing licensed)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: false, status: 500, ...jsonBody({}) })),
    )
    const { result } = renderHook(() => useEntitlements())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has('governance')).toBe(false)
  })
})
