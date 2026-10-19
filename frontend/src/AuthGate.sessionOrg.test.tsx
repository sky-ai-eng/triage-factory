// MultiAuthGate and the session's org. Routes with no org in their path act in
// the session's org rather than the URL's, so a session left in an org that
// requires SSO breaks every such read on another org's page while /api/me
// calls that page usable. What's pinned is that the gate moves the session to
// the org on screen before rendering its page, only when the session's org is
// barred, and attempts it once.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { useEffect } from 'react'
import { MemoryRouter, Route, Routes } from 'react-router'
import AuthGate from './AuthGate'
import { AuthProvider } from './contexts/AuthContext'
import { OrgProvider } from './contexts/OrgContext'
import { reconnectWebSocket } from './hooks/useWebSocket'
import { jsonBody } from './test/apiResponse'
import type { AuthOrg, MeResponse } from './types'

vi.mock('./hooks/useWebSocket', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./hooks/useWebSocket')>()),
  reconnectWebSocket: vi.fn(),
}))

const SSO_REQUIRED_BODY = { errors: [{ reason: 'SSO_REQUIRED', message: 'requires SSO' }] }

// The server's side: the session's org, which orgs require SSO, and how the
// next POST /api/me/active-org answers.
let sessionOrg: string
let barredIds: Set<string>
let moveAnswer: 'ok' | 'sso' | number
let fetchMock: ReturnType<typeof vi.fn>

function org(id: string, name: string): AuthOrg {
  return barredIds.has(id)
    ? { id, name, role: 'member', sso_required: true, sso_start_url: `/sso/${id}` }
    : { id, name, role: 'member', sso_required: false }
}

function me(): MeResponse {
  return {
    id: 'u1',
    orgs: [org('org-a', 'Alpha'), org('org-b', 'Beta'), org('org-c', 'Gamma')],
    active_org_id: sessionOrg,
    teams: [],
    org_creation_enabled: true,
    is_operator: false,
  }
}

function moveCalls() {
  return fetchMock.mock.calls.filter(([url]) => String(url) === '/api/me/active-org')
}

beforeEach(() => {
  sessionOrg = 'org-b'
  barredIds = new Set(['org-b'])
  moveAnswer = 'ok'
  fetchMock = vi.fn((url: unknown, init?: RequestInit) => {
    const path = String(url)
    if (path === '/api/me') {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(me()) })
    }
    if (path === '/api/me/active-org') {
      const target = (JSON.parse(String(init?.body)) as { org_id: string }).org_id
      if (moveAnswer === 'sso') {
        barredIds.add(target)
        return Promise.resolve({ ok: false, status: 403, ...jsonBody(SSO_REQUIRED_BODY) })
      }
      if (typeof moveAnswer === 'number') {
        return Promise.resolve({ ok: false, status: moveAnswer, ...jsonBody({ errors: [] }) })
      }
      sessionOrg = target
      return Promise.resolve({ ok: true, status: 200, ...jsonBody({}) })
    }
    return Promise.resolve({ ok: true, status: 200, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  vi.mocked(reconnectWebSocket).mockClear()
  window.localStorage.clear()
})

// The org the session was in when the page mounted, which is the org its
// first reads with no org in their path would act in.
let sessionOrgAtMount: string | null
function Page() {
  useEffect(() => {
    sessionOrgAtMount = sessionOrg
  }, [])
  return <p>the page</p>
}

function renderAt(path: string) {
  sessionOrgAtMount = null
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <OrgProvider>
          <Routes>
            <Route
              path="/orgs/:org_id/*"
              element={
                <AuthGate mode="multi">
                  <Page />
                </AuthGate>
              }
            />
          </Routes>
        </OrgProvider>
      </AuthProvider>
    </MemoryRouter>,
  )
}

describe('MultiAuthGate with the session in an SSO-required org', () => {
  it('moves the session to the org on screen before rendering its page', async () => {
    renderAt('/orgs/org-a/board')

    expect(await screen.findByText('the page')).toBeInTheDocument()
    expect(moveCalls()).toHaveLength(1)
    expect(moveCalls()[0][1]).toEqual(
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ org_id: 'org-a' }) }),
    )
    expect(sessionOrgAtMount).toBe('org-a')
    expect(reconnectWebSocket).toHaveBeenCalledTimes(1)
  })

  it('leaves a session org that is usable where it is', async () => {
    sessionOrg = 'org-c'
    renderAt('/orgs/org-a/board')

    expect(await screen.findByText('the page')).toBeInTheDocument()
    expect(moveCalls()).toHaveLength(0)
    expect(sessionOrgAtMount).toBe('org-c')
  })

  it('shows the sign-in, and moves nothing, when the org on screen is the barred one', async () => {
    renderAt('/orgs/org-b/board')

    expect(await screen.findByText('This organization requires single sign-on')).toBeInTheDocument()
    expect(moveCalls()).toHaveLength(0)
  })

  it('renders the page after one failed attempt', async () => {
    moveAnswer = 500
    renderAt('/orgs/org-a/board')

    expect(await screen.findByText('the page')).toBeInTheDocument()
    // Let a second attempt surface if a failure were retried.
    await new Promise((r) => setTimeout(r, 20))
    expect(moveCalls()).toHaveLength(1)
  })

  it('shows the sign-in when the org on screen started requiring SSO too', async () => {
    moveAnswer = 'sso'
    renderAt('/orgs/org-a/board')

    await waitFor(() => expect(moveCalls()).toHaveLength(1))
    expect(await screen.findByText('This organization requires single sign-on')).toBeInTheDocument()
    expect(screen.getByText('Alpha')).toBeInTheDocument()
    expect(screen.queryByText('the page')).not.toBeInTheDocument()
  })
})
