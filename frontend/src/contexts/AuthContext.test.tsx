// AuthContext's SSO_REQUIRED hook: a 403 SSO_REQUIRED from any call means an
// org now requires a sign-in this session did not use, so /api/me's per-org
// sso_required is stale and is read again. Every read on a page can fail at
// once, so the burst costs one re-read, and the 401 funnel is never involved.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { AuthProvider, useAuth } from './AuthContext'
import { apiJSON } from '../lib/apiClient'
import { jsonBody } from '../test/apiResponse'

function meCalls(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.filter(([url]) => String(url) === '/api/me').length
}

function Status() {
  const auth = useAuth()
  return <p>status {auth.status}</p>
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn((url: unknown) =>
    Promise.resolve(
      String(url) === '/api/me'
        ? { ok: true, status: 200, ...jsonBody({ id: 'u1', orgs: [] }) }
        : {
            ok: false,
            status: 403,
            ...jsonBody({ errors: [{ reason: 'SSO_REQUIRED', message: 'requires SSO' }] }),
          },
    ),
  )
  vi.stubGlobal('fetch', fetchMock)
})

describe('AuthProvider on SSO_REQUIRED', () => {
  it('re-reads /api/me once for a burst of refusals and stays signed in', async () => {
    render(
      <AuthProvider>
        <Status />
      </AuthProvider>,
    )
    expect(await screen.findByText('status authed')).toBeInTheDocument()
    expect(meCalls(fetchMock)).toBe(1)

    await Promise.allSettled([
      apiJSON('/api/teams/list'),
      apiJSON('/api/dashboard/stats'),
      apiJSON('/api/tasks/list'),
    ])

    await waitFor(() => expect(meCalls(fetchMock)).toBe(2))
    // Let a stray second re-read surface if the burst were not collapsed.
    await new Promise((r) => setTimeout(r, 20))
    expect(meCalls(fetchMock)).toBe(2)
    expect(screen.getByText('status authed')).toBeInTheDocument()
  })
})
