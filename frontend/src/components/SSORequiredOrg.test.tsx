// The SSO-required org: an org whose every org-scoped call answers 403
// SSO_REQUIRED for this session. What's pinned is what the viewer gets in its
// place — the interstitial on the org's route (org name, the requirement, and a
// link to the org's own sign-in), the org picker offering that same sign-in
// rather than a switch the server would refuse, and a way to another org the
// session can use.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import AuthGate from '../AuthGate'
import OrgPicker from './OrgPicker'
import { OrgProvider } from '../contexts/OrgContext'
import { jsonBody } from '../test/apiResponse'
import type { AuthOrg } from '../types'

const authMock = vi.hoisted(() => ({ orgs: [] as AuthOrg[] }))
vi.mock('../contexts/AuthContext', () => {
  const value = () => ({
    status: 'authed',
    me: { id: 'u1', orgs: authMock.orgs },
    orgs: authMock.orgs,
    serverActiveOrgId: null,
    error: null,
    refresh: vi.fn(),
    logout: vi.fn(),
  })
  return { useAuth: value, useOptionalAuth: value }
})
vi.mock('../hooks/useWebSocket', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../hooks/useWebSocket')>()),
  reconnectWebSocket: vi.fn(),
}))

const START_URL = '/api/auth/oauth/saml?provider_id=prov-x&return_to=%2F'
const barred: AuthOrg = {
  id: 'org-x',
  name: 'Acme',
  role: 'member',
  sso_required: true,
  sso_start_url: START_URL,
}
const open: AuthOrg = { id: 'org-y', name: 'Beta', role: 'admin', sso_required: false }

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, ...jsonBody({}) })
  vi.stubGlobal('fetch', fetchMock)
  window.localStorage.clear()
})

function activeOrgCalls() {
  return fetchMock.mock.calls.filter(([url]) => String(url) === '/api/me/active-org')
}

function Where() {
  return <p>at {useLocation().pathname}</p>
}

function renderGate(at: string) {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <OrgProvider>
        <Routes>
          <Route
            path="/orgs/:org_id/*"
            element={
              <AuthGate mode="multi">
                <p>the page</p>
                <Where />
              </AuthGate>
            }
          />
        </Routes>
      </OrgProvider>
    </MemoryRouter>,
  )
}

describe('SSO-required interstitial', () => {
  it('replaces the page on the route of an org that requires SSO', () => {
    authMock.orgs = [barred, open]
    renderGate('/orgs/org-x/board')

    expect(screen.getByText('Acme')).toBeInTheDocument()
    expect(screen.getByText('This organization requires single sign-on')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Sign in with single sign-on' })).toHaveAttribute(
      'href',
      START_URL,
    )
    expect(screen.queryByText('the page')).not.toBeInTheDocument()
  })

  it('renders the page for an org the session can use', () => {
    authMock.orgs = [barred, open]
    renderGate('/orgs/org-y/board')

    expect(screen.getByText('the page')).toBeInTheDocument()
    expect(screen.queryByText('This organization requires single sign-on')).not.toBeInTheDocument()
  })

  it('switches to another usable org, moving the session there', async () => {
    authMock.orgs = [barred, open]
    renderGate('/orgs/org-x/board')

    fireEvent.click(screen.getByRole('button', { name: 'Beta' }))

    await waitFor(() => expect(activeOrgCalls()).toHaveLength(1))
    expect(activeOrgCalls()[0][1]).toEqual(
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ org_id: 'org-y' }) }),
    )
    expect(await screen.findByText('at /orgs/org-y/board')).toBeInTheDocument()
  })

  it('offers no switch when every org requires SSO', () => {
    authMock.orgs = [barred]
    renderGate('/orgs/org-x')

    expect(screen.queryByText('Or switch to')).not.toBeInTheDocument()
  })
})

describe('OrgPicker with an SSO-required org', () => {
  function renderPicker() {
    return render(
      <MemoryRouter initialEntries={['/orgs/org-y/board']}>
        <OrgProvider>
          <Routes>
            <Route
              path="/orgs/:org_id/*"
              element={
                <>
                  <OrgPicker />
                  <Where />
                </>
              }
            />
          </Routes>
        </OrgProvider>
      </MemoryRouter>,
    )
  }

  it("offers the org's sign-in and never calls /api/me/active-org for it", () => {
    authMock.orgs = [barred, open]
    renderPicker()

    fireEvent.click(screen.getByRole('button', { name: 'Beta' }))
    const link = screen.getByRole('link', { name: /Acme.*Sign in with single sign-on/ })
    expect(link).toHaveAttribute('href', START_URL)
    expect(screen.queryByRole('option', { name: /Acme/ })).not.toBeInTheDocument()

    // jsdom does not navigate documents; stop the click at the link so what is
    // left to observe is whether the picker itself made a call.
    link.addEventListener('click', (e) => e.preventDefault())
    fireEvent.click(link)
    expect(activeOrgCalls()).toHaveLength(0)
  })

  it('still switches to an org the session can use', async () => {
    const third: AuthOrg = { id: 'org-z', name: 'Gamma', role: 'member', sso_required: false }
    authMock.orgs = [barred, open, third]
    renderPicker()

    fireEvent.click(screen.getByRole('button', { name: 'Beta' }))
    fireEvent.click(screen.getByRole('option', { name: /Gamma/ }))

    await waitFor(() => expect(activeOrgCalls()).toHaveLength(1))
    expect(await screen.findByText('at /orgs/org-z/board')).toBeInTheDocument()
  })
})
