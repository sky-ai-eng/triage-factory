// AuthContext's two /api/me re-reads. refresh is for a caller that needs the
// answer now, after a write or from a Retry button, and reports a failure.
// revalidate is the background re-read — a timer, a focus event, a 403
// SSO_REQUIRED from another call, a websocket kick — and a failure other than
// 401 keeps the session already loaded, so a transient outage never replaces
// a working page (the SSO interstitial included) with the error screen.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import { AuthProvider, useAuth } from './AuthContext'
import { OrgProvider } from './OrgContext'
import AuthGate from '../AuthGate'
import { apiJSON } from '../lib/apiClient'
import { jsonBody } from '../test/apiResponse'
import type { AuthOrg, MeResponse } from '../types'

const barred: AuthOrg = {
  id: 'org-x',
  name: 'Acme',
  role: 'member',
  sso_required: true,
  sso_start_url: '/api/auth/oauth/saml?provider_id=p&return_to=%2F',
}

function me(orgs: AuthOrg[]): MeResponse {
  return {
    id: 'u1',
    orgs,
    teams: [],
    org_creation_enabled: true,
    can_create_org: true,
    is_operator: false,
  }
}

// The /api/me answer the stub serves next: a body, or a failure status, or a
// network error. Every other path answers 403 SSO_REQUIRED.
let meAnswer: MeResponse | number | 'network'
let fetchMock: ReturnType<typeof vi.fn>

function meCalls() {
  return fetchMock.mock.calls.filter(([url]) => String(url) === '/api/me').length
}

beforeEach(() => {
  meAnswer = me([])
  fetchMock = vi.fn((url: unknown) => {
    if (String(url) !== '/api/me') {
      return Promise.resolve({
        ok: false,
        status: 403,
        ...jsonBody({ errors: [{ reason: 'SSO_REQUIRED', message: 'requires SSO' }] }),
      })
    }
    if (meAnswer === 'network') return Promise.reject(new TypeError('Failed to fetch'))
    if (typeof meAnswer === 'number') {
      return Promise.resolve({ ok: false, status: meAnswer, ...jsonBody({ errors: [] }) })
    }
    return Promise.resolve({ ok: true, status: 200, ...jsonBody(meAnswer) })
  })
  vi.stubGlobal('fetch', fetchMock)
  window.localStorage.clear()
})

function Status() {
  const auth = useAuth()
  return (
    <>
      <p>status {auth.status}</p>
      <p>orgs {auth.orgs.length}</p>
      <button type="button" onClick={() => void auth.refresh()}>
        refresh
      </button>
      <button type="button" onClick={() => void auth.revalidate()}>
        revalidate
      </button>
    </>
  )
}

function renderStatus() {
  return render(
    <AuthProvider>
      <Status />
    </AuthProvider>,
  )
}

describe('AuthProvider re-reads', () => {
  it('keeps the loaded session when a background re-read fails', async () => {
    meAnswer = me([barred])
    renderStatus()
    expect(await screen.findByText('status authed')).toBeInTheDocument()

    for (const failure of [503, 'network'] as const) {
      meAnswer = failure
      const before = meCalls()
      fireEvent.click(screen.getByRole('button', { name: 'revalidate' }))
      await waitFor(() => expect(meCalls()).toBe(before + 1))
      await new Promise((r) => setTimeout(r, 0))
      expect(screen.getByText('status authed')).toBeInTheDocument()
      expect(screen.getByText('orgs 1')).toBeInTheDocument()
    }
  })

  it('still signs out when a background re-read answers 401', async () => {
    renderStatus()
    expect(await screen.findByText('status authed')).toBeInTheDocument()

    meAnswer = 401
    fireEvent.click(screen.getByRole('button', { name: 'revalidate' }))
    expect(await screen.findByText('status unauth')).toBeInTheDocument()
  })

  it('reports a failed refresh, which a caller asks for when it needs the answer now', async () => {
    renderStatus()
    expect(await screen.findByText('status authed')).toBeInTheDocument()

    meAnswer = 503
    fireEvent.click(screen.getByRole('button', { name: 'refresh' }))
    expect(await screen.findByText('status error')).toBeInTheDocument()
  })

  it('re-reads /api/me once for a burst of SSO_REQUIRED refusals', async () => {
    renderStatus()
    expect(await screen.findByText('status authed')).toBeInTheDocument()
    expect(meCalls()).toBe(1)

    await Promise.allSettled([
      apiJSON('/api/teams/list'),
      apiJSON('/api/dashboard/stats'),
      apiJSON('/api/tasks/list'),
    ])

    await waitFor(() => expect(meCalls()).toBe(2))
    // Let a stray second re-read surface if the burst were not collapsed.
    await new Promise((r) => setTimeout(r, 20))
    expect(meCalls()).toBe(2)
    expect(screen.getByText('status authed')).toBeInTheDocument()
  })

  it('keeps the page when an SSO_REQUIRED re-read meets an outage', async () => {
    renderStatus()
    expect(await screen.findByText('status authed')).toBeInTheDocument()

    meAnswer = 503
    await Promise.allSettled([apiJSON('/api/teams/list')])
    await waitFor(() => expect(meCalls()).toBe(2))
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.getByText('status authed')).toBeInTheDocument()
  })
})

describe('the SSO interstitial through an outage', () => {
  it('stays up while /api/me fails, and gives way to the page once it answers', async () => {
    meAnswer = me([barred])
    render(
      <MemoryRouter initialEntries={['/orgs/org-x/board']}>
        <AuthProvider>
          <OrgProvider>
            <Routes>
              <Route
                path="/orgs/:org_id/*"
                element={
                  <AuthGate mode="multi">
                    <p>the page</p>
                  </AuthGate>
                }
              />
            </Routes>
          </OrgProvider>
        </AuthProvider>
      </MemoryRouter>,
    )
    expect(await screen.findByText('This organization requires single sign-on')).toBeInTheDocument()

    meAnswer = 'network'
    const before = meCalls()
    fireEvent.focus(window)
    await waitFor(() => expect(meCalls()).toBe(before + 1))
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.getByText('This organization requires single sign-on')).toBeInTheDocument()
    expect(screen.queryByText('Retry')).not.toBeInTheDocument()

    meAnswer = me([{ ...barred, sso_required: false, sso_start_url: undefined }])
    fireEvent.focus(window)
    expect(await screen.findByText('the page')).toBeInTheDocument()
  })
})
