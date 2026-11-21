import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { useAuthStatus } from './useAuthStatus'
import { jsonBody } from '../test/apiResponse'

const pathOf = (orgId: string) => `/api/orgs/${orgId}/integrations/status`

// stubStatus answers each org's status read from byOrg; an org with no entry
// fails, and an org mapped to 'pending' never answers, so a test can hold it.
function stubStatus(byOrg: Record<string, object | 'pending'>) {
  const fetchMock = vi.fn((input: unknown) => {
    const path = String(input)
    for (const [orgId, body] of Object.entries(byOrg)) {
      if (path === pathOf(orgId)) {
        if (body === 'pending') return new Promise(() => {})
        return Promise.resolve({ ok: true, ...jsonBody(body) })
      }
    }
    return Promise.resolve({ ok: false, status: 500, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const complete = { configured: true, github: true, jira: false, setup_complete: true }
const incomplete = { configured: true, github: false, jira: false, setup_complete: false }

describe('useAuthStatus', () => {
  it('holds its fetch and reports loading while the org is unresolved', async () => {
    const fetchMock = stubStatus({})
    const { result } = renderHook(() => useAuthStatus(null))

    await new Promise((r) => setTimeout(r, 0))
    expect(result.current.loading).toBe(true)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it("reports loading after an org switch until the new org's read lands", async () => {
    stubStatus({ 'org-a': incomplete, 'org-b': 'pending' })
    const { result, rerender } = renderHook(({ orgId }) => useAuthStatus(orgId), {
      initialProps: { orgId: 'org-a' },
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.setup_complete).toBe(false)

    // Org A's incomplete setup must not reach a gate deciding for org B.
    rerender({ orgId: 'org-b' })
    expect(result.current.loading).toBe(true)
    expect(result.current.setup_complete).toBeUndefined()
  })

  it("reads the new org's status after a switch", async () => {
    stubStatus({ 'org-a': incomplete, 'org-b': complete })
    const { result, rerender } = renderHook(({ orgId }) => useAuthStatus(orgId), {
      initialProps: { orgId: 'org-a' },
    })
    await waitFor(() => expect(result.current.loading).toBe(false))

    rerender({ orgId: 'org-b' })
    await waitFor(() => expect(result.current.setup_complete).toBe(true))
    expect(result.current.loading).toBe(false)
  })

  it("does not answer for an org whose read failed with another org's status", async () => {
    stubStatus({ 'org-a': complete })
    const { result, rerender } = renderHook(({ orgId }) => useAuthStatus(orgId), {
      initialProps: { orgId: 'org-a' },
    })
    await waitFor(() => expect(result.current.setup_complete).toBe(true))

    rerender({ orgId: 'org-b' })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.configured).toBe(false)
    expect(result.current.setup_complete).toBeUndefined()
  })
})
