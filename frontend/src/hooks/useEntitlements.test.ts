import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { useEntitlements, resetEntitlementsForTest, FeatureGovernance } from './useEntitlements'
import { jsonBody } from '../test/apiResponse'

// useEntitlements reads the org useApiOrgId names; the mock lets a test pick
// that org and switch it.
const org = vi.hoisted(() => ({ id: 'org-a' as string | null }))
vi.mock('./useApiOrgId', () => ({ useApiOrgId: () => org.id }))

const pathOf = (orgId: string) => `/api/orgs/${orgId}/entitlements`

// useEntitlements holds a module-level cache (one fetch per org), so each test
// clears it (resetEntitlementsForTest) and stubs fetch to return each org's
// licensed feature list.

// stubEntitlements routes each org's entitlements GET to a canned { features }
// body; any other path resolves not-ok so an accidental call is visible.
function stubEntitlements(byOrg: Record<string, string[]>) {
  const fetchMock = vi.fn((input: unknown) => {
    const path = String(input).split('?')[0]
    for (const [orgId, features] of Object.entries(byOrg)) {
      if (path === pathOf(orgId)) {
        return Promise.resolve({ ok: true, ...jsonBody({ features }) })
      }
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => {
  resetEntitlementsForTest()
  org.id = 'org-a'
})

describe('useEntitlements', () => {
  it('reflects the fetched list once the probe resolves', async () => {
    stubEntitlements({ 'org-a': ['governance'] })
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
    stubEntitlements({ 'org-a': [] })
    const { result } = renderHook(() => useEntitlements())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has('governance')).toBe(false)
    expect(result.current.has('sso')).toBe(false)
  })

  it('fetches an org once across multiple consumers', async () => {
    const fetchMock = stubEntitlements({ 'org-a': ['governance'] })
    const a = renderHook(() => useEntitlements())
    const b = renderHook(() => useEntitlements())

    await waitFor(() => expect(a.result.current.loaded).toBe(true))
    await waitFor(() => expect(b.result.current.loaded).toBe(true))

    expect(b.result.current.has('governance')).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it("reads the new org's feature set after an org switch", async () => {
    // Org A licenses governance and org B does not; switching to B must show
    // B's empty set, read from B's own route.
    const fetchMock = stubEntitlements({ 'org-a': ['governance'], 'org-b': [] })
    const { result, rerender } = renderHook(() => useEntitlements())
    await waitFor(() => expect(result.current.has('governance')).toBe(true))

    org.id = 'org-b'
    rerender()
    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has('governance')).toBe(false)
    expect(fetchMock).toHaveBeenCalledWith(pathOf('org-b'), expect.anything())
  })

  it("keeps a previous org's slow answer out of the new org's set", async () => {
    // Org A's probe is slow; we hold its resolver so it lands AFTER the switch
    // to org B. Its governance set must not show for B.
    let landA!: () => void
    const fetchMock = vi.fn((input: unknown) => {
      if (String(input) === pathOf('org-a')) {
        return new Promise((resolve) => {
          landA = () => resolve({ ok: true, ...jsonBody({ features: ['governance'] }) })
        })
      }
      return Promise.resolve({ ok: true, ...jsonBody({ features: [] }) })
    })
    vi.stubGlobal('fetch', fetchMock)

    const { result, rerender } = renderHook(() => useEntitlements())
    expect(result.current.loaded).toBe(false) // org A's fetch is still in flight

    org.id = 'org-b'
    rerender()
    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.has(FeatureGovernance)).toBe(false) // org B

    landA()
    await new Promise((r) => setTimeout(r, 0)) // flush org A's chain
    expect(result.current.has(FeatureGovernance)).toBe(false)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('holds its fetch while the org is unresolved', async () => {
    org.id = null
    const fetchMock = stubEntitlements({})
    const { result } = renderHook(() => useEntitlements())

    await new Promise((r) => setTimeout(r, 0))
    expect(result.current.loaded).toBe(false)
    expect(fetchMock).not.toHaveBeenCalled()
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
